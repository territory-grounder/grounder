package seal

// THE WIRED GATE FOR THE SEALED STORE (TG-275).
//
// This is the test that would have caught the original defect, and none of the existing ones could. The
// resolver was correct. The store was correct. The migration was applied. Every unit test passed. The
// `sealed_secret` table still held ZERO rows on a live deployment, because cmd/worker never called
// config.RegisterStoreResolver — so the process that consumes credentials could not open a single one.
//
// A behavioural test cannot see that: each half works in isolation, and `main` has no seam to test
// through. What the property actually needs is an assertion about the COMPOSITION ROOTS themselves, so
// this reads their source. That is deliberate, not a shortcut — the failure mode being guarded is
// precisely "the wiring line is absent", which is a fact about the root and nowhere else.

import (
	"os"
	"strings"
	"testing"
)

// KILLING MUTATION: delete the RegisterStoreResolver call from cmd/worker/main.go — i.e. restore the state
// this repo actually shipped. RED. Deleting it from cmd/grounder is equally RED.
func TestEveryCompositionRootRegistersTheStoreResolver(t *testing.T) {
	roots := map[string]string{
		"cmd/worker/main.go":   "../../cmd/worker/main.go",
		"cmd/grounder/main.go": "../../cmd/grounder/main.go",
	}
	for name, path := range roots {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !strings.Contains(string(src), "config.RegisterStoreResolver(") {
			t.Errorf("%s never calls config.RegisterStoreResolver — every `store:` SecretRef in that "+
				"process fails closed, so a secret written through the console is unreadable by the "+
				"process that needs it. This is the exact state that left sealed_secret at zero rows.", name)
		}
		// Both roots must go through the SHARED constructor. A root that hand-rolls its own sealer is how
		// two processes end up sealing and unsealing under different keys.
		if !strings.Contains(string(src), "seal.FromEnv(") {
			t.Errorf("%s builds a sealer without seal.FromEnv — duplicated seal construction drifts, and "+
				"the drift is silent until one process cannot open what the other wrote.", name)
		}
	}
}

// VACUITY FLOOR. If the paths ever move, the scan above passes by matching nothing at all, and this whole
// gate becomes decorative while still reporting green — the failure this repo keeps rediscovering.
func TestTheRootScanIsActuallyReadingRealFiles(t *testing.T) {
	for _, p := range []string{"../../cmd/worker/main.go", "../../cmd/grounder/main.go"} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("composition root %s is not where this gate looks (%v) — the gate above would pass "+
				"by reading nothing", p, err)
		}
		if info.Size() < 1024 {
			t.Fatalf("%s is %d bytes — too small to be the composition root; the gate is scanning the "+
				"wrong file", p, info.Size())
		}
	}
}
