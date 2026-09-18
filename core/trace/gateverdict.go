package trace

import "context"

// GateVerdict is one NON-SECRET row of the per-interceptor-gate verdict trail (spec/020 T-020-7, REQ-2007): the
// 1-based ordinal position of a gate in the actuation chokepoint's ordered sequence, the gate's name, its
// resolved verdict ("pass"/"refuse" for an admission gate; "match"/"partial"/"deviation" for the verify tail),
// and a non-secret reason. Keyed by BOTH correlation keys (action_id + external_ref) so it joins the
// decision-tracer walk. It carries NO argv/host/credential (INV-13) — only governance labels.
type GateVerdict struct {
	Ordinal     int
	Gate        string
	Verdict     string
	Reason      string
	ActionID    string
	ExternalRef string
	// Margin, when non-nil, is how far THIS gate's decision was from its threshold — signed and NON-SECRET
	// (TG-178): value - threshold, so a NEGATIVE margin means the value sat on the refusing side by |Margin|
	// and a POSITIVE margin means it cleared by Margin. A small |Margin| is a boundary-case decision — passed
	// or refused within ε of its threshold — which is the reviewable / skill-store-flywheel-preferential set.
	// Nil for a gate with no numeric threshold (a binary match/floor has no margin to be near). First carried
	// for the policy gate's min_confidence clamp (Confidence - min_confidence); other gates adopt it as their
	// margins are threaded out. Observe-only like the rest of this row — never read back to make a decision.
	Margin *float64
}

// GateVerdictSink records one gate-verdict row at an interceptor gate boundary. It is OBSERVE-ONLY by contract:
// the interceptor emits into it as a pure side effect and NEVER reads it back to make a decision, and an Emit
// error MUST NOT change any gate outcome (the actuation chokepoint stays fail-closed regardless). A nil sink is
// a no-op — the interceptor behaves identically without it.
type GateVerdictSink interface {
	Emit(ctx context.Context, gv GateVerdict) error
}
