// Package container runs Android under systemd-nspawn and executes
// commands inside it. The nspawn command line provides the container
// configuration the Android images expect.
package container

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/igos-nusantara/lapis/internal/api"
	"github.com/igos-nusantara/lapis/internal/compat"
	"github.com/igos-nusantara/lapis/internal/drivers"
	"github.com/igos-nusantara/lapis/internal/netsvc"
	"github.com/igos-nusantara/lapis/internal/paths"
	"github.com/igos-nusantara/lapis/internal/seccomp"
	"github.com/igos-nusantara/lapis/internal/sysutil"
)

// Capabilities Android needs that nspawn drops by default.
var extraCaps = []string{
	"CAP_WAKE_ALARM", "CAP_BLOCK_SUSPEND", "CAP_SYS_TIME",
	"CAP_NET_ADMIN", "CAP_SYSLOG", "CAP_IPC_LOCK",
}

// Capabilities nspawn keeps by default that Android does not need.
// CAP_SYS_BOOT stays so Android can end its own PID namespace on shutdown.
var dropCaps = []string{
	"CAP_AUDIT_WRITE", "CAP_IPC_OWNER", "CAP_LEASE", "CAP_LINUX_IMMUTABLE",
	"CAP_NET_BROADCAST", "CAP_SETFCAP", "CAP_SYS_TTY_CONFIG",
}

// Optional device nodes passed through when present on the host.
var optionalDevices = []string{
	"/dev/ashmem", "/dev/fuse", "/dev/ion", "/dev/uhid", "/dev/sw_sync",
	// Vendor GPU nodes.
	"/dev/kgsl-3d0", "/dev/mali0", "/dev/pvr_sync", "/dev/pmsg0", "/dev/dxg",
	// MediaTek media nodes.
	"/dev/Vcodec", "/dev/MTK_SMI", "/dev/mdp_sync", "/dev/mtk_cmdq",
}

var optionalDeviceGlobs = []string{
	"/dev/fb*", "/dev/graphics/fb*", "/dev/video*", "/dev/dma_heap/*",
}

// Optional host directories bound at the same path.
var optionalDirs = []string{
	"/dev/char",
	"/sys/module/lowmemorykiller",
	"/sys/kernel/debug",
	"/sys/class/leds/vibrator",
	"/sys/devices/virtual/timed_output/vibrator",
}

// Spec is the input for building the nspawn command line.
type Spec struct {
	Session    *api.Session
	Binder     drivers.Nodes
	RenderNode string
	// Self is the host path of the lapis binary, used as the shim.
	Self string
}

// Args returns the systemd-nspawn arguments for spec.
func Args(spec Spec) ([]string, error) {
	s := spec.Session
	args := []string{
		"--quiet",
		"--keep-unit",
		"--register=no",
		"--machine=" + paths.Machine,
		"--directory=" + paths.RootfsDir,
		"--read-only",
		"--hostname=" + paths.Machine,
		"--private-users=no",
		"--resolv-conf=off",
		"--timezone=off",
		"--link-journal=no",
		"--console=pipe",
		"--notify-ready=no",
		// Same as `lxc-stop -k`: Android keeps no state that needs a
		// clean shutdown outside /data.
		"--kill-signal=SIGKILL",
		"--network-veth-extra=" + netsvc.HostIf + ":eth0",
		"--capability=" + strings.Join(extraCaps, ","),
		"--drop-capability=" + strings.Join(dropCaps, ","),
		// The images' init was built for LXC containers.
		"--setenv=container=lxc",
		"--tmpfs=/var:mode=0755",
		"--tmpfs=/mnt_extra:mode=0755",
		"--tmpfs=" + paths.ContainerXDGRuntimeDir + ":mode=0755",
	}

	bind := func(src, dst string) { args = append(args, "--bind="+src+":"+dst) }
	bindRO := func(src, dst string) { args = append(args, "--bind-ro="+src+":"+dst) }

	for _, d := range optionalDevices {
		if sysutil.Exists(d) {
			bind(d, d)
		}
	}
	for _, g := range optionalDeviceGlobs {
		matches, _ := filepath.Glob(g)
		for _, m := range matches {
			bind(m, m)
		}
	}
	for _, d := range optionalDirs {
		if sysutil.Exists(d) {
			bind(d, d)
		}
	}
	if spec.RenderNode != "" {
		bind(spec.RenderNode, spec.RenderNode)
	}
	if sysutil.Exists("/dev/net/tun") {
		bind("/dev/net/tun", "/dev/tun")
	}
	if sysutil.Exists("/mnt/wslg") {
		bind("/mnt/wslg", "/mnt_extra/wslg")
	}

	// Binder contexts appear under their standard names in the container.
	for i, dst := range []string{"/dev/binder", "/dev/vndbinder", "/dev/hwbinder"} {
		src, err := filepath.EvalSymlinks(spec.Binder.Paths()[i])
		if err != nil {
			return nil, fmt.Errorf("binder node: %w", err)
		}
		bind(src, dst)
	}

	if sysutil.IsMount(paths.BPFDir) {
		bind(paths.BPFDir, "/sys/fs/bpf")
	}
	bind(paths.HostPerms, "/vendor/etc/host-permissions")
	bindRO(paths.PropFile, compat.PropFile)

	// Session: Wayland and PulseAudio sockets plus user data.
	wl, err := WaylandSocket(s)
	if err != nil {
		return nil, err
	}
	bind(wl, filepath.Join(paths.ContainerXDGRuntimeDir, paths.ContainerWaylandDisplay))
	if pulse := filepath.Join(s.PulseRuntimePath, "native"); sysutil.Exists(pulse) {
		bind(pulse, filepath.Join(paths.ContainerPulseDir, "native"))
	}
	bind(s.DataDir, "/data")

	// PID 1 is lapis's static shim, which adapts nspawn's /dev to what
	// Android's first-stage init expects and then execs /init.
	bindRO(spec.Self, ShimPath)
	return append(args, ShimPath, "container", "init-shim"), nil
}

// ShimPath is where the lapis binary appears inside the container.
const ShimPath = "/lapis-init"

// nspawn always populates /dev with these, but the images' first-stage
// init creates them itself and aborts when mknod fails with EEXIST.
var initCreatedNodes = []string{"/dev/random", "/dev/urandom"}

// The images' hwcomposer passes Wayland input to InputFlinger through FIFOs
// in /dev/input, and creates that directory itself, as the system user, once
// it is connected to the compositor. nspawn's /dev is root-owned 0755, so the
// mkdir failed and every click and key was dropped ("Failed to open pipe to
// InputFlinger"). /dev is opened to the system group instead. /dev/input must
// not be created here: init's keychord watcher would see the FIFOs appear and
// block opening them before InputFlinger exists, hanging the boot.
const aidSystem = 1000

// InitShim runs as the container's PID 1 and replaces itself with /init.
func InitShim() error {
	if os.Getpid() != 1 {
		return fmt.Errorf("init-shim must run as PID 1")
	}
	for _, n := range initCreatedNodes {
		if err := os.Remove(n); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Chown("/dev", 0, aidSystem); err != nil {
		return err
	}
	if err := os.Chmod("/dev", 0o775); err != nil {
		return err
	}
	return syscall.Exec("/init", []string{"/init"}, os.Environ())
}

// WaylandSocket resolves the session's Wayland socket path on the host.
func WaylandSocket(s *api.Session) (string, error) {
	p := s.WaylandDisplay
	if !filepath.IsAbs(p) {
		p = filepath.Join(s.XDGRuntimeDir, p)
	}
	return filepath.EvalSymlinks(p)
}

// LoadSession reads the session written by the control service.
func LoadSession() (*api.Session, error) {
	data, err := os.ReadFile(paths.SessionFile)
	if err != nil {
		return nil, fmt.Errorf("no session: %w", err)
	}
	var s api.Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Exec replaces the current process with systemd-nspawn, under the
// container seccomp filter. It is the ExecStart of the container unit.
func Exec(spec Spec) error {
	nspawn, err := exec.LookPath("systemd-nspawn")
	if err != nil {
		return fmt.Errorf("systemd-nspawn not found (install systemd-container): %w", err)
	}
	args, err := Args(spec)
	if err != nil {
		return err
	}
	if err := seccomp.Install(); err != nil {
		return err
	}
	// nspawn's own syscall filter would block calls Android needs (and
	// that ours turns into no-ops), so it is disabled in favor of ours.
	env := append(os.Environ(), "SYSTEMD_SECCOMP=0")
	return syscall.Exec(nspawn, append([]string{nspawn}, args...), env)
}
