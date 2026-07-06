package main

import "testing"

func TestReleaseNewer(t *testing.T) {
	tests := []struct {
		latest  string
		current string
		want    bool
	}{
		{"v0.1.11", "v0.1.10", true},
		{"v0.2.0", "v0.1.99", true},
		{"v1.0.0", "v0.9.9", true},
		{"v0.1.9", "v0.1.9", false},
		{"v0.1.8", "v0.1.9", false},
		{"not-a-version", "v0.1.9", false},
		{"v0.1.11", "not-a-version", false},
	}

	for _, tt := range tests {
		if got := releaseNewer(tt.latest, tt.current); got != tt.want {
			t.Fatalf("releaseNewer(%q, %q) = %v, want %v", tt.latest, tt.current, got, tt.want)
		}
	}
}
