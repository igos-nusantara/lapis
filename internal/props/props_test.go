package props

import (
	"strings"
	"testing"

	"github.com/igos-nusantara/lapis/internal/api"
	"github.com/igos-nusantara/lapis/internal/compat"
	"github.com/igos-nusantara/lapis/internal/config"
)

func TestBaseAndSession(t *testing.T) {
	cfg := config.Default()
	cfg.Properties["ro.opengles.version"] = "196609"

	l := Base(cfg, HostInfo{RenderNode: "/dev/dri/renderD128", VulkanHAL: "radeon"})
	AddSession(l, &api.Session{
		UserName: "deck", UID: 1000, GID: 1000,
		DataDir: "/mnt/data/lapis", LCDDensity: "240", BackgroundStart: true,
	}, true)

	want := map[string]string{
		"sys.use_memfd":                     "true",
		"ro.hardware.gralloc":               "gbm",
		"ro.hardware.egl":                   "mesa",
		"gralloc.gbm.device":                "/dev/dri/renderD128",
		"ro.hardware.vulkan":                "radeon",
		"ro.opengles.version":               "196609", // config override wins
		compat.PersistProp("multi_windows"): "true",
		compat.Prop("wayland_display"):      "wayland-0",
		compat.Prop("xdg_runtime_dir"):      "/run/xdg",
		compat.Prop("host.uid"):             "1000",
		compat.Prop("host_data_path"):       "/mnt_extra/data/lapis",
		"ro.sf.lcd_density":                 "240",
		compat.Prop("updater.disabled"):     "true",
	}
	for k, v := range want {
		if got := l.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if n := strings.Count(strings.Join(l.Lines(), "\n"), "ro.opengles.version="); n != 1 {
		t.Errorf("ro.opengles.version appears %d times", n)
	}
}

func TestSoftwareRendering(t *testing.T) {
	l := Base(config.Default(), HostInfo{Ashmem: true})
	if l.Get("ro.hardware.egl") != "swiftshader" || l.Get("ro.hardware.gralloc") != "default" {
		t.Errorf("expected swiftshader fallback, got egl=%q gralloc=%q", l.Get("ro.hardware.egl"), l.Get("ro.hardware.gralloc"))
	}
	if l.Get("sys.use_memfd") != "" {
		t.Error("memfd must not be forced when ashmem exists")
	}
}

func TestDisplayModes(t *testing.T) {
	for _, c := range []struct {
		d    config.Display
		want string
	}{
		{config.Display{}, "160"},
		{config.Display{Mode: config.DisplayTablet}, "160"},
		{config.Display{Mode: config.DisplayPhone}, "320"},
		{config.Display{Mode: "custom", Density: 240}, "240"},
	} {
		cfg := config.Default()
		cfg.Display = c.d
		if got := Base(cfg, HostInfo{}).Get("ro.sf.lcd_density"); got != c.want {
			t.Errorf("%+v: density %q, want %q", c.d, got, c.want)
		}
	}
	cfg := config.Default()
	cfg.Display = config.Display{Mode: "watch"}
	if _, err := cfg.Display.LCDDensity(); err == nil {
		t.Error("unknown mode accepted")
	}
}

func TestMaximizeApps(t *testing.T) {
	for _, c := range []struct {
		d    config.Display
		want bool
	}{
		{config.Display{}, true},
		{config.Display{Mode: config.DisplayTablet}, true},
		{config.Display{Mode: config.DisplayPhone}, false},
		{config.Display{Mode: config.DisplayPhone, Windows: config.WindowsMaximized}, true},
		{config.Display{Mode: config.DisplayTablet, Windows: config.WindowsFreeform}, false},
	} {
		if got := c.d.MaximizeApps(); got != c.want {
			t.Errorf("%+v: %v, want %v", c.d, got, c.want)
		}
	}
}
