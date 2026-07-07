package main

import "testing"

func TestReleaseNewer(t *testing.T) {
	tests := []struct {
		latest  string
		current string
		want    bool
	}{
		{"v0.1.12", "v0.1.11", true},
		{"v0.2.0", "v0.1.99", true},
		{"v1.0.0", "v0.9.9", true},
		{"v0.1.9", "v0.1.9", false},
		{"v0.1.8", "v0.1.9", false},
		{"not-a-version", "v0.1.9", false},
		{"v0.1.12", "not-a-version", false},
	}

	for _, tt := range tests {
		if got := releaseNewer(tt.latest, tt.current); got != tt.want {
			t.Fatalf("releaseNewer(%q, %q) = %v, want %v", tt.latest, tt.current, got, tt.want)
		}
	}
}

func TestRejectSendSeedFlag(t *testing.T) {
	tests := [][]string{
		{"-seed", "abc"},
		{"--seed", "abc"},
		{"-seed=abc"},
		{"--seed=abc"},
	}

	for _, args := range tests {
		if err := rejectSendSeedFlag(args); err == nil {
			t.Fatalf("rejectSendSeedFlag(%v) = nil, want error", args)
		}
	}

	if err := rejectSendSeedFlag([]string{"-seeds", "82.22.32.82:9009"}); err != nil {
		t.Fatalf("rejectSendSeedFlag(-seeds) = %v, want nil", err)
	}
}
