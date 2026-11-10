package main

// TG-681: the per-site front-door knob. The fail direction is pinned — a knob that cannot be parsed must be
// record-only, never a refusal nobody configured — and the boot line must name the posture it runs in.

import (
	"strings"
	"testing"
	"time"
)

func TestSiteLivenessBoundParsesOrStaysRecordOnly(t *testing.T) {
	env := func(v string) func(k, def string) string {
		return func(k, def string) string {
			if k == siteLivenessKey {
				return v
			}
			return def
		}
	}
	cases := map[string]time.Duration{
		"":       0,
		"  ":     0,
		"bogus":  0,
		"-1h":    0,
		"0":      0,
		"24h":    24 * time.Hour,
		" 90m ":  90 * time.Minute,
		"36h30m": 36*time.Hour + 30*time.Minute,
	}
	for raw, want := range cases {
		if got := siteLivenessBound(env(raw)); got != want {
			t.Errorf("%s=%q: got %v, want %v", siteLivenessKey, raw, got, want)
		}
	}
}

func TestSiteLivenessPostureNamesTheMode(t *testing.T) {
	if s := siteLivenessPosture(0); !strings.Contains(s, "RECORD-ONLY") || !strings.Contains(s, siteLivenessKey) {
		t.Fatalf("record-only posture must say so and name the knob, got %q", s)
	}
	if s := siteLivenessPosture(24 * time.Hour); !strings.Contains(s, "ENFORCING") || !strings.Contains(s, "24h0m0s") {
		t.Fatalf("an enforcing posture must say so and carry the bound, got %q", s)
	}
}
