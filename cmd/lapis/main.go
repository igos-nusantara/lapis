// Command lapis runs Android (LineageOS images) in a systemd-nspawn
// container on the host kernel.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/igos-nusantara/lapis/internal/api"
	"github.com/igos-nusantara/lapis/internal/binder"
	"github.com/igos-nusantara/lapis/internal/compat"
	"github.com/igos-nusantara/lapis/internal/config"
	"github.com/igos-nusantara/lapis/internal/container"
	"github.com/igos-nusantara/lapis/internal/control"
	"github.com/igos-nusantara/lapis/internal/doctor"
	"github.com/igos-nusantara/lapis/internal/drivers"
	"github.com/igos-nusantara/lapis/internal/extras"
	"github.com/igos-nusantara/lapis/internal/images"
	"github.com/igos-nusantara/lapis/internal/netsvc"
	"github.com/igos-nusantara/lapis/internal/paths"
	"github.com/igos-nusantara/lapis/internal/session"
	"github.com/igos-nusantara/lapis/internal/sysutil"
	"github.com/igos-nusantara/lapis/internal/units"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `lapis - Android on systemd-nspawn

Setup (root):
  lapis doctor                     check whether this host can run Android
  lapis install [-prefix DIR]      install binary and systemd units
  lapis uninstall                  remove units (keeps images and data)
  lapis version
  lapis init [-type GAPPS|VANILLA] [-force]
                                      download Android images
  lapis upgrade                    download newer images

Daily use (desktop user):
  lapis session start              start Android for this session (foreground)
  lapis session stop
  lapis restart                    restart Android (the lapis-session unit)
  lapis status
  lapis app list
  lapis app launch <package>
  lapis app intent <action> <uri>
  lapis app install <file.apk>     install or update an app
  lapis app uninstall <package>
  lapis app packages               installed apps and their version codes
  lapis show-full-ui                show the whole Android desktop in one window
  sudo lapis display [tablet|phone|<dpi>|maximized|freeform|fit|full]
                                      show or set the layout (default tablet,
                                      apps maximized)
  lapis freeze | unfreeze
  lapis prop get <name>

Extras and tools:
  lapis adb connect                authorize this user's adb key and connect
  sudo lapis extras list | install <id> | remove <id>
                                      ARM translation: libhoudini (most apps) or libndk

Debugging (root):
  lapis shell [command...]         shell inside Android
  lapis logcat [args...]
  lapis gapps-id                   print the GSF id for Play certification
  lapis debug services             list binder services
  lapis debug call <service> <interface> <code> [s:str|i:int|b:bool]... [-> s|i]
  lapis debug app-id <package>|full-ui   Wayland app id of an Android window
`

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2:]); err != nil {
		// shell and logcat pass the Android command's exit status through.
		var ee *exec.ExitError
		if (os.Args[1] == "shell" || os.Args[1] == "logcat") && errors.As(err, &ee) {
			os.Exit(ee.ExitCode())
		}
		fmt.Fprintln(os.Stderr, "lapis:", err)
		os.Exit(1)
	}
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
}

func self() string {
	p, err := os.Executable()
	if err != nil {
		return "lapis"
	}
	return p
}

func run(cmd string, args []string) error {
	switch cmd {
	case "doctor":
		if !doctor.Run(os.Stdout) {
			return errors.New("this host cannot run lapis yet (see FAIL lines)")
		}
		return nil

	case "install":
		fs := flag.NewFlagSet("install", flag.ExitOnError)
		prefix := fs.String("prefix", "", "directory for the binary (default /usr/local/bin, or /var/lib/lapis/bin on read-only /usr)")
		fs.Parse(args)
		return units.Install(*prefix)

	case "uninstall":
		// "lapis uninstall com.example.app" is a likely slip for
		// "lapis app uninstall"; never remove lapis itself on it.
		if len(args) != 0 {
			return fmt.Errorf("lapis uninstall removes lapis itself and takes no arguments; to remove an app use: lapis app uninstall %s", args[0])
		}
		return units.Uninstall()

	case "init", "upgrade":
		return initImages(cmd, args)

	case "session":
		if len(args) != 1 {
			return errors.New("usage: lapis session start|stop|restart")
		}
		switch args[0] {
		case "start":
			ctx, cancel := signalContext()
			defer cancel()
			return session.Run(ctx, self())
		case "stop":
			_, err := control.Call(api.Request{Op: api.OpStop})
			return err
		case "restart":
			return sysutil.Run("systemctl", "--user", "restart", session.SessionUnit)
		}
		return fmt.Errorf("unknown session command %q", args[0])

	case "restart":
		return sysutil.Run("systemctl", "--user", "restart", session.SessionUnit)

	case "status":
		resp, err := control.Call(api.Request{Op: api.OpStatus})
		if err != nil {
			return err
		}
		fmt.Println("container:", resp.State)
		if resp.Session != nil {
			fmt.Println("user:     ", resp.Session.UserName)
			fmt.Println("data:     ", resp.Session.DataDir)
			fmt.Println("ip:       ", netsvc.GuestIP)
		}
		return nil

	case "app":
		return app(args)

	case "display":
		return displayCmd(args)

	case "show-full-ui":
		return session.ShowFullUI()

	case "freeze", "unfreeze":
		op := api.OpFreeze
		if cmd == "unfreeze" {
			op = api.OpUnfreeze
		}
		_, err := control.Call(api.Request{Op: op})
		return err

	case "prop":
		if len(args) != 2 || args[0] != "get" {
			return errors.New("usage: lapis prop get <name>")
		}
		resp, err := control.Call(api.Request{Op: api.OpGetprop, Prop: args[1]})
		if err != nil {
			return err
		}
		fmt.Println(resp.Value)
		return nil

	case "shell":
		return shell(args)

	case "logcat":
		return shell(append([]string{"/system/bin/logcat"}, args...))

	case "gapps-id":
		return gappsID()

	case "debug":
		return debug(args)

	case "adb":
		if len(args) != 1 || args[0] != "connect" {
			return errors.New("usage: lapis adb connect")
		}
		return adbConnect()

	case "extras":
		return extrasCmd(args)

	// Internal commands used by the systemd units.
	case "container":
		if len(args) > 0 && args[0] == "attach-exec" {
			return container.AttachExec(args[1:])
		}
		if len(args) != 1 {
			return errors.New("usage: lapis container prepare|exec|cleanup|init-shim")
		}
		switch args[0] {
		case "prepare":
			return container.Prepare()
		case "exec":
			return container.ExecFromSession()
		case "cleanup":
			return container.Cleanup()
		case "init-shim":
			return container.InitShim()
		}
		return fmt.Errorf("unknown container command %q", args[0])

	case "drivers":
		if len(args) != 1 || args[0] != "setup" {
			return errors.New("usage: lapis drivers setup")
		}
		// Also load loop and overlay here: the container unit's device
		// policy only covers device classes that exist when it starts.
		for _, m := range []string{"loop", "overlay"} {
			sysutil.Run("modprobe", "-q", m)
		}
		if _, err := drivers.Setup(); err != nil {
			return err
		}
		drivers.ProbeAshmem()
		return nil

	case "net":
		if len(args) != 1 || args[0] != "serve" {
			return errors.New("usage: lapis net serve")
		}
		ctx, cancel := signalContext()
		defer cancel()
		return netsvc.Serve(ctx)

	case "control":
		if len(args) != 1 || args[0] != "handle" {
			return errors.New("usage: lapis control handle")
		}
		return control.HandleStdio()

	case "version", "--version":
		fmt.Println("lapis", version)
		return nil

	case "units":
		fs := flag.NewFlagSet("units", flag.ExitOnError)
		bin := fs.String("bin", "/usr/bin/lapis", "installed binary path")
		out := fs.String("out", "", "staging root to write usr/lib/systemd/... into")
		fs.Parse(args)
		if *out == "" {
			return errors.New("usage: lapis units -bin /usr/bin/lapis -out DIR")
		}
		return units.Render(*bin, *out)

	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	}
	return fmt.Errorf("unknown command %q (see lapis help)", cmd)
}

func initImages(cmd string, args []string) error {
	if err := sysutil.RequireRoot(cmd); err != nil {
		return err
	}
	release, err := images.Lock()
	if err != nil {
		return err
	}
	defer release()

	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	sysType := fs.String("type", "", "system image type: GAPPS (Google Play) or VANILLA")
	force := fs.Bool("force", false, "re-download even if up to date")
	fs.Parse(args)

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cmd == "init" {
		if *sysType != "" {
			cfg.Images.SystemType = *sysType
		}
		cfg.Images.Arch = images.Arch()
		cfg.Images.SystemOTA = images.SystemOTA(cfg.Images)
		cfg.Images.VendorOTA = images.VendorOTA(cfg.Images)
		if err := cfg.Save(); err != nil {
			return err
		}
	} else if cfg.Images.SystemOTA == "" {
		return images.ErrNotInitialized
	}
	changed, err := images.Update(cfg, *force, os.Stdout)
	if err != nil {
		return err
	}
	if changed && container.State() != api.StateStopped {
		fmt.Println("Images updated; restart the session to use them.")
	}
	return nil
}

func app(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lapis app list | launch <package> | intent <action> <uri> | install <file.apk> | uninstall <package> | packages")
	}
	switch args[0] {
	case "list":
		apps, err := session.Apps()
		if err != nil {
			return err
		}
		for _, a := range apps {
			fmt.Printf("%-45s %s\n", a.Package, a.Label)
		}
		return nil
	case "launch":
		if len(args) != 2 {
			return errors.New("usage: lapis app launch <package>")
		}
		return session.Launch(args[1])
	case "install":
		if len(args) != 2 {
			return errors.New("usage: lapis app install <file.apk>")
		}
		path, err := filepath.Abs(args[1])
		if err != nil {
			return err
		}
		_, err = control.Call(api.Request{Op: api.OpInstall, Path: path})
		return err
	case "uninstall":
		if len(args) != 2 {
			return errors.New("usage: lapis app uninstall <package>")
		}
		_, err := control.Call(api.Request{Op: api.OpUninstall, Package: args[1]})
		return err
	case "packages":
		resp, err := control.Call(api.Request{Op: api.OpPackages})
		if err != nil {
			return err
		}
		for _, p := range resp.Packages {
			fmt.Printf("%-45s %d\n", p.Package, p.VersionCode)
		}
		return nil
	case "intent":
		if len(args) != 3 {
			return errors.New("usage: lapis app intent <action> <uri>")
		}
		return session.Intent(args[1], args[2])
	}
	return fmt.Errorf("unknown app command %q", args[0])
}

func shell(argv []string) error {
	if err := sysutil.RequireRoot("shell"); err != nil {
		return err
	}
	// Commands join the container's cgroup, so a frozen (suspended)
	// container must be thawed for them to run; freeze it again after.
	if container.State() == api.StateFrozen {
		if err := sysutil.Run("systemctl", "thaw", paths.ContainerUnit); err != nil {
			return err
		}
		defer sysutil.Run("systemctl", "freeze", paths.ContainerUnit)
	}
	s, _ := container.LoadSession()
	cmd, err := container.Command(s, argv...)
	if err != nil {
		return err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func gappsID() error {
	if err := sysutil.RequireRoot("gapps-id"); err != nil {
		return err
	}
	s, _ := container.LoadSession()
	id, err := container.Output(s, "/system/bin/sqlite3",
		"/data/data/com.google.android.gsf/databases/gservices.db",
		"select value from main where name = 'android_id';")
	if err != nil {
		return fmt.Errorf("could not read the GSF id (is this a GAPPS image and has Android finished booting?): %w", err)
	}
	if id == "" {
		return errors.New("GSF id not generated yet; open the Play Store once and retry")
	}
	fmt.Println(id)
	fmt.Println("Register it at https://www.google.com/android/uncertified , then clear Play Store data or restart the session.")
	return nil
}

func debug(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lapis debug services | call ... | app-id <package>|full-ui")
	}
	if args[0] == "app-id" && len(args) == 2 {
		if args[1] == "full-ui" {
			fmt.Println(compat.FullUIApp)
		} else {
			fmt.Println(compat.AppID(args[1]))
		}
		return nil
	}
	c, err := session.OpenBinder()
	if err != nil {
		return err
	}
	switch args[0] {
	case "services":
		names, err := c.ListServices()
		for _, n := range names {
			fmt.Println(n)
		}
		return err
	case "call":
		return debugCall(c, args[1:])
	}
	return fmt.Errorf("unknown debug command %q", args[0])
}

// debugCall performs a raw AIDL call; "-> s" or "-> i" reads one result.
func debugCall(c *binder.Conn, args []string) error {
	if len(args) < 3 {
		return errors.New("usage: lapis debug call <service> <interface> <code> [s:str|i:int|b:bool]... [-> s|i]")
	}
	// Short names (platform, clipboard, IPlatform, ...) expand to the
	// images' service and interface names.
	// The images' names win over Android's own services of the same
	// short name (e.g. "clipboard").
	if !strings.Contains(args[0], ".") {
		if _, ok, _ := c.CheckService(compat.Service(args[0])); ok {
			args[0] = compat.Service(args[0])
		}
	}
	if !strings.Contains(args[1], ".") {
		args[1] = compat.Interface(args[1])
	}
	h, ok, err := c.CheckService(args[0])
	if err != nil || !ok {
		return fmt.Errorf("service %s not found: %v", args[0], err)
	}
	code, err := strconv.Atoi(args[2])
	if err != nil {
		return err
	}
	p := binder.NewRequest(args[1])
	ret := ""
	rest := args[3:]
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		if a == "->" && i+1 < len(rest) {
			ret = rest[i+1]
			break
		}
		kind, val, _ := strings.Cut(a, ":")
		switch kind {
		case "s":
			p.WriteString16(val)
		case "i":
			n, err := strconv.Atoi(val)
			if err != nil {
				return err
			}
			p.WriteInt32(int32(n))
		case "b":
			p.WriteBool(val == "true")
		default:
			return fmt.Errorf("bad argument %q", a)
		}
	}
	r, err := c.Transact(h, uint32(code), p, 0)
	if err != nil {
		return err
	}
	if err := r.ReadException(); err != nil {
		return err
	}
	switch ret {
	case "s":
		v, err := r.ReadString16()
		fmt.Println(v)
		return err
	case "i":
		v, err := r.ReadInt32()
		fmt.Println(v)
		return err
	}
	return nil
}

func adbConnect() error {
	adb, err := exec.LookPath("adb")
	if err != nil {
		return errors.New("adb not found; install android-tools (or platform-tools)")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	keyPath := home + "/.android/adbkey"
	if _, err := os.Stat(keyPath + ".pub"); err != nil {
		os.MkdirAll(home+"/.android", 0o700)
		if err := exec.Command(adb, "keygen", keyPath).Run(); err != nil {
			return fmt.Errorf("adb keygen: %w", err)
		}
	}
	pub, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		return err
	}
	if _, err := control.Call(api.Request{Op: api.OpAdbKey, Key: string(pub)}); err != nil {
		return err
	}
	// adbd only rereads adb_keys on a new server connection.
	exec.Command(adb, "disconnect", netsvc.GuestIP.String()+":5555").Run()
	cmd := exec.Command(adb, "connect", netsvc.GuestIP.String()+":5555")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func extrasCmd(args []string) error {
	if len(args) == 0 || args[0] == "list" {
		installed := map[string]bool{}
		for _, id := range extras.Installed() {
			installed[id] = true
		}
		for _, e := range extras.Catalog {
			mark := " "
			if installed[e.ID] {
				mark = "*"
			}
			fmt.Printf("%s %-12s %s\n", mark, e.ID, e.Description)
		}
		return nil
	}
	if len(args) != 2 {
		return errors.New("usage: lapis extras list | install <id> | remove <id>")
	}
	if err := sysutil.RequireRoot("extras " + args[0]); err != nil {
		return err
	}
	switch args[0] {
	case "install":
		return extras.Install(args[1], os.Stdout)
	case "remove":
		return extras.Remove(args[1])
	}
	return fmt.Errorf("unknown extras command %q", args[0])
}

func displayCmd(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		dpi, err := cfg.Display.LCDDensity()
		if err != nil {
			return err
		}
		mode := cfg.Display.Mode
		if cfg.Display.Density > 0 {
			mode = "custom"
		}
		windows := config.WindowsFreeform
		if cfg.Display.MaximizeApps() {
			windows = config.WindowsMaximized
		}
		size := "fit to app"
		if cfg.Display.FullSizeWindows {
			size = "full display"
		}
		fmt.Printf("mode: %s\ndensity: %d dpi\nwindows: %s\nwindow size: %s\n", mode, dpi, windows, size)
		return nil
	}
	if len(args) != 1 {
		return errors.New("usage: lapis display [tablet|phone|<dpi>|maximized|freeform|fit|full]")
	}
	if err := sysutil.RequireRoot("display"); err != nil {
		return err
	}
	switch args[0] {
	case "fit", "full":
		cfg.Display.FullSizeWindows = args[0] == "full"
	case config.WindowsMaximized, config.WindowsFreeform:
		cfg.Display.Windows = args[0]
	case config.DisplayTablet, config.DisplayPhone:
		cfg.Display = config.Display{Mode: args[0], FullSizeWindows: cfg.Display.FullSizeWindows}
	default:
		dpi, err := strconv.Atoi(args[0])
		if err != nil || dpi < 80 || dpi > 640 {
			return errors.New("use tablet, phone, maximized, freeform, fit, full or a dpi between 80 and 640")
		}
		cfg.Display = config.Display{Mode: "custom", Density: dpi, Windows: cfg.Display.Windows, FullSizeWindows: cfg.Display.FullSizeWindows}
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Println("saved; restart the session to apply")
	return nil
}
