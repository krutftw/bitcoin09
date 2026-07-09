package main

import "testing"

func TestDefaultMainnetSeeds(t *testing.T) {
	seeds := defaultSeeds(paramsFor("mainnet"))
	if len(seeds) < 4 {
		t.Fatalf("default mainnet seeds = %v, want at least 4", seeds)
	}
	for _, seed := range []string{"seed.btc09.org:9009", "178.128.105.41:9009", "103.80.18.140:9009", "108.190.240.138:9009"} {
		found := false
		for _, got := range seeds {
			if got == seed {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("default mainnet seeds missing %s: %v", seed, seeds)
		}
	}
}

func TestReleaseNewer(t *testing.T) {
	tests := []struct {
		latest  string
		current string
		want    bool
	}{
		{"v0.1.15", "v0.1.14", true},
		{"v0.2.0", "v0.1.99", true},
		{"v1.0.0", "v0.9.9", true},
		{"v0.1.9", "v0.1.9", false},
		{"v0.1.8", "v0.1.9", false},
		{"not-a-version", "v0.1.9", false},
		{"v0.1.15", "not-a-version", false},
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

	if err := rejectSendSeedFlag([]string{"-seeds", "178.128.105.41:9009"}); err != nil {
		t.Fatalf("rejectSendSeedFlag(-seeds) = %v, want nil", err)
	}
}
