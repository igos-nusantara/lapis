package session

import "testing"

func TestLabel(t *testing.T) {
	cases := map[string]string{
		"com.google.android.youtube": "Youtube",
		"com.whatsapp":               "Whatsapp",
		"com.android.settings":       "Settings",
		"org.mozilla.firefox":        "Firefox",
		"com.spotify.music":          "Music",
		"com.example.app":            "Example",
	}
	for pkg, want := range cases {
		if got := Label(pkg); got != want {
			t.Errorf("Label(%q) = %q, want %q", pkg, got, want)
		}
	}
}
