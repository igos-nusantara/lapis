// Package doctor checks whether the host can run the container.
package doctor

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/igos-nusantara/lapis/internal/config"
	"github.com/igos-nusantara/lapis/internal/drivers"
	"github.com/igos-nusantara/lapis/internal/gpu"
	"github.com/igos-nusantara/lapis/internal/images"
	"github.com/igos-nusantara/lapis/internal/sysutil"
)

type level int

const (
	ok level = iota
	warn
	fail
)

func (l level) String() string {
	return [...]string{"  OK ", " WARN", " FAIL"}[l]
}

type check struct {
	name   string
	level  level
	detail string
	hint   string
}

// Run prints all checks and returns false when a required one fails.
func Run(w io.Writer) bool {
	var checks []check
	add := func(c check) { checks = append(checks, c) }

	// Kernel
	if okb, why := drivers.Available(); okb {
		add(check{name: "binder", detail: why})
	} else {
		hint := "enable CONFIG_ANDROID_BINDER_IPC=y and CONFIG_ANDROID_BINDERFS=y, or build the binder_linux DKMS module"
		if isSteamOS() {
			hint = "the SteamOS kernel ships without binder; build binder_linux for this kernel (see PLAN.md, SteamOS notes)"
		}
		add(check{name: "binder", level: fail, detail: why, hint: hint})
	}
	if sysutil.Exists("/dev/ashmem") {
		add(check{name: "ashmem", detail: "present"})
	} else {
		add(check{name: "ashmem", level: ok, detail: "absent, Android uses memfd (fine)"})
	}
	if sysutil.HasFilesystem("overlay") {
		add(check{name: "overlayfs", detail: "supported"})
	} else {
		add(check{name: "overlayfs", level: warn, detail: "not listed (module may load on demand)"})
	}
	if sysutil.Exists("/sys/fs/cgroup/cgroup.controllers") {
		add(check{name: "cgroup v2", detail: "unified hierarchy"})
	} else {
		add(check{name: "cgroup v2", level: warn, detail: "legacy/hybrid cgroups; freeze may not work"})
	}

	// Userspace tools
	if v := systemdVersion(); v >= 250 {
		add(check{name: "systemd", detail: strconv.Itoa(v)})
	} else {
		add(check{name: "systemd", level: fail, detail: strconv.Itoa(v), hint: "systemd >= 250 is required"})
	}
	for _, tool := range []struct{ bin, pkg string }{
		{"systemd-nspawn", "systemd-container"},
		{"nsenter", "util-linux"},
		{"mount", "util-linux"},
	} {
		if p, err := exec.LookPath(tool.bin); err == nil {
			add(check{name: tool.bin, detail: p})
		} else {
			add(check{name: tool.bin, level: fail, detail: "missing", hint: "install the " + tool.pkg + " package"})
		}
	}

	// Graphics
	cfg, err := config.Load()
	if err != nil {
		add(check{name: "config", level: fail, detail: err.Error()})
		cfg = config.Default()
	}
	render, err := gpu.RenderNode(cfg.Container.DRMDevice)
	switch {
	case err != nil:
		add(check{name: "gpu", level: fail, detail: err.Error()})
	case render == "":
		add(check{name: "gpu", level: warn, detail: "no supported render node, software rendering (slow)"})
	default:
		dev := filepath.Base(render)
		add(check{name: "gpu", detail: fmt.Sprintf("%s (%s, vulkan=%s)", render, gpu.KernelDriver(dev), gpu.VulkanDriver(dev))})
	}

	// Session
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		wl := filepath.Join(rt, envOr("WAYLAND_DISPLAY", "wayland-0"))
		if sysutil.Exists(wl) {
			add(check{name: "wayland", detail: wl})
		} else {
			add(check{name: "wayland", level: warn, detail: "no Wayland socket in this shell", hint: "run the session from a Wayland desktop"})
		}
		if sysutil.Exists(filepath.Join(rt, "pulse/native")) {
			add(check{name: "audio", detail: "PulseAudio/PipeWire socket found"})
		} else {
			add(check{name: "audio", level: warn, detail: "no pulse socket, Android will be silent"})
		}
	}

	// Images
	if images.Present(cfg.Images.Path) {
		add(check{name: "images", detail: cfg.Images.Path})
	} else {
		add(check{name: "images", level: warn, detail: "not downloaded", hint: "sudo lapis init"})
	}

	good := true
	for _, c := range checks {
		fmt.Fprintf(w, "[%s] %-15s %s\n", c.level, c.name, c.detail)
		if c.hint != "" {
			fmt.Fprintf(w, "        %-15s -> %s\n", "", c.hint)
		}
		if c.level == fail {
			good = false
		}
	}
	return good
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func systemdVersion() int {
	out, err := sysutil.Output("systemctl", "--version")
	if err != nil {
		return 0
	}
	f := strings.Fields(out)
	if len(f) < 2 {
		return 0
	}
	v, _ := strconv.Atoi(f[1])
	return v
}

func isSteamOS() bool {
	return sysutil.FileGet("/etc/os-release", "ID") == "steamos"
}
