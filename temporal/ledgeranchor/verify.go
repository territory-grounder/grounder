package ledgeranchor

import (
	"context"
	"fmt"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
)

// VerifyJob is the CONSUMING half of the ledger-anchor mechanism (TG-509). The recorder (Job, above) periodically
// witnesses the governance ledger's HEAD into the append-only ledger_anchor store — external, tamper-EVIDENT
// records the recorder cannot later rewrite. But recording witnesses only becomes tamper-RESISTANCE when
// something CHECKS the live ledger against them: core/audit.VerifyAgainstAnchors re-derives every recorded anchor
// from the CURRENT chain and returns the first contradiction (the check VerifyChain structurally cannot do —
// it compares the chain against a record made BEFORE any tamper). Until this job existed, that check had zero
// callers: the witnesses were recorded but never checked, so a HEAD that regressed below a witness (a rollback /
// tamper of anything appended before the witness) went undetected. This closes that present-not-reaching loop.
//
// OBSERVE-ONLY, exactly like the recorder: it reads two stores, runs a pure comparison, and SURFACES a
// contradiction. It adjudicates nothing, reaches no actuator, and changes no mutation posture — the operator's
// response to a detected tamper (halt, investigate) is a separate decision, deliberately not taken here.
type VerifyJob struct {
	Ledger  LedgerReader // the full chain in ascending seq (a *db.LedgerStore in production)
	Anchors AnchorReader // the recorded witness history (a *db.AnchorStore in production)
}

// LedgerReader supplies the FULL chain for verification. VerifyAgainstAnchors needs every entry (not just the
// HEAD + window the recorder reads), because it re-derives each witness's digest from the chain up to that seq.
type LedgerReader interface {
	All(ctx context.Context) ([]audit.LedgerEntry, error)
}

// AnchorReader supplies the recorded witnesses to check the chain against.
type AnchorReader interface {
	Anchors(ctx context.Context) ([]audit.Anchor, error)
}

// Run reads the chain + the witnesses and checks the chain against them. It returns (err, ok):
//   - ok=false  ⇒ a read FAILED (the chain or witnesses could not be read), so the verification could not run;
//     err describes it. A read failure is surfaced, NEVER silently a pass — an unverifiable spine is not a clean
//     one (the same fail-closed posture the baseline/verifiability gates take).
//   - ok=true, err != nil ⇒ TAMPER: the live chain contradicts a witness recorded before it could have been
//     tampered. err is the first contradiction VerifyAgainstAnchors found.
//   - ok=true, err == nil ⇒ the chain still matches every witness (or there are no witnesses yet — a fresh spine
//     has nothing to contradict, which is honestly not a tamper).
func (j VerifyJob) Run(ctx context.Context) (error, bool) {
	// Read the ANCHORS BEFORE the chain (TG-516). The recorder and this verifier both fire immediate passes at
	// boot, concurrently, and a witnessed row commits BEFORE its anchor is recorded. Reading the chain FIRST
	// (the original order) let a boot read-race snapshot the chain one row BEHIND a just-recorded anchor:
	// VerifyAgainstAnchors then saw anchor.Seq > chain.maxSeq and cried a FALSE truncation on a perfectly
	// intact spine (observed live 2026-08-17: anchor seq 11408 vs a chain snapshot at 11407 that already held
	// 11408 — self-resolved next pass). Because the chain is append-only + monotonic, a chain read taken AFTER
	// the anchor read is GUARANTEED to include every anchored row (each committed before its anchor, hence
	// before this later read) — so a race can no longer produce anchor-ahead-of-chain, while a REAL truncation
	// (a row genuinely gone and STAYING gone) still persists and still alarms. Cheaper than a shared snapshot,
	// and it cannot mask a real tamper.
	anchors, aerr := j.Anchors.Anchors(ctx)
	if aerr != nil {
		return fmt.Errorf("ledger-verify: read anchors: %w", aerr), false
	}
	if len(anchors) == 0 {
		return nil, true // no witnesses recorded yet — nothing to contradict
	}
	current, lerr := j.Ledger.All(ctx)
	if lerr != nil {
		return fmt.Errorf("ledger-verify: read chain: %w", lerr), false
	}
	return audit.VerifyAgainstAnchors(audit.RowRefsOf(current), anchors), true
}

// RunVerifyPeriodically verifies once IMMEDIATELY (a deploy is when re-checking a fresh, independent witness is
// worth most) and then every `every` until ctx is done. It blocks; run it in a goroutine. Full-chain
// verification is O(chain × anchors), so `every` is deliberately coarse (hourly/daily), not the recorder's
// minute-scale cadence. onTamper fires on a DETECTED contradiction (the critical operator signal); onErr fires
// when the verification could not run (a read gap). A pass NEVER stops the loop and NEVER propagates — like the
// recorder, this must not take the worker down with it.
func RunVerifyPeriodically(ctx context.Context, j VerifyJob, every time.Duration, onTamper, onErr func(error)) {
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
