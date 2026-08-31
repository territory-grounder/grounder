package db

import (
	"context"
	"fmt"
)

// loop_closure.go — HAS THIS LOOP EVER CLOSED? (TG-348)
//
// Four mechanisms are built, wired, running, and their closing step has never once executed. Measured on
// the live database 2026-08-06:
//
//	world manifest        369 entries, ALL status='draft'      approved: 0
//	op-class ratification  10 candidates, ALL status='observing'  ratified: 0
//	graduation ladder     460 action_execution rows            credits: 0
//	bound rollback        460 executions                       inverses run: 0
//
// TG-348's framing is the one worth keeping: **a loop that has never closed once is not a working loop
// with an idle operator — it is an untested path presented as a feature.** Each of these has the same
// three properties: the generating half is real and produces output so dashboards look healthy; the
// consuming half has no exercise, so any defect in it is undiscovered by construction; and nothing
// distinguishes "nobody has needed to yet" from "it would fail if anyone tried".
//
// THE THIRD PROPERTY IS THE COST, AND IT IS WHAT THIS MEASURES. The existing wiring/yield register cannot
// see it: `world.discovery` declares its yield as "manifest drafts written", which reads LIVE and healthy
// at 369 while zero approvals stays invisible. The register measures PRODUCTION; this measures CLOSURE.
//
// The closing predicates come from the tables' own CHECK constraints, not from reading call sites:
//
//	manifest_entry.status     IN (draft, approved, retired_candidate_stale, retired, rejected)
//	opclass_candidate.status  IN (observing, candidate, ratify_ready, ratified, dismissed, expired)
//
// so "closed" cannot drift onto a value the column can never hold — which would pin Closed at 0 forever
// and make every assertion here vacuously true.
//
// THE ROLLBACK LOOP IS NOW WATCHED (TG-404). It used to be deliberately absent: an inverse was recorded
// only inside an execution-log STRING ("rollback[...]" bound to action_id) with no durable row, so "an
// inverse ran" was not countable without parsing prose, and reporting it as Closed=0 would have been
// indistinguishable from reporting it unmeasurable. TG-404 gave the inverse a durable row —
// action_execution.inverts_action_id — so the loop is now first-class here: Generated = forward executions
// (inverts_action_id IS NULL, each a rollback candidate), Closed = inverses that ran (IS NOT NULL). The
// signal the ticket named — "460 executions, 0 inverses run" — is now a metric, not a blind spot.

// LoopClosure is one built loop's generating and closing counts.
type LoopClosure struct {
	// Loop is the metric label — stable, lowercase, and matched by the alert rule.
	Loop string
	// Generated is the upstream artifact count: how much work the producing half has done. It is the
	// DENOMINATOR, and it is what makes Closed=0 interpretable — zero closures against zero generated is
	// an idle system, zero against 369 is a loop that has never once completed.
	Generated int64
	// Closed counts terminal artifacts — the step an operator or a downstream stage performs.
	Closed int64
}

// NeverClosed reports a loop that has produced work and closed none of it. Zero-against-zero is NOT never
// closed: a loop with nothing to close is idle, not broken, and alerting on it would train the operator
// to ignore this signal on every fresh deployment.
func (l LoopClosure) NeverClosed() bool { return l.Generated > 0 && l.Closed == 0 }

// CountLoopClosures measures each built loop. Counts only — it reads no payload.
func (s *Pool) CountLoopClosures(ctx context.Context) ([]LoopClosure, error) {
	out := make([]LoopClosure, 0, 4)

	for _, q := range []struct {
		loop string
		sql  string
	}{
		{"world_manifest", `SELECT count(*), count(*) FILTER (WHERE status = 'approved') FROM manifest_entry`},
		{"opclass_ratification", `SELECT count(*), count(*) FILTER (WHERE status = 'ratified') FROM opclass_candidate`},
		// The graduation ladder earns a credit from an execution, so executions are the denominator.
		{"graduation_credit", `SELECT (SELECT count(*) FROM action_execution), (SELECT count(*) FROM graduation_credit)`},
		// The bound-rollback loop (TG-82), now countable via TG-404's inverts_action_id. A forward execution
		// is a rollback CANDIDATE (the denominator); an inverse is the closing step (a rollback that ran).
		{"bound_rollback", `SELECT count(*) FILTER (WHERE inverts_action_id IS NULL), count(*) FILTER (WHERE inverts_action_id IS NOT NULL) FROM action_execution`},
	} {
		var c LoopClosure
		c.Loop = q.loop
		if err := s.QueryRow(ctx, q.sql).Scan(&c.Generated, &c.Closed); err != nil {
			return nil, fmt.Errorf("db: loop closure %s: %w", q.loop, err)
		}
		out = append(out, c)
	}
	return out, nil
}
