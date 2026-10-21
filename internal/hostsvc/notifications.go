package hostsvc

import (
	"errors"
	"log/slog"
	"sync"

	"github.com/godbus/dbus/v5"

	"github.com/igos-nusantara/lapis/internal/binder"
	"github.com/igos-nusantara/lapis/internal/compat"
)

var errUnknown = errors.New("unknown transaction")

const (
	notifyDest = "org.freedesktop.Notifications"
	notifyPath = "/org/freedesktop/Notifications"

	callbackName = "INotifications.INotificationCallback"
)

// notifier forwards Android notifications to the desktop notification
// server and sends action clicks back to Android.
type notifier struct {
	c      *binder.Conn
	bus    *dbus.Conn
	prefix string

	mu        sync.Mutex
	listeners []uint32
	tokens    map[uint32]string
}

func newNotifier(c *binder.Conn, prefix string) (*notifier, error) {
	bus, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, err
	}
	n := &notifier{c: c, bus: bus, prefix: prefix, tokens: map[uint32]string{}}
	if err := bus.AddMatchSignal(dbus.WithMatchInterface(notifyDest), dbus.WithMatchObjectPath(notifyPath)); err != nil {
		return nil, err
	}
	signals := make(chan *dbus.Signal, 16)
	bus.Signal(signals)
	go n.watch(signals)
	return n, nil
}

func (n *notifier) watch(signals chan *dbus.Signal) {
	for s := range signals {
		switch s.Name {
		case notifyDest + ".ActivationToken":
			var id uint32
			var token string
			if dbus.Store(s.Body, &id, &token) == nil {
				n.mu.Lock()
				n.tokens[id] = token
				n.mu.Unlock()
			}
		case notifyDest + ".ActionInvoked":
			var id uint32
			var action string
			if dbus.Store(s.Body, &id, &action) != nil {
				continue
			}
			n.mu.Lock()
			token := n.tokens[id]
			delete(n.tokens, id)
			listeners := append([]uint32(nil), n.listeners...)
			n.mu.Unlock()
			for _, l := range listeners {
				p := binder.NewRequest(compat.Interface(callbackName))
				p.WriteInt32(int32(id))
				p.WriteString16(action)
				p.WriteString16(token)
				if _, err := n.c.Transact(l, binder.FirstCall, p, binder.FlagOneWay); err != nil {
					slog.Debug("notification callback failed", "err", err)
				}
			}
		}
	}
}

// imageData is the freedesktop "image-data" hint (iiibiiay).
type imageData struct {
	Width, Height, Rowstride int32
	HasAlpha                 bool
	BitsPerSample, Channels  int32
	Data                     []byte
}

func (n *notifier) handle(code uint32, r *binder.Reader, reply *binder.Parcel) error {
	switch code {
	case 1: // registerListener(INotificationCallback)
		h, ok, err := r.ReadHandle()
		if err != nil {
			return err
		}
		if ok {
			n.mu.Lock()
			n.listeners = append(n.listeners, h)
			n.mu.Unlock()
			n.c.LinkToDeath(h, func() { n.dropListener(h) })
		}
		reply.WriteInt32(0)
	case 2: // int notify(...)
		id, err := n.notify(r)
		if err != nil {
			return err
		}
		reply.WriteInt32(0)
		reply.WriteInt32(int32(id))
	case 3: // closeNotification(int id)
		id, err := r.ReadInt32()
		if err != nil {
			return err
		}
		n.bus.Object(notifyDest, notifyPath).Call(notifyDest+".CloseNotification", 0, uint32(id))
		reply.WriteInt32(0)
	default:
		return errUnknown
	}
	return nil
}

func (n *notifier) dropListener(h uint32) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for i, l := range n.listeners {
		if l == h {
			n.listeners = append(n.listeners[:i], n.listeners[i+1:]...)
			return
		}
	}
}

// notify parses notify() arguments and posts them over D-Bus.
func (n *notifier) notify(r *binder.Reader) (uint32, error) {
	var errs []error
	i32 := func() int32 { v, err := r.ReadInt32(); errs = append(errs, err); return v }
	str := func() string { v, err := r.ReadString16(); errs = append(errs, err); return v }

	replaces := i32()
	appName := str()
	pkg := str()
	summary := str()
	body := str()

	var actions []string
	for count := i32(); count > 0; count-- {
		if i32() != 0 { // non-null parcelable: size, then fields
			i32()
			actions = append(actions, str(), str())
		}
	}
	var img *imageData
	if i32() != 0 {
		i32()
		img = &imageData{Width: i32(), Height: i32(), Rowstride: i32(), BitsPerSample: 8}
		img.HasAlpha = i32() != 0
		data, err := r.ReadByteArray()
		errs = append(errs, err)
		img.Data = data
		img.Channels = 3
		if img.HasAlpha {
			img.Channels = 4
		}
	}
	category := str()
	suppressSound := i32() != 0
	expire := i32()
	resident := i32() != 0
	transient := i32() != 0
	urgency := byte(i32())
	if err := errors.Join(errs...); err != nil {
		return 0, err
	}

	hints := map[string]dbus.Variant{
		"desktop-entry":  dbus.MakeVariant(n.prefix + pkg),
		"resident":       dbus.MakeVariant(resident),
		"transient":      dbus.MakeVariant(transient),
		"urgency":        dbus.MakeVariant(urgency),
		"suppress-sound": dbus.MakeVariant(suppressSound),
	}
	if category != "" {
		hints["category"] = dbus.MakeVariant(category)
	}
	if img != nil {
		hints["image-data"] = dbus.MakeVariant(*img)
	}
	if actions == nil {
		actions = []string{}
	}
	var id uint32
	err := n.bus.Object(notifyDest, notifyPath).Call(notifyDest+".Notify", 0,
		appName, uint32(replaces), "", summary, body, actions, hints, expire).Store(&id)
	if err != nil {
		slog.Warn("posting notification failed", "err", err)
		return 0, nil
	}
	return id, nil
}
