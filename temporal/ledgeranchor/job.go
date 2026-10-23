// Package ledgeranchor is the periodic HEAD-anchor recorder for TG's governance ledger (TG-80 P1#1): every
// interval it reads the append-only spine's HEAD, folds it into a deterministic digest (core/audit.
// ComputeAnchor), and records that anchor as an external WITNESS of the HEAD over time.
//
// It is OBSERVE-ONLY — it adjudicates nothing, gates nothing, and NEVER reaches an actuator or changes the
// mutation posture. The heavy lifting is the pure core/audit anchor math; this package is the periodic
// orchestration around it, structured exactly like temporal/calibrate (a Job with a Run pass and a
// RunPeriodically loop the worker runs in a goroutine).
//
// THE INDEPENDENCE PROPERTY the anchor rests on: the Sink (ledger_anchor, migration 0092) is written by a
// principal that holds INSERT + SELECT but NOT UPDATE/DELETE on the store — the same REVOKE the spine carries
// (0015) — so the recorder cannot later rewrite a witness to match a tampered ledger. Run returns the anchor,
// so a Temporal-activity wrapper can also land it in event history (a separate credential domain), which is
// the stronger airgap; this package delivers the witness-over-time that stands on the DB REVOKE alone.
package ledgeranchor

import (
	"context"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
)

// HeadReader supplies the ledger HEAD + trailing window (a *db.LedgerStore in production, a fake in tests).
type HeadReader interface {
	Head(ctx context.Context, window int) (audit.HeadState, error)
}

// AnchorSink durably records one witness (a *db.AnchorStore in production, a fake in tests).
type AnchorSink interface {
	Record(ctx context.Context, a audit.Anchor) error
}

// Job records one HEAD anchor per pass. Window defaults to audit.DefaultAnchorWindow when unset; Now stamps
// the recorded anchor (nil ⇒ left zero); Emit is an optional observe hook (log/metrics), nil to discard.
type Job struct {
	Head   HeadReader
	Sink   AnchorSink
	Window int
	Now    func() time.Time
	Emit   func(audit.Anchor)
}

// Run records one HEAD anchor and returns (anchor, recorded, error). recorded is FALSE with a nil error when
// the ledger is EMPTY (Seq 0): there is nothing to witness yet, and anchoring "nothing" would plant a phantom
// fixed point that the very first real append would appear to regress from. A read or write error is returned
// (the caller logs it and keeps the loop running); the chain is untouched either way — this path only reads
// the ledger and appends to a separate table.
func (j Job) Run(ctx context.Context) (audit.Anchor, bool, error) {
	window := j.Window
	if window <= 0 {
		window = audit.DefaultAnchorWindow
	}
	hs, err := j.Head.Head(ctx, window)
	if err != nil {
		return audit.Anchor{}, false, err
	}
	if hs.Seq == 0 {
		return audit.Anchor{}, false, nil
	}
	a := audit.ComputeAnchor(hs)
	if j.Now != nil {
		a.At = j.Now()
	}
	if err := j.Sink.Record(ctx, a); err != nil {
		return a, false, err
	}
	if j.Emit != nil {
		j.Emit(a)
	}
	return a, true, nil
}

// RunPeriodically records one anchor IMMEDIATELY and then one every `every` until ctx is done. It blocks;
// callers run it in a goroutine.
//
// The immediate pass is the point, not a nicety — the same deploy-time blind window the calibrator's
// RunPeriodically fixes: a bare `for range t.C` would leave the spine UNWITNESSED for a full interval after
// every worker start, and a deploy is exactly when a fresh, independent witness is worth most. onErr receives
// a failed pass; a pass NEVER stops the loop and never propagates — the recorder is observe-only and must not
// take the worker down with it.
func RunPeriodically(ctx context.Context, j Job, every time.Duration, onErr func(error)) {
	pass := func() {
		cctx, cancel := context.WithTimeout(ctx, every)
		defer cancel()
		if _, _, err := j.Run(cctx); err != nil && onErr != nil {
			onErr(err)
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
