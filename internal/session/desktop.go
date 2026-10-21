package session

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/igos-nusantara/lapis/internal/api"
	"github.com/igos-nusantara/lapis/internal/control"
	"github.com/igos-nusantara/lapis/internal/hostsvc"
)

// System apps that stay out of the host app menu.
var hiddenApps = map[string]bool{
	"com.android.calculator2": true, "com.android.camera2": true,
	"com.android.contacts": true, "com.android.deskclock": true,
	"com.android.documentsui": true, "com.android.email": true,
	"com.android.gallery3d": true, "com.android.inputmethod.latin": true,
	"com.android.settings": true, "com.google.android.gms": true,
	"org.lineageos.aperture": true, "org.lineageos.eleven": true,
	"org.lineageos.etar": true, "org.lineageos.jelly": true,
	"org.lineageos.recorder": true,
}

// DesktopPrefix names lapis's .desktop files.
const DesktopPrefix = "lapis."

// Entry is one app shown in the host menu.
type Entry struct {
	Package string
	Label   string
}

func entryPath(s *api.Session, pkg string) string {
	return filepath.Join(s.XDGDataHome, "applications", DesktopPrefix+pkg+".desktop")
}

// listEntries returns the launchable apps. PackageManager (through the
// control service) decides which apps exist: the platform service's list
// can stay stale after an app is removed. The platform service supplies
// the labels.
func listEntries(p *hostsvc.Platform) ([]Entry, error) {
	labels := map[string]string{}
	var platformOrder []string
	if p != nil {
		if apps, err := p.AppsInfo(); err == nil {
			for _, a := range apps {
				if a.Launchable() {
					labels[a.PackageName] = a.Name
					platformOrder = append(platformOrder, a.PackageName)
				}
			}
		}
	}
	resp, err := control.Call(api.Request{Op: api.OpApps})
	if err != nil {
		if len(platformOrder) == 0 {
			return nil, err
		}
		// No control service (e.g. not the session owner): trust the
		// platform list.
		var out []Entry
		for _, pkg := range platformOrder {
			out = append(out, Entry{Package: pkg, Label: labels[pkg]})
		}
		return out, nil
	}
	var out []Entry
	for _, a := range resp.Apps {
		label := labels[a.Package]
		if label == "" {
			label = Label(a.Package)
		}
		out = append(out, Entry{Package: a.Package, Label: label})
	}
	return out, nil
}

// Pruner removes the entries of apps PackageManager no longer lists as
// launchable. Right after boot Android reports a partial app list for a
// while, so an entry is only removed when its app is missing from two
// consecutive checks, and a list that suddenly shrank by half is ignored.
type Pruner struct {
	missing map[string]bool
}

func (pr *Pruner) Prune(s *api.Session) error {
	// Until the user's storage is unlocked PackageManager only reports
	// direct-boot-aware apps.
	if v, err := control.Call(api.Request{Op: api.OpGetprop, Prop: "sys.user.0.ce_available"}); err != nil || v.Value != "true" {
		return err
	}
	resp, err := control.Call(api.Request{Op: api.OpApps})
	if err != nil {
		return err
	}
	present := map[string]bool{}
	for _, a := range resp.Apps {
		present[entryPath(s, a.Package)] = true
	}
	entries, _ := filepath.Glob(filepath.Join(s.XDGDataHome, "applications", DesktopPrefix+"*.desktop"))
	var gone []string
	for _, f := range entries {
		if !present[f] {
			gone = append(gone, f)
		}
	}
	if len(gone)*2 > len(entries) {
		pr.missing = nil
		return nil
	}
	missing := map[string]bool{}
	for _, f := range gone {
		if pr.missing[f] {
			slog.Info("removing desktop entry of uninstalled app", "file", filepath.Base(f))
			os.Remove(f)
			continue
		}
		missing[f] = true
	}
	pr.missing = missing
	return nil
}

// SyncDesktopEntries writes one .desktop file per launchable Android app.
func SyncDesktopEntries(s *api.Session, self string, p *hostsvc.Platform) error {
	entries, err := listEntries(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(s.XDGDataHome, "applications"), 0o755); err != nil {
		return err
	}
	for _, e := range entries {
		path := entryPath(s, e.Package)
		want := desktopEntry(s, self, e)
		if cur, err := os.ReadFile(path); err == nil && string(cur) == want {
			continue
		}
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			return err
		}
	}
	// Stale entries are removed by Pruner, which tolerates the partial app
	// lists Android reports around unlock.
	return nil
}

func WriteDesktopEntry(s *api.Session, self string, e Entry) error {
	return os.WriteFile(entryPath(s, e.Package), []byte(desktopEntry(s, self, e)), 0o644)
}

func RemoveDesktopEntry(s *api.Session, pkg string) {
	os.Remove(entryPath(s, pkg))
}

func desktopEntry(s *api.Session, self string, e Entry) string {
	icon := filepath.Join(s.DataDir, "icons", e.Package+".png")
	if _, err := os.Stat(icon); err != nil {
		icon = "android"
	}
	label := e.Label
	if label == "" {
		label = Label(e.Package)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[Desktop Entry]\nType=Application\n")
	fmt.Fprintf(&b, "Name=%s\n", strings.ReplaceAll(label, "\n", " "))
	fmt.Fprintf(&b, "Comment=Android app %s\n", e.Package)
	fmt.Fprintf(&b, "Exec=%s app launch %s\n", self, e.Package)
	fmt.Fprintf(&b, "Icon=%s\n", icon)
	fmt.Fprintf(&b, "Categories=X-Lapis-App;\n")
	fmt.Fprintf(&b, "Actions=app-settings;\n")
	if hiddenApps[e.Package] {
		fmt.Fprintf(&b, "NoDisplay=true\n")
	}
	fmt.Fprintf(&b, "\n[Desktop Action app-settings]\nName=App Settings\n")
	fmt.Fprintf(&b, "Exec=%s app intent android.settings.APPLICATION_DETAILS_SETTINGS package:%s\n", self, e.Package)
	return b.String()
}

// Label derives a readable name from a package name when Android cannot
// be asked: com.google.android.youtube -> Youtube.
func Label(pkg string) string {
	parts := strings.Split(pkg, ".")
	last := parts[len(parts)-1]
	for i := len(parts) - 1; i > 0; i-- {
		switch parts[i] {
		case "android", "app", "apps", "mobile", "client", "ui":
			continue
		}
		last = parts[i]
		break
	}
	if last == "" {
		return pkg
	}
	return strings.ToUpper(last[:1]) + last[1:]
}
