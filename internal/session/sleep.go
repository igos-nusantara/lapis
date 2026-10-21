package session

import (
	"context"
	"log/slog"
	"syscall"

	"github.com/godbus/dbus/v5"

	"github.com/igos-nusantara/lapis/internal/api"
	"github.com/igos-nusantara/lapis/internal/control"
)

// watchSleep freezes the container when the host goes to sleep and thaws
// it on resume. A logind delay inhibitor gives the freeze time to finish.
func watchSleep(ctx context.Context) {
	bus, err := dbus.ConnectSystemBus()
	if err != nil {
		slog.Info("sleep handling disabled", "err", err)
		return
	}
	defer bus.Close()
	login := bus.Object("org.freedesktop.login1", "/org/freedesktop/login1")
	inhibit := func() int {
		var fd dbus.UnixFD
		err := login.Call("org.freedesktop.login1.Manager.Inhibit", 0,
			"sleep", "lapis", "Freeze Android before sleep", "delay").Store(&fd)
		if err != nil {
			slog.Warn("sleep inhibitor not granted", "err", err)
			return -1
		}
		return int(fd)
	}
	release := func(fd int) {
		if fd >= 0 {
			syscall.Close(fd)
		}
	}

	if err := bus.AddMatchSignal(
		dbus.WithMatchSender("org.freedesktop.login1"),
		dbus.WithMatchInterface("org.freedesktop.login1.Manager"),
		dbus.WithMatchMember("PrepareForSleep"),
	); err != nil {
		slog.Warn("sleep handling disabled", "err", err)
		return
	}
	signals := make(chan *dbus.Signal, 4)
	bus.Signal(signals)

	lock := inhibit()
	frozeIt := false
	for {
		select {
		case <-ctx.Done():
			release(lock)
			return
		case sig := <-signals:
			var sleeping bool
			if sig == nil || dbus.Store(sig.Body, &sleeping) != nil {
				continue
			}
			if sleeping {
				resp, err := control.Call(api.Request{Op: api.OpStatus})
				frozeIt = err == nil && resp.State == api.StateRunning
				if frozeIt {
					slog.Info("host is going to sleep; freezing Android")
					control.Call(api.Request{Op: api.OpFreeze})
				}
				release(lock)
				lock = -1
			} else {
				if frozeIt {
					slog.Info("host resumed; thawing Android")
					control.Call(api.Request{Op: api.OpUnfreeze})
					frozeIt = false
				}
				lock = inhibit()
			}
		}
	}
}
