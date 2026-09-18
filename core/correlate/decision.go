package correlate

import (
	"time"

	"github.com/territory-grounder/grounder/core/execclass"
)

// Decision is the ROUTING DECISION as a durable record: which execution topology an incident was sent
// down, the classifier inputs that produced it, and the correlation evidence behind the one input that
// used to be a guess.
//
// WHY THIS EXISTS AT ALL (TG-169). The execution class was computed at the top of the Runner workflow and
// written NOWHERE. It rode out on RunnerResult and stopped there: no column, no table, no ledger entry.
// So "why did this incident get the deep path and that one the standard path?" was unanswerable against
// TG's own history — for every one of the thousands of sessions already recorded — and a routing rule
// could be changed with no way to measure what it changed. A decision that leaves no trail cannot be
// reviewed, and an unreviewable decision is indistinguishable from a random one.
//
// INPUTS, NOT JUST THE OUTPUT. `Inputs` is the whole execclass.Input the classifier was called with, not
// a summary of it. The class alone is a conclusion; re-deriving the premises later is guesswork the
// moment the rules move, and the rules are expected to move (execclass.Input carries eight signals and
// this stage feeds one of them today). Persisting the input makes a past decision REPLAYABLE against a
// future classifier, which is the only way to tell "the rule changed" from "the estate changed".
//
// OBSERVABILITY + REVIEW ONLY. Nothing reads a persisted decision back into a gate: it authorizes
// nothing, releases nothing and vetoes nothing (INV-08). Non-secret identifiers and counts only — host
// names, source slugs, external refs — never a payload, never a credential (INV-13).
type Decision struct {
	// ExternalRef is the session correlation key the whole pipeline joins on.
	ExternalRef string
	// ExecClass is the topology chosen (an execclass.Class value).
	ExecClass execclass.Class
	// Inputs is the FULL classifier input, persisted so the decision can be re-derived later.
	Inputs execclass.Input
	// Verdict is the correlation stage's own answer and evidence (see Verdict).
	Verdict Verdict
	// ClusterID is the DURABLE cluster identity this correlated session joined (alert_cluster, migration
	// 0085) — the id every member of one storm shares, so the record can answer "which cascade was this?"
	// against a stable key rather than a per-arrival window. 0 for an uncorrelated incident or a deployment
	// with no durable cluster store (TG-385).
	ClusterID int64
	// Election is the causal-subject decision for the cluster (see Election): who investigates, the
	// runner-up, and the rule that decided. Zero value for an uncorrelated incident. Persisted alongside the
	// verdict so the collapse (TG-376) is auditable — a wrong election is reviewable, never silent.
	Election Election
	// DecidedAt is when the stage ran.
	DecidedAt time.Time
}
