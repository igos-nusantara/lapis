package seccomp

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// TestInstall loads the real filter in a child process (it cannot be
// removed again) and checks how the kernel answers a few syscalls.
func TestInstall(t *testing.T) {
	if os.Getenv("LAPIS_SECCOMP_CHILD") == "1" {
		if err := Install(); err != nil {
			fmt.Println("install:", err)
			os.Exit(1)
		}
		// keyctl(KEYCTL_GET_KEYRING_ID, KEY_SPEC_SESSION_KEYRING, 0) would
		// normally return a keyring id; the filter turns it into 0.
		r, _, errno := unix.Syscall(unix.SYS_KEYCTL, 0, ^uintptr(2), 0)
		fmt.Printf("keyctl=%d/%d\n", r, errno)
		_, _, errno = unix.Syscall(unix.SYS_INIT_MODULE, 0, 0, 0)
		fmt.Printf("init_module=%d\n", errno)
		fmt.Printf("getpid_ok=%v\n", unix.Getpid() > 0)
		os.Exit(0)
	}
	if _, err := HostArches(); err != nil {
		t.Skip(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestInstall$")
	cmd.Env = append(os.Environ(), "LAPIS_SECCOMP_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child failed: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"keyctl=0/0",
		fmt.Sprintf("init_module=%d", unix.EPERM),
		"getpid_ok=true",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in child output:\n%s", want, got)
		}
	}
}
