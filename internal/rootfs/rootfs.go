// Package rootfs assembles the container root: system.img as /, vendor.img
// as /vendor, each with a writable overlay for local changes and extras.
package rootfs

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/igos-nusantara/lapis/internal/compat"
	"github.com/igos-nusantara/lapis/internal/hwchook"
	"github.com/igos-nusantara/lapis/internal/images"
	"github.com/igos-nusantara/lapis/internal/paths"
	"github.com/igos-nusantara/lapis/internal/sysutil"
)

// Mount points created on the overlay before nspawn runs, because the
// container root is read-only once the container starts.
var mountpoints = []string{
	"/usr", // systemd-nspawn refuses trees without /usr
	"/run", "/tmp", "/var", "/mnt_extra", "/data",
	"/vendor/etc/host-permissions",
}

func Mount(imagesDir string) error {
	if !images.Present(imagesDir) {
		return images.ErrNotInitialized
	}
	if err := sysutil.UmountAll(paths.RootfsDir); err != nil {
		return err
	}
	for _, d := range []string{
		paths.RootfsDir, paths.OverlayDir, filepath.Join(paths.OverlayDir, "vendor"),
		filepath.Join(paths.OverlayRW, "system"), filepath.Join(paths.OverlayRW, "vendor"),
		filepath.Join(paths.OverlayWork, "system"), filepath.Join(paths.OverlayWork, "vendor"),
		paths.HostPerms,
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}

	if err := mountImage(filepath.Join(imagesDir, "system.img"), paths.RootfsDir); err != nil {
		return err
	}
	// The composer hook is derived from both images and written into the
	// overlay tree before the overlays are mounted on top of it.
	vendor := filepath.Join(paths.RootfsDir, "vendor")
	if err := mountImage(filepath.Join(imagesDir, "vendor.img"), vendor); err != nil {
		return err
	}
	if err := hwchook.Install(paths.RootfsDir, paths.OverlayDir); err != nil {
		return fmt.Errorf("composer hook: %w", err)
	}
	if err := unix.Unmount(vendor, 0); err != nil {
		return fmt.Errorf("unmount %s: %w", vendor, err)
	}
	if err := overlay(paths.RootfsDir, []string{paths.OverlayDir, paths.RootfsDir}, "system"); err != nil {
		return err
	}
	if err := os.MkdirAll(vendor, 0o755); err != nil {
		return err
	}
	if err := mountImage(filepath.Join(imagesDir, "vendor.img"), vendor); err != nil {
		return err
	}
	if err := overlay(vendor, []string{filepath.Join(paths.OverlayDir, "vendor"), vendor}, "vendor"); err != nil {
		return err
	}

	for _, d := range mountpoints {
		if err := os.MkdirAll(filepath.Join(paths.RootfsDir, d), 0o755); err != nil {
			return err
		}
	}
	// File bind targets: the property file and the init shim.
	for _, f := range []string{compat.PropFile, "/lapis-init"} {
		p := filepath.Join(paths.RootfsDir, f)
		if !sysutil.Exists(p) {
			if err := os.WriteFile(p, nil, 0o755); err != nil {
				return err
			}
		}
	}
	return nil
}

func Umount() error {
	return sysutil.UmountAll(paths.RootfsDir)
}

// mountImage loop-mounts a filesystem image read-only. util-linux mount
// handles loop device allocation and auto-clear on unmount.
func mountImage(img, target string) error {
	return sysutil.Run("mount", "-o", "ro,loop", img, target)
}

func overlay(target string, lower []string, name string) error {
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s,xino=off",
		strings.Join(lower, ":"),
		filepath.Join(paths.OverlayRW, name),
		filepath.Join(paths.OverlayWork, name))
	if err := unix.Mount("overlay", target, "overlay", 0, opts); err != nil {
		return fmt.Errorf("overlay on %s: %w", target, err)
	}
	return nil
}

// SDKVersion reads ro.build.version.sdk from the mounted system image.
func SDKVersion() int {
	v, _ := strconv.Atoi(sysutil.FileGet(filepath.Join(paths.RootfsDir, "system/build.prop"), "ro.build.version.sdk"))
	return v
}

// AndroidRelease returns ro.build.version.release of the system image,
// mounting it temporarily when the container is not running.
func AndroidRelease(imagesDir string) (string, error) {
	const key = "ro.build.version.release"
	if sysutil.IsMount(paths.RootfsDir) {
		if v := sysutil.FileGet(filepath.Join(paths.RootfsDir, "system/build.prop"), key); v != "" {
			return v, nil
		}
	}
	if !images.Present(imagesDir) {
		return "", images.ErrNotInitialized
	}
	dir, err := os.MkdirTemp("", "lapis-system-")
	if err != nil {
		return "", err
	}
	defer os.Remove(dir)
	if err := mountImage(filepath.Join(imagesDir, "system.img"), dir); err != nil {
		return "", err
	}
	defer unix.Unmount(dir, 0)
	v := sysutil.FileGet(filepath.Join(dir, "system/build.prop"), key)
	if v == "" {
		return "", fmt.Errorf("%s not found in system.img", key)
	}
	return v, nil
}
