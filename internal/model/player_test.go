package model

import "testing"

func TestValidUsername(t *testing.T) {
	valid := []string{"Steve", "Alex_99", "abc", "ABCDEFGHIJKLMNOP", "a_b_c"}
	for _, u := range valid {
		if !ValidUsername(u) {
			t.Errorf("ValidUsername(%q) = false, want true", u)
		}
	}
	invalid := []string{
		"",                  // empty
		"ab",                // too short
		"ABCDEFGHIJKLMNOPQ", // 17 chars, too long
		"has space",         // space
		"dash-name",         // dash
		"emoji😀",            // non-ascii
		"dot.name",          // dot
	}
	for _, u := range invalid {
		if ValidUsername(u) {
			t.Errorf("ValidUsername(%q) = true, want false", u)
		}
	}
}
