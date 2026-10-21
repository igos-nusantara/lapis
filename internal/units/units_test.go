package units

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestVerify renders the system units and runs systemd-analyze on them.
func TestVerify(t *testing.T) {
	analyze, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("systemd-analyze not available")
	}
	bin, err := exec.LookPath("true")
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	var files []string
	for name, body := range systemUnits {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(strings.ReplaceAll(body, "@BIN@", bin)), 0o644); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(name, "@") {
			files = append(files, p)
		}
	}
	out, err := exec.Command(analyze, append([]string{"verify", "--man=no"}, files...)...).CombinedOutput()
	// Only report problems that concern our own units.
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "lapis") {
			t.Errorf("systemd-analyze: %s", line)
		}
	}
	if err != nil && !t.Failed() {
		t.Logf("systemd-analyze exited with %v (unrelated warnings):\n%s", err, out)
	}
}
