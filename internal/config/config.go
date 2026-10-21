// Package config loads and saves the host-wide lapis configuration.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"

	"github.com/igos-nusantara/lapis/internal/compat"
	"github.com/igos-nusantara/lapis/internal/paths"
)

type Config struct {
	Images     Images            `toml:"images"`
	Container  Container         `toml:"container"`
	Display    Display           `toml:"display"`
	Properties map[string]string `toml:"properties"`
}

type Images struct {
	// OTA channel settings.
	SystemChannel string `toml:"system_channel"`
	VendorChannel string `toml:"vendor_channel"`
	RomType       string `toml:"rom_type"`
	SystemType    string `toml:"system_type"`
	VendorType    string `toml:"vendor_type"`
	Arch          string `toml:"arch"`

	// Filled in by `lapis init`.
	SystemOTA      string `toml:"system_ota"`
	VendorOTA      string `toml:"vendor_ota"`
	SystemDatetime int64  `toml:"system_datetime"`
	VendorDatetime int64  `toml:"vendor_datetime"`

	// Directory holding system.img and vendor.img.
	Path string `toml:"path"`
}

type Container struct {
	// Optional render node override, e.g. /dev/dri/renderD129.
	DRMDevice string `toml:"drm_device"`
}

// Display modes. Android picks phone or tablet layouts from the screen's
// smallest width in dp (>= 600dp is a tablet), which the density sets.
const (
	DisplayTablet = "tablet"
	DisplayPhone  = "phone"
)

type Display struct {
	// "tablet" (default), "phone", or "custom" with Density set.
	Mode string `toml:"mode"`
	// Density in dpi; overrides Mode when non-zero.
	Density int `toml:"density"`
	// "maximized" opens every app filling the display (tablet layout),
	// "freeform" keeps Android's phone-sized windows. Empty follows Mode.
	Windows string `toml:"windows"`
}

const (
	WindowsMaximized = "maximized"
	WindowsFreeform  = "freeform"
)

// MaximizeApps reports whether launched apps should fill the display.
func (d Display) MaximizeApps() bool {
	switch d.Windows {
	case WindowsMaximized:
		return true
	case WindowsFreeform:
		return false
	}
	return d.Mode != DisplayPhone
}

// Densities for the modes: at 160 dpi a 768px-high display is 768dp wide
// at its smallest (tablet); at 320 dpi it is 384dp (phone).
var modeDensity = map[string]int{DisplayTablet: 160, DisplayPhone: 320}

// LCDDensity returns the density to give Android.
func (d Display) LCDDensity() (int, error) {
	if d.Density > 0 {
		return d.Density, nil
	}
	mode := d.Mode
	if mode == "" {
		mode = DisplayTablet
	}
	dpi, ok := modeDensity[mode]
	if !ok {
		return 0, fmt.Errorf("unknown display mode %q (tablet, phone, or set density)", d.Mode)
	}
	return dpi, nil
}

func Default() *Config {
	return &Config{
		Images: Images{
			SystemChannel: compat.SystemChannel,
			VendorChannel: compat.VendorChannel,
			RomType:       "lineage",
			SystemType:    "GAPPS",
			VendorType:    "MAINLINE",
			Path:          paths.ImagesDir,
		},
		Display: Display{Mode: DisplayTablet},
		Properties: map[string]string{
			// Every Android app gets its own host window.
			compat.PersistProp("multi_windows"): "true",
		},
	}
}

// Load reads the config file, falling back to defaults when it is missing.
func Load() (*Config, error) {
	cfg := Default()
	_, err := toml.DecodeFile(paths.ConfigFile, cfg)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	if cfg.Properties == nil {
		cfg.Properties = map[string]string{}
	}
	return cfg, nil
}

func (c *Config) Save() error {
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o755); err != nil {
		return err
	}
	tmp := paths.ConfigFile + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if err := toml.NewEncoder(f).Encode(c); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, paths.ConfigFile)
}
