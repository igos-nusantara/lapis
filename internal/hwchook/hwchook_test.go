package hwchook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const vendorRc = `service vendor.hwcomposer-2-1 /vendor/bin/hw/android.hardware.graphics.composer@2.1-service
    interface android.hardware.graphics.composer@2.1::IComposer default
    class hal animation
    user system
`

const systemRc = `on boot
    start foo

service vendor.hwcomposer-2-1 /vendor/bin/hw/android.hardware.graphics.composer@2.1-service --hint
    override
    class hal animation
    user host

service audio /vendor/bin/audio
    class hal
`

func write(t *testing.T, root, path, body string) {
	t.Helper()
	p := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestEffectiveDefinition(t *testing.T) {
	root := t.TempDir()
	write(t, root, "vendor/etc/init/composer.rc", vendorRc)
	def, _ := composerService(root)
	if !strings.Contains(def, "user system") {
		t.Fatalf("vendor-only definition: %q", def)
	}
	// The system definition comes first and the vendor one has no override.
	write(t, root, "system/etc/init/init.extra.rc", systemRc)
	def, _ = composerService(root)
	if !strings.Contains(def, "--hint") || strings.Contains(def, "audio") || strings.Contains(def, "start foo") {
		t.Fatalf("effective definition: %q", def)
	}
}

func TestWithPreload(t *testing.T) {
	out := withPreload("service c /bin/c --hint\n    override\n    setenv LD_PRELOAD /x.so\n    user host\n")
	want := "service c /bin/c --hint\n    override\n" + preload + "\n    user host\n"
	if out != want {
		t.Fatalf("got\n%s\nwant\n%s", out, want)
	}
}

func TestInstall(t *testing.T) {
	root, overlay := t.TempDir(), t.TempDir()
	write(t, root, "vendor/etc/init/composer.rc", vendorRc)
	write(t, root, "system/etc/init/init.extra.rc", systemRc)
	if err := Install(root, overlay); err != nil {
		t.Fatal(err)
	}
	rc, err := os.ReadFile(filepath.Join(overlay, "system", "etc", "init", rcName))
	if err != nil || !strings.Contains(string(rc), preload) || !strings.Contains(string(rc), "--hint") {
		t.Fatalf("rc: %v\n%s", err, rc)
	}
	if lib, err := os.ReadFile(filepath.Join(overlay, "vendor", "lib64", libName)); err != nil || string(lib[:4]) != "\x7fELF" {
		t.Fatalf("library: %v", err)
	}

	// An image without a composer service: the earlier install is removed.
	os.RemoveAll(filepath.Join(root, "vendor"))
	os.RemoveAll(filepath.Join(root, "system"))
	if err := Install(root, overlay); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"system/etc/init/" + rcName, "vendor/lib64/" + libName} {
		if _, err := os.Stat(filepath.Join(overlay, p)); err == nil {
			t.Fatalf("stale %s left behind", p)
		}
	}
}
