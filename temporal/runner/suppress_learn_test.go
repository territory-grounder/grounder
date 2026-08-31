package runner

import (
	"context"
	"testing"
	"time"

	_ "time/tzdata"

	"github.com/territory-grounder/grounder/core/audit"
	coregov "github.com/territory-grounder/grounder/core/governance"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/suppression"
)

// These oracles drive the PRODUCTION object — the same *LiveSuppressGate cmd/worker constructs and assigns
// to Deps.Suppress — through its real Decide path. Nothing about the learned lane is stubbed: the learner,
// the registry, the two-phase verifier, the demotion lookup and the evidence store are the real types.

type countingReopen struct{ n int }

func (r *countingReopen) Reopen(context.Context, string) error { r.n++; return nil }

type countingPager struct{ n int }

func (p *countingPager) Page(context.Context, string, string) error { p.n++; return nil }

// learnRig is the production wiring in miniature: a gate with the learned lane armed, over real governance
// stores.
type learnRig struct {
	gate     *LiveSuppressGate
	reopen   *countingReopen
	pager    *countingPager
	demotion *coregov.MemDemotionStore
	evidence *coregov.MemEvidenceStore
	demoter  *coregov.Demoter
}

func newLearnRig(declared ...suppression.Schedule) *learnRig {
	reopen, pager := &countingReopen{}, &countingPager{}
	demotion := coregov.NewMemDemotionStore()
	evidence := coregov.NewMemEvidenceStore()
	ledger := audit.NewLedger()
	learner := &suppression.Learner{
		Registry: suppression.NewScheduleRegistry(),
		Window:   suppression.WindowEvaluator{PreBuffer: 5 * time.Minute, PostWindow: 10 * time.Minute},
		Verifier: &suppression.TwoPhaseVerifier{Reopen: reopen, Pager: pager},
		Timezone: "UTC",
	}
	return &learnRig{
		gate: &LiveSuppressGate{
			Schedules:       declared,
			RebootPreBuffer: 5 * time.Minute,
			RebootWindow:    10 * time.Minute,
			Learn:           learner,
			Demotions:       coregov.DemotionLookupOf(demotion),
			Evidence:        evidence,
			LearnRenewFor:   90 * 24 * time.Hour,
			Ledger:          ledger,
			Log:             NewRecentTriageLog(time.Minute),
		},
		reopen: reopen, pager: pager, demotion: demotion, evidence: evidence,
		demoter: &coregov.Demoter{Store: demotion, Ledger: ledger},
	}
}

// reboot fires a reboot-class alert with a boot reason through the gate and returns the decision.
func (r *learnRig) reboot(t *testing.T, ref, host, reason string, at time.Time) suppression.Decision {
	t.Helper()
	d, err := r.gate.Decide(context.Background(), suppression.Alert{
		ExternalRef: ref, Host: host, AlertRule: "HostDown", Severity: ingest.SeverityWarning,
		IsReboot: true, BootReason: reason, ObservedAt: at,
	}, at)
	if err != nil {
		t.Fatalf("decide %s: %v", ref, err)
	}
	return d
}

// The head-to-head mechanism, end to end on the live gate: an UNDECLARED but regular nightly reboot is
// escalated the first two nights (observe-before-live) and suppressed on the third — the recall the
// predecessor had and TG did not.
func TestLearnedLaneObserveVerifyPromoteOnTheLiveGate(t *testing.T) {
	r := newLearnRig()
	n1 := time.Date(2026, 7, 12, 3, 0, 0, 0, time.UTC)
	n2, n3 := n1.Add(24*time.Hour), n1.Add(48*time.Hour)

	if r.reboot(t, "TG-1", "web01", "systemd-reboot", n1).Outcome.Suppressing() {
		t.Fatal("the FIRST sighting of an unknown schedule must be investigated — nothing is learned yet")
	}
	if r.reboot(t, "TG-2", "web01", "systemd-reboot", n2).Outcome.Suppressing() {
		t.Fatal("the occurrence that PROMOTES the schedule must still be investigated — promotion happens after the decision")
	}
	d3 := r.reboot(t, "TG-3", "web01", "systemd-reboot", n3.Add(4*time.Minute))
	if !d3.Outcome.Suppressing() || d3.Phase != suppression.PhaseScheduledReboot {
		t.Fatalf("the third on-cadence reboot must be suppressed in phase SR by the LEARNED schedule, got %+v", d3)
	}
	if d3.Signals["schedule_source"] != "learned" {
		t.Fatalf("the suppression must be attributed to the learned lane, got %q", d3.Signals["schedule_source"])
	}
	if r.reopen.n != 0 || r.pager.n != 0 {
		t.Fatal("a clean-boot suppression must neither reopen nor page")
	}
}

// #12 on the live gate: a crash-reboot never becomes a schedule, however regular it is.
func TestCrashRebootNeverLearnedOnTheLiveGate(t *testing.T) {
	r := newLearnRig()
	base := time.Date(2026, 7, 12, 3, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		at := base.Add(time.Duration(i) * 24 * time.Hour)
		if r.reboot(t, "TG-x", "web01", "kernel panic - not syncing", at).Outcome.Suppressing() {
			t.Fatalf("night %d: a crash reboot must always be investigated", i+1)
		}
	}
	if len(r.gate.Learn.Live()) != 0 {
		t.Fatal("four regular CRASH reboots must never produce a live learned schedule")
	}
}

// #5 on the live gate: a learned pattern that suppresses an incident which then needed action is reversed
// in-path, demoted, recorded as evidence, and — after the scheduled demote pass — is blocked by the
// governance demotion even if it were to re-promote. Learning without unlearning is the ratchet this closes.
func TestLearnedPatternDemotesOnEvidenceAndStopsSuppressing(t *testing.T) {
	r := newLearnRig()
	n1 := time.Date(2026, 7, 12, 3, 0, 0, 0, time.UTC)
	n2, n3, n4 := n1.Add(24*time.Hour), n1.Add(48*time.Hour), n1.Add(72*time.Hour)
	r.reboot(t, "TG-1", "web01", "systemd-reboot", n1)
	r.reboot(t, "TG-2", "web01", "systemd-reboot", n2)
	if len(r.gate.Learn.Live()) != 1 {
		t.Fatal("setup: the schedule must be live after two verified occurrences")
	}

	// The next "on-schedule" reboot is actually an OOM crash that happened to land in the window.
	d := r.reboot(t, "TG-3", "web01", "oom-kill invoked", n3.Add(2*time.Minute))
	if d.Outcome.Suppressing() {
		t.Fatalf("a suppression whose boot was NOT clean must be REVERSED to escalation, got %+v", d)
	}
	if r.reopen.n != 1 || r.pager.n != 1 {
		t.Fatalf("the two-phase verify must reopen the incident and page the approver graph (REQ-406), got reopen=%d page=%d", r.reopen.n, r.pager.n)
	}
	if len(r.gate.Learn.Live()) != 0 {
		t.Fatal("the misfiring learned row must be demoted out of LIVE immediately")
	}

	// The scheduled demote pass turns the recorded evidence into a durable analysis-only row.
	n, err := r.gate.DemotePass(context.Background(), r.demoter, 24*time.Hour, n3.Add(time.Hour))
	if err != nil {
		t.Fatalf("demote pass: %v", err)
	}
	if n != 1 {
		t.Fatalf("the demote pass must write exactly one demotion row from the evidence, got %d", n)
	}

	// Even if the pattern re-earns promotion, the demotion blocks it: two more clean occurrences promote the
	// row again, and the very next on-schedule reboot is STILL investigated.
	r.reboot(t, "TG-4", "web01", "systemd-reboot", n4)
	r.reboot(t, "TG-5", "web01", "systemd-reboot", n4.Add(24*time.Hour))
	d6 := r.reboot(t, "TG-6", "web01", "systemd-reboot", n4.Add(48*time.Hour).Add(3*time.Minute))
	if d6.Outcome.Suppressing() {
		t.Fatalf("a governance-demoted tuple must not be suppressed by a re-promoted learned pattern, got %+v", d6)
	}
}

// The declared lane is untouched by all of this: an operator-declared schedule suppresses on the FIRST
// reboot (no observe-before-live), regardless of boot reason, and regardless of a demotion on the tuple.
func TestDeclaredScheduleIsUnaffectedByTheLearnedLane(t *testing.T) {
	at := time.Date(2026, 7, 12, 3, 2, 0, 0, time.UTC)
	declared := suppression.Schedule{
		Host: "db01", Kind: "declared", Cron: "0 3 * * *", Timezone: "UTC",
		Source: suppression.SourceDeclared, Status: suppression.SchLive,
		ValidFrom: at.Add(-24 * time.Hour), ValidUntil: at.Add(365 * 24 * time.Hour),
	}
	r := newLearnRig(declared)
	d := r.reboot(t, "TG-1", "db01", "systemd-reboot", at)
	if !d.Outcome.Suppressing() || d.Signals["schedule_source"] != "declared" {
		t.Fatalf("a declared schedule must suppress on the first reboot, got %+v", d)
	}
	if r.reopen.n != 0 || r.pager.n != 0 {
		t.Fatal("the learned lane's verify must not run for a DECLARED suppression")
	}

	// A live demotion on the tuple does not touch the declared lane.
	if err := r.demotion.Write(context.Background(), coregov.DemotionRow{
		Tuple: coregov.Tuple{Host: "db01", AlertRule: "HostDown"}, Reason: coregov.LearnedSuppressionReason,
		ValidFrom: at.Add(-time.Hour), ValidUntil: at.Add(720 * time.Hour),
	}); err != nil {
		t.Fatalf("write demotion: %v", err)
	}
	if d2 := r.reboot(t, "TG-2", "db01", "systemd-reboot", at.Add(24*time.Hour)); !d2.Outcome.Suppressing() {
		t.Fatalf("an operator-DECLARED schedule keeps its behavior under a demotion, got %+v", d2)
	}
}

// A shifted schedule does not inherit the previous schedule's promotion ON THE LIVE PATH: after the shift,
// the first reboot at the new time is investigated, not suppressed.
func TestShiftedScheduleDoesNotInheritPromotionOnTheLiveGate(t *testing.T) {
	r := newLearnRig()
	n1 := time.Date(2026, 7, 12, 3, 0, 0, 0, time.UTC)
	r.reboot(t, "TG-1", "web01", "systemd-reboot", n1)
	r.reboot(t, "TG-2", "web01", "systemd-reboot", n1.Add(24*time.Hour))
	if len(r.gate.Learn.Live()) != 1 {
		t.Fatal("setup: the 03:00 schedule must be live")
	}
	// the reboot moves to 05:00
	shifted := n1.Add(48*time.Hour + 2*time.Hour)
	if r.reboot(t, "TG-3", "web01", "systemd-reboot", shifted).Outcome.Suppressing() {
		t.Fatal("the first reboot at a SHIFTED time must be investigated — it inherits no promotion")
	}
	if r.reboot(t, "TG-4", "web01", "systemd-reboot", shifted.Add(24*time.Hour)).Outcome.Suppressing() {
		t.Fatal("the shifted schedule's SECOND sighting is what promotes it — it must still be investigated")
	}
	d := r.reboot(t, "TG-5", "web01", "systemd-reboot", shifted.Add(48*time.Hour))
	if !d.Outcome.Suppressing() {
		t.Fatalf("the shifted schedule suppresses only after earning its OWN two occurrences, got %+v", d)
	}
}

// The promotion THRESHOLD is a second, independent guard from the cadence requirement, and this is the
// case where it is the only one left: a jittery nightly reboot whose derived window contains just ONE of
// the two sightings registers a pattern with a single in-window boot. It must stay observing — one boot
// may never promote — and only the next in-window occurrence makes it live.
func TestLearnedPatternWithOneInWindowBootStaysObserving(t *testing.T) {
	r := newLearnRig()
	n1 := time.Date(2026, 7, 12, 3, 0, 0, 0, time.UTC)
	// 03:00 then 03:12 the next night: a daily cadence within tolerance, but the signature anchors on 03:12,
	// so the 03:00 boot falls OUTSIDE the derived [03:07, 03:22] window and only one boot counts.
	r.reboot(t, "TG-1", "web01", "systemd-reboot", n1)
	r.reboot(t, "TG-2", "web01", "systemd-reboot", n1.Add(24*time.Hour+12*time.Minute))
	if n := len(r.gate.Learn.Live()); n != 0 {
		t.Fatalf("a pattern with ONE in-window boot must not be live, got %d live", n)
	}
	if d := r.reboot(t, "TG-3", "web01", "systemd-reboot", n1.Add(48*time.Hour+10*time.Minute)); d.Outcome.Suppressing() {
		t.Fatalf("one in-window boot must never promote — the third reboot must be investigated, got %+v", d)
	}
	if d := r.reboot(t, "TG-4", "web01", "systemd-reboot", n1.Add(72*time.Hour+14*time.Minute)); !d.Outcome.Suppressing() {
		t.Fatalf("after a SECOND in-window occurrence the pattern must suppress, got %+v", d)
	}
}

// Two distinct schedules on ONE host coexist, because the registry identity carries the signature (#10).
// Keyed on (host, kind) alone the second pattern would displace the first — and the first would silently
// stop suppressing while carrying the second's promotion.
func TestTwoLearnedSchedulesOnOneHostCoexist(t *testing.T) {
	r := newLearnRig()
	n1 := time.Date(2026, 7, 12, 3, 0, 0, 0, time.UTC)
	r.reboot(t, "TG-1", "web01", "systemd-reboot", n1)
	r.reboot(t, "TG-2", "web01", "systemd-reboot", n1.Add(24*time.Hour))

	// A SECOND, unrelated nightly reboot appears at 21:00 and earns its own promotion.
	e1 := n1.Add(48*time.Hour + 18*time.Hour)
	r.reboot(t, "TG-3", "web01", "systemd-reboot", e1)
	r.reboot(t, "TG-4", "web01", "systemd-reboot", e1.Add(24*time.Hour))
	if n := len(r.gate.Learn.Live()); n != 2 {
		t.Fatalf("two distinct schedules on one host must be two live rows, got %d", n)
	}
	// BOTH still suppress: the 03:00 one was not displaced by the 21:00 one.
	if d := r.reboot(t, "TG-5", "web01", "systemd-reboot", n1.Add(96*time.Hour)); !d.Outcome.Suppressing() {
		t.Fatalf("the original 03:00 schedule must still suppress, got %+v", d)
	}
	if d := r.reboot(t, "TG-6", "web01", "systemd-reboot", e1.Add(48*time.Hour)); !d.Outcome.Suppressing() {
		t.Fatalf("the second 21:00 schedule must suppress too, got %+v", d)
	}
}

// The learned lane is DARK unless armed: with no learner wired, a perfectly regular reboot is never
// suppressed and nothing is learned (the default posture).
func TestLearnedLaneIsDarkByDefault(t *testing.T) {
	gate := &LiveSuppressGate{
		RebootPreBuffer: 5 * time.Minute, RebootWindow: 10 * time.Minute,
		Ledger: audit.NewLedger(), Log: NewRecentTriageLog(time.Minute),
	}
	base := time.Date(2026, 7, 12, 3, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		at := base.Add(time.Duration(i) * 24 * time.Hour)
		d, err := gate.Decide(context.Background(), suppression.Alert{
			ExternalRef: "TG-x", Host: "web01", AlertRule: "HostDown", Severity: ingest.SeverityWarning,
			IsReboot: true, BootReason: "systemd-reboot", ObservedAt: at,
		}, at)
		if err != nil {
			t.Fatalf("decide: %v", err)
		}
		if d.Outcome.Suppressing() {
			t.Fatalf("night %d: with the learned lane unarmed nothing may be suppressed", i+1)
		}
	}
}

// A critical-severity reboot is never suppressed by a learned schedule (the severity floor runs before
// every phase, including the learned lane).
func TestLearnedLaneNeverSuppressesCritical(t *testing.T) {
	r := newLearnRig()
	n1 := time.Date(2026, 7, 12, 3, 0, 0, 0, time.UTC)
	r.reboot(t, "TG-1", "web01", "systemd-reboot", n1)
	r.reboot(t, "TG-2", "web01", "systemd-reboot", n1.Add(24*time.Hour))
	d, err := r.gate.Decide(context.Background(), suppression.Alert{
		ExternalRef: "TG-3", Host: "web01", AlertRule: "HostDown", Severity: ingest.SeverityCritical,
		IsReboot: true, BootReason: "systemd-reboot", ObservedAt: n1.Add(48 * time.Hour),
	}, n1.Add(48*time.Hour))
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if d.Outcome.Suppressing() {
		t.Fatalf("a critical reboot must never be suppressed by a learned schedule, got %+v", d)
	}
}
