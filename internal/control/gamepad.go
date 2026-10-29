package control

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/igos-nusantara/lapis/internal/container"
	"github.com/igos-nusantara/lapis/internal/paths"
	"github.com/igos-nusantara/lapis/internal/sysutil"
)

// Wayland has no protocol for game controllers, so Android gets the host's
// evdev nodes for them. The container may read and write those nodes but not
// create any (no "m" in DeviceAllow), and every input node in it that is not
// a current controller is removed, so a minor number reused by a keyboard
// never reaches Android.

const (
	inputMajor = 13
	aidInput   = 1004 // Android's input group, which InputFlinger reads with
)

// syncGamepads makes the container's /dev/input hold exactly the host's
// game controllers.
func syncGamepads() error {
	pid, err := container.LeaderPID()
	if err != nil {
		return err
	}
	root, err := os.Open(fmt.Sprintf("/proc/%d/root", pid))
	if err != nil {
		return err
	}
	defer root.Close()
	// The container's root owns this tree: never follow its symlinks out
	// of it.
	fd, err := unix.Openat2(int(root.Fd()), "dev/input", &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS,
	})
	if errors.Is(err, unix.ENOENT) {
		return nil // Android's hwcomposer creates it once the display is up
	}
	if err != nil {
		return err
	}
	dir := os.NewFile(uintptr(fd), "/dev/input")
	defer dir.Close()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return err
	}

	want := hostGamepads()
	for _, name := range names {
		var st unix.Stat_t
		if unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW) != nil ||
			st.Mode&unix.S_IFMT != unix.S_IFCHR || unix.Major(st.Rdev) != inputMajor {
			continue // the hwcomposer's input FIFOs
		}
		if minor, ok := want[name]; ok && unix.Minor(st.Rdev) == minor {
			delete(want, name)
			continue
		}
		if err := unix.Unlinkat(fd, name, 0); err != nil {
			return err
		}
	}

	old := unix.Umask(0)
	defer unix.Umask(old)
	for name, minor := range want {
		if err := sysutil.Run("systemctl", "set-property", "--runtime", paths.ContainerUnit,
			"DeviceAllow=/dev/input/"+name+" rw"); err != nil {
			return err
		}
		if err := unix.Mknodat(fd, name, unix.S_IFCHR|0o660, int(unix.Mkdev(inputMajor, minor))); err != nil {
			return err
		}
		if err := unix.Fchownat(fd, name, 0, aidInput, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
	}
	return nil
}

// hostGamepads returns the host's game controllers: event node name to
// minor number, as udev classified them.
func hostGamepads() map[string]uint32 {
	found := map[string]uint32{}
	nodes, _ := filepath.Glob("/sys/class/input/event*")
	for _, n := range nodes {
		dev, err := os.ReadFile(filepath.Join(n, "dev"))
		if err != nil {
			continue
		}
		major, minor, _ := strings.Cut(strings.TrimSpace(string(dev)), ":")
		m, err := strconv.ParseUint(minor, 10, 32)
		if major != strconv.Itoa(inputMajor) || err != nil {
			continue
		}
		db, err := os.ReadFile("/run/udev/data/c" + major + ":" + minor)
		if err == nil && isJoystick(string(db)) {
			found[filepath.Base(n)] = uint32(m)
		}
	}
	return found
}

// isJoystick reads a udev database entry.
func isJoystick(db string) bool {
	for _, line := range strings.Split(db, "\n") {
		if line == "E:ID_INPUT_JOYSTICK=1" {
			return true
		}
	}
	return false
}
