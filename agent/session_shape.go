package agent

import "fmt"

// DETERMINISTIC SESSION SHAPE — what kind of session this was, computed without the judge (MECH-605).
//
// TG's judge is the SOLE per-session grader. Every one of its five dimensions is LLM-scored, so if the
// judge is unavailable, mis-prompted, or simply wrong about a session, there is no independent signal at
// all. The predecessor composes its verdict hard-checks-first — a safety veto and a trajectory veto run
// BEFORE the judge and can override it — and suppresses grades computed from a "husk": a row with no
// tool calls and at most one turn must not override a real grade.
//
// MEASURED IN PRODUCTION 2026-08-01, and this is why the check is worth having rather than a parity
// import. Of 3,202 judged sessions, 503 carry step_count = 0. Splitting them by outcome separates two
// completely different things:
//
//	232  no-proposal:stop      — a fast stand-down. Taking no investigation step before declining to act
//	                             is CORRECT, not degenerate; the alert did not warrant one.
//	243  proposed              — a remedy proposed with ZERO investigation steps. None was executed.
//
// Those 243 are graded by the judge on the same five dimensions as a session that actually investigated,
// and their scores pool into the eval scorecard indistinguishably. That is the degenerate-grade problem
// in TG's own data.
//
// OBSERVATIONAL. This classifies; it changes no grade, suppresses no row, and alters no eval arithmetic.
// Making degenerate sessions stop counting would move the scorecard baseline, which is an owner's call
// and an eval-gated change — not something to slip in behind a classifier. What this delivers is the
// distinction, recorded per session, so the question "how many of our grades come from sessions that
// never looked?" becomes answerable at all.

// Shape is a session's deterministic classification.
type Shape string

const (
	// ShapeInvestigated — the loop took at least one investigation step.
	ShapeInvestigated Shape = "investigated"
	// ShapeFastStandDown — no steps AND no proposal. Correct behaviour, named so it is never conflated
	// with the case below.
	ShapeFastStandDown Shape = "fast-stand-down"
	// ShapeUnexaminedProposal — a proposal produced with NO investigation step. The sharp case: the
	// model proposed a remedy from the seed alone.
	ShapeUnexaminedProposal Shape = "unexamined-proposal"
)

// ClassifySession names a session's shape from two facts the spine already records, and explains it.
//
// The reason is returned rather than derived by the caller so the recorded note carries the counts that
// produced it — a bare shape string tells an operator what, never why.
func ClassifySession(steps int, proposed bool) (Shape, string) {
	// No clamp on a negative step count, deliberately: `steps > 0` already excludes it, so the row falls
	// through to the proposed/stand-down split exactly as a zero would. A defensive `if steps < 0 { steps
	// = 0 }` was written here first and a mutation deleting it changed NOTHING — it was dead code, and
	// dead defensive code is worse than none: it implies a hazard is handled somewhere the reader will
	// not check. TestNegativeStepCountIsTreatedAsZero still pins the behaviour.
	switch {
	case steps > 0:
		return ShapeInvestigated, fmt.Sprintf("%d investigation step(s)", steps)
	case proposed:
		return ShapeUnexaminedProposal,
			"a remedy was proposed with NO investigation step — the model reasoned from the seed alone"
	default:
		return ShapeFastStandDown,
			"no investigation step and no proposal — declining to act without looking is correct here"
	}
}

// Degenerate reports whether a shape should be treated as a husk for grading purposes.
//
// ONLY the unexamined proposal qualifies. A fast stand-down has zero steps too, and treating step count
// alone as the test would mark 232 correct stand-downs degenerate alongside the 243 that matter —
// exactly the conflation this type exists to prevent.
func (s Shape) Degenerate() bool { return s == ShapeUnexaminedProposal }
