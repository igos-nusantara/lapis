package wlproxy

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// enc builds a Wayland message from uint32/int32/string arguments.
func enc(id uint32, op uint16, vals ...any) []byte {
	var body []byte
	for _, v := range vals {
		switch v := v.(type) {
		case uint32:
			body = le.AppendUint32(body, v)
		case int32:
			body = le.AppendUint32(body, uint32(v))
		case int:
			body = le.AppendUint32(body, uint32(v))
		case string:
			body = le.AppendUint32(body, uint32(len(v)+1))
			body = append(body, v...)
			body = append(body, 0)
			for len(body)%4 != 0 {
				body = append(body, 0)
			}
		}
	}
	m := le.AppendUint32(nil, id)
	m = le.AppendUint32(m, uint32(headerSize+len(body))<<16|uint32(op))
	return append(m, body...)
}

func isApp(id string) bool { return strings.HasPrefix(id, "android.") }

// window sets up registry objects, a window surface (10) with an xdg
// surface (11) and toplevel (12), and one layer subsurface.
func window(t *testing.T, c *conn, appID string) {
	t.Helper()
	for _, m := range [][]byte{
		enc(1, 1, uint32(2)),                        // get_registry -> 2
		enc(2, 0, 1, "wl_compositor", 4, uint32(3)), // bind -> 3
		enc(2, 0, 2, "wl_subcompositor", 1, uint32(4)),
		enc(2, 0, 3, "wp_viewporter", 1, uint32(5)),
		enc(2, 0, 4, "xdg_wm_base", 2, uint32(6)),
		enc(3, 0, uint32(10)),                         // window surface
		enc(6, 2, uint32(11), uint32(10)),             // xdg_surface
		enc(11, 1, uint32(12)),                        // toplevel
		enc(12, 3, appID),                             // set_app_id
		enc(3, 0, uint32(20)),                         // layer surface
		enc(4, 1, uint32(21), uint32(20), uint32(10)), // subsurface 21 of 10
		enc(5, 1, uint32(22), uint32(20)),             // viewport for 20
		enc(21, 1, int32(435), int32(27)),             // set_position
		enc(22, 2, int32(406), int32(722)),            // set_destination
	} {
		c.request(m)
	}
}

func geometryOf(m []byte) rect {
	a := args(m)
	return rect{a.i32(), a.i32(), a.i32(), a.i32()}
}

func TestGeometryFollowsLayers(t *testing.T) {
	c := newConn(isApp)
	window(t, c, "android.org.example")

	// hwcomposer's own full-display geometry becomes the app's extent.
	out := c.request(enc(11, 3, int32(0), int32(0), int32(1280), int32(800)))
	if len(out) != 1 || geometryOf(out[0]) != (rect{435, 27, 406, 722}) {
		t.Fatalf("geometry = %+v", geometryOf(out[0]))
	}

	// Moving the layer injects a new geometry before the commit.
	c.request(enc(21, 1, int32(0), int32(28)))
	c.request(enc(22, 2, int32(1280), int32(752)))
	out = c.request(enc(10, 6))
	if len(out) != 2 {
		t.Fatalf("expected injected geometry before commit, got %d messages", len(out))
	}
	if id, op := header(out[0]); id != 11 || op != 3 || geometryOf(out[0]) != (rect{0, 28, 1280, 752}) {
		t.Fatalf("injected %d/%d %+v", id, op, geometryOf(out[0]))
	}
	// Unchanged layers: no injection.
	if out = c.request(enc(10, 6)); len(out) != 1 {
		t.Fatalf("unexpected injection: %d messages", len(out))
	}
}

func TestFullUIWindowUntouched(t *testing.T) {
	c := newConn(isApp)
	window(t, c, "Desktop")
	m := enc(11, 3, int32(0), int32(0), int32(1280), int32(800))
	if out := c.request(m); string(out[0]) != string(m) {
		t.Fatal("full-UI geometry was rewritten")
	}
}

func TestConfigureNeutralized(t *testing.T) {
	for _, appID := range []string{"android.org.example", "Desktop"} {
		c := newConn(isApp)
		window(t, c, appID)
		out := c.event(enc(12, 0, int32(75), int32(50), uint32(0)))
		a := args(out[0])
		if w, h := a.i32(), a.i32(); w != 0 || h != 0 {
			t.Fatalf("%s: configure %dx%d, want 0x0", appID, w, h)
		}
	}
}

func TestDeletedIDsForgotten(t *testing.T) {
	c := newConn(isApp)
	window(t, c, "android.org.example")
	c.event(enc(1, 1, uint32(12))) // delete_id(toplevel)
	c.event(enc(1, 1, uint32(11))) // delete_id(xdg_surface)
	// Id 11 reused for something else must not be treated as a window.
	c.request(enc(3, 0, uint32(11)))
	m := enc(11, 3, int32(1), int32(2), int32(3), int32(4))
	if out := c.request(m); string(out[0]) != string(m) {
		t.Fatal("stale xdg_surface state used after delete_id")
	}
}

// TestRelay runs the proxy between a fake client and compositor over unix
// sockets, including a file descriptor and a message split across writes.
func TestRelay(t *testing.T) {
	dir := t.TempDir()
	upPath := filepath.Join(dir, "compositor")
	compositor, err := net.ListenUnix("unix", &net.UnixAddr{Name: upPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer compositor.Close()
	l, err := Listen(filepath.Join(dir, "proxy"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Serve(ctx, l, upPath, isApp)

	client, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: filepath.Join(dir, "proxy"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := compositor.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	// A request carrying an fd, then a message written in two halves.
	f, _ := os.CreateTemp(dir, "fd")
	f.WriteString("buffer")
	attach := enc(10, 1, uint32(30), int32(0), int32(0))
	if _, _, err := client.WriteMsgUnix(attach, unix.UnixRights(int(f.Fd())), nil); err != nil {
		t.Fatal(err)
	}
	geo := enc(1, 1, uint32(2))
	client.Write(geo[:5])
	time.Sleep(50 * time.Millisecond)
	client.Write(geo[5:])

	server.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, 0, 64)
	var fds []int
	for len(got) < len(attach)+len(geo) {
		b := make([]byte, 64)
		oob := make([]byte, unix.CmsgSpace(4*maxFDs))
		n, oobn, _, _, err := server.ReadMsgUnix(b, oob)
		if err != nil {
			t.Fatal(err)
		}
		fds = append(fds, parseFDs(oob[:oobn])...)
		got = append(got, b[:n]...)
	}
	if string(got) != string(attach)+string(geo) {
		t.Fatalf("relayed bytes differ")
	}
	if len(fds) != 1 {
		t.Fatalf("fds = %v, want one", fds)
	}
	buf := make([]byte, 6)
	if n, _ := unix.Pread(fds[0], buf, 0); string(buf[:n]) != "buffer" {
		t.Fatalf("fd content %q", buf[:n])
	}
	unix.Close(fds[0])
}
