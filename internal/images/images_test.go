package images

import (
	"os"
	"testing"

	"github.com/igos-nusantara/lapis/internal/compat"
	"github.com/igos-nusantara/lapis/internal/config"
)

func TestOTAURLs(t *testing.T) {
	im := config.Default().Images
	im.Arch = "x86_64"
	if got, want := SystemOTA(im), compat.SystemChannel+"/lineage/"+compat.DeviceDir("x86_64")+"/GAPPS.json"; got != want {
		t.Errorf("SystemOTA = %s, want %s", got, want)
	}
	if got, want := VendorOTA(im), compat.VendorChannel+"/"+compat.DeviceDir("x86_64")+"/MAINLINE.json"; got != want {
		t.Errorf("VendorOTA = %s, want %s", got, want)
	}
}

// Set LAPIS_NET_TEST=1 to check the live OTA channels.
func TestFetchLive(t *testing.T) {
	if os.Getenv("LAPIS_NET_TEST") != "1" {
		t.Skip("network test")
	}
	im := config.Default().Images
	im.Arch = Arch()
	for _, url := range []string{SystemOTA(im), VendorOTA(im)} {
		entries, err := Fetch(url)
		if err != nil {
			t.Fatal(err)
		}
		e := entries[0]
		if len(e.ID) != 64 || e.URL == "" || e.Size == 0 {
			t.Errorf("%s: suspicious entry %+v", url, e)
		}
		t.Logf("%s -> %s (%d MiB)", url, e.Filename, e.Size>>20)
	}
}
