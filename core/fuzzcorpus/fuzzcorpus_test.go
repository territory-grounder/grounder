package fuzzcorpus_test

// The shared battery must be HOSTILE, not vacuous: a corpus that every boundary trivially passes proves
// nothing. These assertions pin that each adversarial class actually crosses the untrusted-text screen the
// way §3.2 intends — the corpus's own oracle — and that Benign survives, so a boundary can't "pass" by
// eating everything. It imports core/screen (the canonical boundary) as the reference detector.

import (
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/fuzzcorpus"
	"github.com/territory-grounder/grounder/core/screen"
)

// Every Injection/Evasion string must be DETECTED by the raw screen (before scrubbing) — otherwise it is not
// actually adversarial and adds no coverage. This is the anti-vacuity guard on the corpus itself.
func TestInjectionAndEvasionAreActuallyDetected(t *testing.T) {
	for _, class := range []struct {
		name    string
		strings []string
	}{
		{"Injection", fuzzcorpus.Injection},
		{"Evasion", fuzzcorpus.Evasion},
	} {
		for _, s := range class.strings {
			if len(screen.Detect(s)) == 0 {
				t.Errorf("%s seed %q is NOT detected by the screen — a vacuous 'hostile' seed adds no coverage", class.name, s)
			}
		}
	}
}

// Every Injection/Evasion/Metachar/Oversized string must be NEUTRALIZED by Scrub (Detect(Scrub(x)) empty) —
// the completeness property every boundary relies on. If the shared corpus itself contained a string the
// screen could not neutralize, that would be a live INV-02/03 bypass, caught here rather than in each suite.
func TestBatteryIsFullyNeutralized(t *testing.T) {
	for _, s := range fuzzcorpus.Strings() {
		out, _ := screen.Scrub(s)
		if resid := screen.Detect(out); len(resid) > 0 {
			t.Errorf("corpus seed survives scrubbing (INV-02/03 bypass): %q → %q resid=%v", s, out, resid)
		}
	}
}

// Secrets must be REDACTED (the scrubbed text no longer contains the credential span). Needles reference the
// ASSEMBLED exported values, so this file carries no contiguous secret literal either.
func TestSecretsAreRedacted(t *testing.T) {
	needles := map[int]string{
		0: fuzzcorpus.SecretBearer,
		1: fuzzcorpus.SecretAWSKey,
		3: fuzzcorpus.SecretPassword,
	}
	for i, needle := range needles {
		out, _ := screen.Scrub(fuzzcorpus.Secrets[i])
		if strings.Contains(out, needle) {
			t.Errorf("Secrets[%d]: credential %q survived Scrub: %q", i, needle, out)
		}
	}
}

// Benign text must survive: if the union's clean strings tripped Detect, every boundary would "neutralize"
// them and the corpus could not distinguish a real scrubber from one that eats all input.
func TestBenignSurvives(t *testing.T) {
	for _, s := range fuzzcorpus.Benign {
		if hits := screen.Detect(s); len(hits) > 0 {
			t.Errorf("Benign seed %q trips Detect %v — the corpus's control is not actually clean", s, hits)
		}
	}
}

// Strings() is the deterministic union of every class, deduplicated by neither (order is the coverage
// contract) — assert it is the concatenation, so a suite seeding by index is reproducible.
func TestStringsIsTheStableUnion(t *testing.T) {
	want := len(fuzzcorpus.Benign) + len(fuzzcorpus.Injection) + len(fuzzcorpus.Evasion) +
		len(fuzzcorpus.Secrets) + len(fuzzcorpus.Metachar) + len(fuzzcorpus.Oversized)
	got := fuzzcorpus.Strings()
	if len(got) != want {
		t.Fatalf("Strings() len = %d, want the union %d", len(got), want)
	}
	if got[0] != fuzzcorpus.Benign[0] {
		t.Errorf("Strings() must lead with Benign for stable, reproducible indexing")
	}
}
