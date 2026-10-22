package container

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/igos-nusantara/lapis/internal/api"
	"github.com/igos-nusantara/lapis/internal/paths"
	"github.com/igos-nusantara/lapis/internal/seccomp"
	"github.com/igos-nusantara/lapis/internal/sysutil"
)

// androidEnv is the Android runtime environment for commands run in the
// container.
var androidEnv = []string{
	"PATH=/product/bin:/apex/com.android.runtime/bin:/apex/com.android.art/bin:/system_ext/bin:/system/bin:/system/xbin:/odm/bin:/vendor/bin:/vendor/xbin",
	"ANDROID_ROOT=/system",
	"ANDROID_DATA=/data",
	"ANDROID_STORAGE=/storage",
	"ANDROID_ART_ROOT=/apex/com.android.art",
	"ANDROID_I18N_ROOT=/apex/com.android.i18n",
	"ANDROID_TZDATA_ROOT=/apex/com.android.tzdata",
	"ANDROID_RUNTIME_ROOT=/apex/com.android.runtime",
	"ANDROID_ASSETS=/system/app",
	"EXTERNAL_STORAGE=/sdcard", // uiautomator and other shell tools need it
	"ASEC_MOUNTPOINT=/mnt/asec",
	"DOWNLOAD_CACHE=/data/cache",
}

// Env returns the environment for Android commands, adding the
// CLASSPATH/SYSTEMSERVER variables Android generated in its data dir.
func Env(s *api.Session) []string {
	env := append([]string{}, androidEnv...)
	if s == nil {
		return env
	}
	f, err := os.Open(filepath.Join(s.DataDir, "system/environ/classpath"))
	if err != nil {
		return env
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		// Lines look like: export CLASSPATH /apex/...
		parts := strings.SplitN(sc.Text(), " ", 3)
		if len(parts) == 3 && (strings.Contains(parts[1], "CLASSPATH") || strings.Contains(parts[1], "SYSTEMSERVER")) {
			env = append(env, parts[1]+"="+parts[2])
		}
	}
	return env
}

// State returns the container state from its systemd unit.
func State() string {
	active, _ := sysutil.Output("systemctl", "is-active", paths.ContainerUnit)
	switch active {
	case "active", "reloading", "activating", "deactivating":
		// A restart (e.g. Android rebooting itself) passes through
		// deactivating and activating; it is not a stop.
	default:
		return api.StateStopped
	}
	freezer, _ := sysutil.Output("systemctl", "show", "-P", "FreezerState", paths.ContainerUnit)
	if freezer == "frozen" || freezer == "freezing" {
		return api.StateFrozen
	}
	return api.StateRunning
}

// LeaderPID returns the host PID of the container's init.
func LeaderPID() (int, error) {
	out, err := sysutil.Output("systemctl", "show", "-P", "MainPID", paths.ContainerUnit)
	if err != nil {
		return 0, err
	}
	mainPID, _ := strconv.Atoi(out)
	if mainPID <= 0 {
		return 0, errors.New("container is not running")
	}
	selfNS, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return 0, err
	}
	// Breadth-first search below nspawn for the first process living in
	// another PID namespace: that is the container's PID 1.
	queue := []int{mainPID}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		for _, child := range children(pid) {
			ns, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/pid", child))
			if err == nil && ns != selfNS {
				return child, nil
			}
			queue = append(queue, child)
		}
	}
	return 0, errors.New("container init process not found")
}

func children(pid int) []int {
	tasks, _ := filepath.Glob(fmt.Sprintf("/proc/%d/task/*/children", pid))
	var out []int
	for _, t := range tasks {
		data, err := os.ReadFile(t)
		if err != nil {
			continue
		}
		for _, f := range strings.Fields(string(data)) {
			if n, err := strconv.Atoi(f); err == nil {
				out = append(out, n)
			}
		}
	}
	return out
}

// Command builds a command that runs argv inside the container as root.
// It enters the container's namespaces with nsenter, is created directly
// in the container's cgroup (device policy applies) and runs under the
// container's seccomp filter, via `lapis container attach-exec`.
func Command(s *api.Session, argv ...string) (*exec.Cmd, error) {
	if len(argv) == 0 {
		argv = []string{"/system/bin/sh"}
	}
	pid, err := LeaderPID()
	if err != nil {
		return nil, err
	}
	nsenter, err := exec.LookPath("nsenter")
	if err != nil {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cg, err := os.Open(cgroupDir(pid))
	if err != nil {
		return nil, fmt.Errorf("container cgroup: %w", err)
	}
	args := []string{"container", "attach-exec", nsenter, "--target", strconv.Itoa(pid),
		"--mount", "--uts", "--ipc", "--net", "--pid", "--cgroup",
		"--root", "--wd", "--"}
	cmd := exec.Command(self, append(args, argv...)...)
	cmd.Env = Env(s)
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(cg.Fd())}
	// The fd must stay open until the child is created.
	runtime.SetFinalizer(cmd, func(*exec.Cmd) { cg.Close() })
	return cmd, nil
}

// cgroupDir returns the cgroup v2 directory of a process.
func cgroupDir(pid int) string {
	data, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			return filepath.Join("/sys/fs/cgroup", rest)
		}
	}
	return ""
}

// AttachExec installs the container seccomp filter and execs argv (the
// nsenter command line built by Command).
func AttachExec(argv []string) error {
	if len(argv) == 0 {
		return errors.New("attach-exec: missing command")
	}
	if err := seccomp.Install(); err != nil {
		return err
	}
	return syscall.Exec(argv[0], argv, os.Environ())
}

// Output runs argv in the container and returns its trimmed stdout.
func Output(s *api.Session, argv ...string) (string, error) {
	cmd, err := Command(s, argv...)
	if err != nil {
		return "", err
	}
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("%s: %w: %s", argv[0], err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
