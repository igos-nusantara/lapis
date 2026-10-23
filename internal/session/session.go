// Package session is the unprivileged side: it describes the user's
// desktop session to the control service, waits for Android, and keeps
// .desktop launchers in sync.
package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/igos-nusantara/lapis/internal/api"
	"github.com/igos-nusantara/lapis/internal/compat"
	"github.com/igos-nusantara/lapis/internal/config"
	"github.com/igos-nusantara/lapis/internal/control"
	"github.com/igos-nusantara/lapis/internal/hostsvc"
	"github.com/igos-nusantara/lapis/internal/wlproxy"
)

// FromEnv builds a session description from the caller's environment.
func FromEnv() (*api.Session, error) {
	u, err := user.Current()
	if err != nil {
		return nil, err
	}
	if os.Geteuid() == 0 {
		return nil, errors.New("start the session as your desktop user, not with sudo")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	s := &api.Session{
		UserName:         u.Username,
		UID:              os.Getuid(),
		GID:              os.Getgid(),
		PID:              os.Getpid(),
		XDGDataHome:      envOr("XDG_DATA_HOME", filepath.Join(home, ".local/share")),
		XDGRuntimeDir:    os.Getenv("XDG_RUNTIME_DIR"),
		WaylandDisplay:   envOr("WAYLAND_DISPLAY", "wayland-0"),
		PulseRuntimePath: os.Getenv("PULSE_RUNTIME_PATH"),
		LCDDensity:       envOr("LAPIS_DPI", "0"),
		BackgroundStart:  true,
	}
	if s.XDGRuntimeDir == "" {
		return nil, errors.New("XDG_RUNTIME_DIR is not set; run this from your desktop session")
	}
	if s.PulseRuntimePath == "" {
		s.PulseRuntimePath = filepath.Join(s.XDGRuntimeDir, "pulse")
	}
	wl := s.WaylandDisplay
	if !filepath.IsAbs(wl) {
		wl = filepath.Join(s.XDGRuntimeDir, wl)
	}
	if _, err := os.Stat(wl); err != nil {
		return nil, fmt.Errorf("Wayland socket %s not found; lapis needs a Wayland compositor", wl)
	}
	// A socket file can outlive its compositor; Android's display would
	// then never come up, so check that someone is listening.
	conn, err := net.DialTimeout("unix", wl, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("no Wayland compositor is listening on %s (stale WAYLAND_DISPLAY?): %w", wl, err)
	}
	conn.Close()
	s.DataDir = filepath.Join(s.XDGDataHome, "lapis", "data")
	if err := os.MkdirAll(s.DataDir, 0o755); err != nil {
		return nil, err
	}
	return s, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// Run starts the container for this session and stays attached until the
// container stops or ctx is cancelled (which stops the container).
func Run(ctx context.Context, self string) error {
	s, err := FromEnv()
	if err != nil {
		return err
	}
	proxied := true
	if err := startWaylandProxy(ctx, s); err != nil {
		slog.Warn("window fitting disabled", "err", err)
		proxied = false
	}
	before, _ := control.Call(api.Request{Op: api.OpStatus})
	if _, err := control.Call(api.Request{Op: api.OpStart, Session: s}); err != nil {
		return err
	}
	// The container binds the session's Wayland socket when it starts. A
	// container left running by an earlier session holds that session's
	// proxy socket, which this session has just replaced, so Android's
	// display could never connect again: restart it onto the new one.
	if proxied && before != nil && before.State != api.StateStopped {
		slog.Info("restarting the Android container left by an earlier session")
		if _, err := control.Call(api.Request{Op: api.OpRestart}); err != nil {
			return err
		}
	}
	slog.Info("Android container started, waiting for boot")

	go watchSleep(ctx)
	conn, err := runHostServices(ctx, s, self)
	if err != nil {
		slog.Warn("host binder services unavailable", "err", err)
	}
	var p *hostsvc.Platform
	if waitBoot(ctx) {
		slog.Info("Android booted")
		if conn != nil {
			p, _ = Platform(conn, 30*time.Second)
		}
		if err := SyncDesktopEntries(s, self, p); err != nil {
			slog.Warn("desktop entries not updated", "err", err)
		}
	}

	// Package events come from the image's launcher and are missed while
	// it is not running, so the app list is also compared periodically.
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	var pruner Pruner
	state := api.StateRunning
	stoppedOnce := false
	// Periodic work talks to Android over binder, which blocks while the
	// container is frozen; it runs beside the main loop so stopping the
	// session never waits on it.
	busy := make(chan struct{}, 1)
	unhealthy := 0 // consecutive periodic runs Android did not answer
	periodic := func(tick int) {
		defer func() { <-busy }()
		if err := pruner.Prune(s); err != nil {
			slog.Warn("pruning desktop entries failed", "err", err)
			// Android's own watchdog restarts a stuck system_server within a
			// minute or two; when that does not bring it back, restart the
			// whole container.
			if unhealthy++; unhealthy >= unhealthyLimit {
				slog.Warn("Android has not answered for a while; restarting the container")
				unhealthy = 0
				if _, err := control.Call(api.Request{Op: api.OpRestart}); err != nil {
					slog.Warn("restarting Android failed", "err", err)
				}
			}
		} else {
			unhealthy = 0
		}
		if conn == nil {
			return
		}
		if p == nil {
			p, _ = Platform(conn, time.Second)
		} else if _, err := p.GetProp("sys.boot_completed", ""); err != nil {
			// Android restarted: the old handle is gone.
			p, _ = Platform(conn, time.Second)
		}
		if p != nil && tick%12 == 0 {
			if err := SyncDesktopEntries(s, self, p); err != nil {
				slog.Warn("desktop sync failed", "err", err)
			}
		}
	}
	for tick := 1; ; tick++ {
		// Leave a suspended (frozen) Android asleep.
		if tick%3 == 0 && state == api.StateRunning {
			select {
			case busy <- struct{}{}:
				go periodic(tick)
			default: // previous run still busy
			}
		}
		select {
		case <-ctx.Done():
			slog.Info("stopping Android container")
			_, err := control.Call(api.Request{Op: api.OpStop})
			return err
		case <-t.C:
			resp, err := control.Call(api.Request{Op: api.OpStatus})
			if err != nil {
				return err
			}
			// Two polls in a row, so a restart between them is not taken
			// for the end of the session.
			if resp.State == api.StateStopped {
				if stoppedOnce {
					slog.Info("Android container stopped")
					return nil
				}
				stoppedOnce = true
				continue
			}
			stoppedOnce = false
			state = resp.State
		}
	}
}

func waitBoot(ctx context.Context) bool {
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		resp, err := control.Call(api.Request{Op: api.OpGetprop, Prop: "sys.boot_completed"})
		if err == nil && resp.Value == "1" {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(2 * time.Second):
		}
	}
	slog.Warn("Android did not finish booting within 3 minutes")
	return false
}

// unhealthyLimit is how many periodic runs (15 s apart) Android may fail to
// answer before the session restarts the container: 3 minutes.
const unhealthyLimit = 12

// proxySocket is the Wayland socket Android connects to when windows are
// fitted to their apps.
const proxySocket = "lapis-wayland"

// startWaylandProxy puts the window-fitting proxy between Android and the
// compositor, unless the configuration keeps full-size windows.
func startWaylandProxy(ctx context.Context, s *api.Session) error {
	if cfg, err := config.Load(); err == nil && cfg.Display.FullSizeWindows {
		return nil
	}
	upstream := s.WaylandDisplay
	if !filepath.IsAbs(upstream) {
		upstream = filepath.Join(s.XDGRuntimeDir, upstream)
	}
	l, err := wlproxy.Listen(filepath.Join(s.XDGRuntimeDir, proxySocket))
	if err != nil {
		return err
	}
	appPrefix := compat.AppID("")
	opts := wlproxy.Options{
		IsApp: func(appID string) bool { return strings.HasPrefix(appID, appPrefix) },
		// The app follows its host window (resize, tiling, maximize).
		Resize: func(appID string, cur, want wlproxy.Size) {
			_, err := control.Call(api.Request{Op: api.OpResize,
				Package: strings.TrimPrefix(appID, appPrefix),
				ToWidth: int(want.W), ToHeight: int(want.H)})
			if err != nil {
				slog.Warn("resizing app to its window failed", "app", appID, "err", err)
			}
		},
	}
	go func() {
		err := wlproxy.Serve(ctx, l, upstream, opts)
		if err != nil {
			slog.Warn("wayland proxy stopped", "err", err)
		}
	}()
	s.WaylandDisplay = proxySocket
	return nil
}
