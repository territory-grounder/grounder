package regime

import (
	"context"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// THE DEFERRED VERIFIER'S BASELINE ARM — the async twin of the 2026-07-28 synchronous false deviation
// (governance ledger 5153-5155). The deferred verify adjudicates MINUTES after launch against an estate-wide
// observation; without a baseline, every alert already firing at launch reads as the launched job's cascade,
// and that verdict feeds the same graduation ladder the synchronous path feeds. These oracles pin the three
// behaviours: a pre-anomalous host does not deviate; an absent baseline WITHHOLDS a would-be deviation rather
// than manufacturing one; and neither rule can blind a genuinely new cascade or launder a clean match.

func reserveAndBind(ctx context.Context, t *testing.T, av *AsyncVerify, actionID string) {
	t.Helper()
	if err := av.Reserve(ctx, LaunchIntent{ActionID: actionID, OpClass: "disk-grow", Lane: RegimeAWXJob, Prediction: prediction("web01")}); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := av.BindHandle(ctx, actionID, "job-"+actionID); err != nil {
		t.Fatalf("BindHandle: %v", err)
	}
}

// TestDeferredVerifyDoesNotBlameALaunchForAPreexistingIncident — the ledger-5153 shape on the deferred path.
// An unrelated host is alerting when the job resolves, and the host arm reports it already held an OPEN
// incident at LaunchedAt. The verdict must be a clean MATCH.
func TestDeferredVerifyDoesNotBlameALaunchForAPreexistingIncident(t *testing.T) {
	ctx := context.Background()
	observed := []verify.ObservedAlert{{Host: "stale-host07", Rule: "Device-Down", Site: "nl"}}
	var askedAsOf time.Time
	av, store, poller, _ := newChannel(t,
		WithObserver(func(context.Context, verify.Prediction) ([]verify.ObservedAlert, bool) { return observed, true }),
		WithPreAnomalous(func(_ context.Context, asOf time.Time) (map[string]bool, bool) {
			askedAsOf = asOf
			return map[string]bool{"stale-host07": true}, true
		}))
	reserveAndBind(ctx, t, av, "a-pre")
	poller.Script("job-a-pre", JobSuccessful)

	if _, err := av.Verify(ctx, "a-pre"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	rec, _ := store.Get(ctx, "a-pre")
	if !rec.Verified || rec.Verdict != safety.VerdictMatch {
		t.Fatalf("a host already broken at launch must not adjudicate as the launch's cascade, got verified=%t verdict=%q",
			rec.Verified, rec.Verdict)
	}
	if askedAsOf.IsZero() || !askedAsOf.Equal(rec.LaunchedAt) {
		t.Fatalf("the host arm must be anchored at the record's LaunchedAt (the last pre-effect instant this "+
			"record carries), got asOf=%v launched=%v — any other anchor lets the job's own effects launder into "+
			"the baseline", askedAsOf, rec.LaunchedAt)
	}
}

// TestDeferredDeviationWithoutABaselineIsWithheldNotManufactured — the epistemics oracle, model-independent:
// a deviation this path cannot ground in ANY baseline must resolve unverified (visible, no graduation effect),
// never as a verdict. Both absence shapes are exercised: arm wired-but-failing, and arm not wired at all.
func TestDeferredDeviationWithoutABaselineIsWithheldNotManufactured(t *testing.T) {
	ctx := context.Background()
	observed := []verify.ObservedAlert{{Host: "surprise42", Rule: "Device-Down", Site: "nl"}}

	// (a) wired but failing.
	av, store, poller, grad := newChannel(t,
		WithObserver(func(context.Context, verify.Prediction) ([]verify.ObservedAlert, bool) { return observed, true }),
		WithPreAnomalous(func(context.Context, time.Time) (map[string]bool, bool) { return nil, false }))
	reserveAndBind(ctx, t, av, "a-failarm")
	poller.Script("job-a-failarm", JobSuccessful)
	if _, err := av.Verify(ctx, "a-failarm"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	rec, _ := store.Get(ctx, "a-failarm")
	if rec.Verified || rec.Verdict != "" {
		t.Fatalf("a deviation computed with an unreadable baseline must be WITHHELD (verified=false, no verdict), "+
			"got verified=%t verdict=%q — this is the manufactured verdict that halted actuation for 1h49m", rec.Verified, rec.Verdict)
	}
	for _, g := range grad.Recorded() {
		if g.Verified && g.Verdict == safety.VerdictDeviation {
			t.Fatal("a withheld verdict fed a VERIFIED deviation to graduation — the demote fired anyway")
		}
	}

	// (b) not wired at all — the pre-fix production shape.
	av2, store2, poller2, _ := newChannel(t,
		WithObserver(func(context.Context, verify.Prediction) ([]verify.ObservedAlert, bool) { return observed, true }))
	reserveAndBind(ctx, t, av2, "a-nowire")
	poller2.Script("job-a-nowire", JobSuccessful)
	if _, err := av2.Verify(ctx, "a-nowire"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	rec2, _ := store2.Get(ctx, "a-nowire")
	if rec2.Verified || rec2.Verdict != "" {
		t.Fatalf("with NO host arm wired a would-be deviation must be withheld, got verified=%t verdict=%q", rec2.Verified, rec2.Verdict)
	}
}

// TestDeferredBaselineNeitherBlindsNorLaunders — the two fail-directions the arm must not open:
//   - a genuinely NEW cascade (host arm established, reports nothing open) must still DEVIATE, verified; and
//   - a clean quiet estate must still MATCH even when the host arm is absent — an absent baseline can only
//     have ADDED surprises, so match/partial verdicts stand and the withhold applies to deviations alone.
func TestDeferredBaselineNeitherBlindsNorLaunders(t *testing.T) {
	ctx := context.Background()

	// (a) real cascade, established-but-empty host arm ⇒ deviation stands.
	av, store, poller, _ := newChannel(t,
		WithObserver(func(context.Context, verify.Prediction) ([]verify.ObservedAlert, bool) {
			return []verify.ObservedAlert{{Host: "surprise42", Rule: "Device-Down", Site: "nl"}}, true
		}),
		WithPreAnomalous(func(context.Context, time.Time) (map[string]bool, bool) { return map[string]bool{}, true }))
	reserveAndBind(ctx, t, av, "a-real")
	poller.Script("job-a-real", JobSuccessful)
	if _, err := av.Verify(ctx, "a-real"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	rec, _ := store.Get(ctx, "a-real")
	if !rec.Verified || rec.Verdict != safety.VerdictDeviation {
		t.Fatalf("an established-but-empty baseline must not blind a real cascade, got verified=%t verdict=%q",
			rec.Verified, rec.Verdict)
	}

	// (b) quiet estate, no arm ⇒ match stands (the withhold must not convert every unbaselined success into
	// an unverified run — that would zero graduation accrual for the whole async lane).
	av2, store2, poller2, _ := newChannel(t,
		WithObserver(func(context.Context, verify.Prediction) ([]verify.ObservedAlert, bool) { return nil, true }))
	reserveAndBind(ctx, t, av2, "a-quiet")
	poller2.Script("job-a-quiet", JobSuccessful)
	if _, err := av2.Verify(ctx, "a-quiet"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	rec2, _ := store2.Get(ctx, "a-quiet")
	if !rec2.Verified || rec2.Verdict != safety.VerdictMatch {
		t.Fatalf("a quiet estate must stay a verified MATCH with no baseline arm (nothing to withhold), got "+
			"verified=%t verdict=%q", rec2.Verified, rec2.Verdict)
	}
}
