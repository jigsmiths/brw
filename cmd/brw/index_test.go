package main

import "testing"

// brew info resolves a bare name through the newest installed keg, revisions included.
func TestOlderVersionComparesVersions(t *testing.T) {
	for _, tt := range []struct {
		a, b  string
		older bool
	}{
		{"1.9", "1.10", true},
		{"1.10", "1.9", false},
		{"1.0", "1.0_1", true},
		{"1.0_2", "1.0_1", false},
		{"2.0", "2.0", false},
		{"1.0-rc1", "1.0", true},
	} {
		if got := olderVersion(tt.a, tt.b); got != tt.older {
			t.Errorf("olderVersion(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.older)
		}
	}
}
