package groundnet

import (
	"context"
	"sync"

	"github.com/territory-grounder/grounder/core/policy"
)

// ReGrad is the subordinate-not-authority re-graduator (REQ-2109/2110). It LANDS a foreign wisdom's
// op-class as a LOCAL candidate that has earned NO authority — a hint at graduation LevelApprove
// (propose-only) — and READS the authority an op-class has since earned on THIS node. It never
// inherits the producer's trust and its own asserted outcome never grants standing (INV-08).
//
// It introduces NO new graduation-ladder writer (TG-436): a landed foreign op-class re-graduates
// through the consuming node's EXISTING, already-grounded graduation flow — the same path as any
// local op-class (spec/015: a local verified run → credits.Claim exactly-once grounding →
// Ladder.Record). This module only lands the candidate and reads the resulting level; it holds the
// ladder's STORE for reading, never the ladder for writing, so it cannot introduce an ungrounded or
// undeduplicated promote. It reaches no actuator, lifts no floor, and changes no mutation posture
// (INV-22) — landing is pure bookkeeping.
//
// It implements the seam's ReGraduator interface. The graduation store is injected: the in-memory
// store backs the dormant seam and the in-process e2e; T-021-8 supplies the durable store.
type ReGrad struct {
	store policy.GraduationStore

	mu     sync.Mutex
	landed map[string]WisdomV0 // op-class -> the landed foreign hint (citable as evidence, REQ-2110)
}

// compile-time proof that ReGrad is a usable seam ReGraduator.
var _ ReGraduator = (*ReGrad)(nil)

// NewReGrad constructs the re-graduator over the graduation store the consuming node's ladder reads
// and writes, so Level reflects what LOCAL re-graduation has recorded.
func NewReGrad(store policy.GraduationStore) *ReGrad {
	return &ReGrad{store: store, landed: make(map[string]WisdomV0)}
}

// LandCandidate lands a foreign wisdom as a subordinate candidate. The op-class is recorded as a local
// hint — it may inform investigation ordering and be cited as evidence — but it earns NO autonomous
// authority and does not inherit the producer's trust (REQ-2110). It records NO graduation outcome
// from the foreign statement (INV-08): a foreign chunk's own outcome must never promote a class; only
// the consuming node's own local verified runs do, through the existing grounded flow. Landing is a
// pure, side-effect-free hint record — it never writes the graduation ladder.
func (r *ReGrad) LandCandidate(_ context.Context, w WisdomV0) error {
	if w.OpClass == "" {
		return nil // nothing to land; the payload validator already rejects an empty op_class
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.landed[w.OpClass] = w
	return nil
}

// LandedHint returns the foreign wisdom landed for an op-class, if any — the evidence a local
// investigation may cite (REQ-2110). It grants no authority.
func (r *ReGrad) LandedHint(opClass string) (WisdomV0, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.landed[opClass]
	return w, ok
}
