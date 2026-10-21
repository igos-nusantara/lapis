// Package netsvc provides the container network without external tools:
// a host-side veth address, NAT via nftables, a DHCP server and a DNS
// forwarder.
package netsvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"time"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
	"github.com/miekg/dns"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/igos-nusantara/lapis/internal/sysutil"
)

const (
	// HostIf is the host end of the veth pair systemd-nspawn creates; the
	// container end is eth0.
	HostIf = "lapis0"

	tableName = "lapis"
	leaseTime = 24 * 60 * 60
)

// The address range the images' network setup expects.
var (
	HostIP  = net.IPv4(192, 168, 240, 1).To4()
	GuestIP = net.IPv4(192, 168, 240, 112).To4()
	Netmask = net.CIDRMask(24, 32)
	subnet  = net.IPv4(192, 168, 240, 0).To4()
)

// Serve runs until ctx is cancelled. It is the main process of the network
// unit and signals readiness once NAT rules are in place.
func Serve(ctx context.Context) error {
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o644); err != nil {
		return fmt.Errorf("enable ip_forward: %w", err)
	}
	if err := setupNft(); err != nil {
		return fmt.Errorf("nftables: %w", err)
	}
	defer func() {
		if err := teardownNft(); err != nil {
			slog.Warn("nftables cleanup failed", "err", err)
		}
	}()
	if dockerForward(true) {
		slog.Info("Docker firewall detected; allowed forwarding for " + HostIf + " in DOCKER-USER")
		defer dockerForward(false)
	}
	sysutil.Notify("READY=1")

	// The veth is created and destroyed with the container, and again on
	// every Android reboot, so serve one link lifetime at a time.
	for {
		link, err := waitLink(ctx)
		if err != nil {
			return nil // context cancelled
		}
		if err := configureLink(link); err != nil {
			slog.Error("configure link failed", "err", err)
		} else if err := serveLink(ctx); err != nil {
			slog.Error("network service failed", "err", err)
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

func waitLink(ctx context.Context) (netlink.Link, error) {
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		if l, err := netlink.LinkByName(HostIf); err == nil {
			return l, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

func configureLink(l netlink.Link) error {
	addr := &netlink.Addr{IPNet: &net.IPNet{IP: HostIP, Mask: Netmask}}
	if err := netlink.AddrAdd(l, addr); err != nil && !errors.Is(err, unix.EEXIST) {
		return err
	}
	return netlink.LinkSetUp(l)
}

// serveLink runs DHCP and DNS until the link disappears or ctx ends.
func serveLink(ctx context.Context) error {
	dhcpSrv, err := server4.NewServer(HostIf, &net.UDPAddr{IP: net.IPv4zero, Port: dhcpv4.ServerPort}, handleDHCP)
	if err != nil {
		return fmt.Errorf("dhcp: %w", err)
	}
	go dhcpSrv.Serve()
	defer dhcpSrv.Close()

	listen := net.JoinHostPort(HostIP.String(), "53")
	handler := dns.HandlerFunc(forwardDNS)
	servers := []*dns.Server{
		{Addr: listen, Net: "udp", Handler: handler},
		{Addr: listen, Net: "tcp", Handler: handler},
	}
	for _, s := range servers {
		go func(s *dns.Server) {
			if err := s.ListenAndServe(); err != nil {
				slog.Warn("dns server stopped", "net", s.Net, "err", err)
			}
		}(s)
	}
	defer func() {
		for _, s := range servers {
			s.Shutdown()
		}
	}()
	slog.Info("container network up", "if", HostIf, "host", HostIP, "guest", GuestIP)

	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if _, err := netlink.LinkByName(HostIf); err != nil {
				slog.Info("container link gone")
				return nil
			}
		}
	}
}

// handleDHCP always hands out GuestIP: there is exactly one client.
func handleDHCP(conn net.PacketConn, peer net.Addr, m *dhcpv4.DHCPv4) {
	var mt dhcpv4.MessageType
	switch m.MessageType() {
	case dhcpv4.MessageTypeDiscover:
		mt = dhcpv4.MessageTypeOffer
	case dhcpv4.MessageTypeRequest:
		mt = dhcpv4.MessageTypeAck
	default:
		return
	}
	reply, err := dhcpv4.NewReplyFromRequest(m,
		dhcpv4.WithMessageType(mt),
		dhcpv4.WithServerIP(HostIP),
		dhcpv4.WithYourIP(GuestIP),
		dhcpv4.WithNetmask(Netmask),
		dhcpv4.WithRouter(HostIP),
		dhcpv4.WithDNS(HostIP),
		dhcpv4.WithLeaseTime(leaseTime),
		dhcpv4.WithOption(dhcpv4.OptServerIdentifier(HostIP)),
	)
	if err != nil {
		slog.Warn("dhcp reply", "err", err)
		return
	}
	dst := peer
	if u, ok := peer.(*net.UDPAddr); ok && (u.IP == nil || u.IP.IsUnspecified()) {
		dst = &net.UDPAddr{IP: net.IPv4bcast, Port: dhcpv4.ClientPort}
	}
	if _, err := conn.WriteTo(reply.ToBytes(), dst); err != nil {
		slog.Warn("dhcp send", "err", err)
	}
}

// upstreams returns the host's resolvers. Loopback resolvers such as
// systemd-resolved's stub work because the forwarder runs on the host.
func upstreams() []string {
	data, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "nameserver" {
			out = append(out, f[1])
		}
	}
	return out
}

func forwardDNS(w dns.ResponseWriter, r *dns.Msg) {
	proto := "udp"
	if _, ok := w.RemoteAddr().(*net.TCPAddr); ok {
		proto = "tcp"
	}
	c := &dns.Client{Net: proto, Timeout: 4 * time.Second}
	for _, up := range upstreams() {
		resp, _, err := c.Exchange(r, net.JoinHostPort(up, "53"))
		if err == nil {
			w.WriteMsg(resp)
			return
		}
	}
	fail := new(dns.Msg)
	fail.SetRcode(r, dns.RcodeServerFailure)
	w.WriteMsg(fail)
}

func ifname(n string) []byte {
	b := make([]byte, 16)
	copy(b, n)
	return b
}

// setupNft installs the forwarding and NAT rules in lapis's own
// tables so they never touch other firewall rules.
func setupNft() error {
	if err := teardownNft(); err != nil {
		return err
	}
	c, err := nftables.New()
	if err != nil {
		return err
	}

	inet := c.AddTable(&nftables.Table{Family: nftables.TableFamilyINet, Name: tableName})
	input := c.AddChain(&nftables.Chain{
		Name: "input", Table: inet, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookInput, Priority: nftables.ChainPriorityFilter,
	})
	for _, proto := range []byte{unix.IPPROTO_UDP, unix.IPPROTO_TCP} {
		for _, port := range []uint16{53, 67} {
			c.AddRule(&nftables.Rule{Table: inet, Chain: input, Exprs: []expr.Any{
				&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifname(HostIf)},
				&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{proto}},
				&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.BigEndian.PutUint16(port)},
				&expr.Verdict{Kind: expr.VerdictAccept},
			}})
		}
	}
	forward := c.AddChain(&nftables.Chain{
		Name: "forward", Table: inet, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookForward, Priority: nftables.ChainPriorityFilter,
	})
	for _, key := range []expr.MetaKey{expr.MetaKeyIIFNAME, expr.MetaKeyOIFNAME} {
		c.AddRule(&nftables.Rule{Table: inet, Chain: forward, Exprs: []expr.Any{
			&expr.Meta{Key: key, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifname(HostIf)},
			&expr.Verdict{Kind: expr.VerdictAccept},
		}})
	}

	ip := c.AddTable(&nftables.Table{Family: nftables.TableFamilyIPv4, Name: tableName})
	post := c.AddChain(&nftables.Chain{
		Name: "postrouting", Table: ip, Type: nftables.ChainTypeNAT,
		Hooknum: nftables.ChainHookPostrouting, Priority: nftables.ChainPriorityNATSource,
	})
	mask := []byte(Netmask)
	c.AddRule(&nftables.Rule{Table: ip, Chain: post, Exprs: []expr.Any{
		// ip saddr 192.168.240.0/24
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 12, Len: 4},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: mask, Xor: []byte{0, 0, 0, 0}},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: subnet},
		// ip daddr != 192.168.240.0/24
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: mask, Xor: []byte{0, 0, 0, 0}},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: subnet},
		&expr.Masq{},
	}})
	return c.Flush()
}

func teardownNft() error {
	c, err := nftables.New()
	if err != nil {
		return err
	}
	tables, err := c.ListTables()
	if err != nil {
		return err
	}
	found := false
	for _, t := range tables {
		if t.Name == tableName {
			c.DelTable(t)
			found = true
		}
	}
	if !found {
		return nil
	}
	return c.Flush()
}

// dockerForwardRules let container traffic pass Docker's FORWARD chain,
// whose policy is DROP. DOCKER-USER is the chain Docker reserves for this.
var dockerForwardRules = [][]string{
	{"-i", HostIf, "-j", "ACCEPT"},
	{"-o", HostIf, "-j", "ACCEPT"},
}

// dockerForward adds (or removes) the DOCKER-USER rules. It reports
// whether Docker's chain exists.
func dockerForward(add bool) bool {
	if sysutil.Run("iptables", "-w", "-n", "-L", "DOCKER-USER") != nil {
		return false
	}
	for _, r := range dockerForwardRules {
		check := append([]string{"-w", "-C", "DOCKER-USER"}, r...)
		present := sysutil.Run("iptables", check...) == nil
		switch {
		case add && !present:
			if err := sysutil.Run("iptables", append([]string{"-w", "-I", "DOCKER-USER"}, r...)...); err != nil {
				slog.Warn("DOCKER-USER rule not added", "err", err)
			}
		case !add && present:
			sysutil.Run("iptables", append([]string{"-w", "-D", "DOCKER-USER"}, r...)...)
		}
	}
	return true
}
