package main

// BOOT RECONCILIATION (TG-674, deep-review F16). A worker that boots trusts three durable rows for its authority:
// policy_mode (may it actuate at all), mutation_breaker_state (did a deviation trip turn mutation off) and
// policy_graduation (which op-classes act without a vote). Every one of them is REWRITABLE state — a database
// restore from last night's dump, a reverted migration, a hand UPDATE — and until this file nothing compared
// them with the one record that is not: the hash-chained governance ledger, which carries every mode
// transition, every kill and every breaker trip as an appended row. A restore that rewound policy_mode from
// Shadow (after a kill) to Semi-auto (from the dump) therefore came back up ACTUATING with the ledger saying
// it should not — silently, with the console reading green.
//
// THE RULE (owner-ruled 2026-11-07, lane E coordinator): compare at boot, BEFORE publishing authority, and hold
// authority ONLY in the dangerous direction. Dangerous = the stored row claims MORE authority than the ledger's
// last governing event (an actuating mode over a ledgered kill, transition-to-Shadow or breaker trip; a CLOSED
// breaker over a ledgered trip with no rearm after it): then ledger `boot:inconsistent-state`, publish
// `tg_boot_posture_inconsistent`, and ForceShadow through the controller — durable, so every sibling process
// follows (TG-570) and an operator transition is the only way back. Safe direction = the row claims LESS (a
// Shadow row under a ledgered Semi-auto transition; an OPEN breaker after a ledgered rearm): publish, ledger and
// page, change nothing — a restore that lost autonomy costs heals, not safety. The ladder is reconciled DB
// against DB (policy_graduation rungs against the exactly-once graduation_credit rows) because ladder
// transitions are not ledgered yet (lane B's work); that half is labelled weak evidence and never holds.
//
// KNOWN SAFE-DIRECTION NOISE: the recon-burst kill (core/safety/recon_budget.go) and the cost breaker call
// ForceShadow WITHOUT a ledger row, so a worker that restarts after one of them reads a stored Shadow under a
// ledgered Semi-auto — inconsistent in the LESS direction. That is published and ledgered as a warning, never
// held; it is a fact about those kills' audit trail, not about this check.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/breaker"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/metrics"
	"github.com/territory-grounder/grounder/core/policy"
)

// bootReconcileInputs is everything the pure reconciliation reads — gathered by wireBootReconcile from the
// stores, faked in tests.
//
// ABSENT IS NEVER CLEAR (owner's second prompt, 2026-11-07, §2.F.6). Each read states what an absent subject
// means, and none of them resolves to "fine":
//   - policy_mode row absent ⇒ Shadow (the controller's own fail-closed reading), compared like any value;
//   - no mode-governing ledger row ⇒ the ledger expects Shadow, so an actuating stored row is DANGEROUS;
//   - mutation_breaker_state row absent (BreakerKnown=false) over a ledgered trip ⇒ DANGEROUS (a missing row
//     is a rolled-back kill, not a closed breaker);
//   - no breaker ledger row ⇒ nothing to compare, the stored breaker stands (its own contract reads a store
//     ERROR as OPEN);
//   - policy_graduation rows absent ⇒ no rungs to check (an absent rung is approve, the least authority);
//   - graduation_credit absent for a rung at auto/auto_notice ⇒ 0 credits ⇒ a ladder finding.
type bootReconcileInputs struct {
	// StoredMode is the policy_mode row (Shadow when absent); StoredModeErr is a read error (treated as unknown).
	StoredMode    policy.Mode
	StoredModeErr error
	// LedgerErr is a failure to READ the ledger's governing rows. Under an ACTUATING stored row that is doubt
	// about authority, and doubt holds (TG-652's posture): the row cannot be verified, so it is not trusted.
	LedgerErr error
	// ModeEvent is the ledger's latest authority-governing row (mode-transition, mode-seed, safety:halt,
	// safety:breaker-trip), if any.
	ModeEvent      audit.LedgerEntry
	ModeEventFound bool
	// BreakerState is the mutation breaker's stored state; BreakerKnown false when no row exists yet.
	BreakerState breaker.State
	BreakerKnown bool
	BreakerErr   error
	// BreakerEvent is the ledger's latest breaker row (safety:breaker-trip / safety:breaker-rearm), if any.
	BreakerEvent      audit.LedgerEntry
	BreakerEventFound bool
	// Rungs and Credits are the stored ladder and the exactly-once credits behind it (DB vs DB).
	Rungs           []db.GraduationRung
	Credits         map[string]int
	Threshold       int
	NoticeThreshold int
}

// bootPostureReport is the reconciliation's verdict.
type bootPostureReport struct {
	// Inconsistent: a LEDGER-backed row (mode, breaker) disagrees with the ledger's last governing event.
	Inconsistent bool
	// Dangerous: the disagreement is in the direction of MORE authority than the ledger records — held.
	Dangerous bool
	Findings  []string
	// LadderFindings are the DB-against-DB rung checks (policy_graduation vs graduation_credit) — weak
	// evidence by construction until ladder transitions are ledgered (lane B), so they are their own channel:
	// published and warned on, never folded into Inconsistent and never held.
	LadderFindings []string
	ExpectedMode   policy.Mode
}

// modeEventPatterns are the ledger decisions that GOVERN the active mode, newest wins.
var modeEventPatterns = []string{"mode-transition:%", "mode-seed:%", "safety:halt", "safety:breaker-trip"}

// breakerEventPatterns are the ledger decisions that govern the mutation breaker, newest wins.
var breakerEventPatterns = []string{"safety:breaker-trip", "safety:breaker-rearm"}

// modeRank orders the four modes by authority (Shadow < HITL < Semi-auto < Full-auto).
func modeRank(m policy.Mode) int {
	switch m {
	case policy.ModeHITL:
		return 1
	case policy.ModeSemiAuto:
		return 2
	case policy.ModeFullAuto:
		return 3
	}
	return 0
}

// expectedModeFrom reads the mode the ledger's latest governing row says should be active. A kill or a trip
// says Shadow; a transition or seed says its target; nothing on record says Shadow (a fresh deployment).
func expectedModeFrom(e audit.LedgerEntry, found bool) (policy.Mode, string) {
	if !found {
		return policy.ModeShadow, "no mode-governing row on the ledger (fresh spine ⇒ Shadow)"
	}
	d := e.Decision
	switch {
	case d == "safety:halt":
		return policy.ModeShadow, fmt.Sprintf("ledger seq %d: operator /halt forced Shadow", e.Seq)
	case d == "safety:breaker-trip":
		return policy.ModeShadow, fmt.Sprintf("ledger seq %d: the mutation breaker tripped (forced Shadow)", e.Seq)
	case strings.HasPrefix(d, "mode-transition:") || strings.HasPrefix(d, "mode-seed:"):
		arrow := strings.LastIndex(d, "->")
		if arrow < 0 {
			return policy.ModeShadow, fmt.Sprintf("ledger seq %d: unparseable mode row %q ⇒ Shadow", e.Seq, d)
		}
		m, err := policy.ParseMode(strings.TrimSpace(d[arrow+2:]))
		if err != nil {
			return policy.ModeShadow, fmt.Sprintf("ledger seq %d: unparseable mode target in %q ⇒ Shadow", e.Seq, d)
		}
		return m, fmt.Sprintf("ledger seq %d: %s", e.Seq, d)
	}
	return policy.ModeShadow, fmt.Sprintf("ledger seq %d: unrecognised governing row %q ⇒ Shadow", e.Seq, d)
}

// reconcileBootPosture is the pure comparison. It never touches a store.
func reconcileBootPosture(in bootReconcileInputs) bootPostureReport {
	var r bootPostureReport
	expected, why := expectedModeFrom(in.ModeEvent, in.ModeEventFound)
	r.ExpectedMode = expected
	switch {
	case in.LedgerErr != nil && modeRank(in.StoredMode) > modeRank(policy.ModeShadow):
		r.Inconsistent, r.Dangerous = true, true
		r.Findings = append(r.Findings, fmt.Sprintf("mode: the stored row says %s but the ledger could not be read (%v) — an actuating row that cannot be verified against the record is held in Shadow (doubt holds)", in.StoredMode, in.LedgerErr))
	case in.LedgerErr != nil:
		r.Inconsistent = true
		r.Findings = append(r.Findings, fmt.Sprintf("mode: the ledger could not be read (%v); the stored row is %s, which claims nothing to hold", in.LedgerErr, in.StoredMode))
	case in.StoredModeErr != nil:
		r.Inconsistent = true
		r.Findings = append(r.Findings, fmt.Sprintf("mode: the policy_mode row is unreadable (%v); the controller fails closed to Shadow on its own", in.StoredModeErr))
	case modeRank(in.StoredMode) > modeRank(expected):
		r.Inconsistent, r.Dangerous = true, true
		r.Findings = append(r.Findings, fmt.Sprintf("mode: the stored row says %s but the ledger says %s (%s) — the row claims MORE authority than the audited record; holding in Shadow", in.StoredMode, expected, why))
	case modeRank(in.StoredMode) < modeRank(expected):
		r.Inconsistent = true
		r.Findings = append(r.Findings, fmt.Sprintf("mode: the stored row says %s but the ledger says %s (%s) — the row claims LESS authority than the audited record; left as is (safe direction), an operator transition restores it", in.StoredMode, expected, why))
	}

	switch {
	case in.BreakerErr != nil:
		r.Inconsistent = true
		r.Findings = append(r.Findings, fmt.Sprintf("breaker: mutation_breaker_state is unreadable (%v); the breaker reads OPEN on a store error by its own contract", in.BreakerErr))
	case in.BreakerEventFound && in.BreakerEvent.Decision == "safety:breaker-trip" && (!in.BreakerKnown || in.BreakerState != breaker.StateOpen):
		r.Inconsistent, r.Dangerous = true, true
		r.Findings = append(r.Findings, fmt.Sprintf("breaker: the ledger's last breaker event is a TRIP (seq %d) with no rearm after it, but the stored breaker is not open (known=%v state=%v) — the trip was rolled back; holding in Shadow", in.BreakerEvent.Seq, in.BreakerKnown, in.BreakerState))
	case in.BreakerEventFound && in.BreakerEvent.Decision == "safety:breaker-rearm" && in.BreakerKnown && in.BreakerState == breaker.StateOpen:
		r.Inconsistent = true
		r.Findings = append(r.Findings, fmt.Sprintf("breaker: the ledger's last breaker event is a REARM (seq %d) but the stored breaker is OPEN — the rearm was lost; left as is (safe direction)", in.BreakerEvent.Seq))
	}

	rungs := append([]db.GraduationRung(nil), in.Rungs...)
	sort.Slice(rungs, func(i, j int) bool { return rungs[i].OpClass < rungs[j].OpClass })
	for _, g := range rungs {
		need := 0
		switch g.Level {
		case "auto_notice":
			need = in.Threshold
		case "auto":
			need = in.Threshold + in.NoticeThreshold
		default:
			continue
		}
		if need <= 0 {
			continue
		}
		if have := in.Credits[g.OpClass]; have < need {
			r.LadderFindings = append(r.LadderFindings, fmt.Sprintf("ladder (DB vs DB, weak evidence): %s sits at %s on %d verified_clean credit(s), fewer than the %d the rung requires — a rung without its credits; not held (ladder transitions are not ledgered yet)", g.OpClass, g.Level, have, need))
		}
	}
	return r
}

// bootPostureState is what the metrics reader publishes: computed once at boot, never re-read.
type bootPostureState struct {
	measured       bool
	inconsistent   bool
	dangerous      bool
	findings       int
	ladderFindings int
}

var bootPosture atomic.Pointer[bootPostureState]

// bootPostureSamples publishes the boot reconciliation's verdict — ALWAYS, including the all-clear, so an
// absent series is the reconciliation never having run, never a clean posture.
func bootPostureSamples() []metrics.Sample {
	st := bootPosture.Load()
	if st == nil {
		return nil
	}
	b2f := func(b bool) float64 {
		if b {
			return 1
		}
		return 0
	}
	return []metrics.Sample{
		{Name: "tg_boot_posture_inconsistent", Kind: metrics.Gauge,
			Help: "1 when the boot reconciliation (TG-674) found the stored policy_mode / mutation_breaker_state / policy_graduation rows " +
				"disagreeing with the governance ledger's last governing rows; 0 when they agreed. Emitted on every scrape once the " +
				"worker has booted, so absence means the check never ran.",
			Value: b2f(st.inconsistent)},
		{Name: "tg_boot_posture_held", Kind: metrics.Gauge,
			Help: "1 when the boot reconciliation forced this process to Shadow because a stored row claimed MORE authority than the " +
				"ledger (the dangerous direction); an operator mode transition is the only release.",
			Value: b2f(st.dangerous)},
		{Name: "tg_boot_posture_findings", Kind: metrics.Gauge,
			Help:  "how many ledger-backed disagreements (mode, breaker) the boot reconciliation recorded.",
			Value: float64(st.findings)},
		{Name: "tg_boot_posture_ladder_findings", Kind: metrics.Gauge,
			Help: "how many stored ladder rungs (policy_graduation) sit above what their exactly-once verified_clean credits support — a " +
				"database-against-database check, weak evidence until ladder transitions are ledgered; warned on, never held.",
			Value: float64(st.ladderFindings)},
	}
}

// bootReconcileStores is the store surface wireBootReconcile reads.
type bootReconcileStores struct {
	mode interface {
		Load(context.Context) (policy.Mode, error)
	}
	ledger interface {
		LatestDecisionMatching(context.Context, []string) (audit.LedgerEntry, bool, error)
	}
	breaker breaker.Store
	grad    interface {
		Rungs(context.Context) ([]db.GraduationRung, error)
		VerifiedCleanCredits(context.Context) (map[string]int, error)
	}
}

// shadowForcer is the slice of the mode controller the reconciliation needs to hold authority.
type shadowForcer interface{ ForceShadow(reason string) }

// ledgerAppender appends the boot:inconsistent-state row.
type ledgerAppender interface {
	Append(audit.GovDecision) (audit.LedgerEntry, error)
}

// runBootReconcile gathers, compares, records and (in the dangerous direction only) holds. It is the
// testable seam under wireBootReconcile: every dependency is an interface.
func runBootReconcile(ctx context.Context, st bootReconcileStores, lg ledgerAppender, forcer shadowForcer, threshold, noticeThreshold int) bootPostureReport {
	in := bootReconcileInputs{Threshold: threshold, NoticeThreshold: noticeThreshold, Credits: map[string]int{}}
	in.StoredMode, in.StoredModeErr = st.mode.Load(ctx)
	if errors.Is(in.StoredModeErr, policy.ErrModeAbsent) {
		in.StoredMode, in.StoredModeErr = policy.ModeShadow, nil
	}
	var err error
	if in.ModeEvent, in.ModeEventFound, err = st.ledger.LatestDecisionMatching(ctx, modeEventPatterns); err != nil {
		in.LedgerErr = fmt.Errorf("ledger read failed, mode cannot be reconciled: %w", err)
	}
	if st.breaker != nil {
		rec, ok, berr := st.breaker.Load(ctx, "mutation")
		in.BreakerErr = berr
		in.BreakerKnown = ok
		in.BreakerState = rec.State
	}
	if in.BreakerEvent, in.BreakerEventFound, err = st.ledger.LatestDecisionMatching(ctx, breakerEventPatterns); err != nil && in.BreakerErr == nil {
		in.BreakerErr = fmt.Errorf("ledger read failed, breaker cannot be reconciled: %w", err)
	}
	if st.grad != nil {
		if rungs, gerr := st.grad.Rungs(ctx); gerr == nil {
			in.Rungs = rungs
			if credits, cerr := st.grad.VerifiedCleanCredits(ctx); cerr == nil {
				in.Credits = credits
			} else {
				log.Printf("boot reconcile: graduation credits unreadable (%v) — the ladder half is skipped this boot", cerr)
				in.Rungs = nil
			}
		} else {
			log.Printf("boot reconcile: policy_graduation unreadable (%v) — the ladder half is skipped this boot", gerr)
		}
	}
	rep := reconcileBootPosture(in)
	bootPosture.Store(&bootPostureState{measured: true, inconsistent: rep.Inconsistent, dangerous: rep.Dangerous, findings: len(rep.Findings), ladderFindings: len(rep.LadderFindings)})
	for _, f := range rep.LadderFindings {
		log.Printf("boot reconcile: LADDER (weak evidence, not held) — %s", f)
	}
	if !rep.Inconsistent {
		log.Printf("boot reconcile: stored posture agrees with the governance ledger (mode %s expected %s; %d rung(s) checked, %d without their credits) — authority published as stored (TG-674)", in.StoredMode, rep.ExpectedMode, len(in.Rungs), len(rep.LadderFindings))
		return rep
	}
	for _, f := range rep.Findings {
		log.Printf("boot reconcile: INCONSISTENT — %s", f)
	}
	// The HOLD comes before the audit row (the same order the /halt handler and the breaker use): a stalled
	// ledger must never delay turning authority off.
	if rep.Dangerous && forcer != nil {
		forcer.ForceShadow("boot reconciliation (TG-674): a stored authority row claims more than the governance ledger records — holding in Shadow until an operator transition")
		log.Print("boot reconcile: HELD IN SHADOW — a stored row claimed more authority than the ledger; POST /v1/mode by an authorised operator is the only release (TG-674)")
	}
	if lg != nil {
		if _, aerr := lg.Append(audit.GovDecision{
			Decision: "boot:inconsistent-state",
			Reason:   fmt.Sprintf("dangerous=%v; %s", rep.Dangerous, strings.Join(rep.Findings, " | ")),
			ActionID: fmt.Sprintf("boot-reconcile-%d", time.Now().UTC().UnixNano()),
			Withheld: rep.Dangerous,
		}); aerr != nil {
			log.Printf("boot reconcile: could not append boot:inconsistent-state to the ledger: %v", aerr)
		}
	}
	return rep
}

// wireBootReconcile runs the reconciliation once at boot against the live stores and returns the metrics
// reader. Nothing without a pool: an in-memory worker has no stored posture to reconcile.
func wireBootReconcile(ctx context.Context, pool *db.Pool, lg *audit.Ledger, ctl *policy.ModeController, ladder *policy.Ladder) func() []metrics.Sample {
	if pool == nil {
		log.Print("boot reconcile: no DB pool — nothing to reconcile (the mode fails closed to Shadow in memory)")
		return bootPostureSamples
	}
	threshold, notice := 0, 0
	if ladder != nil {
		threshold, notice = ladder.Threshold(), ladder.NoticeThreshold()
	}
	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var la ledgerAppender
	if lg != nil {
		la = lg
	}
	var forcer shadowForcer
	if ctl != nil {
		forcer = ctl
	}
	runBootReconcile(rctx, bootReconcileStores{
		mode:    db.NewPolicyModeStore(pool),
		ledger:  db.NewLedgerStore(pool),
		breaker: db.NewBreakerStore(pool),
		grad:    db.NewPolicyGraduationStore(pool),
	}, la, forcer, threshold, notice)
	return bootPostureSamples
}
