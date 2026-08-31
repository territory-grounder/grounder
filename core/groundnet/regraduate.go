package groundnet

import (
	"context"

	"github.com/territory-grounder/grounder/core/policy"
)

// The re-graduation model (REQ-2110), and why this module writes NO graduation ladder:
//
// A landed foreign op-class earns local standing ONLY by re-graduating against local traffic and
// local verified outcomes — the SAME way any local op-class does. That happens through the consuming
// node's EXISTING, already-grounded graduation flow (spec/015: a locally-verified `match` run →
// credits.Claim exactly-once grounding (TG-436 / REQ-2804) → Ladder.Record). groundnet deliberately
// does NOT add a groundnet-specific promote: a new, ungrounded Ladder.Record writer would be exactly
// the ungrounded/undeduplicated-promote hazard TG-436 forbids. So this module lands the candidate
// (ingest.go) and reads the level it has EARNED (below), and the producer's asserted outcome never
// enters the ladder — a poisoned chunk that never produces a local verified-clean run never earns
// authority.

// Level reports the graduation level an op-class has EARNED on THIS node — the authority it holds. An
// op-class the local ladder has never promoted (including a freshly-landed foreign candidate) reads
// LevelApprove: propose-only, no autonomous authority (REQ-2110). It fails closed to LevelApprove on
// any store error — an absent or unreadable class never reads as autonomous.
func (r *ReGrad) Level(ctx context.Context, opClass string) policy.Level {
	st, err := r.store.Load(ctx, opClass)
	if err != nil {
		return policy.LevelApprove
	}
	return st.Level
}
