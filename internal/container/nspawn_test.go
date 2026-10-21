package container

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/igos-nusantara/lapis/internal/api"
	"github.com/igos-nusantara/lapis/internal/compat"
	"github.com/igos-nusantara/lapis/internal/drivers"
)

func TestArgs(t *testing.T) {
	dir := t.TempDir()
	// Fake binder nodes and a Wayland socket.
	for _, n := range []string{"b", "vb", "hb", "wayland-0", "pulse/native"} {
		p := filepath.Join(dir, n)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, nil, 0o644)
	}
	s := &api.Session{
		XDGRuntimeDir:    dir,
		WaylandDisplay:   "wayland-0",
		PulseRuntimePath: filepath.Join(dir, "pulse"),
		DataDir:          "/home/u/.local/share/lapis/data",
	}
	// Binder paths are absolute host paths under /dev; point them at the
	// fake files through relative names.
	nodes := drivers.Nodes{
		Binder:    "../" + filepath.Join(dir, "b")[1:],
		VndBinder: "../" + filepath.Join(dir, "vb")[1:],
		HwBinder:  "../" + filepath.Join(dir, "hb")[1:],
	}
	args, err := Args(Spec{Session: s, Binder: nodes, RenderNode: "/dev/dri/renderD128", Self: "/usr/local/bin/lapis"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, "\n")
	for _, want := range []string{
		"--keep-unit",
		"--directory=/var/lib/lapis/rootfs",
		"--network-veth-extra=lapis0:eth0",
		"--setenv=container=lxc",
		"--bind=" + filepath.Join(dir, "b") + ":/dev/binder",
		"--bind=" + filepath.Join(dir, "hb") + ":/dev/hwbinder",
		"--bind=" + filepath.Join(dir, "wayland-0") + ":/run/xdg/wayland-0",
		"--bind=" + filepath.Join(dir, "pulse/native") + ":/run/xdg/pulse/native",
		"--bind=/home/u/.local/share/lapis/data:/data",
		"--bind-ro=/run/lapis/android.prop:" + compat.PropFile,
		"--bind=/dev/dri/renderD128:/dev/dri/renderD128",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q", want)
		}
	}
	if tail := strings.Join(args[len(args)-3:], " "); tail != "/lapis-init container init-shim" {
		t.Errorf("command = %q", tail)
	}
	if !strings.Contains(joined, "--bind-ro=/usr/local/bin/lapis:/lapis-init") {
		t.Error("shim not bound")
	}
	if strings.Contains(joined, "CAP_SYS_BOOT") {
		t.Error("CAP_SYS_BOOT must not be dropped")
	}
}
