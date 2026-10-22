// Package hwchook installs liblapis_hwc.so (see hook.c) into the overlay
// and preloads it into Android's composer service, so games whose
// SurfaceView layer lacks the task prefix still get their own host window.
package hwchook

//go:generate sh build.sh

import (
	_ "embed"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

var (
	//go:embed liblapis_hwc-x86_64.so
	libX86_64 []byte
	//go:embed liblapis_hwc-aarch64.so
	libAarch64 []byte
)

const (
	libName = "liblapis_hwc.so"
	libPath = "/vendor/lib64/" + libName
	preload = "    setenv LD_PRELOAD " + libPath
	rcName  = "zz-lapis-composer-hook.rc" // sorts after the image's own rc files
)

func library() []byte {
	switch runtime.GOARCH {
	case "amd64":
		return libX86_64
	case "arm64":
		return libAarch64
	}
	return nil
}

// Install writes the library and an rc file that redefines the composer
// service with LD_PRELOAD into overlay, the overlay tree laid over the root
// (overlay/system, overlay/vendor). root has the unmodified images mounted.
// Files from an earlier install are replaced, or removed when the image has
// no composer service to hook.
//
// The rc file goes to /system/etc/init, not /vendor: the images redefine
// the service there with "override", and init parses that directory after
// /vendor/etc/init, so only a later file in the same directory wins.
func Install(root, overlay string) error {
	lib := filepath.Join(overlay, "vendor", "lib64", libName)
	rc := filepath.Join(overlay, "system", "etc", "init", rcName)
	os.Remove(lib)
	os.Remove(rc)

	def, err := composerService(root)
	if err != nil || def == "" || library() == nil {
		return err
	}
	for _, d := range []string{filepath.Dir(lib), filepath.Dir(rc)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(lib, library(), 0o644); err != nil {
		return err
	}
	return os.WriteFile(rc, []byte(withPreload(def)), 0o644)
}

// Directories init reads services from.
var initDirs = []string{"vendor/etc/init", "odm/etc/init", "system/etc/init", "system/system_ext/etc/init", "product/etc/init"}

// composerService returns the composer service definition init ends up
// using: the last one marked "override", otherwise the first one.
func composerService(root string) (string, error) {
	var first, override string
	for _, dir := range initDirs {
		files, err := filepath.Glob(filepath.Join(root, dir, "*.rc"))
		if err != nil {
			return "", err
		}
		sort.Strings(files)
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				return "", err
			}
			for _, block := range services(string(data)) {
				if first == "" {
					first = block
				}
				if hasOption(block, "override") {
					override = block
				}
			}
		}
	}
	if override != "" {
		return override, nil
	}
	return first, nil
}

// services returns the composer service blocks of an rc file.
func services(rc string) []string {
	var out []string
	var cur []string
	flush := func() {
		if cur != nil {
			out = append(out, strings.Join(cur, "\n")+"\n")
		}
		cur = nil
	}
	for _, l := range strings.Split(rc, "\n") {
		f := strings.Fields(l)
		if len(f) > 0 && (f[0] == "service" || f[0] == "on" || f[0] == "import") && !strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "\t") {
			flush()
			if f[0] == "service" && len(f) >= 3 && strings.Contains(f[2], "graphics.composer") {
				cur = []string{l}
			}
			continue
		}
		if cur != nil && strings.TrimSpace(l) != "" && !strings.HasPrefix(strings.TrimSpace(l), "#") {
			cur = append(cur, l)
		}
	}
	flush()
	return out
}

func hasOption(block, opt string) bool {
	for _, l := range strings.Split(block, "\n")[1:] {
		if f := strings.Fields(l); len(f) > 0 && f[0] == opt {
			return true
		}
	}
	return false
}

// withPreload returns the definition as an override that preloads the hook.
func withPreload(def string) string {
	lines := strings.Split(strings.TrimRight(def, "\n"), "\n")
	out := []string{lines[0], "    override", preload}
	for _, l := range lines[1:] {
		if f := strings.Fields(l); len(f) > 0 && (f[0] == "override" || (f[0] == "setenv" && len(f) > 1 && f[1] == "LD_PRELOAD")) {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n") + "\n"
}
