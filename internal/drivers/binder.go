// Package drivers prepares the host kernel interfaces Android needs:
// binder (via binderfs) and, when available, ashmem.
package drivers

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/igos-nusantara/lapis/internal/sysutil"
)

// Candidate device names, in the order the images look for them. The first entry is the one
// created when none exist; the last existing entry wins.
var (
	binderNames    = []string{"anbox-binder", "puddlejumper", "bonder", "binder"}
	vndbinderNames = []string{"anbox-vndbinder", "vndpuddlejumper", "vndbonder", "vndbinder"}
	hwbinderNames  = []string{"anbox-hwbinder", "hwpuddlejumper", "hwbonder", "hwbinder"}
)

const binderfsDir = "/dev/binderfs"

// Nodes holds the /dev basenames of the three binder contexts.
type Nodes struct {
	Binder    string
	VndBinder string
	HwBinder  string
}

// Paths returns the absolute host paths of the nodes.
func (n Nodes) Paths() []string {
	return []string{"/dev/" + n.Binder, "/dev/" + n.VndBinder, "/dev/" + n.HwBinder}
}

func anyExists(names []string) bool {
	for _, n := range names {
		if sysutil.Exists("/dev/" + n) {
			return true
		}
	}
	return false
}

func lastExisting(names []string) string {
	found := ""
	for _, n := range names {
		if sysutil.Exists("/dev/" + n) {
			found = n
		}
	}
	return found
}

// binderfs_device from <linux/android/binderfs.h>.
type binderfsDevice struct {
	Name  [256]byte
	Major uint32
	Minor uint32
}

// _IOWR('b', 1, struct binderfs_device)
const binderCtlAdd = 0xC1086201

func allocNodes(names []string) error {
	ctl, err := os.OpenFile(filepath.Join(binderfsDir, "binder-control"), os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer ctl.Close()
	for _, name := range names {
		var dev binderfsDevice
		copy(dev.Name[:], name)
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, ctl.Fd(), binderCtlAdd, uintptr(unsafe.Pointer(&dev)))
		if errno != 0 && errno != unix.EEXIST {
			return fmt.Errorf("binderfs: add %s: %w", name, errno)
		}
		link := "/dev/" + name
		if !sysutil.Exists(link) {
			if err := os.Symlink(filepath.Join(binderfsDir, name), link); err != nil {
				return err
			}
		}
	}
	return nil
}

// Setup makes sure binder, vndbinder and hwbinder nodes exist, loading the
// module and mounting binderfs when needed, and returns their names.
func Setup() (Nodes, error) {
	var missing []string
	for _, group := range [][]string{binderNames, vndbinderNames, hwbinderNames} {
		if !anyExists(group) {
			missing = append(missing, group[0])
		}
	}

	if len(missing) > 0 {
		if !sysutil.HasFilesystem("binder") {
			// Out-of-tree binder_linux (DKMS) takes the device list as a
			// parameter; harmless when binder is built in.
			if err := sysutil.Run("modprobe", "binder_linux", "devices="+strings.Join(missing, ",")); err != nil {
				slog.Warn("loading binder_linux failed", "err", err)
			}
		}
		if sysutil.HasFilesystem("binder") {
			if err := os.MkdirAll(binderfsDir, 0o755); err != nil {
				return Nodes{}, err
			}
			if !sysutil.IsMount(binderfsDir) {
				if err := unix.Mount("binder", binderfsDir, "binder", 0, ""); err != nil {
					return Nodes{}, fmt.Errorf("mount binderfs: %w", err)
				}
			}
			if err := allocNodes(missing); err != nil {
				return Nodes{}, err
			}
		}
	}

	nodes, err := Find()
	if err != nil {
		return nodes, err
	}
	// Android services run under many uids inside the container.
	for _, p := range nodes.Paths() {
		if err := os.Chmod(p, 0o666); err != nil {
			return nodes, err
		}
	}
	return nodes, nil
}

// Find returns the already prepared binder nodes without changing anything.
func Find() (Nodes, error) {
	n := Nodes{
		Binder:    lastExisting(binderNames),
		VndBinder: lastExisting(vndbinderNames),
		HwBinder:  lastExisting(hwbinderNames),
	}
	if n.Binder == "" || n.VndBinder == "" || n.HwBinder == "" {
		return n, errors.New("binder nodes not found: the kernel needs CONFIG_ANDROID_BINDER_IPC + CONFIG_ANDROID_BINDERFS (or the binder_linux module); run `lapis doctor`")
	}
	return n, nil
}

// Available reports whether binder can be provided at all on this host.
func Available() (bool, string) {
	if anyExists(binderNames) {
		return true, "binder device present"
	}
	if sysutil.HasFilesystem("binder") {
		return true, "binderfs supported by the kernel"
	}
	if _, err := sysutil.Output("modinfo", "-n", "binder_linux"); err == nil {
		return true, "binder_linux module available (not loaded)"
	}
	return false, "no binder support in the running kernel"
}

// ProbeAshmem tries to load ashmem; Android falls back to memfd without it.
func ProbeAshmem() bool {
	if !sysutil.Exists("/dev/ashmem") {
		_ = sysutil.Run("modprobe", "-q", "ashmem_linux")
	}
	return sysutil.Exists("/dev/ashmem")
}
