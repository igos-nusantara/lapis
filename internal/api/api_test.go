package api

import "testing"

func TestValidPackage(t *testing.T) {
	for _, p := range []string{"com.shopee.id", "org.supertuxkart.stk", "a_b.c1"} {
		if !ValidPackage(p) {
			t.Errorf("%q rejected", p)
		}
	}
	for _, p := range []string{"", "nodot", "com..x", "com.x;rm", "-n com.x", "com.x/.Main"} {
		if ValidPackage(p) {
			t.Errorf("%q accepted", p)
		}
	}
}
