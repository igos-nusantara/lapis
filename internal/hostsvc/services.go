package hostsvc

import (
	"context"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/igos-nusantara/lapis/internal/binder"
	"github.com/igos-nusantara/lapis/internal/compat"
)

// Package state changes reported by IUserMonitor.
const (
	PackageAdded   = 0
	PackageRemoved = 1
	PackageUpdated = 2
)

// Hooks connects the services to the rest of lapis.
type Hooks struct {
	UserUnlocked   func(uid int32)
	PackageChanged func(mode int32, pkg string)
	Suspend        func()
	Reboot         func()
	// DesktopEntryPrefix names the .desktop files ("lapis.").
	DesktopEntryPrefix string
}

type service struct {
	name  string
	iface string
	fn    binder.Handler
}

// Run publishes the host services and re-publishes them whenever Android
// (and with it the service manager) restarts. It returns when ctx ends.
func Run(ctx context.Context, c *binder.Conn, h Hooks) {
	notif, err := newNotifier(c, h.DesktopEntryPrefix)
	if err != nil {
		slog.Info("notifications disabled", "err", err)
	}
	services := []service{
		{compat.Service("usermonitor"), compat.Interface("IUserMonitor"), userMonitor(h)},
		{compat.Service("clipboard"), compat.Interface("IClipboard"), clipboard()},
		{compat.Service("hardware"), compat.Interface("IHardware"), hardware(h)},
	}
	if notif != nil {
		services = append(services, service{compat.Service("notifications"), compat.Interface("INotifications"), notif.handle})
	}
	locals := make([]*binder.Local, len(services))
	for i, s := range services {
		locals[i] = c.NewLocal(s.iface, s.fn)
	}

	for ctx.Err() == nil {
		// Wait for a live service manager.
		for ctx.Err() == nil {
			if _, err := c.ListServices(); err == nil {
				break
			}
			sleep(ctx, time.Second)
		}
		ok := true
		for i, s := range services {
			if err := c.AddService(s.name, locals[i]); err != nil {
				slog.Warn("binder addService failed", "service", s.name, "err", err)
				ok = false
			}
		}
		if !ok {
			sleep(ctx, 2*time.Second)
			continue
		}
		slog.Info("host binder services published")
		// Handle 0 cannot carry a death notification (the kernel keeps no
		// reference for it), but servicemanager registers itself as
		// "manager", which gives us a real handle to watch.
		died := make(chan struct{})
		if h, ok, err := c.CheckService("manager"); err == nil && ok {
			c.LinkToDeath(h, func() { close(died) })
		} else {
			slog.Warn("cannot watch the service manager", "err", err)
		}
		select {
		case <-ctx.Done():
		case <-died:
			slog.Info("Android service manager died; waiting to republish")
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func userMonitor(h Hooks) binder.Handler {
	return func(code uint32, r *binder.Reader, reply *binder.Parcel) error {
		switch code {
		case 1: // userUnlocked(int uid)
			uid, err := r.ReadInt32()
			if err != nil {
				return err
			}
			if h.UserUnlocked != nil {
				go h.UserUnlocked(uid)
			}
		case 2: // packageStateChanged(int mode, String pkg, int uid)
			mode, err := r.ReadInt32()
			if err != nil {
				return err
			}
			pkg, err := r.ReadString16()
			if err != nil {
				return err
			}
			if h.PackageChanged != nil {
				go h.PackageChanged(mode, pkg)
			}
		default:
			return errUnknown
		}
		reply.WriteInt32(0)
		return nil
	}
}

// clipboard bridges to the Wayland clipboard through wl-clipboard, like
// other desktop tools do.
func clipboard() binder.Handler {
	return func(code uint32, r *binder.Reader, reply *binder.Parcel) error {
		switch code {
		case 1: // sendClipboardData(String)
			text, err := r.ReadString16()
			if err != nil {
				return err
			}
			if err := exec.Command("wl-copy", "--", text).Run(); err != nil {
				slog.Debug("wl-copy failed", "err", err)
			}
			reply.WriteInt32(0)
		case 2: // String getClipboardData()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, "wl-paste", "--no-newline", "--type", "text/plain").Output()
			if err != nil {
				out = nil
			}
			reply.WriteInt32(0)
			reply.WriteString16(string(out))
		default:
			return errUnknown
		}
		return nil
	}
}

func hardware(h Hooks) binder.Handler {
	return func(code uint32, r *binder.Reader, reply *binder.Parcel) error {
		switch code {
		case 1, 2: // enableNFC / enableBluetooth(boolean) -> int
			reply.WriteInt32(0)
			reply.WriteInt32(0)
			return nil
		case 3: // suspend()
			if h.Suspend != nil {
				go h.Suspend()
			}
		case 4: // reboot()
			if h.Reboot != nil {
				go h.Reboot()
			}
		case 5, 6: // upgrade: images are updated with `lapis upgrade`
			slog.Info("Android requested an OTA upgrade; use `sudo lapis upgrade`")
		case 7: // shutdownRequest(String reason)
			reason, _ := r.ReadString16()
			slog.Info("Android is shutting down", "reason", strings.TrimSpace(reason))
		default:
			return errUnknown
		}
		reply.WriteInt32(0)
		return nil
	}
}
