package seccomp

import (
	"encoding/binary"
	"testing"

	"golang.org/x/sys/unix"
)

// eval interprets the subset of classic BPF that Build emits.
func eval(t *testing.T, prog []unix.SockFilter, arch, nr uint32) uint32 {
	t.Helper()
	data := make([]byte, 64)
	binary.LittleEndian.PutUint32(data[offNr:], nr)
	binary.LittleEndian.PutUint32(data[offArch:], arch)
	var acc uint32
	for pc := 0; pc < len(prog); pc++ {
		in := prog[pc]
		switch in.Code {
		case ldAbs:
			acc = binary.LittleEndian.Uint32(data[in.K:])
		case jeqK:
			if acc == in.K {
				pc += int(in.Jt)
			} else {
				pc += int(in.Jf)
			}
		case jgeK:
			if acc >= in.K {
				pc += int(in.Jt)
			} else {
				pc += int(in.Jf)
			}
		case retK:
			return in.K
		default:
			t.Fatalf("unexpected opcode %#x at %d", in.Code, pc)
		}
	}
	t.Fatal("program fell off the end")
	return 0
}

func TestFilter(t *testing.T) {
	prog := Build([]uint32{auditArchX86_64, auditArchI386})
	cases := []struct {
		name string
		arch uint32
		nr   uint32
		want uint32
	}{
		{"x86_64 read allowed", auditArchX86_64, 0, retAllow},
		{"x86_64 reboot allowed", auditArchX86_64, 169, retAllow},
		{"x86_64 keyctl no-op", auditArchX86_64, 250, retErrno},
		{"x86_64 init_module EPERM", auditArchX86_64, 175, retErrno | uint32(unix.EPERM)},
		{"x86_64 open_by_handle_at ENOSYS", auditArchX86_64, 304, retErrno | uint32(unix.ENOSYS)},
		{"x86_64 clock_settime no-op", auditArchX86_64, 227, retErrno},
		{"x32 refused", auditArchX86_64, x32SyscallBit | 1, retErrno | uint32(unix.ENOSYS)},
		{"i386 keyctl no-op", auditArchI386, 288, retErrno},
		{"i386 stime no-op", auditArchI386, 25, retErrno},
		// i386 nr 250 is fadvise64, must not be confused with x86_64 keyctl.
		{"i386 nr 250 allowed", auditArchI386, 250, retAllow},
		{"i386 init_module EPERM", auditArchI386, 128, retErrno | uint32(unix.EPERM)},
		{"unknown arch allowed", 0x1234, 250, retAllow},
	}
	for _, c := range cases {
		if got := eval(t, prog, c.arch, c.nr); got != c.want {
			t.Errorf("%s: got %#x, want %#x", c.name, got, c.want)
		}
	}
}

func TestTablesCoverRules(t *testing.T) {
	known := map[string]bool{}
	for _, r := range rules {
		known[r.name] = true
	}
	for arch, tbl := range tables {
		for name := range tbl {
			if !known[name] {
				t.Errorf("arch %#x: %s has a number but no rule", arch, name)
			}
		}
		// Every arch must at least cover the keyring and module calls.
		for _, must := range []string{"keyctl", "add_key", "init_module", "swapon"} {
			if _, ok := tbl[must]; !ok {
				t.Errorf("arch %#x: missing %s", arch, must)
			}
		}
	}
}
