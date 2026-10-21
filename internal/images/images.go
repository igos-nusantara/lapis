// Package images downloads the LineageOS system and vendor images from
// their OTA channels, verifies them and extracts them.
package images

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"golang.org/x/sys/unix"

	"github.com/igos-nusantara/lapis/internal/compat"
	"github.com/igos-nusantara/lapis/internal/config"
	"github.com/igos-nusantara/lapis/internal/paths"
)

// Entry is one build in an OTA channel JSON file.
type Entry struct {
	Datetime int64  `json:"datetime"`
	Filename string `json:"filename"`
	ID       string `json:"id"` // sha256 of the zip
	RomType  string `json:"romtype"`
	Size     int64  `json:"size"`
	URL      string `json:"url"`
	Version  string `json:"version"`
}

// Arch returns the image architecture name for this host.
func Arch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "386":
		return "x86"
	case "arm":
		return "arm"
	default:
		return runtime.GOARCH
	}
}

func SystemOTA(im config.Images) string {
	return fmt.Sprintf("%s/%s/%s/%s.json", im.SystemChannel, im.RomType, compat.DeviceDir(im.Arch), im.SystemType)
}

func VendorOTA(im config.Images) string {
	return fmt.Sprintf("%s/%s/%s.json", im.VendorChannel, compat.DeviceDir(im.Arch), im.VendorType)
}

var client = &http.Client{Timeout: 0}

// Fetch downloads and parses an OTA channel file.
func Fetch(url string) ([]Entry, error) {
	c := &http.Client{Timeout: 30 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	var doc struct {
		Response []Entry `json:"response"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	if len(doc.Response) == 0 {
		return nil, fmt.Errorf("%s: no images in channel", url)
	}
	return doc.Response, nil
}

// Lock takes the exclusive image-update lock, so two init/upgrade runs
// never write the same files.
func Lock() (release func(), err error) {
	if err := os.MkdirAll(paths.WorkDir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(paths.WorkDir, ".images.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another `lapis init` or `lapis upgrade` is already running")
	}
	return func() { f.Close() }, nil
}

// Update downloads newer system/vendor images. It returns true when any
// image changed. With force, the newest build is installed regardless.
func Update(cfg *config.Config, force bool, progress io.Writer) (bool, error) {
	changed := false
	steps := []struct {
		kind     string
		url      string
		datetime *int64
	}{
		{"system", cfg.Images.SystemOTA, &cfg.Images.SystemDatetime},
		{"vendor", cfg.Images.VendorOTA, &cfg.Images.VendorDatetime},
	}
	for _, s := range steps {
		entries, err := Fetch(s.url)
		if err != nil {
			return changed, err
		}
		var pick *Entry
		for i := range entries {
			if force || entries[i].Datetime > *s.datetime {
				pick = &entries[i]
				break
			}
		}
		if pick == nil {
			fmt.Fprintf(progress, "%s image is up to date\n", s.kind)
			continue
		}
		fmt.Fprintf(progress, "Downloading %s (%d MiB)\n", pick.Filename, pick.Size>>20)
		zipPath := filepath.Join(cfg.Images.Path, pick.Filename)
		if err := download(pick, zipPath, progress); err != nil {
			return changed, err
		}
		fmt.Fprintf(progress, "Extracting %s.img\n", s.kind)
		if err := extract(zipPath, s.kind+".img", cfg.Images.Path); err != nil {
			return changed, err
		}
		os.Remove(zipPath)
		*s.datetime = pick.Datetime
		if err := cfg.Save(); err != nil {
			return changed, err
		}
		changed = true
	}
	if changed {
		// Old overlay upper layers may not match the new images.
		os.RemoveAll(paths.OverlayRW)
		os.RemoveAll(paths.OverlayWork)
	}
	return changed, nil
}

// Download mirrors (SourceForge) vary wildly in speed; a connection that
// stays below minSpeed for a whole window is dropped and resumed through a
// fresh redirect, which usually lands on another mirror.
var (
	minSpeed    int64 = 512 << 10 // bytes per second
	speedWindow       = 20 * time.Second
	maxAttempts       = 10
)

var errSlow = errors.New("download interrupted or too slow")

// download fetches e.URL into dest, resuming partial downloads and
// switching away from slow mirrors, and verifies the sha256.
func download(e *Entry, dest string, progress io.Writer) error {
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err = downloadOnce(e, dest, progress)
		if !errors.Is(err, errSlow) {
			return err
		}
		fmt.Fprintf(progress, "  %v; retrying with another mirror (%d/%d)\n", err, attempt, maxAttempts)
	}
	return err
}

// slowGuard fails reads once throughput over a window drops below
// minSpeed, and closes the body when no data arrives for a whole window.
type slowGuard struct {
	r       io.ReadCloser
	start   time.Time
	counted int64
	stall   *time.Timer
}

func newSlowGuard(r io.ReadCloser) *slowGuard {
	g := &slowGuard{r: r, start: time.Now()}
	g.stall = time.AfterFunc(speedWindow, func() { r.Close() })
	return g
}

func (g *slowGuard) Read(b []byte) (int, error) {
	n, err := g.r.Read(b)
	if n > 0 {
		g.stall.Reset(speedWindow)
	}
	g.counted += int64(n)
	if el := time.Since(g.start); el >= speedWindow {
		if g.counted < int64(el.Seconds())*minSpeed {
			return n, errSlow
		}
		g.start, g.counted = time.Now(), 0
	}
	return n, err
}

func downloadOnce(e *Entry, dest string, progress io.Writer) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	part := dest + ".part"
	h := sha256.New()
	var have int64
	if f, err := os.Open(part); err == nil {
		have, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return err
		}
	}

	req, err := http.NewRequest(http.MethodGet, e.URL, nil)
	if err != nil {
		return err
	}
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}
	resp, err := client.Do(req)
	if err != nil {
		// Connection and TLS failures are usually one bad mirror.
		return fmt.Errorf("%w: %v", errSlow, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("%w: HTTP %d", errSlow, resp.StatusCode)
	}

	flags := os.O_CREATE | os.O_WRONLY
	switch {
	case have > 0 && resp.StatusCode == http.StatusPartialContent:
		flags |= os.O_APPEND
		fmt.Fprintf(progress, "Resuming at %d MiB\n", have>>20)
	case resp.StatusCode == http.StatusOK:
		flags |= os.O_TRUNC
		have = 0
		h.Reset()
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && have == e.Size:
		// Already complete.
	default:
		return fmt.Errorf("download %s: HTTP %d", e.URL, resp.StatusCode)
	}

	out, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		pw := &progressWriter{w: progress, total: e.Size, done: have}
		body := newSlowGuard(resp.Body)
		_, err := io.Copy(io.MultiWriter(out, h, pw), body)
		body.stall.Stop()
		if err != nil {
			out.Close()
			// Slow, stalled or dropped connections are resumed from the
			// partial file through a new request.
			return fmt.Errorf("%w: %v", errSlow, err)
		}
		pw.finish()
	}
	if err := out.Close(); err != nil {
		return err
	}

	if sum := hex.EncodeToString(h.Sum(nil)); sum != e.ID {
		os.Remove(part)
		return fmt.Errorf("%s: sha256 mismatch (got %s, want %s)", e.Filename, sum, e.ID)
	}
	return os.Rename(part, dest)
}

// extract writes the zip member called name into dir atomically.
func extract(zipPath, name, dir string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		// Only accept the expected basename; never trust paths in the zip.
		if filepath.Base(f.Name) != name || f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		tmp := filepath.Join(dir, name+".tmp")
		out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, rc); err != nil {
			out.Close()
			os.Remove(tmp)
			return err
		}
		if err := out.Sync(); err != nil {
			out.Close()
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
		return os.Rename(tmp, filepath.Join(dir, name))
	}
	return fmt.Errorf("%s: %s not found in archive", zipPath, name)
}

// Present reports whether both images are installed.
func Present(dir string) bool {
	for _, n := range []string{"system.img", "vendor.img"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			return false
		}
	}
	return true
}

type progressWriter struct {
	w     io.Writer
	total int64
	done  int64
	last  time.Time
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.done += int64(len(b))
	if time.Since(p.last) > 2*time.Second {
		p.last = time.Now()
		if p.total > 0 {
			fmt.Fprintf(p.w, "  %d / %d MiB (%d%%)\n", p.done>>20, p.total>>20, p.done*100/p.total)
		} else {
			fmt.Fprintf(p.w, "  %d MiB\n", p.done>>20)
		}
	}
	return len(b), nil
}

func (p *progressWriter) finish() {
	fmt.Fprintf(p.w, "  %d MiB done\n", p.done>>20)
}

// ErrNotInitialized is returned when images have not been downloaded yet.
var ErrNotInitialized = errors.New("Android images not installed; run `sudo lapis init` first")
