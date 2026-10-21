package images

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var modTime = time.Unix(0, 0)

func zipWith(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("images/" + name)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(body)
	zw.Close()
	return buf.Bytes()
}

func TestDownloadResumeAndExtract(t *testing.T) {
	payload := bytes.Repeat([]byte("android"), 50000)
	archive := zipWith(t, "system.img", payload)
	sum := sha256.Sum256(archive)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "x.zip", modTime, bytes.NewReader(archive))
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "x.zip")
	// Simulate an interrupted earlier download.
	os.WriteFile(dest+".part", archive[:len(archive)/3], 0o644)

	e := &Entry{URL: srv.URL, ID: hex.EncodeToString(sum[:]), Size: int64(len(archive)), Filename: "x.zip"}
	if err := download(e, dest, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := extract(dest, "system.img", dir); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "system.img"))
	if !bytes.Equal(got, payload) {
		t.Error("extracted image differs")
	}

	// A wrong checksum must be rejected and the partial file removed.
	bad := *e
	bad.ID = hex.EncodeToString(make([]byte, 32))
	if err := download(&bad, filepath.Join(dir, "y.zip"), io.Discard); err == nil {
		t.Error("expected checksum error")
	}
	if _, err := os.Stat(filepath.Join(dir, "y.zip.part")); err == nil {
		t.Error("partial file kept after checksum failure")
	}
}

func TestSlowMirrorIsRetried(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 64<<10)
	archive := zipWith(t, "vendor.img", payload)
	sum := sha256.Sum256(archive)
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			// First "mirror" trickles a few bytes and then stalls.
			w.Header().Set("Content-Length", fmt.Sprint(len(archive)))
			w.Write(archive[:100])
			w.(http.Flusher).Flush()
			time.Sleep(300 * time.Millisecond)
			return
		}
		http.ServeContent(w, r, "x.zip", modTime, bytes.NewReader(archive))
	}))
	defer srv.Close()

	oldWindow, oldSpeed := speedWindow, minSpeed
	speedWindow, minSpeed = 100*time.Millisecond, 1<<20
	defer func() { speedWindow, minSpeed = oldWindow, oldSpeed }()

	dest := filepath.Join(t.TempDir(), "x.zip")
	e := &Entry{URL: srv.URL, ID: hex.EncodeToString(sum[:]), Size: int64(len(archive)), Filename: "x.zip"}
	if err := download(e, dest, io.Discard); err != nil {
		t.Fatal(err)
	}
	if requests < 2 {
		t.Errorf("slow mirror was not abandoned (requests=%d)", requests)
	}
}

func TestServerErrorIsRetried(t *testing.T) {
	archive := zipWith(t, "vendor.img", []byte("data"))
	sum := sha256.Sum256(archive)
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			http.Error(w, "mirror down", http.StatusBadGateway)
			return
		}
		http.ServeContent(w, r, "x.zip", modTime, bytes.NewReader(archive))
	}))
	defer srv.Close()
	e := &Entry{URL: srv.URL, ID: hex.EncodeToString(sum[:]), Size: int64(len(archive)), Filename: "x.zip"}
	if err := download(e, filepath.Join(t.TempDir(), "x.zip"), io.Discard); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Errorf("requests = %d, want 2", requests)
	}
}
