package skills

import (
	"strings"
	"testing"
)

// The embed loader fails CLOSED: a seed the registry names but the embed lacks is a panic at wiring time,
// never a silently thinner library (the killing mutation for the loader; the byte-level killing mutation
// for the bodies is the golden — which caught a real mangling during this very extraction: the
// debugging-protocol literal's backtick concatenation survived the first swap and the golden reddened).
func TestSeedBodyFailsClosedOnMissingSeed(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("seedBody on a missing seed must panic (fail closed), not return")
		}
		if !strings.Contains(r.(string), "missing") {
			t.Fatalf("panic = %v, want the missing-seed message", r)
		}
	}()
	_ = seedBody("no-such-skill")
}

// Every registry body is non-blank — the empty-input half of the loader's contract, asserted over the REAL
// registry so a blanked seeds/*.md fails here (and at boot) rather than composing an empty protocol.
func TestEveryRegistryBodyNonBlank(t *testing.T) {
	for _, s := range Default().All() {
		if strings.TrimSpace(s.Body) == "" {
			t.Errorf("skill %q has a blank body", s.Name)
		}
	}
}
