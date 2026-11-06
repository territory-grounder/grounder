package policy

import (
	"context"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
)

// withEmbedded substitutes the embedded-registry predicate for one test. It is the counterpart to withTiers,
// and necessary for the same reason: every class shipped today IS embedded, so without the seam the AUTO
// ceiling can never be observed holding anything back, and a control that cannot fail is not a control.
func withEmbedded(t *testing.T, f func(string) bool) {
	t.Helper()
	prev := isEmbeddedClass
	isEmbeddedClass = f
	t.Cleanup(func() { isEmbeddedClass = prev })
}

// THE WIDENED LADDER (spec/028 REQ-2804/2807/2808, ADR-0016 decision 2).
//
// These oracles cover the rung that was inserted between approve and auto, and — more importantly — the
// CEILING above it. The ceiling is the whole reason TG can admit capabilities at runtime at all: a class the
// operator ratified may earn the right to ACT, but the rung where NO HUMAN WATCHES stays behind a code
// release. Everything below tries to reach that rung the wrong way.

// TestOverlayClassHoldsAtTheAutoCeiling is the load-bearing oracle of ADR-0016 decision 2.
//
// RED CONTROL EXECUTED: removed the isEmbeddedClass condition from the auto_notice→auto promotion ->
//
//	"an OVERLAY-ONLY class reached the SILENT auto rung — ratification lifted its own ceiling, which is the
//	 tamper-domain collapse ADR-0016 exists to prevent"
func TestOverlayClassHoldsAtTheAutoCeiling(t *testing.T) {
	ctx := context.Background()
	// A ratified, overlay-only class: registered enough to have an auto-eligible tier (so the tier floor is
	// NOT what stops it), but absent from the embedded registry.
	const op = "rotate-appliance-log"
	withTiers(t, map[string]string{op: opschema.TierLowReversible})
	withEmbedded(t, func(s string) bool { return s != op })

	l := NewLadder(2, NewMemGraduationStore(), nil)
	recordN(t, l, op, OutcomeVerifiedClean, 2)
	if got := l.LevelOf(ctx, op); got != LevelAutoNotice {
		t.Fatalf("a ratified class must climb the FIRST rung normally — earning the right to act is not what "+
			"the ceiling withholds; got %v", got)
	}

	// Now over-earn the second bar by a wide margin. No amount of good behaviour may buy the silent rung.
	r := recordN(t, l, op, OutcomeVerifiedClean, DefaultNoticeThreshold*3)
	if l.LevelOf(ctx, op) == LevelAuto {
		t.Fatal("an OVERLAY-ONLY class reached the SILENT auto rung — ratification lifted its own ceiling, " +
			"which is the tamper-domain collapse ADR-0016 exists to prevent")
	}
	if !r.CeilingHeld {
		t.Error("the hold must be REPORTED (CeilingHeld) — a class silently stalled at 10/10 forever reads to " +
			"an operator as a broken ladder rather than a policy, and the console cannot offer the embed-export")
	}
	if !strings.Contains(r.Reason, "embed-export") {
		t.Errorf("the reason must name the REMEDY, or the operator learns only that it stopped: %q", r.Reason)
	}
	// The streak is PINNED, not reset — the class has genuinely earned auto and is waiting on a code release.
	if r.NoticeRunCount != DefaultNoticeThreshold {
		t.Errorf("a held class must PIN its streak at the bar (%d), got %d — a reset would show an operator a "+
			"class endlessly re-climbing a ladder it can never finish", DefaultNoticeThreshold, r.NoticeRunCount)
	}

	// The same class, once EMBEDDED by a code release, completes the climb on its very next clean run: the
	// ceiling withheld the rung, it did not destroy the evidence.
	withEmbedded(t, func(string) bool { return true })
	if r2 := recordN(t, l, op, OutcomeVerifiedClean, 1); !r2.Promoted || r2.To != LevelAuto {
		t.Fatalf("after the embed-export code release the earned streak must complete the climb: %+v", r2)
	}
}

// TestFirstClimbLandsAtAutoNoticeAndNeverSkipsIt proves the new rung cannot be jumped (REQ-2807).
//
// RED CONTROL EXECUTED: made the approve→? promotion target LevelAuto again ->
//
//	"the first climb went straight to the SILENT rung, skipping auto_notice — a class would act unobserved
//	 having never once been watched acting"
func TestFirstClimbLandsAtAutoNoticeAndNeverSkipsIt(t *testing.T) {
	ctx := context.Background()
	const op = "restart-service" // embedded, so nothing but the rung order can stop it
	l := NewLadder(3, NewMemGraduationStore(), nil)

	for i := 1; i <= 3; i++ {
		r := recordN(t, l, op, OutcomeVerifiedClean, 1)
		switch {
		case i < 3 && r.To != LevelApprove:
			t.Fatalf("run %d/3: climbed early to %v", i, r.To)
		case i == 3:
			if r.To != LevelAutoNotice {
				t.Fatalf("the first climb went straight to %v, skipping auto_notice — a class would act "+
					"unobserved having never once been watched acting", r.To)
			}
			if !r.Promoted {
				t.Fatal("reaching auto_notice IS a promotion — it is the moment the human vote stops being required")
			}
		}
	}
	// The second climb is a SEPARATE, longer streak that starts from zero.
	if st := l.State(ctx, op); st.NoticeRunCount != 0 || st.CleanRunCount != 0 {
		t.Fatalf("both streaks must be spent on arrival at auto_notice, got clean=%d notice=%d",
			st.CleanRunCount, st.NoticeRunCount)
	}
	if r := recordN(t, l, op, OutcomeVerifiedClean, DefaultNoticeThreshold-1); r.To != LevelAutoNotice {
		t.Fatalf("the second bar must be its own %d-run climb, not a continuation of the first: reached %v after %d",
			DefaultNoticeThreshold, r.To, DefaultNoticeThreshold-1)
	}
}

// TestAutoNoticeActsWithoutAVoteButAutoIsStillDistinct pins what the rung MEANS.
//
// RED CONTROL EXECUTED: made Level.Verdict return VerdictApprove for LevelAutoNotice ->
//
//	"auto_notice routed to a human VOTE — the rung would then be indistinguishable from approve and the
//	 whole earned-autonomy ladder would have gained nothing"
func TestAutoNoticeActsWithoutAVoteButAutoIsStillDistinct(t *testing.T) {
	if got := LevelAutoNotice.Verdict(); got != VerdictAuto {
		t.Fatalf("auto_notice routed to a human VOTE (%v) — the rung would then be indistinguishable from "+
			"approve and the whole earned-autonomy ladder would have gained nothing", got)
	}
	if got := graduatedVerdict(LevelAutoNotice, VerdictAuto); got != VerdictAuto {
		t.Fatalf("graduatedVerdict must honor auto at auto_notice, got %v", got)
	}
	// ...but a DENY is still never lifted, at any rung.
	for _, lv := range []Level{LevelApprove, LevelAutoNotice, LevelAuto} {
		if got := graduatedVerdict(lv, VerdictDeny); got != VerdictDeny {
			t.Errorf("graduation lifted a DENY at %v — graduation may only ever downgrade", lv)
		}
	}
	// The two autonomous rungs must remain DISTINGUISHABLE, or the band floor that carries the notice
	// (REQ-2809) has nothing to key on.
	if LevelAutoNotice == LevelAuto {
		t.Fatal("auto_notice and auto collapsed to one value — the notice band floor has nothing to key on")
	}
	if LevelAutoNotice.String() != "auto_notice" || LevelAuto.String() != "auto" {
		t.Fatalf("the durable spellings must match migration 0050's level CHECK, got %q and %q",
			LevelAutoNotice, LevelAuto)
	}
}

// TestZeroValueAndCorruptLevelsStillFailClosed re-proves the oldest law of this enum survives the insertion.
//
// RED CONTROL EXECUTED: added LevelAutoNotice to the iota block ahead of LevelApprove ->
//
//	"the ZERO VALUE of Level is no longer approve — every un-initialised ClassState in the process would
//	 come up autonomous"
func TestZeroValueAndCorruptLevelsStillFailClosed(t *testing.T) {
	var zero Level
	if zero != LevelApprove {
		t.Fatal("the ZERO VALUE of Level is no longer approve — every un-initialised ClassState in the " +
			"process would come up autonomous")
	}
	if zero.Verdict() != VerdictApprove {
		t.Fatalf("the zero level must permit at most approve, got %v", zero.Verdict())
	}
	for _, corrupt := range []Level{-1, 3, 99} {
		if corrupt.valid() {
			t.Errorf("level %d must be rejected as corrupt", corrupt)
		}
		if corrupt.Verdict() != VerdictApprove || corrupt.String() != "approve" {
			t.Errorf("a corrupt level %d must resolve to approve, got verdict=%v name=%q",
				corrupt, corrupt.Verdict(), corrupt)
		}
	}
	// A corrupt persisted level never loads as autonomous, at EITHER rung.
	l := NewLadder(2, NewMemGraduationStore().Seed(ClassState{OpClass: "restart-service", Level: 42}), nil)
	if got := l.LevelOf(context.Background(), "restart-service"); got != LevelApprove {
		t.Fatalf("a corrupt persisted level loaded as %v — it must fail closed to approve", got)
	}
}

// TestDeviationDropsAllTheWayFromEitherRung proves demotion is to the BOTTOM, not one rung (REQ-2810).
//
// RED CONTROL EXECUTED: made OutcomeDeviated step down one rung (auto->auto_notice) instead of to approve ->
//
//	"a verified DEVIATION left the class still acting without a vote — the evidence that the op does not
//	 work must not leave any earned autonomy standing"
func TestDeviationDropsAllTheWayFromEitherRung(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		climb func(t *testing.T, l *Ladder)
	}{
		{"from auto_notice", func(t *testing.T, l *Ladder) { recordN(t, l, "restart-service", OutcomeVerifiedClean, 2) }},
		{"from auto", func(t *testing.T, l *Ladder) { climbToAuto(t, l, "restart-service", 2) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := NewLadder(2, NewMemGraduationStore(), nil)
			tc.climb(t, l)
			r, err := l.Record(ctx, "restart-service", OutcomeDeviated)
			if err != nil {
				t.Fatalf("Record(deviated): %v", err)
			}
			if r.To != LevelApprove {
				t.Fatalf("a verified DEVIATION left the class at %v — still acting without a vote; the "+
					"evidence that the op does not work must not leave any earned autonomy standing", r.To)
			}
			if !r.Demoted {
				t.Error("a drop from an autonomous rung must be RECORDED as a demotion for the ledger")
			}
			if r.CleanRunCount != 0 || r.NoticeRunCount != 0 {
				t.Errorf("both climbs must reset on a deviation, got clean=%d notice=%d", r.CleanRunCount, r.NoticeRunCount)
			}
		})
	}
}

// TestPerClassThresholdMayOnlyRaiseTheBar mirrors `CHECK (promote_threshold >= 5)` in Go (REQ-2803).
//
// RED CONTROL EXECUTED: honored the resolver's value unconditionally in thresholdForLocked ->
//
//	"a per-class threshold BELOW the compiled default was honored — a ratification could buy a faster climb
//	 than the code's own conservative bar"
func TestPerClassThresholdMayOnlyRaiseTheBar(t *testing.T) {
	ctx := context.Background()
	const op = "restart-service"

	// A hostile/corrupt row asks for a 1-run climb. It must buy nothing.
	low := NewLadder(DefaultPromoteThreshold, NewMemGraduationStore(), nil).
		WithPerClassThreshold(func(string) (int, bool) { return 1, true })
	if r := recordN(t, low, op, OutcomeVerifiedClean, 1); r.Promoted {
		t.Fatal("a per-class threshold BELOW the compiled default was honored — a ratification could buy a " +
			"faster climb than the code's own conservative bar")
	}
	if r := recordN(t, low, op, OutcomeVerifiedClean, DefaultPromoteThreshold-1); !r.Promoted {
		t.Fatalf("the ladder-wide default must still apply when the resolver is ignored: %+v", r)
	}

	// A higher bar IS honored — that is the direction ratification may move it.
	high := NewLadder(DefaultPromoteThreshold, NewMemGraduationStore(), nil).
		WithPerClassThreshold(func(string) (int, bool) { return DefaultPromoteThreshold + 4, true })
	if r := recordN(t, high, op, OutcomeVerifiedClean, DefaultPromoteThreshold); r.Promoted {
		t.Fatalf("a RAISED per-class bar was ignored — a medium-tier class would climb as fast as a "+
			"low-reversible one: %+v", r)
	}
	if r := recordN(t, high, op, OutcomeVerifiedClean, 4); !r.Promoted || r.To != LevelAutoNotice {
		t.Fatalf("the raised bar must eventually be reachable: %+v", r)
	}
	if got := high.LevelOf(ctx, op); got != LevelAutoNotice {
		t.Fatalf("level = %v", got)
	}

	// The tier table REQ-2803 names: low-reversible 5, everything else 10.
	if got := PromoteThresholdForTier(opschema.TierLowReversible); got != DefaultPromoteThreshold {
		t.Errorf("low-reversible bar = %d, want %d", got, DefaultPromoteThreshold)
	}
	if got := PromoteThresholdForTier("medium"); got != DefaultNoticeThreshold {
		t.Errorf("medium bar = %d, want %d", got, DefaultNoticeThreshold)
	}
	// An UNKNOWN tier must land on the HIGHER bar, never the lower — an unreadable declaration is not permission.
	if got := PromoteThresholdForTier("wharrgarbl"); got < DefaultNoticeThreshold {
		t.Errorf("an unknown tier resolved to the fast bar (%d) — an unreadable declaration must never buy speed", got)
	}
}

// TestUnpersistedPromotionIsRefusedAtBothRungs extends ErrPromotionNotPersisted to the new climb.
//
// RED CONTROL EXECUTED: scoped the persist refusal to `res.To == LevelAutoNotice` ->
//
//	"an unpersisted promotion to the SILENT rung took effect in memory — a restart would show the class at
//	 auto_notice while the running process acted unobserved"
func TestUnpersistedPromotionIsRefusedAtBothRungs(t *testing.T) {
	ctx := context.Background()
	const op = "restart-service"
	store := &failOnSaveStore{MemGraduationStore: NewMemGraduationStore()}
	l := NewLadder(2, store, nil)

	// Climb to auto_notice with saves working, then break the store before the SECOND promotion.
	recordN(t, l, op, OutcomeVerifiedClean, 2)
	if l.LevelOf(ctx, op) != LevelAutoNotice {
		t.Fatal("setup: class did not reach auto_notice")
	}
	recordN(t, l, op, OutcomeVerifiedClean, DefaultNoticeThreshold-1)
	store.fail = true

	r, err := l.Record(ctx, op, OutcomeVerifiedClean)
	if err == nil {
		t.Fatal("a promotion whose Save failed must return ErrPromotionNotPersisted")
	}
	if r.To == LevelAuto || l.LevelOf(ctx, op) == LevelAuto {
		t.Fatal("an unpersisted promotion to the SILENT rung took effect in memory — a restart would show " +
			"the class at auto_notice while the running process acted unobserved")
	}
	if !strings.Contains(r.Reason, "auto_notice") {
		t.Errorf("the refusal must name the rung the class STAYS at: %q", r.Reason)
	}
}

// failOnSaveStore is a MemGraduationStore whose Save can be broken mid-test.
type failOnSaveStore struct {
	*MemGraduationStore
	fail bool
}

func (s *failOnSaveStore) Save(ctx context.Context, st ClassState) error {
	if s.fail {
		return context.DeadlineExceeded
	}
	return s.MemGraduationStore.Save(ctx, st)
}
