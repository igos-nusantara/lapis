// Package extras installs optional components into the system overlay,
// ARM translation layers (libndk_translation
// from ChromeOS, Intel libhoudini) so ARM-only apps from Google Play run.
package extras

import (
	"archive/zip"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/igos-nusantara/lapis/internal/config"
	"github.com/igos-nusantara/lapis/internal/paths"
	"github.com/igos-nusantara/lapis/internal/rootfs"
)

type source struct {
	url    string
	md5    string
	prefix string // directory inside the archive holding the files
}

type Extra struct {
	ID          string
	Description string
	sources     map[string]source // by Android major version
	props       map[string]string
	initRC      string
}

var abiProps = map[string]string{
	"ro.product.cpu.abilist":       "x86_64,x86,arm64-v8a,armeabi-v7a,armeabi",
	"ro.product.cpu.abilist32":     "x86,armeabi-v7a,armeabi",
	"ro.product.cpu.abilist64":     "x86_64,arm64-v8a",
	"ro.dalvik.vm.isa.arm":         "x86",
	"ro.dalvik.vm.isa.arm64":       "x86_64",
	"ro.enable.native.bridge.exec": "1",
}

func withABI(extra map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range abiProps {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

const (
	ndkRepo     = "https://github.com/supremegamers/vendor_google_proprietary_ndk_translation-prebuilt/archive/"
	houdiniRepo = "https://github.com/supremegamers/vendor_intel_proprietary_houdini/archive/"
)

// Catalog lists the installable extras.
var Catalog = []Extra{
	{
		ID:          "libndk",
		Description: "ARM translation from ChromeOS (recommended on AMD CPUs)",
		sources: map[string]source{
			"11": {ndkRepo + "9324a8914b649b885dad6f2bfd14a67e5d1520bf.zip", "c9572672d1045594448068079b34c350",
				"vendor_google_proprietary_ndk_translation-prebuilt-9324a8914b649b885dad6f2bfd14a67e5d1520bf/prebuilts/"},
			"13": {ndkRepo + "68734c52556d3d7a6db34c603dd9276915c29f2f.zip", "0b2207c490fcb400aa5c87fcf0d52d38",
				"vendor_google_proprietary_ndk_translation-prebuilt-68734c52556d3d7a6db34c603dd9276915c29f2f/prebuilts/"},
		},
		props: withABI(map[string]string{
			"ro.dalvik.vm.native.bridge":            "libndk_translation.so",
			"ro.vendor.enable.native.bridge.exec":   "1",
			"ro.vendor.enable.native.bridge.exec64": "1",
			"ro.ndk_translation.version":            "0.2.3",
		}),
	},
	{
		ID:          "libhoudini",
		Description: "ARM translation from Intel (recommended on Intel CPUs)",
		sources: map[string]source{
			"11": {houdiniRepo + "81f2a51ef539a35aead396ab7fce2adf89f46e88.zip", "fbff756612b4144797fbc99eadcb6653",
				"vendor_intel_proprietary_houdini-81f2a51ef539a35aead396ab7fce2adf89f46e88/prebuilts/"},
			"13": {houdiniRepo + "2f8f088671182e17e67321e098e8411a3972a628.zip", "37fe0899f1e4da7f9a724cca1c1ab1ea",
				"vendor_intel_proprietary_houdini-2f8f088671182e17e67321e098e8411a3972a628/prebuilts/"},
		},
		props: withABI(map[string]string{"ro.dalvik.vm.native.bridge": "libhoudini.so"}),
		initRC: `on early-init
    mount binfmt_misc binfmt_misc /proc/sys/fs/binfmt_misc

on property:ro.enable.native.bridge.exec=1
    exec -- /system/bin/sh -c "echo ':arm_exe:M::\\\\x7f\\\\x45\\\\x4c\\\\x46\\\\x01\\\\x01\\\\x01\\\\x00\\\\x00\\\\x00\\\\x00\\\\x00\\\\x00\\\\x00\\\\x00\\\\x00\\\\x02\\\\x00\\\\x28::/system/bin/houdini:P' > /proc/sys/fs/binfmt_misc/register"
    exec -- /system/bin/sh -c "echo ':arm_dyn:M::\\\\x7f\\\\x45\\\\x4c\\\\x46\\\\x01\\\\x01\\\\x01\\\\x00\\\\x00\\\\x00\\\\x00\\\\x00\\\\x00\\\\x00\\\\x00\\\\x00\\\\x03\\\\x00\\\\x28::/system/bin/houdini:P' >> /proc/sys/fs/binfmt_misc/register"
`,
	},
}

func Find(id string) (*Extra, error) {
	for i := range Catalog {
		if Catalog[i].ID == id {
			return &Catalog[i], nil
		}
	}
	return nil, fmt.Errorf("unknown extra %q (available: libndk, libhoudini)", id)
}

var (
	cacheDir    = filepath.Join(paths.WorkDir, "cache")
	manifestDir = filepath.Join(paths.WorkDir, "extras")
	overlaySys  = filepath.Join(paths.OverlayDir, "system")
)

func manifest(id string) string { return filepath.Join(manifestDir, id+".list") }

// Installed lists the installed extras.
func Installed() []string {
	files, _ := filepath.Glob(filepath.Join(manifestDir, "*.list"))
	var out []string
	for _, f := range files {
		out = append(out, strings.TrimSuffix(filepath.Base(f), ".list"))
	}
	sort.Strings(out)
	return out
}

// Install downloads, verifies and unpacks an extra into the overlay and
// adds its properties. It takes effect on the next container start.
func Install(id string, progress io.Writer) error {
	e, err := Find(id)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	release, err := rootfs.AndroidRelease(cfg.Images.Path)
	if err != nil {
		return err
	}
	major, _, _ := strings.Cut(release, ".")
	src, ok := e.sources[major]
	if !ok {
		return fmt.Errorf("%s is not available for Android %s", id, release)
	}
	for _, other := range Installed() {
		if other != id {
			return fmt.Errorf("%s is installed; only one ARM translation layer can be active (lapis extras remove %s)", other, other)
		}
	}

	zipPath := filepath.Join(cacheDir, id+"-"+major+".zip")
	if err := fetch(src, zipPath, progress); err != nil {
		return err
	}
	fmt.Fprintln(progress, "Unpacking into the system overlay")
	files, err := unpack(zipPath, src.prefix, overlaySys)
	if err != nil {
		return err
	}
	if e.initRC != "" {
		rel := "etc/init/" + id + ".rc"
		if err := os.MkdirAll(filepath.Join(overlaySys, "etc/init"), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(overlaySys, rel), []byte(e.initRC), 0o644); err != nil {
			return err
		}
		files = append(files, rel)
	}

	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(manifest(id), []byte(strings.Join(files, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	for k, v := range e.props {
		cfg.Properties[k] = v
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Fprintf(progress, "%s installed (%d files); restart the session to use it\n", id, len(files))
	return nil
}

// Remove deletes an extra's files and properties.
func Remove(id string) error {
	e, err := Find(id)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(manifest(id))
	if err != nil {
		return fmt.Errorf("%s is not installed", id)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	// Deepest paths first so directories empty out before removal.
	sort.Sort(sort.Reverse(sort.StringSlice(lines)))
	for _, rel := range lines {
		if rel == "" || strings.Contains(rel, "..") {
			continue
		}
		p := filepath.Join(overlaySys, rel)
		if fi, err := os.Lstat(p); err == nil && fi.IsDir() {
			os.Remove(p) // only if empty
		} else {
			os.Remove(p)
		}
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	for k := range e.props {
		delete(cfg.Properties, k)
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	return os.Remove(manifest(id))
}

func fetch(src source, dest string, progress io.Writer) error {
	if sum, err := md5File(dest); err == nil && sum == src.md5 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	fmt.Fprintln(progress, "Downloading", src.url)
	resp, err := http.Get(src.url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", src.url, resp.StatusCode)
	}
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	h := md5.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != src.md5 {
		os.Remove(tmp)
		return fmt.Errorf("md5 mismatch for %s: got %s, want %s", src.url, sum, src.md5)
	}
	return os.Rename(tmp, dest)
}

func md5File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// unpack extracts entries below prefix into dest with Android ownership
// and modes (bin: root:shell 0755, others root:root 0644), returning the
// relative paths written.
func unpack(zipPath, prefix, dest string) ([]string, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var written []string
	for _, f := range zr.File {
		rel, ok := strings.CutPrefix(f.Name, prefix)
		if !ok || rel == "" {
			continue
		}
		clean := filepath.Clean(rel)
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			return nil, fmt.Errorf("unsafe path in archive: %s", f.Name)
		}
		target := filepath.Join(dest, clean)
		inBin := strings.HasPrefix(clean, "bin/") || clean == "bin"
		gid := 0
		if inBin {
			gid = 2000
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return nil, err
			}
			os.Chown(target, 0, gid)
			written = append(written, clean)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, err
		}
		if f.Mode()&os.ModeSymlink != 0 {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			link, _ := io.ReadAll(rc)
			rc.Close()
			os.Remove(target)
			if err := os.Symlink(string(link), target); err != nil {
				return nil, err
			}
			written = append(written, clean)
			continue
		}
		mode := os.FileMode(0o644)
		if inBin {
			mode = 0o755
		}
		if err := writeFile(f, target, mode); err != nil {
			return nil, err
		}
		os.Chown(target, 0, gid)
		written = append(written, clean)
	}
	if len(written) == 0 {
		return nil, fmt.Errorf("%s: nothing found under %s", zipPath, prefix)
	}
	return written, nil
}

func writeFile(f *zip.File, target string, mode os.FileMode) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(target, mode)
}
