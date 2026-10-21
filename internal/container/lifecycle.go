package container

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/igos-nusantara/lapis/internal/api"
	"github.com/igos-nusantara/lapis/internal/config"
	"github.com/igos-nusantara/lapis/internal/drivers"
	"github.com/igos-nusantara/lapis/internal/paths"
	"github.com/igos-nusantara/lapis/internal/props"
	"github.com/igos-nusantara/lapis/internal/rootfs"
	"github.com/igos-nusantara/lapis/internal/sysutil"
)

// Prepare is the ExecStartPre of the container unit: it sets up kernel
// drivers, device permissions, the root filesystem and properties.
func Prepare() error {
	s, err := LoadSession()
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if _, err := drivers.Setup(); err != nil {
		return err
	}
	drivers.ProbeAshmem()

	host, err := props.DetectHost(cfg)
	if err != nil {
		return err
	}
	setDevicePermissions(host.RenderNode)

	if err := rootfs.Mount(cfg.Images.Path); err != nil {
		return err
	}
	if err := mountBPF(); err != nil {
		// Android still boots, but network stats and app removal crash
		// system_server without BPF maps.
		slog.Warn("no bpf filesystem for Android", "err", err)
	}
	resetLaunchBoundsOnModeChange(cfg, s)
	list := props.Base(cfg, host)
	props.AddSession(list, s, true)
	if err := props.Write(paths.PropFile, list); err != nil {
		return err
	}
	slog.Info("container prepared", "sdk", rootfs.SDKVersion(), "render", host.RenderNode)
	return nil
}

// ExecFromSession is the ExecStart of the container unit.
func ExecFromSession() error {
	s, err := LoadSession()
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	nodes, err := drivers.Find()
	if err != nil {
		return err
	}
	host, err := props.DetectHost(cfg)
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	return Exec(Spec{Session: s, Binder: nodes, RenderNode: host.RenderNode, Self: self})
}

// Cleanup is the ExecStopPost of the container unit.
func Cleanup() error {
	if sysutil.IsMount(paths.BPFDir) {
		unix.Unmount(paths.BPFDir, unix.MNT_DETACH)
	}
	if err := rootfs.Umount(); err != nil {
		return fmt.Errorf("unmount rootfs: %w", err)
	}
	return nil
}

// mountBPF gives the container its own bpf filesystem instance for
// /sys/fs/bpf. systemd-nspawn's /sys has no such directory, so Android's
// bpfloader could not pin its programs and maps, and NetworkStatsService
// would crash system_server whenever an app is removed. A separate
// instance keeps Android's pinned objects out of the host's /sys/fs/bpf.
func mountBPF() error {
	if sysutil.IsMount(paths.BPFDir) {
		return nil
	}
	if err := os.MkdirAll(paths.BPFDir, 0o700); err != nil {
		return err
	}
	return unix.Mount("bpf", paths.BPFDir, "bpf", 0, "mode=0700")
}

// setDevicePermissions opens up the nodes Android services use under
// their own uids.
func setDevicePermissions(render string) {
	nodes := []string{"/dev/ashmem", "/dev/sw_sync", "/dev/ion", "/dev/pvr_sync",
		"/dev/Vcodec", "/dev/MTK_SMI", "/dev/mdp_sync", "/dev/mtk_cmdq"}
	if render != "" {
		nodes = append(nodes, render)
	}
	for _, g := range []string{"/dev/fb*", "/dev/video*", "/dev/dma_heap/*", "/dev/graphics/*"} {
		m, _ := filepath.Glob(g)
		nodes = append(nodes, m...)
	}
	for _, n := range nodes {
		if !sysutil.Exists(n) {
			continue
		}
		if err := os.Chmod(n, 0o666); err != nil {
			slog.Warn("chmod failed", "node", n, "err", err)
		}
	}
}

// resetLaunchBoundsOnModeChange forgets the window bounds Android saved for
// each app when the display mode changed, so apps open the way the new mode
// wants (maximized or phone-sized) instead of where they were last.
func resetLaunchBoundsOnModeChange(cfg *config.Config, s *api.Session) {
	dpi, _ := cfg.Display.LCDDensity()
	mode := fmt.Sprintf("%d/%t", dpi, cfg.Display.MaximizeApps())
	stamp := filepath.Join(paths.WorkDir, "display.applied")
	if old, err := os.ReadFile(stamp); err == nil && string(old) == mode {
		return
	}
	files, _ := filepath.Glob(filepath.Join(s.DataDir, "system_ce", "*", "launch_params", "*.xml"))
	for _, f := range files {
		os.Remove(f)
	}
	if err := os.WriteFile(stamp, []byte(mode), 0o644); err != nil {
		slog.Warn("display mode not recorded", "err", err)
	}
}
