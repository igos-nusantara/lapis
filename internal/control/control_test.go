package control

import (
	"testing"

	"github.com/igos-nusantara/lapis/internal/api"
)

func TestFitTask(t *testing.T) {
	disp := []int{0, 0, 1280, 752}
	for _, c := range []struct {
		name           string
		l, tp, r, b    int
		w, h           int
		wl, wt, wr, wb int
	}{
		// A maximized task shrinks to a 640 tile and moves up to stay on
		// the display.
		{"tile", 0, 28, 1280, 780, 640, 778, 0, 0, 640, 752},
		// A phone window grows by the drag and keeps its place.
		{"grow", 424, 54, 864, 746, 700, 600, 424, 54, 1124, 654},
		// Growing past the right edge moves the task left.
		{"edge", 900, 54, 1200, 600, 640, 586, 640, 54, 1280, 640},
		// Never below the minimum or above the display.
		{"tiny", 0, 0, 640, 400, 120, 90, 0, 0, 200, 200},
		{"huge", 0, 0, 640, 400, 3000, 3000, 0, 0, 1280, 752},
	} {
		l, tp, r, b := fitTask(c.l, c.tp, c.r, c.b, api.Request{ToWidth: c.w, ToHeight: c.h}, disp)
		if l != c.wl || tp != c.wt || r != c.wr || b != c.wb {
			t.Errorf("%s: got [%d,%d][%d,%d], want [%d,%d][%d,%d]", c.name, l, tp, r, b, c.wl, c.wt, c.wr, c.wb)
		}
	}
}
