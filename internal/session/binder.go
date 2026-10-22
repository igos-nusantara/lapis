package session

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/igos-nusantara/lapis/internal/api"
	"github.com/igos-nusantara/lapis/internal/binder"
	"github.com/igos-nusantara/lapis/internal/control"
	"github.com/igos-nusantara/lapis/internal/drivers"
	"github.com/igos-nusantara/lapis/internal/hostsvc"
)

// OpenBinder connects to Android's binder context on the host.
func OpenBinder() (*binder.Conn, error) {
	nodes, err := drivers.Find()
	if err != nil {
		return nil, err
	}
	return binder.Open("/dev/" + nodes.Binder)
}

// Platform returns an IPlatform client, waiting up to timeout.
func Platform(c *binder.Conn, timeout time.Duration) (*hostsvc.Platform, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return hostsvc.GetPlatform(ctx, c)
}

// thawIfFrozen wakes the container before talking to it (binder calls to
// a frozen Android block) and reports whether it was frozen.
func thawIfFrozen() bool {
	if resp, err := control.Call(api.Request{Op: api.OpStatus}); err == nil && resp.State == api.StateFrozen {
		control.Call(api.Request{Op: api.OpUnfreeze})
		return true
	}
	return false
}

// Launch opens an app window, over binder when possible.
func Launch(pkg string) error {
	// Android's platform service accepts any string and reports success.
	if !api.ValidPackage(pkg) {
		return fmt.Errorf("invalid package name %q", pkg)
	}
	thawIfFrozen()
	if c, err := OpenBinder(); err == nil {
		if p, err := Platform(c, 5*time.Second); err == nil {
			err := p.Launch(pkg)
			if err == nil {
				// Resizing tasks needs root; the control service does it
				// when the display mode asks for maximized apps.
				control.Call(api.Request{Op: api.OpMaximize, Package: pkg})
				return nil
			}
			slog.Warn("binder launch failed, using control service", "err", err)
		}
	}
	_, err := control.Call(api.Request{Op: api.OpLaunch, Package: pkg})
	return err
}

// Intent opens an intent such as an app's settings page.
func Intent(action, uri string) error {
	thawIfFrozen()
	c, err := OpenBinder()
	if err != nil {
		return err
	}
	p, err := Platform(c, 5*time.Second)
	if err != nil {
		return err
	}
	pkg, err := p.Intent(action, uri)
	if err == nil && pkg != "" {
		control.Call(api.Request{Op: api.OpMaximize, Package: pkg})
	}
	return err
}

// ShowFullUI shows the whole Android desktop, over binder when possible.
func ShowFullUI() error {
	thawIfFrozen()
	if c, err := OpenBinder(); err == nil {
		if p, err := Platform(c, 5*time.Second); err == nil {
			if err := p.ShowFullUI(); err == nil {
				return nil
			}
		}
	}
	_, err := control.Call(api.Request{Op: api.OpFullUI})
	return err
}

// Apps lists launchable apps with their labels.
func Apps() ([]Entry, error) {
	// Listing is not a reason to keep Android awake: freeze it again after.
	if thawIfFrozen() {
		defer control.Call(api.Request{Op: api.OpFreeze})
	}
	var p *hostsvc.Platform
	if c, err := OpenBinder(); err == nil {
		p, _ = Platform(c, 5*time.Second)
	}
	return listEntries(p)
}

// runHostServices publishes the binder services for this session.
func runHostServices(ctx context.Context, s *api.Session, self string) (*binder.Conn, error) {
	c, err := OpenBinder()
	if err != nil {
		return nil, err
	}
	platform := func() *hostsvc.Platform {
		p, err := Platform(c, 30*time.Second)
		if err != nil {
			slog.Warn("IPlatform unavailable", "err", err)
			return nil
		}
		return p
	}
	hooks := hostsvc.Hooks{
		DesktopEntryPrefix: DesktopPrefix,
		UserUnlocked: func(uid int32) {
			slog.Info("Android user unlocked", "uid", uid)
			if err := SyncDesktopEntries(s, self, platform()); err != nil {
				slog.Warn("desktop entries not updated", "err", err)
			}
		},
		PackageChanged: func(mode int32, pkg string) {
			slog.Info("Android package changed", "package", pkg, "mode", mode)
			if mode == hostsvc.PackageRemoved {
				RemoveDesktopEntry(s, pkg)
				return
			}
			p := platform()
			if p == nil {
				return
			}
			info, err := p.AppInfo(pkg)
			if err != nil || info == nil {
				return
			}
			if !info.Launchable() {
				RemoveDesktopEntry(s, pkg)
				return
			}
			WriteDesktopEntry(s, self, Entry{Package: pkg, Label: info.Name})
		},
		Suspend: func() {
			slog.Info("Android requested suspend; freezing container")
			control.Call(api.Request{Op: api.OpFreeze})
		},
		Reboot: func() {
			slog.Info("Android requested reboot")
			control.Call(api.Request{Op: api.OpRestart})
		},
	}
	go hostsvc.Run(ctx, c, hooks)
	return c, nil
}
