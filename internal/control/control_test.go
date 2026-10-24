package control

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/igos-nusantara/lapis/internal/api"
)

func TestFitTask(t *testing.T) {
	disp := []int{0, 0, 1280, 752}
	for _, c := range []struct {
		name           string
		l, tp, r, b    int
		w, h           int
		wl, wt, wr, wb int
	}{
		// A maximized task shrinks to a 640 tile and moves up to stay on
		// the display.
		{"tile", 0, 28, 1280, 780, 640, 778, 0, 0, 640, 752},
		// A phone window grows by the drag and keeps its place.
		{"grow", 424, 54, 864, 746, 700, 600, 424, 54, 1124, 654},
		// Growing past the right edge moves the task left.
		{"edge", 900, 54, 1200, 600, 640, 586, 640, 54, 1280, 640},
		// Never below the minimum or above the display.
		{"tiny", 0, 0, 640, 400, 120, 90, 0, 0, 200, 200},
		{"huge", 0, 0, 640, 400, 3000, 3000, 0, 0, 1280, 752},
	} {
		l, tp, r, b := fitTask(c.l, c.tp, c.r, c.b, api.Request{ToWidth: c.w, ToHeight: c.h}, disp)
		if l != c.wl || tp != c.wt || r != c.wr || b != c.wb {
			t.Errorf("%s: got [%d,%d][%d,%d], want [%d,%d][%d,%d]", c.name, l, tp, r, b, c.wl, c.wt, c.wr, c.wb)
		}
	}
}

func TestParsePackages(t *testing.T) {
	out := "package:org.fdroid.fdroid versionCode:1021050\n" +
		"package:com.example.app versionCode:7\n" +
		"WARNING: linker noise\n" +
		"package:bad versionCode:1\n"
	got := parsePackages(out)
	want := []api.Package{{Package: "org.fdroid.fdroid", VersionCode: 1021050}, {Package: "com.example.app", VersionCode: 7}}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("package %d: got %v, want %v", i, got[i], want[i])
		}
	}
}

func TestOpenOwnedAPK(t *testing.T) {
	dir := t.TempDir()
	apk := filepath.Join(dir, "app.apk")
	if err := os.WriteFile(apk, []byte("PK"), 0o644); err != nil {
		t.Fatal(err)
	}
	uid := uint32(os.Getuid())

	f, size, err := openOwnedAPK(apk, uid)
	if err != nil {
		t.Fatalf("own APK: %v", err)
	}
	f.Close()
	if size != 2 {
		t.Errorf("size %d, want 2", size)
	}

	link := filepath.Join(dir, "link.apk")
	if err := os.Symlink(apk, link); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "pipe.apk")
	if err := unix.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		path string
		uid  uint32
	}{
		"relative":    {"app.apk", uid},
		"not an apk":  {filepath.Join(dir, "app.txt"), uid},
		"symlink":     {link, uid},
		"fifo":        {fifo, uid},
		"other owner": {apk, uid + 1},
	} {
		if f, _, err := openOwnedAPK(c.path, c.uid); err == nil {
			f.Close()
			t.Errorf("%s: accepted", name)
		}
	}
}
