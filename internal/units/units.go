// Package units installs lapis's systemd units and binary.
package units

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/igos-nusantara/lapis/internal/paths"
	"github.com/igos-nusantara/lapis/internal/sysutil"
)

const (
	systemDir = "/etc/systemd/system"
	userDir   = "/etc/systemd/user"
)

// Unit templates; @BIN@ is replaced by the installed binary path.
var systemUnits = map[string]string{
	"lapis-control.socket": `[Unit]
Description=Lapis control socket

[Socket]
ListenStream=` + paths.ControlSocket + `
SocketMode=0666
Accept=yes
MaxConnections=16
MaxConnectionsPerSource=8

[Install]
WantedBy=sockets.target
`,
	"lapis-control@.service": `[Unit]
Description=Lapis control request

[Service]
ExecStart=@BIN@ control handle
StandardInput=socket
StandardOutput=socket
StandardError=journal
TimeoutSec=3min
# Commands started inside Android (am, cmd, getprop) inherit this
# sandbox, so only restrictions that cannot break them are used.
ProtectSystem=full
PrivateTmp=yes
ProtectKernelModules=yes
RestrictSUIDSGID=yes
RestrictRealtime=yes
LockPersonality=yes
`,
	"lapis-net.service": `[Unit]
Description=Lapis container network (NAT, DHCP, DNS)
PartOf=` + paths.ContainerUnit + `
Before=` + paths.ContainerUnit + `

[Service]
Type=notify
ExecStart=@BIN@ net serve
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_BIND_SERVICE CAP_NET_RAW
# iptables(-legacy) takes this lock when adding Docker forwarding rules.
ReadWritePaths=-/run/xtables.lock
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_UNIX AF_NETLINK AF_INET AF_INET6
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
`,
	paths.BinderUnit: `[Unit]
Description=Lapis kernel setup (binder, loop, overlay)
Before=` + paths.ContainerUnit + `

# Loads binder, loop and overlay and creates the binderfs nodes outside
# the container unit: its device policy is resolved when it starts, so the
# modules must already be loaded for their device classes to exist.
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=@BIN@ drivers setup
`,
	paths.ContainerUnit: `[Unit]
Description=Lapis Android container
Requires=` + paths.BinderUnit + `
Wants=` + paths.NetworkUnit + `
After=` + paths.NetworkUnit + ` ` + paths.BinderUnit + `

[Service]
Type=notify
ExecStartPre=@BIN@ container prepare
ExecStart=@BIN@ container exec
ExecStopPost=@BIN@ container cleanup
KillMode=mixed
Delegate=yes
Slice=machine.slice
TasksMax=16384
# Only the devices Android and systemd-nspawn need (rwm: nspawn mknods
# the basic nodes inside the container). Missing paths are skipped.
DevicePolicy=closed
DeviceAllow=/dev/null rwm
DeviceAllow=/dev/zero rwm
DeviceAllow=/dev/full rwm
DeviceAllow=/dev/random rwm
DeviceAllow=/dev/urandom rwm
DeviceAllow=/dev/kmsg rwm
DeviceAllow=/dev/tty rwm
DeviceAllow=/dev/console rwm
DeviceAllow=/dev/ptmx rwm
DeviceAllow=char-pts rw
DeviceAllow=/dev/net/tun rwm
DeviceAllow=/dev/fuse rwm
DeviceAllow=/dev/uhid rwm
DeviceAllow=/dev/ashmem rwm
DeviceAllow=/dev/loop-control rw
DeviceAllow=block-loop rw
DeviceAllow=char-binder rw
DeviceAllow=char-rust_binder rw
DeviceAllow=char-drm rw
DeviceAllow=char-dma_heap rw
DeviceAllow=char-video4linux rw
# 133 = container rebooted itself (systemd-nspawn convention)
SuccessExitStatus=133
RestartForceExitStatus=133
RestartSec=1
`,
}

var userUnits = map[string]string{
	"lapis-session.service": `[Unit]
Description=Lapis Android session
After=graphical-session.target
PartOf=graphical-session.target

[Service]
ExecStart=@BIN@ session start
TimeoutStopSec=30
`,
}

// Render writes the units for a packaged install (binary at bin) into
// root/usr/lib/systemd/{system,user}.
func Render(bin, root string) error {
	for _, set := range []struct {
		dir   string
		units map[string]string
	}{{"usr/lib/systemd/system", systemUnits}, {"usr/lib/systemd/user", userUnits}} {
		dir := filepath.Join(root, set.dir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		for name, body := range set.units {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.ReplaceAll(body, "@BIN@", bin)), 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

// Install copies the running binary to prefix (or a writable fallback on
// read-only /usr systems), writes the units and enables the socket.
func Install(prefix string) error {
	if err := sysutil.RequireRoot("install"); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	bin, err := installBinary(self, prefix)
	if err != nil {
		return err
	}
	fmt.Println("installed binary:", bin)

	for _, set := range []struct {
		dir   string
		units map[string]string
	}{{systemDir, systemUnits}, {userDir, userUnits}} {
		if err := os.MkdirAll(set.dir, 0o755); err != nil {
			return err
		}
		for name, body := range set.units {
			p := filepath.Join(set.dir, name)
			if err := os.WriteFile(p, []byte(strings.ReplaceAll(body, "@BIN@", bin)), 0o644); err != nil {
				return err
			}
			fmt.Println("wrote", p)
		}
	}
	for _, d := range []string{paths.WorkDir, paths.ImagesDir, paths.OverlayDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	if err := sysutil.Run("systemctl", "daemon-reload"); err != nil {
		return err
	}
	return sysutil.Run("systemctl", "enable", "--now", paths.ControlSocketU)
}

func installBinary(self, prefix string) (string, error) {
	candidates := []string{prefix}
	if prefix == "" {
		// /usr/local is read-only on image-based systems such as SteamOS.
		candidates = []string{"/usr/local/bin", filepath.Join(paths.WorkDir, "bin")}
	}
	var lastErr error
	for _, dir := range candidates {
		dst := filepath.Join(dir, "lapis")
		if dst == self {
			return dst, nil
		}
		if err := copyFile(self, dst); err != nil {
			lastErr = err
			continue
		}
		return dst, nil
	}
	return "", lastErr
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".new"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// Uninstall stops everything and removes the units. Images and user data
// are kept.
func Uninstall() error {
	if err := sysutil.RequireRoot("uninstall"); err != nil {
		return err
	}
	_ = sysutil.Run("systemctl", "stop", paths.ContainerUnit, paths.NetworkUnit, paths.BinderUnit)
	_ = sysutil.Run("systemctl", "disable", "--now", paths.ControlSocketU)
	var errs []error
	for name := range systemUnits {
		if err := os.Remove(filepath.Join(systemDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	for name := range userUnits {
		if err := os.Remove(filepath.Join(userDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	_ = sysutil.Run("systemctl", "daemon-reload")
	fmt.Println("units removed; images in", paths.WorkDir, "and user data were kept")
	return errors.Join(errs...)
}
