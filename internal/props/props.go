// Package props generates the property file through which the Android
// images learn about the host (compat.PropFile).
package props

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/igos-nusantara/lapis/internal/api"
	"github.com/igos-nusantara/lapis/internal/compat"
	"github.com/igos-nusantara/lapis/internal/config"
	"github.com/igos-nusantara/lapis/internal/gpu"
	"github.com/igos-nusantara/lapis/internal/paths"
	"github.com/igos-nusantara/lapis/internal/sysutil"
)

// List is an ordered key=value list where Set replaces existing keys.
type List struct {
	keys []string
	vals map[string]string
}

func (l *List) Set(k, v string) {
	if l.vals == nil {
		l.vals = map[string]string{}
	}
	if _, ok := l.vals[k]; !ok {
		l.keys = append(l.keys, k)
	}
	l.vals[k] = v
}

func (l *List) Get(k string) string { return l.vals[k] }

func (l *List) Lines() []string {
	out := make([]string, 0, len(l.keys))
	for _, k := range l.keys {
		out = append(out, k+"="+l.vals[k])
	}
	return out
}

// HostInfo is everything Base needs to know about the host.
type HostInfo struct {
	Ashmem     bool
	RenderNode string // "" means software rendering
	VulkanHAL  string
}

func DetectHost(cfg *config.Config) (HostInfo, error) {
	render, err := gpu.RenderNode(cfg.Container.DRMDevice)
	if err != nil {
		return HostInfo{}, err
	}
	h := HostInfo{Ashmem: sysutil.Exists("/dev/ashmem"), RenderNode: render}
	if render != "" {
		h.VulkanHAL = gpu.VulkanDriver(filepath.Base(render))
	}
	return h, nil
}

// Base builds the host-independent part for a desktop (MAINLINE vendor)
// host.
func Base(cfg *config.Config, h HostInfo) *List {
	l := &List{}
	if !h.Ashmem {
		l.Set("sys.use_memfd", "true")
	}
	l.Set("ro.adb.secure", "1")
	l.Set("ro.debuggable", "0")

	if h.RenderNode != "" {
		l.Set("gralloc.gbm.device", h.RenderNode)
		l.Set("debug.stagefright.ccodec", "0")
		l.Set("ro.hardware.gralloc", "gbm")
		l.Set("ro.hardware.egl", "mesa")
	} else {
		l.Set("debug.stagefright.ccodec", "0")
		l.Set("ro.hardware.gralloc", "default")
		l.Set("ro.hardware.egl", "swiftshader")
	}
	if h.VulkanHAL != "" {
		l.Set("ro.hardware.vulkan", h.VulkanHAL)
	}
	l.Set("ro.hardware.camera", "v4l2")
	l.Set("ro.opengles.version", "196610")
	if dpi, err := cfg.Display.LCDDensity(); err == nil {
		l.Set("ro.sf.lcd_density", strconv.Itoa(dpi))
	}

	// The in-Android updater calls back into a host service lapis does
	// not provide; images are updated with `lapis upgrade` instead.
	l.Set(compat.Prop("updater.disabled"), "true")
	l.Set(compat.Prop("tools_version"), compat.ToolsVersion)
	l.Set("ro.vndk.lite", "true")

	for _, product := range []string{"brand", "device", "manufacturer", "model", "name"} {
		data, err := os.ReadFile("/proc/device-tree/" + product)
		if err != nil {
			continue
		}
		if v := strings.TrimSpace(strings.TrimRight(string(data), "\x00")); v != "" {
			l.Set(compat.ProductProp(product), v)
		}
	}

	keys := make([]string, 0, len(cfg.Properties))
	for k := range cfg.Properties {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		l.Set(k, cfg.Properties[k])
	}
	return l
}

// AddSession appends the per-session properties.
func AddSession(l *List, s *api.Session, stubSensors bool) {
	l.Set(compat.Prop("host.user"), s.UserName)
	l.Set(compat.Prop("host.uid"), strconv.Itoa(s.UID))
	l.Set(compat.Prop("host.gid"), strconv.Itoa(s.GID))
	l.Set(compat.Prop("host_data_path"), strings.ReplaceAll(s.DataDir, "/mnt/", "/mnt_extra/"))
	l.Set(compat.Prop("background_start"), strconv.FormatBool(s.BackgroundStart))
	l.Set(compat.Prop("xdg_runtime_dir"), paths.ContainerXDGRuntimeDir)
	l.Set(compat.Prop("pulse_runtime_path"), paths.ContainerPulseDir)
	l.Set(compat.Prop("wayland_display"), paths.ContainerWaylandDisplay)
	if stubSensors {
		l.Set(compat.Prop("stub_sensors_hal"), "1")
	}
	// A per-session override (LAPIS_DPI) wins over the configured mode.
	if s.LCDDensity != "" && s.LCDDensity != "0" {
		l.Set("ro.sf.lcd_density", s.LCDDensity)
	}
}

func Write(path string, l *List) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data := strings.Join(l.Lines(), "\n") + "\n"
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(data), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
