package ledgeranchor

// The CONSUMING half of the Temporal witness (TG-80 P1.1), the same recorded/never-checked shape TG-509 fixed
// for the DB-side anchor: TemporalWitness.Record (witness.go) durably lands a HEAD anchor in Temporal's event
// history, but recording only becomes tamper-RESISTANCE once something checks the live spine against it.
//
// WHAT THIS CATCHES THAT VerifyJob CANNOT. VerifyJob (verify.go) cross-checks the live governance_ledger chain
// against ledger_anchor — but both live in the SAME Postgres instance, so an actor who can rewrite one under a
// reversed REVOKE can rewrite the other identically (job.go's own residual). TemporalVerifyJob instead
// cross-checks the DB-recorded anchor against Temporal's INDEPENDENTLY stored witness: if an actor rewrites
// governance_ledger and ledger_anchor consistently within TG's Postgres instance, the Temporal-side witness —
// recorded in a different database, under a different credential, before the tamper — still shows the OLD,
// true HEAD. A live chain (or DB anchor) that no longer agrees with it is exposed precisely because the
// Temporal witness was never reachable through the credential that performed the rewrite.
//
// THE LOAD-BEARING WORD IS "before". This holds for any seq whose Temporal witness was already recorded
// before the tamper — which is every ALREADY-witnessed row, i.e. the ledger's past. It does NOT hold for a
// row an attacker tampers AND witnesses (via a squatted, pre-registered WitnessWorkflowID) in the SAME attack
// window, before the legitimate periodic recorder ever reaches that seq — there, DB anchor, live chain, and
// Temporal witness can all be made to agree on the same forged content, and this comparison finds nothing to
// contradict. See temporal/ledgeranchor/witness.go's "THE SQUAT RESIDUAL" for the full analysis; a clean
// result from this job is NOT a claim that the freshest, not-yet-witnessed slice of the ledger is safe from a
// coordinated (DB + unauthenticated-Temporal-frontend) attacker — only that no ALREADY-witnessed row has been
// rewritten.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
)

// DefaultTemporalVerifyRecent bounds how many of the most-recently-DB-recorded anchors a pass cross-checks
// against Temporal when TemporalVerifyJob.Recent is left unset (<=0) — a deliberate finite default so a
// zero-value job never scans an unbounded ledger_anchor history against Temporal on every pass. Older DB
// anchors are not re-checked every pass: they either predate Temporal witnessing (benign) or have already
// been covered by a previous pass; the RECENT ones are where a fresh tamper would show up first.
const DefaultTemporalVerifyRecent = 8

// ErrTemporalWitnessMismatch reports that a Temporal-witnessed HEAD anchor disagrees with either the
// DB-recorded anchor for the same seq or the live governance_ledger chain — two INDEPENDENT witnesses, stored
// under different credentials in different databases, have diverged. That is only possible if one of them was
// tampered (TG-80 P1.1's cross-check — the check VerifyJob's DB-vs-DB comparison structurally cannot make).
var ErrTemporalWitnessMismatch = errors.New("audit: Temporal-witnessed ledger HEAD anchor disagrees with the DB-recorded anchor or the live chain (tamper)")

// ErrTemporalWitnessUnavailable is the FAIL-SAFE outcome when the DB has recorded anchors but NONE of the
// ones checked have a matching Temporal witness. This is deliberately NEVER folded into a clean pass (ok=true,
// err=nil) — an unverifiable spine is not a verified one (the same posture VerifyJob's own read-failure branch
// takes). Benign causes (Temporal witnessing wired after these DB anchors existed; a namespace retention
// window shorter than the verify cadence) are indistinguishable, from here, from a witness having been
// erased — which is exactly why this must surface rather than be silently absorbed.
var ErrTemporalWitnessUnavailable = errors.New("audit: no Temporal-witnessed HEAD anchor could be found for any recently-checked seq — the cross-check cannot run (fail-safe: not a clean verdict)")

// TemporalAnchorReader reads back the anchor Temporal witnessed for (domain, seq) — a *TemporalWitness
// (witness.go) in production, a fake in tests.
type TemporalAnchorReader interface {
	ReadWitness(ctx context.Context, domain string, seq int64) (a audit.Anchor, found bool, err error)
}

// TemporalVerifyJob cross-checks the DB-recorded anchor history against Temporal's independently-stored
// witnesses, and both against the live chain. OBSERVE-ONLY, exactly like VerifyJob: it reads three sources,
// runs a pure comparison, and surfaces a contradiction — it adjudicates nothing and reaches no actuator.
type TemporalVerifyJob struct {
	Anchors  AnchorReader         // the DB witness history for the domain (a *db.AnchorStore.Scoped(domain) in prod)
	Ledger   LedgerReader         // the full live chain (a *db.LedgerStore in prod) — same interface VerifyJob uses
	Temporal TemporalAnchorReader // Temporal read-back (a *TemporalWitness in prod)
	Domain   string
	Recent   int // cross-check only the last N DB-recorded anchors per pass; <=0 uses DefaultTemporalVerifyRecent
}

// Run cross-checks the most recent DB-recorded anchors against their Temporal witnesses and the live chain.
// Return shape mirrors VerifyJob.Run: (err, ok) where
//   - ok=false  ⇒ the check could not run at all (a store read failed, or no Temporal witness could be found
//     for anything recently recorded) — fail-safe, NEVER a clean pass;
//   - ok=true, err!=nil ⇒ TAMPER: a Temporal witness disagrees with the DB anchor or the live chain;
//   - ok=true, err==nil ⇒ every checked seq that had a Temporal witness agreed with both (and at least one
//     did), or there is nothing recorded anywhere yet (a fresh spine, honestly not a tamper).
func (j TemporalVerifyJob) Run(ctx context.Context) (error, bool) {
	anchors, aerr := j.Anchors.Anchors(ctx)
	if aerr != nil {
		return fmt.Errorf("ledger-temporal-verify: read DB anchors: %w", aerr), false
	}
	if len(anchors) == 0 {
		return nil, true // nothing recorded anywhere yet — a fresh spine, nothing to contradict (mirrors VerifyJob)
	}

	recent := j.Recent
	if recent <= 0 {
		recent = DefaultTemporalVerifyRecent
	}
	if len(anchors) > recent {
		anchors = anchors[len(anchors)-recent:]
	}

	current, lerr := j.Ledger.All(ctx)
	if lerr != nil {
		return fmt.Errorf("ledger-temporal-verify: read chain: %w", lerr), false
	}
	bySeq := make(map[int64]audit.RowRef, len(current))
	for _, e := range current {
		bySeq[e.Seq] = audit.RowRef{Seq: e.Seq, Hash: e.Hash}
	}

	checked := 0
	for _, dbA := range anchors {
		tA, found, terr := j.Temporal.ReadWitness(ctx, j.Domain, dbA.Seq)
		if terr != nil {
			return fmt.Errorf("ledger-temporal-verify: read temporal witness at seq %d: %w", dbA.Seq, terr), false
		}
		if !found {
			continue // absent from Temporal is not itself proof of anything — see the fail-safe check below
		}
		checked++
		if tA.Hash != dbA.Hash || tA.Digest != dbA.Digest {
			return fmt.Errorf("%w: seq %d — Temporal witness hash %s, DB-recorded anchor hash %s",
				ErrTemporalWitnessMismatch, dbA.Seq, shortHash(tA.Hash), shortHash(dbA.Hash)), true
		}
		row, present := bySeq[dbA.Seq]
		if !present || row.Hash != tA.Hash {
			return fmt.Errorf("%w: seq %d — the Temporal-witnessed HEAD is absent from or disagrees with the LIVE chain (row present=%v)",
				ErrTemporalWitnessMismatch, dbA.Seq, present), true
		}
	}
	if checked == 0 {
		return ErrTemporalWitnessUnavailable, false
	}
	return nil, true
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// RunTemporalVerifyPeriodically verifies once IMMEDIATELY and then every `every` until ctx is done — same
// shape as RunVerifyPeriodically (verify.go), kept fully independent (its own goroutine, its own ticker) so a
// defect in one cross-check can never silence the other. onTamper fires on a DETECTED divergence (the critical
// operator signal); onErr fires when the cross-check could not run at all, including the fail-safe
// ErrTemporalWitnessUnavailable case (callers that want to distinguish it can errors.Is on the error passed
// in). A pass never stops the loop and never propagates — observe-only, like every other cron arm here.
func RunTemporalVerifyPeriodically(ctx context.Context, j TemporalVerifyJob, every time.Duration, onTamper, onErr func(error)) {
	pass := func() {
		cctx, cancel := context.WithTimeout(ctx, every)
		defer cancel()
		err, ok := j.Run(cctx)
		switch {
		case !ok:
			if onErr != nil {
				onErr(err)
			}
		case err != nil:
			if onTamper != nil {
				onTamper(err)
			}
		}
	}
	pass()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pass()
		}
	}
}
