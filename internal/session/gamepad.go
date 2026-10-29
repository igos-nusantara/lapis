package session

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"

	"golang.org/x/sys/unix"

	"github.com/igos-nusantara/lapis/internal/api"
	"github.com/igos-nusantara/lapis/internal/control"
)

// syncGamepads asks the control service to hand the host's game
// controllers to Android.
func syncGamepads() {
	if _, err := control.Call(api.Request{Op: api.OpGamepads}); err != nil {
		slog.Warn("game controllers not passed to Android", "err", err)
	}
}

// watchInput syncs game controllers whenever an input device comes or goes.
func watchInput(ctx context.Context) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, unix.NETLINK_KOBJECT_UEVENT)
	if err != nil {
		slog.Warn("game controller hotplug disabled", "err", err)
		return
	}
	// Group 2 carries events udev has finished with, so its database
	// already says whether the device is a joystick.
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: 2}); err != nil {
		unix.Close(fd)
		slog.Warn("game controller hotplug disabled", "err", err)
		return
	}
	f := os.NewFile(uintptr(fd), "udev-monitor")
	go func() { <-ctx.Done(); f.Close() }()
	buf := make([]byte, 16*1024)
	for {
		n, err := f.Read(buf)
		if err != nil && (ctx.Err() != nil || !errors.Is(err, unix.ENOBUFS)) {
			if ctx.Err() == nil {
				slog.Warn("game controller hotplug stopped", "err", err)
			}
			return
		}
		// ENOBUFS: events were dropped, so sync anyway.
		if err != nil || bytes.Contains(buf[:n], []byte("SUBSYSTEM=input")) {
			syncGamepads()
		}
	}
}
