package control

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func needRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (mknod)")
	}
}

func TestSyncInputDir(t *testing.T) {
	needRoot(t)
	root := t.TempDir()
	in := filepath.Join(root, "dev/input")
	os.MkdirAll(in, 0o755)
	// The hwcomposer's FIFO stays; a node that is no longer a controller,
	// or whose minor changed, goes.
	unix.Mkfifo(filepath.Join(in, "wl_events"), 0o600)
	unix.Mknod(filepath.Join(in, "event9"), unix.S_IFCHR|0o600, int(unix.Mkdev(inputMajor, 9)))
	unix.Mknod(filepath.Join(in, "event5"), unix.S_IFCHR|0o600, int(unix.Mkdev(inputMajor, 6)))
	unix.Mknod(filepath.Join(in, "event3"), unix.S_IFCHR|0o660, int(unix.Mkdev(inputMajor, 3)))

	f, _ := os.Open(root)
	defer f.Close()
	var allowed []string
	err := syncInputDir(f, map[string]uint32{"event5": 5, "event3": 3},
		func(name string) error { allowed = append(allowed, name); return nil })
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(in, "wl_events")); err != nil {
		t.Error("FIFO removed")
	}
	if _, err := os.Lstat(filepath.Join(in, "event9")); err == nil {
		t.Error("stale event9 kept")
	}
	var st unix.Stat_t
	if err := unix.Lstat(filepath.Join(in, "event5"), &st); err != nil {
		t.Fatal(err)
	}
	if unix.Minor(st.Rdev) != 5 || st.Mode&0o777 != 0o660 || st.Gid != aidInput {
		t.Errorf("event5: minor %d mode %o gid %d", unix.Minor(st.Rdev), st.Mode&0o777, st.Gid)
	}
	if len(allowed) != 1 || allowed[0] != "event5" {
		t.Errorf("allowed %v, want only event5 (event3 exists)", allowed)
	}
}

func TestSyncInputDirSymlink(t *testing.T) {
	needRoot(t)
	root, outside := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(root, "dev"), 0o755)
	os.Symlink(outside, filepath.Join(root, "dev/input"))
	f, _ := os.Open(root)
	defer f.Close()
	err := syncInputDir(f, map[string]uint32{"event5": 5}, func(string) error { return nil })
	if err == nil {
		t.Error("followed a symlink out of the container")
	}
	if names, _ := os.ReadDir(outside); len(names) != 0 {
		t.Errorf("created %v outside the container", names)
	}
}

// Left and right stick, triggers and the D-pad.
var axes = []uint{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x10, 0x11}

// TestHostGamepads creates a virtual gamepad and expects udev to report it.
func TestHostGamepads(t *testing.T) {
	needRoot(t)
	u, err := os.OpenFile("/dev/uinput", os.O_WRONLY, 0)
	if err != nil {
		t.Skip("no /dev/uinput:", err)
	}
	defer u.Close()
	const (
		uiSetEvbit   = 0x40045564
		uiSetKeybit  = 0x40045565
		uiSetAbsbit  = 0x40045567
		uiDevCreate  = 0x5501
		uiDevDestry  = 0x5502
		evKey, evAbs = 0x01, 0x03
		btnSouth     = 0x130
	)
	fd := int(u.Fd())
	// Shaped like a real controller: udev's joystick heuristic rejects
	// devices with only a button or two.
	sets := [][2]uint{{uiSetEvbit, evKey}, {uiSetEvbit, evAbs}}
	for _, b := range []uint{btnSouth, btnSouth + 1, btnSouth + 3, btnSouth + 4, btnSouth + 6, btnSouth + 7, btnSouth + 10, btnSouth + 11} {
		sets = append(sets, [2]uint{uiSetKeybit, b}) // A B X Y TL TR SELECT START
	}
	for _, a := range axes {
		sets = append(sets, [2]uint{uiSetAbsbit, a})
	}
	for _, c := range sets {
		if err := unix.IoctlSetInt(fd, c[0], int(c[1])); err != nil {
			t.Fatal(err)
		}
	}
	// struct uinput_user_dev: name, input_id, ff_effects_max, absmax/min/fuzz/flat.
	var dev struct {
		Name                           [80]byte
		Bustype, Vendor, Product, Ver  uint16
		FFEffectsMax                   uint32
		Absmax, Absmin, Absfuzz, Absfl [64]int32
	}
	copy(dev.Name[:], "lapis test gamepad")
	dev.Bustype, dev.Vendor, dev.Product = 0x03, 0x1234, 0x5678
	for _, a := range axes {
		dev.Absmin[a], dev.Absmax[a] = -1, 1 // hats
		if a < 0x10 {
			dev.Absmin[a], dev.Absmax[a] = 0, 255
		}
	}
	var buf bytes.Buffer
	binary.Write(&buf, binary.NativeEndian, &dev)
	if _, err := u.Write(buf.Bytes()); err != nil {
		t.Fatal(err)
	}
	before := hostGamepads()
	if err := unix.IoctlSetInt(fd, uiDevCreate, 0); err != nil {
		t.Fatal(err)
	}

	added := ""
	for try := 0; try < 50 && added == ""; try++ {
		time.Sleep(100 * time.Millisecond)
		for name := range hostGamepads() {
			if _, ok := before[name]; !ok {
				added = name
			}
		}
	}
	if added == "" {
		nodes, _ := filepath.Glob("/sys/class/input/event*")
		for _, n := range nodes {
			name, _ := os.ReadFile(filepath.Join(n, "device/name"))
			dev, _ := os.ReadFile(filepath.Join(n, "dev"))
			db, err := os.ReadFile("/run/udev/data/c" + string(bytes.TrimSpace(dev)))
			t.Logf("%s %q udev=%v\n%s", n, bytes.TrimSpace(name), err, db)
		}
		t.Fatal("udev did not report the virtual gamepad as a joystick")
	}
	t.Log("gamepad:", added)

	unix.IoctlSetInt(fd, uiDevDestry, 0)
	for try := 0; try < 50; try++ {
		if _, ok := hostGamepads()[added]; !ok {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Error("removed gamepad still reported")
}
