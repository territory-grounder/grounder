package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/breaker"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/policy"
)

// A REWOUND FIXTURE (TG-674): the stored policy_mode row says Semi-auto (a restored dump), the ledger's last
// governing row says the operator moved to Shadow after it. The row claims MORE authority than the audited
// record ⇒ dangerous ⇒ held. KILLING MUTATION: drop the modeRank(stored) > modeRank(expected) branch and the
// first assertion fails.
func TestRewoundModeRowIsHeldInShadow(t *testing.T) {
	rep := reconcileBootPosture(bootReconcileInputs{
		StoredMode:     policy.ModeSemiAuto,
		ModeEvent:      audit.LedgerEntry{Seq: 22000, Decision: "mode-transition:Semi-auto->Shadow"},
		ModeEventFound: true,
	})
	if !rep.Inconsistent || !rep.Dangerous {
		t.Fatalf("a stored actuating mode over a ledgered transition to Shadow is the dangerous direction: %+v", rep)
	}
	if rep.ExpectedMode != policy.ModeShadow || !strings.Contains(strings.Join(rep.Findings, "\n"), "MORE authority") {
		t.Fatalf("the finding must name the direction and the expected mode: %+v", rep)
	}
}

// The SAFE direction: the row says Shadow, the ledger says Semi-auto. Publish, do not change anything.
func TestShadowRowUnderALedgeredActuatingModeIsPublishedNotHeld(t *testing.T) {
	rep := reconcileBootPosture(bootReconcileInputs{
		StoredMode:     policy.ModeShadow,
		ModeEvent:      audit.LedgerEntry{Seq: 22001, Decision: "mode-transition:Shadow->Semi-auto"},
		ModeEventFound: true,
	})
	if !rep.Inconsistent || rep.Dangerous {
		t.Fatalf("a row claiming LESS authority is inconsistent but never held: %+v", rep)
	}
}

// Kills and trips govern too: a Semi-auto row over a ledgered /halt or breaker trip is dangerous; a Semi-auto row
// whose last governing event IS the transition that set it is consistent; a fresh spine with an actuating row is
// the restore shape and is held.
func TestKillsTripsAndFreshSpines(t *testing.T) {
	for _, c := range []struct {
		name      string
		event     string
		found     bool
		dangerous bool
	}{
		{"halt", "safety:halt", true, true},
		{"trip", "safety:breaker-trip", true, true},
		{"own transition", "mode-transition:Shadow->Semi-auto", true, false},
		{"seed", "mode-seed:Shadow->Semi-auto", true, false},
		{"fresh spine", "", false, true},
	} {
		rep := reconcileBootPosture(bootReconcileInputs{StoredMode: policy.ModeSemiAuto, ModeEvent: audit.LedgerEntry{Seq: 1, Decision: c.event}, ModeEventFound: c.found})
		if rep.Dangerous != c.dangerous {
			t.Errorf("%s: dangerous=%v, want %v (%+v)", c.name, rep.Dangerous, c.dangerous, rep.Findings)
		}
		if !c.dangerous && rep.Inconsistent {
			t.Errorf("%s: a consistent pair must not be flagged: %+v", c.name, rep.Findings)
		}
	}
	// Full-auto over a ledgered Semi-auto is MORE authority too.
	rep := reconcileBootPosture(bootReconcileInputs{StoredMode: policy.ModeFullAuto, ModeEvent: audit.LedgerEntry{Decision: "mode-transition:Shadow->Semi-auto"}, ModeEventFound: true})
	if !rep.Dangerous {
		t.Fatal("Full-auto over a ledgered Semi-auto claims more authority than the record")
	}
}

func TestBreakerRollbackIsHeldAndLostRearmIsPublished(t *testing.T) {
	// The ledger's last breaker event is a trip; the stored breaker is closed (the trip was rolled back).
	rep := reconcileBootPosture(bootReconcileInputs{
		StoredMode: policy.ModeShadow, ModeEvent: audit.LedgerEntry{Decision: "mode-transition:Semi-auto->Shadow"}, ModeEventFound: true,
		BreakerKnown: true, BreakerState: breaker.StateClosed,
		BreakerEvent: audit.LedgerEntry{Seq: 9, Decision: "safety:breaker-trip"}, BreakerEventFound: true,
	})
	if !rep.Dangerous || !strings.Contains(strings.Join(rep.Findings, "\n"), "rolled back") {
		t.Fatalf("a closed breaker over a ledgered trip is a rolled-back kill: %+v", rep)
	}
	// An absent breaker row over a ledgered trip is the same shape.
	rep = reconcileBootPosture(bootReconcileInputs{BreakerKnown: false, BreakerEvent: audit.LedgerEntry{Decision: "safety:breaker-trip"}, BreakerEventFound: true})
	if !rep.Dangerous {
		t.Fatalf("no breaker row over a ledgered trip must hold: %+v", rep)
	}
	// Open breaker after a ledgered rearm: inconsistent, safe direction.
	rep = reconcileBootPosture(bootReconcileInputs{BreakerKnown: true, BreakerState: breaker.StateOpen, BreakerEvent: audit.LedgerEntry{Decision: "safety:breaker-rearm"}, BreakerEventFound: true})
	if !rep.Inconsistent || rep.Dangerous {
		t.Fatalf("an open breaker after a rearm is published, never held: %+v", rep)
	}
	// Trip on record and the breaker IS open: consistent.
	rep = reconcileBootPosture(bootReconcileInputs{BreakerKnown: true, BreakerState: breaker.StateOpen, BreakerEvent: audit.LedgerEntry{Decision: "safety:breaker-trip"}, BreakerEventFound: true})
	if rep.Inconsistent {
		t.Fatalf("an open breaker over a ledgered trip agrees: %+v", rep)
	}
}

// The ladder half is DB against DB and never holds: a rung without its credits is a labelled finding only.
func TestLadderWithoutCreditsIsWeakEvidenceNeverHeld(t *testing.T) {
	rep := reconcileBootPosture(bootReconcileInputs{
		Rungs:     []db.GraduationRung{{OpClass: "start-guest", Level: "auto"}, {OpClass: "start-service", Level: "auto_notice"}, {OpClass: "disk-grow", Level: "approve"}},
		Credits:   map[string]int{"start-guest": 16, "start-service": 2},
		Threshold: 5, NoticeThreshold: 10,
	})
	if rep.Dangerous || rep.Inconsistent {
		t.Fatalf("the ladder half is weak evidence: it must never hold and never raise the ledger-backed inconsistent flag: %+v", rep)
	}
	if len(rep.LadderFindings) != 1 || !strings.Contains(rep.LadderFindings[0], "start-service") || !strings.Contains(rep.LadderFindings[0], "weak evidence") {
		t.Fatalf("exactly the under-credited rung is flagged on its own channel, labelled weak evidence: %+v", rep.LadderFindings)
	}
	// ABSENT IS NOT CLEAR: a rung at auto with NO credit rows at all reads as 0 credits and is flagged.
	rep = reconcileBootPosture(bootReconcileInputs{Rungs: []db.GraduationRung{{OpClass: "restart-service", Level: "auto"}}, Credits: map[string]int{}, Threshold: 5, NoticeThreshold: 10})
	if len(rep.LadderFindings) != 1 || !strings.Contains(rep.LadderFindings[0], "0 verified_clean") {
		t.Fatalf("absent credits must read as 0, never as 'fine': %+v", rep.LadderFindings)
	}
}

func TestConsistentPostureIsSilent(t *testing.T) {
	rep := reconcileBootPosture(bootReconcileInputs{
		StoredMode: policy.ModeSemiAuto, ModeEvent: audit.LedgerEntry{Decision: "mode-transition:Shadow->Semi-auto"}, ModeEventFound: true,
		BreakerKnown: true, BreakerState: breaker.StateClosed, BreakerEvent: audit.LedgerEntry{Decision: "safety:breaker-rearm"}, BreakerEventFound: true,
		Rungs: []db.GraduationRung{{OpClass: "start-guest", Level: "auto"}}, Credits: map[string]int{"start-guest": 16}, Threshold: 5, NoticeThreshold: 10,
	})
	if rep.Inconsistent || rep.Dangerous || len(rep.Findings) != 0 || len(rep.LadderFindings) != 0 {
		t.Fatalf("a consistent posture has nothing to say: %+v", rep)
	}
}

// --- the seam under the wiring: stores are interfaces, the hold is observable ---

type fakeModeStore struct {
	mode policy.Mode
	err  error
}

func (f fakeModeStore) Load(context.Context) (policy.Mode, error) { return f.mode, f.err }

type fakeLedgerTail struct{ byPattern map[string]audit.LedgerEntry }

func (f fakeLedgerTail) LatestDecisionMatching(_ context.Context, patterns []string) (audit.LedgerEntry, bool, error) {
	var best audit.LedgerEntry
	found := false
	for _, p := range patterns {
		if e, ok := f.byPattern[p]; ok && (!found || e.Seq > best.Seq) {
			best, found = e, true
		}
	}
	return best, found, nil
}

type fakeGrad struct{}

func (fakeGrad) Rungs(context.Context) ([]db.GraduationRung, error) { return nil, nil }
func (fakeGrad) VerifiedCleanCredits(context.Context) (map[string]int, error) {
	return map[string]int{}, nil
}

type fakeForcer struct{ reasons []string }

func (f *fakeForcer) ForceShadow(reason string) { f.reasons = append(f.reasons, reason) }

func TestRunBootReconcileHoldsOnlyInTheDangerousDirection(t *testing.T) {
	lg := audit.NewLedger()
	forcer := &fakeForcer{}
	st := bootReconcileStores{
		mode:    fakeModeStore{mode: policy.ModeSemiAuto},
		ledger:  fakeLedgerTail{byPattern: map[string]audit.LedgerEntry{"safety:halt": {Seq: 50, Decision: "safety:halt"}}},
		breaker: breaker.NewMemStore(),
		grad:    fakeGrad{},
	}
	rep := runBootReconcile(context.Background(), st, lg, forcer, 5, 10)
	if !rep.Dangerous || len(forcer.reasons) != 1 {
		t.Fatalf("a Semi-auto row over a ledgered halt must ForceShadow exactly once: rep=%+v forces=%v", rep, forcer.reasons)
	}
	entries := lg.Entries()
	if len(entries) != 1 || entries[0].Decision != "boot:inconsistent-state" || !entries[0].Withheld {
		t.Fatalf("the hold must be ledgered as boot:inconsistent-state with autonomy withheld: %+v", entries)
	}
	if s := bootPosture.Load(); s == nil || !s.inconsistent || !s.dangerous {
		t.Fatalf("the gauges must publish the hold: %+v", s)
	}

	// Safe direction: Shadow row, ledger says Semi-auto — ledgered, published, NOT forced.
	forcer.reasons = nil
	lg = audit.NewLedger()
	st.mode = fakeModeStore{mode: policy.ModeShadow}
	st.ledger = fakeLedgerTail{byPattern: map[string]audit.LedgerEntry{"mode-transition:%": {Seq: 60, Decision: "mode-transition:Shadow->Semi-auto"}}}
	rep = runBootReconcile(context.Background(), st, lg, forcer, 5, 10)
	if rep.Dangerous || len(forcer.reasons) != 0 || !rep.Inconsistent || len(lg.Entries()) != 1 || lg.Entries()[0].Withheld {
		t.Fatalf("the safe direction is recorded and never forced: rep=%+v forces=%v entries=%+v", rep, forcer.reasons, lg.Entries())
	}

	// Consistent: nothing ledgered, nothing forced, gauges read 0.
	lg = audit.NewLedger()
	st.mode = fakeModeStore{mode: policy.ModeSemiAuto}
	rep = runBootReconcile(context.Background(), st, lg, forcer, 5, 10)
	if rep.Inconsistent || len(lg.Entries()) != 0 || len(forcer.reasons) != 0 {
		t.Fatalf("a consistent posture appends and forces nothing: rep=%+v", rep)
	}
	samples := bootPostureSamples()
	if len(samples) != 4 || samples[0].Value != 0 || samples[1].Value != 0 {
		t.Fatalf("the gauges are emitted at 0 on a clean posture: %+v", samples)
	}

	// An absent mode row is Shadow, not an error.
	st.mode = fakeModeStore{mode: policy.ModeShadow, err: fmt.Errorf("db: %w", policy.ErrModeAbsent)}
	st.ledger = fakeLedgerTail{}
	if rep := runBootReconcile(context.Background(), st, lg, forcer, 5, 10); rep.Inconsistent {
		t.Fatalf("an absent row on a fresh spine is consistent Shadow: %+v", rep)
	}
	// An actuating row whose ledger cannot be READ is doubt about authority, and doubt holds; a Shadow row with an
	// unreadable ledger claims nothing and is only published.
	forcer.reasons = nil
	st.mode = fakeModeStore{mode: policy.ModeSemiAuto}
	st.ledger = failingLedgerTail{}
	if rep := runBootReconcile(context.Background(), st, audit.NewLedger(), forcer, 5, 10); !rep.Dangerous || len(forcer.reasons) != 1 {
		t.Fatalf("an actuating row under an unreadable ledger must be held: %+v forces=%v", rep, forcer.reasons)
	}
	forcer.reasons = nil
	st.mode = fakeModeStore{mode: policy.ModeShadow}
	if rep := runBootReconcile(context.Background(), st, audit.NewLedger(), forcer, 5, 10); rep.Dangerous || !rep.Inconsistent || len(forcer.reasons) != 0 {
		t.Fatalf("a Shadow row under an unreadable ledger is published, not held: %+v forces=%v", rep, forcer.reasons)
	}
}

type failingLedgerTail struct{}

func (failingLedgerTail) LatestDecisionMatching(context.Context, []string) (audit.LedgerEntry, bool, error) {
	return audit.LedgerEntry{}, false, errors.New("db down")
}
