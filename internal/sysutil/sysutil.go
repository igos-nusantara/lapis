// Package sysutil contains small host helpers: running commands, mount
// table queries, sd_notify and key=value file parsing.
package sysutil

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Run executes a command and folds its stderr into the returned error.
func Run(name string, args ...string) error {
	_, err := Output(name, args...)
	return err
}

// Output executes a command and returns its trimmed stdout.
func Output(name string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
		}
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// unescapeMount decodes the octal escapes used in /proc/self/mountinfo.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// MountPoints returns all current mount points.
func MountPoints() ([]string, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		out = append(out, unescapeMount(fields[4]))
	}
	return out, sc.Err()
}

func IsMount(path string) bool {
	mps, err := MountPoints()
	if err != nil {
		return false
	}
	for _, mp := range mps {
		if mp == path {
			return true
		}
	}
	return false
}

// UmountAll unmounts everything at or below prefix, deepest first.
func UmountAll(prefix string) error {
	mps, err := MountPoints()
	if err != nil {
		return err
	}
	var targets []string
	for _, mp := range mps {
		if mp == prefix || strings.HasPrefix(mp, prefix+"/") {
			targets = append(targets, mp)
		}
	}
	// Longer paths first; repeated entries are stacked mounts and need
	// one unmount each.
	sort.SliceStable(targets, func(i, j int) bool { return len(targets[i]) > len(targets[j]) })
	for _, t := range targets {
		if err := unix.Unmount(t, 0); err != nil {
			if err := unix.Unmount(t, unix.MNT_DETACH); err != nil && err != unix.EINVAL {
				return fmt.Errorf("umount %s: %w", t, err)
			}
		}
	}
	return nil
}

// HasFilesystem reports whether the kernel lists fstype in /proc/filesystems.
func HasFilesystem(fstype string) bool {
	data, err := os.ReadFile("/proc/filesystems")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[len(fields)-1] == fstype {
			return true
		}
	}
	return false
}

// FileGet returns the value of key in a key=value file (build.prop, uevent).
func FileGet(path, key string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		if k == key {
			return v
		}
	}
	return ""
}

// Notify sends a state string to systemd if running under a notify unit.
func Notify(state string) error {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" {
		return nil
	}
	if sock[0] == '@' {
		sock = "\x00" + sock[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: sock, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte(state))
	return err
}

// Exists reports whether path exists (without following a final symlink).
func Exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// RequireRoot returns an error when not running as root.
func RequireRoot(what string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("%s needs root; run it with sudo", what)
	}
	return nil
}
