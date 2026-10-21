// Package seccomp installs the container's syscall filter as a raw BPF
// program, so no libseccomp is needed. The filter is installed right
// before exec'ing systemd-nspawn and is inherited by the whole container.
package seccomp

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	retAllow = 0x7fff0000 // SECCOMP_RET_ALLOW
	retErrno = 0x00050000 // SECCOMP_RET_ERRNO

	auditArchX86_64  = 0xc000003e
	auditArchI386    = 0x40000003
	auditArchAarch64 = 0xc00000b7
	auditArchARM     = 0x40000028

	x32SyscallBit = 0x40000000

	// struct seccomp_data offsets
	offNr   = 0
	offArch = 4
)

// rule maps a syscall to the errno it returns. Errno 0 turns the call into
// a successful no-op: Android expects clock and keyring calls to work, but
// they must not touch the host.
type rule struct {
	name  string
	errno uint16
}

// reboot is intentionally allowed: inside a PID namespace it only ends the
// container, which systemd-nspawn reports as shutdown or reboot (exit 133).
var rules = []rule{
	{"init_module", uint16(unix.EPERM)},
	{"finit_module", uint16(unix.EPERM)},
	{"delete_module", uint16(unix.EPERM)},
	{"_sysctl", uint16(unix.EPERM)},
	{"kexec_file_load", uint16(unix.EPERM)},
	{"kexec_load", uint16(unix.EPERM)},
	{"open_by_handle_at", uint16(unix.ENOSYS)},
	{"adjtimex", 0},
	{"clock_adjtime", 0},
	{"clock_adjtime64", 0},
	{"clock_settime", 0},
	{"clock_settime64", 0},
	{"settimeofday", 0},
	{"stime", 0},
	{"add_key", 0},
	{"keyctl", 0},
	{"request_key", 0},
	{"swapoff", 0},
	{"swapon", 0},
}

// Syscall numbers per audit arch. Calls missing from an arch are skipped.
var tables = map[uint32]map[string]uint32{
	auditArchX86_64: {
		"init_module": 175, "finit_module": 313, "delete_module": 176,
		"_sysctl": 156, "kexec_file_load": 320, "kexec_load": 246,
		"open_by_handle_at": 304, "adjtimex": 159, "clock_adjtime": 305,
		"clock_settime": 227, "settimeofday": 164, "add_key": 248,
		"keyctl": 250, "request_key": 249, "swapoff": 168, "swapon": 167,
	},
	auditArchI386: {
		"init_module": 128, "finit_module": 350, "delete_module": 129,
		"_sysctl": 149, "kexec_load": 283, "open_by_handle_at": 342,
		"adjtimex": 124, "clock_adjtime": 343, "clock_adjtime64": 405,
		"clock_settime": 264, "clock_settime64": 404, "settimeofday": 79,
		"stime": 25, "add_key": 286, "keyctl": 288, "request_key": 287,
		"swapoff": 115, "swapon": 87,
	},
	auditArchAarch64: {
		"init_module": 105, "finit_module": 273, "delete_module": 106,
		"kexec_file_load": 294, "kexec_load": 104, "open_by_handle_at": 265,
		"adjtimex": 171, "clock_adjtime": 266, "clock_settime": 112,
		"settimeofday": 170, "add_key": 217, "keyctl": 219,
		"request_key": 218, "swapoff": 225, "swapon": 224,
	},
	auditArchARM: {
		"init_module": 128, "finit_module": 379, "delete_module": 129,
		"_sysctl": 149, "kexec_load": 347, "open_by_handle_at": 371,
		"adjtimex": 124, "clock_adjtime": 372, "clock_adjtime64": 405,
		"clock_settime": 262, "clock_settime64": 404, "settimeofday": 79,
		"add_key": 309, "keyctl": 311, "request_key": 310,
		"swapoff": 115, "swapon": 87,
	},
}

// HostArches returns the audit arches the container may use: the native
// one plus its 32-bit compat ABI (Android ships 32-bit zygotes).
func HostArches() ([]uint32, error) {
	switch runtime.GOARCH {
	case "amd64":
		return []uint32{auditArchX86_64, auditArchI386}, nil
	case "arm64":
		return []uint32{auditArchAarch64, auditArchARM}, nil
	}
	return nil, fmt.Errorf("seccomp: unsupported architecture %s", runtime.GOARCH)
}

func stmt(code uint16, k uint32) unix.SockFilter {
	return unix.SockFilter{Code: code, K: k}
}

func jump(code uint16, k uint32, jt, jf uint8) unix.SockFilter {
	return unix.SockFilter{Code: code, Jt: jt, Jf: jf, K: k}
}

const (
	ldAbs = unix.BPF_LD | unix.BPF_W | unix.BPF_ABS
	jeqK  = unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K
	jgeK  = unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K
	retK  = unix.BPF_RET | unix.BPF_K
)

// Build returns the BPF program for the given arches.
func Build(arches []uint32) []unix.SockFilter {
	var prog []unix.SockFilter
	for _, arch := range arches {
		var body []unix.SockFilter
		body = append(body, stmt(ldAbs, offNr))
		if arch == auditArchX86_64 {
			// Refuse the x32 ABI entirely.
			body = append(body,
				jump(jgeK, x32SyscallBit, 0, 1),
				stmt(retK, retErrno|uint32(unix.ENOSYS)))
		}
		for _, r := range rules {
			nr, ok := tables[arch][r.name]
			if !ok {
				continue
			}
			body = append(body,
				jump(jeqK, nr, 0, 1),
				stmt(retK, retErrno|uint32(r.errno)))
		}
		body = append(body, stmt(retK, retAllow))

		prog = append(prog,
			stmt(ldAbs, offArch),
			jump(jeqK, arch, 0, uint8(len(body))))
		prog = append(prog, body...)
	}
	return append(prog, stmt(retK, retAllow))
}

// Install applies the filter to every thread of the current process, so
// it is inherited by the program exec'ed next.
func Install() error {
	arches, err := HostArches()
	if err != nil {
		return err
	}
	filter := Build(arches)
	prog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}

	runtime.LockOSThread()
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("no_new_privs: %w", err)
	}
	_, _, errno := unix.Syscall(unix.SYS_SECCOMP,
		unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC,
		uintptr(unsafe.Pointer(&prog)))
	if errno != 0 {
		return fmt.Errorf("seccomp: %w", errno)
	}
	return nil
}
