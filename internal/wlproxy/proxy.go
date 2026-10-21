// Package wlproxy sits between Android's hwcomposer and the host Wayland
// compositor and makes each app window as large as the app.
//
// The images' hwcomposer gives every app window the size of the whole
// Android display: a transparent toplevel whose window geometry is the
// display, with the app's layers as subsurfaces at their display
// positions. The proxy rewrites the window geometry of app windows to the
// bounding box of their layers, so compositors place, tile and decorate
// the app itself. It also turns size suggestions from the compositor
// (xdg_toplevel.configure) into "client decides", because the hwcomposer
// would otherwise resize the whole Android display to the size of one
// window. Buffers are passed through as file descriptors; no
// pixels are copied.
package wlproxy

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

var le = binary.LittleEndian

// debug logs the window messages the proxy looks at (LAPIS_WAYLAND_DEBUG=1).
var debug = os.Getenv("LAPIS_WAYLAND_DEBUG") == "1"

const (
	maxFDs     = 28 // libwayland's per-message limit
	headerSize = 8
	displayID  = 1
)

// Interfaces the proxy follows.
const (
	ifCompositor    = "wl_compositor"
	ifSubcompositor = "wl_subcompositor"
	ifViewporter    = "wp_viewporter"
	ifWmBase        = "xdg_wm_base"
	ifRegistry      = "wl_registry"
	ifSurface       = "wl_surface"
	ifSubsurface    = "wl_subsurface"
	ifViewport      = "wp_viewport"
	ifXdgSurface    = "xdg_surface"
	ifToplevel      = "xdg_toplevel"
)

// IsAppWindow decides which toplevels are app windows (by app id).
type IsAppWindow func(appID string) bool

// Serve accepts Android's connections on listener and relays each one to
// the compositor socket at upstream until ctx ends.
func Serve(ctx context.Context, listener *net.UnixListener, upstream string, isApp IsAppWindow) error {
	go func() {
		<-ctx.Done()
		listener.Close()
	}()
	for {
		down, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		up, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: upstream, Net: "unix"})
		if err != nil {
			slog.Warn("wayland proxy: compositor unreachable", "err", err)
			down.Close()
			continue
		}
		c := newConn(isApp)
		go c.pump(down, up, c.request)
		go c.pump(up, down, c.event)
	}
}

type point struct{ x, y int32 }
type size struct{ w, h int32 }
type rect struct{ x, y, w, h int32 }

type subsurface struct{ surface, parent uint32 }

// conn holds the object state of one client connection.
type conn struct {
	isApp IsAppWindow

	mu          sync.Mutex
	iface       map[uint32]string     // client object id -> interface
	subsurfaces map[uint32]subsurface // wl_subsurface id
	position    map[uint32]point      // wl_subsurface id -> position
	viewportOf  map[uint32]uint32     // wp_viewport id -> wl_surface id
	destination map[uint32]size       // wl_surface id -> viewport destination
	xdgOf       map[uint32]uint32     // wl_surface id -> xdg_surface id
	surfaceOf   map[uint32]uint32     // xdg_surface id -> wl_surface id
	toplevelOf  map[uint32]uint32     // xdg_toplevel id -> xdg_surface id
	appWindow   map[uint32]bool       // xdg_surface id -> is an app window
	sent        map[uint32]rect       // xdg_surface id -> geometry last sent
}

func newConn(isApp IsAppWindow) *conn {
	return &conn{
		isApp:       isApp,
		iface:       map[uint32]string{displayID: "wl_display"},
		subsurfaces: map[uint32]subsurface{},
		position:    map[uint32]point{},
		viewportOf:  map[uint32]uint32{},
		destination: map[uint32]size{},
		xdgOf:       map[uint32]uint32{},
		surfaceOf:   map[uint32]uint32{},
		toplevelOf:  map[uint32]uint32{},
		appWindow:   map[uint32]bool{},
		sent:        map[uint32]rect{},
	}
}

// pump copies messages from src to dst, letting rewrite change, drop or
// add messages. File descriptors travel with the bytes they arrived with,
// which keeps them ahead of or alongside their messages.
func (c *conn) pump(src, dst *net.UnixConn, rewrite func([]byte) [][]byte) {
	defer src.Close()
	defer dst.Close()
	buf := make([]byte, 0, 1<<16)
	data := make([]byte, 1<<16)
	oob := make([]byte, unix.CmsgSpace(4*maxFDs*2))
	var fds []int
	for {
		n, oobn, _, _, err := src.ReadMsgUnix(data, oob)
		n, oobn = max(n, 0), max(oobn, 0) // negative on some errors
		if oobn > 0 {
			fds = append(fds, parseFDs(oob[:oobn])...)
		}
		buf = append(buf, data[:n]...)
		// Forward every complete message.
		var out []byte
		for len(buf) >= headerSize {
			sz := int(le.Uint32(buf[4:]) >> 16)
			if sz < headerSize || sz > len(buf) {
				break
			}
			for _, m := range rewrite(buf[:sz]) {
				out = append(out, m...)
			}
			buf = buf[sz:]
		}
		// File descriptors wait for the next complete message.
		if len(out) > 0 {
			if werr := writeMsg(dst, out, fds); werr != nil {
				closeFDs(fds)
				return
			}
			closeFDs(fds)
			fds = nil
		}
		buf = append(buf[:0:0], buf...)
		if err != nil {
			closeFDs(fds)
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				slog.Debug("wayland proxy", "err", err)
			}
			return
		}
	}
}

func parseFDs(oob []byte) []int {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return nil
	}
	var fds []int
	for _, m := range msgs {
		if f, err := unix.ParseUnixRights(&m); err == nil {
			fds = append(fds, f...)
		}
	}
	return fds
}

// writeMsg sends data with fds attached, at most maxFDs per sendmsg; when
// more are pending, each batch rides on a single byte of the data.
func writeMsg(dst *net.UnixConn, data []byte, fds []int) error {
	for len(fds) > 0 {
		if len(data) == 0 {
			return errors.New("file descriptors without data")
		}
		chunk := fds[:min(len(fds), maxFDs)]
		part := data
		if len(fds) > len(chunk) {
			part = data[:1]
		}
		n, _, err := dst.WriteMsgUnix(part, unix.UnixRights(chunk...), nil)
		if err != nil {
			return err
		}
		data, fds = data[n:], fds[len(chunk):]
	}
	if len(data) > 0 {
		_, err := dst.Write(data)
		return err
	}
	return nil
}

func closeFDs(fds []int) {
	for _, fd := range fds {
		unix.Close(fd)
	}
}

// reader decodes message arguments.
type reader struct {
	b   []byte
	pos int
	ok  bool
}

func args(msg []byte) *reader { return &reader{b: msg, pos: headerSize, ok: true} }

func (r *reader) u32() uint32 {
	if r.pos+4 > len(r.b) {
		r.ok = false
		return 0
	}
	v := le.Uint32(r.b[r.pos:])
	r.pos += 4
	return v
}

func (r *reader) i32() int32 { return int32(r.u32()) }

func (r *reader) str() string {
	n := int(r.u32())
	if n == 0 || r.pos+n > len(r.b) {
		r.ok = n == 0
		return ""
	}
	s := string(r.b[r.pos : r.pos+n-1])
	r.pos += (n + 3) &^ 3
	return s
}

func header(msg []byte) (id uint32, opcode uint16) {
	return le.Uint32(msg), uint16(le.Uint32(msg[4:]) & 0xffff)
}

func message(id uint32, opcode uint16, vals ...int32) []byte {
	m := make([]byte, headerSize+4*len(vals))
	le.PutUint32(m, id)
	le.PutUint32(m[4:], uint32(len(m))<<16|uint32(opcode))
	for i, v := range vals {
		le.PutUint32(m[headerSize+4*i:], uint32(v))
	}
	return m
}

// request handles one client-to-compositor message.
func (c *conn) request(msg []byte) [][]byte {
	id, op := header(msg)
	a := args(msg)
	c.mu.Lock()
	defer c.mu.Unlock()

	switch c.iface[id] {
	case "wl_display":
		if op == 1 { // get_registry
			c.iface[a.u32()] = ifRegistry
		}
	case ifRegistry:
		if op == 0 { // bind(name, interface, version, new_id)
			a.u32()
			name := a.str()
			a.u32()
			if newID := a.u32(); a.ok {
				c.iface[newID] = name
			}
		}
	case ifCompositor:
		if op == 0 { // create_surface
			c.iface[a.u32()] = ifSurface
		}
	case ifSubcompositor:
		if op == 1 { // get_subsurface(new_id, surface, parent)
			newID, surface, parent := a.u32(), a.u32(), a.u32()
			if a.ok {
				c.iface[newID] = ifSubsurface
				c.subsurfaces[newID] = subsurface{surface, parent}
			}
		}
	case ifSubsurface:
		switch op {
		case 0: // destroy
			delete(c.subsurfaces, id)
			delete(c.position, id)
		case 1: // set_position
			x, y := a.i32(), a.i32()
			if a.ok {
				c.position[id] = point{x, y}
			}
		}
	case ifViewporter:
		if op == 1 { // get_viewport(new_id, surface)
			newID, surface := a.u32(), a.u32()
			if a.ok {
				c.iface[newID] = ifViewport
				c.viewportOf[newID] = surface
			}
		}
	case ifViewport:
		switch op {
		case 0: // destroy
			delete(c.destination, c.viewportOf[id])
			delete(c.viewportOf, id)
		case 2: // set_destination
			w, h := a.i32(), a.i32()
			if a.ok {
				c.destination[c.viewportOf[id]] = size{w, h}
			}
		}
	case ifWmBase:
		if op == 2 { // get_xdg_surface(new_id, surface)
			newID, surface := a.u32(), a.u32()
			if a.ok {
				c.iface[newID] = ifXdgSurface
				c.xdgOf[surface] = newID
				c.surfaceOf[newID] = surface
			}
		}
	case ifXdgSurface:
		switch op {
		case 1: // get_toplevel(new_id)
			if newID := a.u32(); a.ok {
				c.iface[newID] = ifToplevel
				c.toplevelOf[newID] = id
			}
		case 3: // set_window_geometry: use the layers' extent for app windows
			if c.appWindow[id] {
				if r, ok := c.bounds(c.surfaceOf[id]); ok {
					c.sent[id] = r
					return [][]byte{message(id, 3, r.x, r.y, r.w, r.h)}
				}
			}
		}
	case ifToplevel:
		if op == 3 { // set_app_id
			if appID := a.str(); a.ok {
				c.appWindow[c.toplevelOf[id]] = c.isApp(appID)
				if debug {
					slog.Info("wayland: app id", "toplevel", id, "app_id", appID, "app", c.isApp(appID))
				}
			}
		}
	case ifSurface:
		if op == 6 { // commit: keep the geometry in step with the layers
			if xdg, ok := c.xdgOf[id]; ok && c.appWindow[xdg] {
				if r, ok := c.bounds(id); ok && r != c.sent[xdg] {
					c.sent[xdg] = r
					return [][]byte{message(xdg, 3, r.x, r.y, r.w, r.h), msg}
				}
			}
		}
	}
	return [][]byte{msg}
}

// event handles one compositor-to-client message.
func (c *conn) event(msg []byte) [][]byte {
	id, op := header(msg)
	c.mu.Lock()
	defer c.mu.Unlock()

	switch {
	case id == displayID && op == 1: // delete_id
		c.forget(args(msg).u32())
	case c.iface[id] == ifToplevel && op == 0: // configure(width, height, states)
		if debug {
			a := args(msg)
			slog.Info("wayland: configure", "toplevel", id, "w", a.i32(), "h", a.i32(), "app", c.appWindow[c.toplevelOf[id]])
		}
		if len(msg) >= headerSize+8 {
			// The hwcomposer resizes the whole Android display to any
			// suggested window size (on its first window that is a
			// floating WM's minimum, e.g. 75x50). Let Android keep the
			// output size; windows size themselves.
			out := append([]byte(nil), msg...)
			le.PutUint32(out[headerSize:], 0)
			le.PutUint32(out[headerSize+4:], 0)
			return [][]byte{out}
		}
	}
	return [][]byte{msg}
}

// forget drops everything known about a deleted object id.
func (c *conn) forget(id uint32) {
	switch c.iface[id] {
	case ifSurface:
		delete(c.destination, id)
		delete(c.xdgOf, id)
	case ifXdgSurface:
		delete(c.xdgOf, c.surfaceOf[id])
		delete(c.surfaceOf, id)
		delete(c.appWindow, id)
		delete(c.sent, id)
	case ifToplevel:
		delete(c.toplevelOf, id)
	case ifSubsurface:
		delete(c.subsurfaces, id)
		delete(c.position, id)
	case ifViewport:
		delete(c.viewportOf, id)
	}
	delete(c.iface, id)
}

// bounds is the union of the layer subsurfaces of a window surface.
func (c *conn) bounds(parent uint32) (rect, bool) {
	var r rect
	found := false
	for id, sub := range c.subsurfaces {
		if sub.parent != parent {
			continue
		}
		d, ok := c.destination[sub.surface]
		if !ok || d.w <= 1 || d.h <= 1 {
			continue
		}
		p := c.position[id]
		if !found {
			r, found = rect{p.x, p.y, d.w, d.h}, true
			continue
		}
		x0, y0 := min(r.x, p.x), min(r.y, p.y)
		x1, y1 := max(r.x+r.w, p.x+d.w), max(r.y+r.h, p.y+d.h)
		r = rect{x0, y0, x1 - x0, y1 - y0}
	}
	return r, found
}

// Listen creates the proxy socket (removing a stale one).
func Listen(path string) (*net.UnixListener, error) {
	os.Remove(path)
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	l.SetUnlinkOnClose(true)
	return l, nil
}
