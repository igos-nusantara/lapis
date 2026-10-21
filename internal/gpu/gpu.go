// Package gpu picks the DRM render node handed to the container and maps
// it to the Android graphics HAL names.
package gpu

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/igos-nusantara/lapis/internal/sysutil"
)

// Drivers whose userspace Mesa inside Android cannot drive.
var unsupported = map[string]bool{"nvidia": true}

func KernelDriver(renderDev string) string {
	return sysutil.FileGet(fmt.Sprintf("/sys/class/drm/%s/device/uevent", renderDev), "DRIVER")
}

// RenderNode returns the render node to use, or "" for software rendering.
func RenderNode(override string) (string, error) {
	if override != "" {
		if !sysutil.Exists(override) {
			return "", fmt.Errorf("configured drm_device %s does not exist", override)
		}
		if unsupported[KernelDriver(filepath.Base(override))] {
			return "", nil
		}
		return override, nil
	}
	nodes, _ := filepath.Glob("/dev/dri/renderD*")
	sort.Strings(nodes)
	for _, n := range nodes {
		if !unsupported[KernelDriver(filepath.Base(n))] {
			return n, nil
		}
	}
	return "", nil
}

// VulkanDriver maps the kernel driver to the ro.hardware.vulkan value.
func VulkanDriver(renderDev string) string {
	mapping := map[string]string{
		"i915":     "intel",
		"xe":       "intel",
		"amdgpu":   "radeon",
		"panfrost": "panfrost",
		"msm":      "freedreno",
		"msm_dpu":  "freedreno",
		"vc4":      "broadcom",
		"nouveau":  "nouveau",
	}
	drv := KernelDriver(renderDev)
	if drv == "i915" && intelGen(renderDev) > 0 && intelGen(renderDev) < 9 {
		return "intel_hasvk"
	}
	return mapping[drv]
}

// intelGen reads the GPU generation from debugfs; 0 when unknown.
func intelGen(renderDev string) int {
	minor := sysutil.FileGet(fmt.Sprintf("/sys/class/drm/%s/uevent", renderDev), "MINOR")
	data, err := os.ReadFile(fmt.Sprintf("/sys/kernel/debug/dri/%s/i915_capabilities", minor))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "graphics version:") || strings.HasPrefix(line, "gen:") {
			f := strings.Fields(line)
			n, _ := strconv.Atoi(f[len(f)-1])
			return n
		}
	}
	return 0
}
