// Package hostsvc implements the host side of the images' binder
// interfaces: the IPlatform client and the services Android calls back
// (IUserMonitor, IClipboard, IHardware, INotifications).
package hostsvc

import (
	"context"
	"time"

	"github.com/igos-nusantara/lapis/internal/binder"
	"github.com/igos-nusantara/lapis/internal/compat"
)

const (
	platformName = "platform"

	txGetprop           = 1
	txSetprop           = 2
	txGetAppsInfo       = 3
	txGetAppInfo        = 4
	txLaunchApp         = 7
	txSettingsPutString = 9
	txLaunchIntent      = 13
)

// SettingsGlobal is the namespace id of Settings.Global in IPlatform.
const SettingsGlobal = 2

// AppInfo mirrors the images' AppInfo parcelable.
type AppInfo struct {
	Name         string
	PackageName  string
	Action       string
	LaunchIntent string
	ComponentPkg string
	ComponentCls string
	Categories   []string
}

// Launchable reports whether the app belongs in the host app menu.
func (a AppInfo) Launchable() bool {
	for _, c := range a.Categories {
		if c == "android.intent.category.LAUNCHER" {
			return true
		}
	}
	return false
}

// Platform is a client for the IPlatform service inside Android.
type Platform struct {
	c      *binder.Conn
	handle uint32
}

// GetPlatform waits for the IPlatform service to be registered.
func GetPlatform(ctx context.Context, c *binder.Conn) (*Platform, error) {
	h, err := c.WaitService(ctx, compat.Service(platformName))
	if err != nil {
		return nil, err
	}
	return &Platform{c: c, handle: h}, nil
}

func (p *Platform) call(code uint32, fill func(*binder.Parcel)) (*binder.Reader, error) {
	req := binder.NewRequest(compat.Interface("IPlatform"))
	if fill != nil {
		fill(req)
	}
	r, err := p.c.Transact(p.handle, code, req, 0)
	if err != nil {
		return nil, err
	}
	return r, r.ReadException()
}

func (p *Platform) GetProp(key, def string) (string, error) {
	r, err := p.call(txGetprop, func(w *binder.Parcel) { w.WriteString16(key); w.WriteString16(def) })
	if err != nil {
		return "", err
	}
	return r.ReadString16()
}

func (p *Platform) SetProp(key, value string) error {
	_, err := p.call(txSetprop, func(w *binder.Parcel) { w.WriteString16(key); w.WriteString16(value) })
	return err
}

func readAppInfo(r *binder.Reader) (*AppInfo, error) {
	present, err := r.ReadInt32()
	if err != nil || present != 1 {
		return nil, err
	}
	a := &AppInfo{}
	for _, f := range []*string{&a.Name, &a.PackageName, &a.Action, &a.LaunchIntent, &a.ComponentPkg, &a.ComponentCls} {
		if *f, err = r.ReadString16(); err != nil {
			return nil, err
		}
	}
	n, err := r.ReadInt32()
	if err != nil {
		return nil, err
	}
	for i := int32(0); i < n; i++ {
		c, err := r.ReadString16()
		if err != nil {
			return nil, err
		}
		a.Categories = append(a.Categories, c)
	}
	return a, nil
}

func (p *Platform) AppsInfo() ([]AppInfo, error) {
	r, err := p.call(txGetAppsInfo, nil)
	if err != nil {
		return nil, err
	}
	n, err := r.ReadInt32()
	if err != nil {
		return nil, err
	}
	var out []AppInfo
	for i := int32(0); i < n; i++ {
		a, err := readAppInfo(r)
		if err != nil {
			return out, err
		}
		if a != nil {
			out = append(out, *a)
		}
	}
	return out, nil
}

func (p *Platform) AppInfo(pkg string) (*AppInfo, error) {
	r, err := p.call(txGetAppInfo, func(w *binder.Parcel) { w.WriteString16(pkg) })
	if err != nil {
		return nil, err
	}
	return readAppInfo(r)
}

func (p *Platform) LaunchApp(pkg string) error {
	_, err := p.call(txLaunchApp, func(w *binder.Parcel) { w.WriteString16(pkg) })
	return err
}

// LaunchIntent starts an intent and returns the package that handled it.
func (p *Platform) LaunchIntent(action, uri string) (string, error) {
	r, err := p.call(txLaunchIntent, func(w *binder.Parcel) { w.WriteString16(action); w.WriteString16(uri) })
	if err != nil {
		return "", err
	}
	return r.ReadString16()
}

func (p *Platform) SettingsPutString(namespace int32, key, value string) error {
	_, err := p.call(txSettingsPutString, func(w *binder.Parcel) {
		w.WriteInt32(namespace)
		w.WriteString16(key)
		w.WriteString16(value)
	})
	return err
}

// Launch opens pkg in its own window.
func (p *Platform) Launch(pkg string) error {
	if err := p.SetProp(compat.Prop("active_apps"), pkg); err != nil {
		return err
	}
	if err := p.LaunchApp(pkg); err != nil {
		return err
	}
	return p.applyWindowPolicy()
}

// Intent opens an intent (e.g. app settings) the same way and returns the
// package that handled it, which is only known after the first launch.
func (p *Platform) Intent(action, uri string) (string, error) {
	pkg, err := p.LaunchIntent(action, uri)
	if err != nil || pkg == "" {
		return "", err
	}
	if pkg == "android" {
		pkg = compat.FullUIApp
	}
	if err := p.SetProp(compat.Prop("active_apps"), pkg); err != nil {
		return "", err
	}
	if err := p.applyWindowPolicy(); err != nil {
		return "", err
	}
	// Send it again now that its window is allowed, so the activity
	// resumes and draws a frame into the new host window.
	_, err = p.LaunchIntent(action, uri)
	return pkg, err
}

func (p *Platform) applyWindowPolicy() error {
	policy := "immersive.status=*"
	if mw, _ := p.GetProp(compat.PersistProp("multi_windows"), "false"); mw == "true" {
		policy = "immersive.full=*"
	}
	return p.SettingsPutString(SettingsGlobal, "policy_control", policy)
}

// ShowFullUI shows the whole Android desktop in one window.
func (p *Platform) ShowFullUI() error {
	if err := p.SetProp(compat.Prop("active_apps"), compat.FullUIApp); err != nil {
		return err
	}
	if err := p.SettingsPutString(SettingsGlobal, "policy_control", "null*"); err != nil {
		return err
	}
	p.refreshDisplay()
	return nil
}

// refreshDisplay forces SurfaceFlinger to recompose by opening and closing
// the notification shade, so the newly
// allowed window gets a frame.
func (p *Platform) refreshDisplay() {
	const iface = "com.android.internal.statusbar.IStatusBarService"
	h, ok, err := p.c.CheckService("statusbar")
	if err != nil || !ok {
		return
	}
	for i, code := range []uint32{1, 2} { // expandNotificationsPanel, collapsePanels
		if i > 0 {
			time.Sleep(500 * time.Millisecond)
		}
		if r, err := p.c.Transact(h, code, binder.NewRequest(iface), 0); err == nil {
			r.ReadException()
		}
	}
}
