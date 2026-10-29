// Package control is the privileged gateway between users and the
// container. systemd socket-activates one `lapis control handle` per
// connection (Accept=yes); each request is a single JSON line.
package control

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/igos-nusantara/lapis/internal/api"
	"github.com/igos-nusantara/lapis/internal/compat"
	"github.com/igos-nusantara/lapis/internal/config"
	"github.com/igos-nusantara/lapis/internal/container"
	"github.com/igos-nusantara/lapis/internal/paths"
	"github.com/igos-nusantara/lapis/internal/sysutil"
)

// Call sends one request to the control socket and waits for the answer.
func Call(req api.Request) (*api.Response, error) {
	conn, err := net.DialTimeout("unix", paths.ControlSocket, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("lapis control service not reachable (is lapis-control.socket enabled?): %w", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Minute))
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, err
	}
	var resp api.Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, err
	}
	if !resp.OK {
		return &resp, errors.New(resp.Error)
	}
	return &resp, nil
}

// HandleStdio serves the connection systemd passed as stdin.
func HandleStdio() error {
	f := os.NewFile(0, "control-socket")
	conn, err := net.FileConn(f)
	if err != nil {
		return err
	}
	defer conn.Close()
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return errors.New("stdin is not a unix socket")
	}
	cred, err := peerCred(uc)
	if err != nil {
		return err
	}
	conn.SetDeadline(time.Now().Add(3 * time.Minute))

	var req api.Request
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&req); err != nil {
		return err
	}
	resp, err := handle(cred, req)
	if err != nil {
		resp = &api.Response{Error: err.Error()}
	} else {
		resp.OK = true
	}
	return json.NewEncoder(conn).Encode(resp)
}

func peerCred(c *net.UnixConn) (*unix.Ucred, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return nil, err
	}
	var cred *unix.Ucred
	var credErr error
	err = raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	})
	if err != nil {
		return nil, err
	}
	return cred, credErr
}

var errFrozen = errors.New("Android is suspended (frozen); open an app or run `lapis unfreeze`")

var propName = regexp.MustCompile(`^[A-Za-z0-9_.\-:@]+$`)

func handle(cred *unix.Ucred, req api.Request) (*api.Response, error) {
	if req.Op == api.OpStart {
		return start(cred, req.Session)
	}
	if req.Op == api.OpStatus {
		resp := &api.Response{State: container.State()}
		if s, err := container.LoadSession(); err == nil && resp.State != api.StateStopped {
			resp.Session = s
		}
		return resp, nil
	}

	// Everything else is limited to root and the session owner.
	s, err := container.LoadSession()
	if err != nil || container.State() == api.StateStopped {
		return nil, errors.New("Android container is not running")
	}
	if cred.Uid != 0 && int(cred.Uid) != s.UID {
		return nil, errors.New("permission denied: container belongs to another user")
	}

	// Commands run inside a frozen container hang until it is thawed.
	// Opening an app is a reason to wake Android; queries are not.
	if container.State() == api.StateFrozen {
		switch req.Op {
		case api.OpLaunch, api.OpFullUI, api.OpInstall, api.OpUninstall:
			if err := sysutil.Run("systemctl", "thaw", paths.ContainerUnit); err != nil {
				return nil, err
			}
		case api.OpGetprop, api.OpApps, api.OpPackages, api.OpMaximize, api.OpResize, api.OpKeyboard:
			return nil, errFrozen
		}
	}

	switch req.Op {
	case api.OpStop:
		// A frozen container cannot be stopped until it is thawed.
		sysutil.Run("systemctl", "thaw", paths.ContainerUnit)
		return &api.Response{}, sysutil.Run("systemctl", "stop", paths.ContainerUnit)
	case api.OpRestart:
		return &api.Response{}, sysutil.Run("systemctl", "restart", paths.ContainerUnit)
	case api.OpFreeze:
		return &api.Response{}, sysutil.Run("systemctl", "freeze", paths.ContainerUnit)
	case api.OpUnfreeze:
		return &api.Response{}, sysutil.Run("systemctl", "thaw", paths.ContainerUnit)
	case api.OpGetprop:
		if !propName.MatchString(req.Prop) {
			return nil, errors.New("invalid property name")
		}
		v, err := container.Output(s, "/system/bin/getprop", req.Prop)
		return &api.Response{Value: v}, err
	case api.OpApps:
		apps, err := launcherApps(s)
		return &api.Response{Apps: apps}, err
	case api.OpLaunch:
		if !api.ValidPackage(req.Package) {
			return nil, errors.New("invalid package name")
		}
		return &api.Response{}, launch(s, req.Package)
	case api.OpMaximize:
		if !api.ValidPackage(req.Package) {
			return nil, errors.New("invalid package name")
		}
		return &api.Response{}, maximizeIfConfigured(s, req.Package)
	case api.OpResize:
		if !api.ValidPackage(req.Package) {
			return nil, errors.New("invalid package name")
		}
		return &api.Response{}, resizeTask(s, req)
	case api.OpInstall:
		return &api.Response{}, installAPK(s, cred, req.Path)
	case api.OpUninstall:
		if !api.ValidPackage(req.Package) {
			return nil, errors.New("invalid package name")
		}
		return &api.Response{}, uninstall(s, req.Package)
	case api.OpPackages:
		out, err := container.Output(s, "/system/bin/cmd", "package", "list", "packages", "-3", "--show-versioncode")
		if err != nil {
			return nil, err
		}
		return &api.Response{Packages: parsePackages(out)}, nil
	case api.OpAdbKey:
		return &api.Response{}, authorizeAdbKey(s, req.Key)
	case api.OpKeyboard:
		return &api.Response{}, applyKeyboard(s)
	case api.OpFullUI:
		return &api.Response{}, showFullUI(s)
	case api.OpGamepads:
		return &api.Response{}, syncGamepads()
	}
	return nil, fmt.Errorf("unknown operation %q", req.Op)
}

func start(cred *unix.Ucred, s *api.Session) (*api.Response, error) {
	if s == nil {
		return nil, errors.New("missing session")
	}
	if cred.Uid != 0 && int(cred.Uid) != s.UID {
		return nil, errors.New("cannot start a session on behalf of another user")
	}
	if state := container.State(); state != api.StateStopped {
		cur, err := container.LoadSession()
		if err == nil && cur.UID == s.UID {
			// Reattaching to a container left suspended: wake it up.
			if state == api.StateFrozen {
				if err := sysutil.Run("systemctl", "thaw", paths.ContainerUnit); err != nil {
					return nil, err
				}
			}
			return &api.Response{State: container.State(), Session: cur}, nil
		}
		return nil, errors.New("Android container is already used by another session")
	}
	if err := validateSession(s); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(paths.RunDir, 0o755); err != nil {
		return nil, err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(paths.SessionFile, data, 0o600); err != nil {
		return nil, err
	}
	if err := sysutil.Run("systemctl", "start", paths.ContainerUnit); err != nil {
		return nil, fmt.Errorf("%w (see: journalctl -u %s)", err, paths.ContainerUnit)
	}
	return &api.Response{State: container.State(), Session: s}, nil
}

// validateSession ensures every host path the user hands us is theirs, so
// the root service never bind-mounts someone else's files.
func validateSession(s *api.Session) error {
	owned := func(p string) error {
		if strings.ContainsAny(p, "\n\r:") || !filepath.IsAbs(p) {
			return fmt.Errorf("invalid path %q", p)
		}
		var st syscall.Stat_t
		if err := syscall.Stat(p, &st); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		if int(st.Uid) != s.UID {
			return fmt.Errorf("%s is not owned by uid %d", p, s.UID)
		}
		return nil
	}
	wl, err := container.WaylandSocket(s)
	if err != nil {
		return fmt.Errorf("wayland socket: %w", err)
	}
	if err := owned(wl); err != nil {
		return err
	}
	// Android chowns /data to its system user, so the data directory
	// itself may not belong to the user after the first boot; its parent
	// does. Resolve symlinks so the checked path is the one mounted.
	data, err := filepath.EvalSymlinks(s.DataDir)
	if err != nil {
		return fmt.Errorf("data dir: %w", err)
	}
	if err := owned(filepath.Dir(data)); err != nil {
		if err2 := owned(data); err2 != nil {
			return err
		}
	}
	s.DataDir = data
	if s.PulseRuntimePath != "" {
		if p := filepath.Join(s.PulseRuntimePath, "native"); sysutil.Exists(p) {
			if err := owned(p); err != nil {
				return err
			}
		}
	}
	return nil
}

func launcherApps(s *api.Session) ([]api.App, error) {
	out, err := container.Output(s, "/system/bin/cmd", "package", "query-activities",
		"--components", "-a", "android.intent.action.MAIN", "-c", "android.intent.category.LAUNCHER")
	if err != nil {
		return nil, err
	}
	var apps []api.App
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		comp := strings.TrimSpace(line)
		pkg, act, ok := strings.Cut(comp, "/")
		if !ok || !api.ValidPackage(pkg) || seen[pkg] {
			continue
		}
		seen[pkg] = true
		apps = append(apps, api.App{Package: pkg, Activity: act})
	}
	return apps, nil
}

// launch opens an app window: the images' hwcomposer only maps windows
// of packages listed in the active_apps property.
func launch(s *api.Session, pkg string) error {
	// Right after a thaw or a package change PackageManager can report a
	// partial list for several seconds.
	var apps []api.App
	for try := 0; try < 30; try++ {
		var err error
		if apps, err = launcherApps(s); err != nil {
			return err
		}
		if hasApp(apps, pkg) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	for _, a := range apps {
		if a.Package != pkg {
			continue
		}
		if _, err := container.Output(s, "/system/bin/setprop", compat.Prop("active_apps"), pkg); err != nil {
			return err
		}
		if _, err := container.Output(s, "/system/bin/am", "start",
			"-a", "android.intent.action.MAIN", "-c", "android.intent.category.LAUNCHER",
			"-n", a.Package+"/"+a.Activity); err != nil {
			return err
		}
		policy := "immersive.status=*"
		if v, _ := container.Output(s, "/system/bin/getprop", compat.PersistProp("multi_windows")); v == "true" {
			policy = "immersive.full=*"
		}
		if _, err := container.Output(s, "/system/bin/settings", "put", "global", "policy_control", policy); err != nil {
			return err
		}
		return maximizeIfConfigured(s, pkg)
	}
	return fmt.Errorf("%s has no launcher activity", pkg)
}

// showFullUI shows the whole Android desktop in one window.
func showFullUI(s *api.Session) error {
	if _, err := container.Output(s, "/system/bin/setprop", compat.Prop("active_apps"), compat.FullUIApp); err != nil {
		return err
	}
	if _, err := container.Output(s, "/system/bin/settings", "put", "global", "policy_control", "null*"); err != nil {
		return err
	}
	_, err := container.Output(s, "/system/bin/am", "start",
		"-a", "android.intent.action.MAIN", "-c", "android.intent.category.HOME")
	return err
}

// openOwnedAPK opens an APK the caller may hand to Android: a regular file
// it owns (root may pass any). The checks run on the opened file, so a
// link swapped in between cannot point the root service elsewhere.
func openOwnedAPK(path string, uid uint32) (*os.File, int64, error) {
	if !filepath.IsAbs(path) || !strings.HasSuffix(path, ".apk") {
		return nil, 0, errors.New("invalid APK path: an absolute path to an .apk file is needed")
	}
	// O_NONBLOCK: a FIFO must not hang the service before it is rejected.
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, 0, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		f.Close()
		return nil, 0, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		f.Close()
		return nil, 0, fmt.Errorf("%s is not a regular file", path)
	}
	if uid != 0 && st.Uid != uid {
		f.Close()
		return nil, 0, fmt.Errorf("%s is not owned by uid %d", path, uid)
	}
	return f, st.Size, nil
}

// installAPK installs or updates an app. The APK reaches PackageManager
// on stdin, so nothing is copied into Android's storage.
func installAPK(s *api.Session, cred *unix.Ucred, path string) error {
	f, size, err := openOwnedAPK(path, cred.Uid)
	if err != nil {
		return err
	}
	defer f.Close()
	cmd, err := container.Command(s, "/system/bin/cmd", "package", "install", "-r", "-S", strconv.FormatInt(size, 10))
	if err != nil {
		return err
	}
	cmd.Stdin = f
	out, err := cmd.CombinedOutput()
	if result := strings.TrimSpace(string(out)); !strings.Contains(result, "Success") {
		if result == "" && err != nil {
			result = err.Error()
		}
		return fmt.Errorf("install failed: %s", result)
	}
	return nil
}

func uninstall(s *api.Session, pkg string) error {
	cmd, err := container.Command(s, "/system/bin/cmd", "package", "uninstall", pkg)
	if err != nil {
		return err
	}
	out, _ := cmd.CombinedOutput()
	if result := strings.TrimSpace(string(out)); !strings.Contains(result, "Success") {
		return fmt.Errorf("uninstall failed: %s", result)
	}
	return nil
}

// parsePackages reads `cmd package list packages --show-versioncode`:
// "package:org.example versionCode:42" per line.
func parsePackages(out string) []api.Package {
	var pkgs []api.Package
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		name, ok := strings.CutPrefix(fields[0], "package:")
		if !ok || !api.ValidPackage(name) {
			continue
		}
		p := api.Package{Package: name}
		for _, f := range fields[1:] {
			if v, ok := strings.CutPrefix(f, "versionCode:"); ok {
				p.VersionCode, _ = strconv.ParseInt(v, 10, 64)
			}
		}
		pkgs = append(pkgs, p)
	}
	return pkgs
}

var adbKey = regexp.MustCompile(`^[A-Za-z0-9+/=]{100,}( [^\n\r]{0,256})?$`)

// authorizeAdbKey adds a host adb public key to Android's adb_keys, so
// `adb connect` works with ro.adb.secure=1 without an on-screen prompt.
func authorizeAdbKey(s *api.Session, key string) error {
	key = strings.TrimSpace(key)
	if len(key) > 4096 || !adbKey.MatchString(key) {
		return errors.New("invalid adb public key")
	}
	dir := filepath.Join(s.DataDir, "misc", "adb")
	if err := os.MkdirAll(dir, 0o2750); err != nil {
		return err
	}
	path := filepath.Join(dir, "adb_keys")
	existing, _ := os.ReadFile(path)
	if strings.Contains(string(existing), key) {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(key + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// system:shell, as Android's adbd expects.
	return os.Chown(path, 1000, 2000)
}

var (
	taskLine  = regexp.MustCompile(`taskId=(\d+): ([A-Za-z0-9_.]+)/`)
	appBounds = regexp.MustCompile(`mAppBounds=Rect\((\d+), (\d+) - (\d+), (\d+)\)`)
)

// maximizeIfConfigured resizes the package's task to the display's app
// area, which gives apps their tablet layout instead of Android's
// phone-sized freeform window. The task appears shortly after launch.
// applyKeyboard shows or hides Android's on-screen keyboard while the
// desktop's keyboard (which Android sees as a hardware keyboard) is there.
func applyKeyboard(s *api.Session) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	show := "0"
	if cfg.Input.SoftKeyboard {
		show = "1"
	}
	_, err = container.Output(s, "/system/bin/settings", "put", "secure", "show_ime_with_hard_keyboard", show)
	return err
}

func maximizeIfConfigured(s *api.Session, pkg string) error {
	cfg, err := config.Load()
	if err != nil || !cfg.Display.MaximizeApps() {
		return err
	}
	// The host window already decided this app's size (tiling, a resize
	// by the user): don't undo it.
	if fi, err := os.Stat(resizedStamp(pkg)); err == nil && time.Since(fi.ModTime()) < 15*time.Second {
		return nil
	}
	for try := 0; try < 25; try++ {
		out, err := container.Output(s, "/system/bin/am", "stack", "list")
		if err != nil {
			return err
		}
		// Old tasks of the same app can linger in recents; only the newest
		// visible one is the window just opened.
		task, newest, bounds := "", -1, []string(nil)
		for _, line := range strings.Split(out, "\n") {
			if m := taskLine.FindStringSubmatch(line); m != nil && m[2] == pkg && strings.Contains(line, "visible=true") {
				if id, _ := strconv.Atoi(m[1]); id > newest {
					task, newest = m[1], id
				}
			}
			if strings.Contains(line, "mActivityType=home") && bounds == nil {
				if m := appBounds.FindStringSubmatch(line); m != nil {
					bounds = m[1:]
				}
			}
		}
		if task != "" && bounds != nil {
			_, err := container.Output(s, append([]string{"/system/bin/am", "task", "resize", task}, bounds...)...)
			return err
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nil // not fatal: the app is open, just not maximized
}

var taskBounds = regexp.MustCompile(`taskId=(\d+): ([A-Za-z0-9_.]+)/.*bounds=\[(-?\d+),(-?\d+)\]\[(-?\d+),(-?\d+)\]`)

func resizedStamp(pkg string) string { return filepath.Join(paths.RunDir, "resized", pkg) }

// resizeTask makes an app's task as large as its host window. It keeps its
// position unless it would leave the display.
func resizeTask(s *api.Session, req api.Request) error {
	if req.ToWidth < 1 || req.ToHeight < 1 {
		return errors.New("invalid size")
	}
	os.MkdirAll(filepath.Dir(resizedStamp(req.Package)), 0o755)
	os.WriteFile(resizedStamp(req.Package), nil, 0o644)
	for try := 0; try < 25; try++ {
		out, err := container.Output(s, "/system/bin/am", "stack", "list")
		if err != nil {
			return err
		}
		task, newest := "", -1
		var l, t, r, b int
		var disp []int
		for _, line := range strings.Split(out, "\n") {
			if m := taskBounds.FindStringSubmatch(line); m != nil && m[2] == req.Package && strings.Contains(line, "visible=true") {
				if id, _ := strconv.Atoi(m[1]); id > newest {
					task, newest = m[1], id
					l, _ = strconv.Atoi(m[3])
					t, _ = strconv.Atoi(m[4])
					r, _ = strconv.Atoi(m[5])
					b, _ = strconv.Atoi(m[6])
				}
			}
			if strings.Contains(line, "mActivityType=home") && disp == nil {
				if m := appBounds.FindStringSubmatch(line); m != nil {
					for _, v := range m[1:] {
						n, _ := strconv.Atoi(v)
						disp = append(disp, n)
					}
				}
			}
		}
		if task != "" && disp != nil {
			nl, nt, nr, nb := fitTask(l, t, r, b, req, disp)
			_, err := container.Output(s, "/system/bin/am", "task", "resize", task,
				strconv.Itoa(nl), strconv.Itoa(nt), strconv.Itoa(nr), strconv.Itoa(nb))
			return err
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nil
}

// fitTask places a ToWidth x ToHeight task where the task (l, t, r, b) is,
// inside the display's app area. The window's layer extent is not used:
// during transitions it can include display-sized layers.
func fitTask(l, t, r, b int, req api.Request, disp []int) (int, int, int, int) {
	const minTask = 200
	dl, dt, dr, db := disp[0], disp[1], disp[2], disp[3]
	w := max(minTask, min(dr-dl, req.ToWidth))
	h := max(minTask, min(db-dt, req.ToHeight))
	if l+w > dr {
		l = dr - w
	}
	if t+h > db {
		t = db - h
	}
	l, t = max(dl, l), max(dt, t)
	return l, t, l + w, t + h
}

func hasApp(apps []api.App, pkg string) bool {
	for _, a := range apps {
		if a.Package == pkg {
			return true
		}
	}
	return false
}
