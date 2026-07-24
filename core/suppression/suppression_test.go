package suppression

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	_ "time/tzdata" // guarantee LoadLocation works regardless of the host tz database

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/ingest"
)

// a Sunday, for the weekly cron window "0 3 * * 0".
var sunday3am = time.Date(2026, 7, 12, 3, 0, 0, 0, time.UTC)

func ctx() context.Context { return context.Background() }

// panicStage always panics — used to prove a phase failure fails OPEN.
type panicStage struct{}

func (panicStage) Name() Phase { return PhaseKnownPattern }
func (panicStage) Evaluate(context.Context, Alert, time.Time) (Decision, error) {
	panic("boom")
}

func TestSeverityFloorAlwaysEscalates(t *testing.T) {
	// a live schedule that WOULD suppress, but the severity floor short-circuits first.
	sched := Schedule{Host: "h", Kind: "reboot", Cron: "0 3 * * 0", Timezone: "UTC", Status: SchLive, ValidUntil: sunday3am.Add(time.Hour)}
	c := &Chain{Stages: []Stage{&ScheduledStage{Schedules: []Schedule{sched}, Window: WindowEvaluator{PreBuffer: 30 * time.Minute, PostWindow: 30 * time.Minute}}}}
	for _, sev := range []ingest.Severity{ingest.SeverityCritical, ingest.SeverityUnknown} {
		a := Alert{ExternalRef: "TG-1", Host: "h", IsReboot: true, Severity: sev, ObservedAt: sunday3am}
		d, _ := c.Decide(ctx(), a, sunday3am)
		if d.Outcome != OutcomeEscalate || d.Phase != PhaseSeverity {
			t.Fatalf("severity %v must escalate at the floor, got %+v", sev, d)
		}
	}
}

func TestPhasePanicFailsOpen(t *testing.T) {
	c := &Chain{Stages: []Stage{panicStage{}}}
	a := Alert{ExternalRef: "TG-1", Severity: ingest.SeverityWarning}
	d, err := c.Decide(ctx(), a, sunday3am)
	if err != nil || d.Outcome != OutcomeEscalate {
		t.Fatalf("a panicking phase must fail open to escalate, got %+v err=%v", d, err)
	}
}

func TestChainAppendsOneLedgerRecord(t *testing.T) {
	l := audit.NewLedger()
	c := &Chain{Ledger: l, Stages: []Stage{&KnownPatternStage{Patterns: []TransientPattern{{AlertRule: "R"}}}}}
	if _, err := c.Decide(ctx(), Alert{ExternalRef: "TG-1", AlertRule: "R", Severity: ingest.SeverityWarning}, sunday3am); err != nil {
		t.Fatal(err)
	}
	if l.Len() != 1 || l.Verify() != nil {
		t.Fatalf("each chain run appends exactly one verifiable ledger record, len=%d", l.Len())
	}
}

func TestDedupBoundary(t *testing.T) {
	s := &DedupStage{Window: time.Hour}
	now := sunday3am
	// future-dated
	if _, err := s.AcceptEntry(TriageEntry{LoggedAt: now.Add(time.Minute)}, now); !errors.Is(err, ErrMalformedEntry) {
		t.Fatalf("future-dated entry must be rejected at the boundary, got %v", err)
	}
	// within window
	if ok, err := s.AcceptEntry(TriageEntry{LoggedAt: now.Add(-10 * time.Minute)}, now); !ok || err != nil {
		t.Fatalf("an entry within the window must be a candidate, got ok=%v err=%v", ok, err)
	}
	// outside window
	if ok, err := s.AcceptEntry(TriageEntry{LoggedAt: now.Add(-2 * time.Hour)}, now); ok || err != nil {
		t.Fatalf("an entry outside the window is not a candidate, got ok=%v err=%v", ok, err)
	}
}

func TestBlastRadiusValidity(t *testing.T) {
	now := sunday3am
	a := Alert{ExternalRef: "TG-1", Host: "web01", AlertRule: "R", Severity: ingest.SeverityWarning}
	valid := SuppressionPolicy{HostScope: "*", RuleScope: "R", ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour), LastVerifiedAt: now.Add(-time.Minute)}
	if d, _ := (&BlastRadiusStage{Policies: []SuppressionPolicy{valid}, Freshness: time.Hour}).Evaluate(ctx(), a, now); d.Outcome != OutcomeNotice {
		t.Fatalf("a currently-valid policy must fold to a notice, got %+v", d)
	}
	expired := valid
	expired.ValidUntil = now.Add(-time.Minute)
	if d, _ := (&BlastRadiusStage{Policies: []SuppressionPolicy{expired}, Freshness: time.Hour}).Evaluate(ctx(), a, now); d.Outcome != OutcomeEscalate {
		t.Fatalf("an expired policy must fail open, got %+v", d)
	}
	stale := valid
	stale.LastVerifiedAt = now.Add(-48 * time.Hour)
	if d, _ := (&BlastRadiusStage{Policies: []SuppressionPolicy{stale}, Freshness: time.Hour}).Evaluate(ctx(), a, now); d.Outcome != OutcomeEscalate {
		t.Fatalf("a stale-verified policy must fail open, got %+v", d)
	}
}

func liveSched() Schedule {
	return Schedule{Host: "h", Kind: "reboot", Cron: "0 3 * * 0", Timezone: "UTC", Status: SchLive, ValidUntil: sunday3am.Add(24 * time.Hour)}
}

func TestScheduledStage(t *testing.T) {
	w := WindowEvaluator{PreBuffer: 30 * time.Minute, PostWindow: 30 * time.Minute}
	inWindow := Alert{ExternalRef: "TG-1", Host: "h", IsReboot: true, Severity: ingest.SeverityWarning, ObservedAt: sunday3am.Add(10 * time.Minute)}

	// live + in-window ⇒ suppressed
	if d, _ := (&ScheduledStage{Schedules: []Schedule{liveSched()}, Window: w}).Evaluate(ctx(), inWindow, sunday3am); d.Outcome != OutcomeSuppressed {
		t.Fatalf("live + in-window reboot must suppress, got %+v", d)
	}
	// observing ⇒ escalate
	obs := liveSched()
	obs.Status = SchObserving
	if d, _ := (&ScheduledStage{Schedules: []Schedule{obs}, Window: w}).Evaluate(ctx(), inWindow, sunday3am); d.Outcome != OutcomeEscalate {
		t.Fatalf("observing schedule must not suppress, got %+v", d)
	}
	// killed ⇒ escalate
	killed := liveSched()
	killed.KillSwitch = true
	if d, _ := (&ScheduledStage{Schedules: []Schedule{killed}, Window: w}).Evaluate(ctx(), inWindow, sunday3am); d.Outcome != OutcomeEscalate {
		t.Fatalf("kill-switched schedule must not suppress, got %+v", d)
	}
	// outside window ⇒ escalate
	outside := Alert{ExternalRef: "TG-1", Host: "h", IsReboot: true, Severity: ingest.SeverityWarning, ObservedAt: sunday3am.Add(3 * time.Hour)}
	if d, _ := (&ScheduledStage{Schedules: []Schedule{liveSched()}, Window: w}).Evaluate(ctx(), outside, sunday3am); d.Outcome != OutcomeEscalate {
		t.Fatalf("out-of-window reboot must escalate, got %+v", d)
	}
	// not a reboot ⇒ escalate
	notReboot := inWindow
	notReboot.IsReboot = false
	if d, _ := (&ScheduledStage{Schedules: []Schedule{liveSched()}, Window: w}).Evaluate(ctx(), notReboot, sunday3am); d.Outcome != OutcomeEscalate {
		t.Fatalf("non-reboot must escalate, got %+v", d)
	}
	// NOT-YET-VALID (valid_from in the future) ⇒ escalate — regression for the adversarial-review finding
	// that Suppresses only checked valid_until. A temporally-bounded row applies only while now >= valid_from.
	notYet := liveSched()
	notYet.ValidFrom = sunday3am.Add(time.Hour) // becomes valid an hour AFTER the alert
	if d, _ := (&ScheduledStage{Schedules: []Schedule{notYet}, Window: w}).Evaluate(ctx(), inWindow, sunday3am); d.Outcome != OutcomeEscalate {
		t.Fatalf("a not-yet-valid (future valid_from) schedule must not suppress, got %+v", d)
	}
}

// The scheduled-reboot window is ASYMMETRIC [fire−5m, fire+10m]: a boot AFTER the fire (detection + reboot
// lag) is suppressed out to +10m, but a boot BEFORE the fire is only tolerated to −5m. A symmetric ±tolerance
// cannot express this — the exact port defect (a fire+8m boot escalated, or a widened ±10m wrongly suppressed
// a fire−8m boot).
func TestCronWindowAsymmetric(t *testing.T) {
	w := WindowEvaluator{PreBuffer: 5 * time.Minute, PostWindow: 10 * time.Minute}
	sc := Schedule{Cron: "0 7 * * *", Timezone: "UTC"}
	fire := time.Date(2026, 7, 16, 7, 0, 0, 0, time.UTC)
	// 8m AFTER the fire → inside the +10m post-window (the confirmed scenario a symmetric ±5m escalated).
	if !w.Contains(sc, fire.Add(8*time.Minute)) {
		t.Fatal("a boot 8m after the fire must be in-window (suppressed)")
	}
	// 8m BEFORE the fire → outside the −5m pre-buffer.
	if w.Contains(sc, fire.Add(-8*time.Minute)) {
		t.Fatal("a boot 8m before the fire must be out-of-window (escalated)")
	}
	// boundaries are inclusive on both sides and exclusive just beyond.
	if !w.Contains(sc, fire.Add(10*time.Minute)) || w.Contains(sc, fire.Add(11*time.Minute)) {
		t.Fatal("post-window boundary must be +10m inclusive")
	}
	if !w.Contains(sc, fire.Add(-5*time.Minute)) || w.Contains(sc, fire.Add(-6*time.Minute)) {
		t.Fatal("pre-buffer boundary must be −5m inclusive")
	}
}

func TestCronWindowDSTAware(t *testing.T) {
	// a New York 3am window; an alert at 3:10 New York local time is in-window regardless of the UTC offset.
	w := WindowEvaluator{PreBuffer: 30 * time.Minute, PostWindow: 30 * time.Minute}
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("tzdata unavailable")
	}
	sc := Schedule{Cron: "0 3 * * 0", Timezone: "America/New_York"}
	localSun := time.Date(2026, 7, 12, 3, 10, 0, 0, loc)
	if !w.Contains(sc, localSun) {
		t.Fatalf("a 3:10 local reboot must be in the 3am window")
	}
	if w.Contains(sc, time.Date(2026, 7, 12, 6, 0, 0, 0, loc)) {
		t.Fatalf("a 6am local reboot must be out of window")
	}
}

func TestKnownPattern(t *testing.T) {
	warn := func(rule string) Alert {
		return Alert{ExternalRef: "TG-1", Host: "anyhost", AlertRule: rule, Severity: ingest.SeverityWarning}
	}
	// a confident, transient, current pattern suppresses host-agnostically ("FlappyLink" carries the keyword).
	s := &KnownPatternStage{Patterns: []TransientPattern{{AlertRule: "FlappyLink", Estate: "dc1", Confidence: 0.9}}}
	if d, _ := s.Evaluate(ctx(), warn("FlappyLink"), sunday3am); d.Outcome != OutcomeSuppressed {
		t.Fatalf("a confident, transient, current pattern must suppress, got %+v", d)
	}
	// a different rule does not match
	if d, _ := s.Evaluate(ctx(), warn("Other"), sunday3am); d.Outcome != OutcomeEscalate {
		t.Fatalf("a different rule must not match, got %+v", d)
	}
	// gate 1 — below the confidence floor fails open
	low := &KnownPatternStage{Patterns: []TransientPattern{{AlertRule: "FlappyLink", Confidence: 0.5}}}
	if d, _ := low.Evaluate(ctx(), warn("FlappyLink"), sunday3am); d.Outcome != OutcomeEscalate {
		t.Fatalf("a below-floor confidence must fail open, got %+v", d)
	}
	// gate 2 — a standing (non-transient) fault is never auto-suppressed even at high confidence
	standing := &KnownPatternStage{Patterns: []TransientPattern{{AlertRule: "DiskFull", Confidence: 0.99}}}
	if d, _ := standing.Evaluate(ctx(), warn("DiskFull"), sunday3am); d.Outcome != OutcomeEscalate {
		t.Fatalf("a non-transient rule must never be auto-suppressed, got %+v", d)
	}
	// gate 3 — a stale learned pattern fails open
	stale := &KnownPatternStage{Patterns: []TransientPattern{{AlertRule: "FlappyLink", Confidence: 0.9, LastSeen: sunday3am.Add(-8 * 24 * time.Hour)}}}
	if d, _ := stale.Evaluate(ctx(), warn("FlappyLink"), sunday3am); d.Outcome != OutcomeEscalate {
		t.Fatalf("a stale learned pattern must fail open, got %+v", d)
	}
}

type fakeReopen struct{ reopened []string }

func (f *fakeReopen) Reopen(_ context.Context, ref string) error {
	f.reopened = append(f.reopened, ref)
	return nil
}

type fakePager struct{ paged []string }

func (f *fakePager) Page(_ context.Context, ref, tier string) error {
	f.paged = append(f.paged, ref+"@"+tier)
	return nil
}

func TestTwoPhaseVerify(t *testing.T) {
	ro, pg := &fakeReopen{}, &fakePager{}
	v := &TwoPhaseVerifier{Reopen: ro, Pager: pg}
	// dirty boot ⇒ reopen + page
	if r, _ := v.Verify(ctx(), "TG-1", "kernel-panic"); !r.Reopened || len(ro.reopened) != 1 || len(pg.paged) != 1 {
		t.Fatalf("a dirty boot must reopen and page, got %+v", r)
	}
	// clean boot ⇒ confirmed
	if r, _ := v.Verify(ctx(), "TG-2", CleanBootReason); !r.Confirmed || r.Reopened {
		t.Fatalf("a clean systemd-reboot must confirm, got %+v", r)
	}
}

func TestPromoteObserveBeforeLive(t *testing.T) {
	w := WindowEvaluator{PreBuffer: 30 * time.Minute, PostWindow: 30 * time.Minute}
	reg := NewScheduleRegistry()
	reg.RegisterObserving(Schedule{Host: "h", Kind: "reboot", Cron: "0 3 * * 0", Timezone: "UTC", ValidUntil: sunday3am.Add(30 * 24 * time.Hour)})

	twoBoots := []Boot{{At: sunday3am.Add(5 * time.Minute)}, {At: sunday3am.Add(7*24*time.Hour + 3*time.Minute)}} // two Sundays 3am-ish
	if st := reg.Promote("h", "reboot", w, twoBoots, true, sunday3am.Add(8*24*time.Hour)); st != SchLive {
		t.Fatalf("two in-window boots must promote to live, got %v", st)
	}

	// single in-window boot stays observing
	reg2 := NewScheduleRegistry()
	reg2.RegisterObserving(Schedule{Host: "h", Kind: "reboot", Cron: "0 3 * * 0", Timezone: "UTC", ValidUntil: sunday3am.Add(30 * 24 * time.Hour)})
	if st := reg2.Promote("h", "reboot", w, []Boot{{At: sunday3am.Add(5 * time.Minute)}}, true, sunday3am.Add(8*24*time.Hour)); st != SchObserving {
		t.Fatalf("a single boot must stay observing, got %v", st)
	}

	// cron gone ⇒ disabled (drift)
	reg3 := NewScheduleRegistry()
	reg3.RegisterObserving(Schedule{Host: "h", Kind: "reboot", Cron: "0 3 * * 0", Timezone: "UTC", ValidUntil: sunday3am.Add(30 * 24 * time.Hour), Status: SchLive})
	if st := reg3.Promote("h", "reboot", w, nil, false, sunday3am); st != SchDisabled {
		t.Fatalf("a removed cron must drift to disabled, got %v", st)
	}
}

// A weekly re-discovery of an already-live schedule must NOT demote it back to observing (P1-10).
func TestReDiscoveryPreservesPromotion(t *testing.T) {
	reg := NewScheduleRegistry()
	reg.RegisterObserving(Schedule{Host: "h", Kind: "reboot", Cron: "0 3 * * 0", Timezone: "UTC", ValidUntil: sunday3am.Add(30 * 24 * time.Hour)})
	s, _ := reg.Get("h", "reboot")
	s.Status = SchLive
	s.ObservedCount = 10
	// re-scan with a refreshed window
	reg.RegisterObserving(Schedule{Host: "h", Kind: "reboot", Cron: "0 3 * * 0", Timezone: "UTC", ValidUntil: sunday3am.Add(60 * 24 * time.Hour)})
	s2, _ := reg.Get("h", "reboot")
	if s2.Status != SchLive || s2.ObservedCount != 10 {
		t.Fatalf("re-discovery must preserve the live promotion state, got status=%v count=%d", s2.Status, s2.ObservedCount)
	}
	if !s2.ValidUntil.After(sunday3am.Add(59 * 24 * time.Hour)) {
		t.Fatal("re-discovery must refresh the descriptive fields (ValidUntil)")
	}
	// a genuinely NEW schedule still starts observing (observe-before-live intact)
	reg.RegisterObserving(Schedule{Host: "h2", Kind: "reboot", Cron: "0 4 * * 0", Timezone: "UTC"})
	n, _ := reg.Get("h2", "reboot")
	if n.Status != SchObserving || n.ObservedCount != 0 {
		t.Fatalf("a new schedule must start observing, got %v", n.Status)
	}
}

// The two-phase verify confirms any genuinely clean boot marker and reopens reactive/unknown boots (P1-12).
func TestBootClassification(t *testing.T) {
	for _, clean := range []string{"systemd-reboot", "reached target reboot.target", "syncing filesystems"} {
		if !IsCleanBoot(clean) {
			t.Errorf("%q must be a clean boot", clean)
		}
	}
	for _, reactive := range []string{"oom-kill invoked", "kernel panic - not syncing", "watchdog: BUG", "self-heal restart", "thermal shutdown"} {
		if IsCleanBoot(reactive) {
			t.Errorf("%q must NOT be a clean boot", reactive)
		}
		if !IsReactiveBoot(reactive) {
			t.Errorf("%q must be a reactive boot", reactive)
		}
	}
	if IsCleanBoot("some-unrecognized-boot-reason") {
		t.Error("an unknown boot reason must not be treated as clean")
	}
	// a reason that mentions a clean marker but ALSO a reactive one is reactive (never confirms)
	if IsCleanBoot("reached target reboot.target after oom-kill") {
		t.Error("a boot with any reactive signal must never be clean")
	}
}

// Promotion accumulates DISTINCT in-window boots across runs, dedups a boot seen twice, and never promotes
// on a single boot even if it appears in overlapping lookbacks (P1-11).
func TestPromoteAccumulatesAndDedups(t *testing.T) {
	reg := NewScheduleRegistry()
	reg.RegisterObserving(Schedule{Host: "h", Kind: "reboot", Cron: "0 3 * * 0", Timezone: "UTC", ValidUntil: sunday3am.Add(90 * 24 * time.Hour)})
	w := WindowEvaluator{PreBuffer: 30 * time.Minute, PostWindow: 30 * time.Minute}
	boot1 := sunday3am.Add(5 * time.Minute)
	// run 1: the SAME boot appears twice (overlapping lookbacks) — must count as ONE, stay observing.
	if st := reg.Promote("h", "reboot", w, []Boot{{At: boot1}, {At: boot1}}, true, sunday3am.Add(time.Hour)); st != SchObserving {
		t.Fatalf("one distinct boot (seen twice) must not promote, got %v", st)
	}
	s, _ := reg.Get("h", "reboot")
	if s.ObservedCount != 1 {
		t.Fatalf("a repeated boot must dedup to count 1, got %d", s.ObservedCount)
	}
	// run 2 (a week later): a NEW distinct boot accumulates → reaches the threshold → live.
	boot2 := sunday3am.Add(7*24*time.Hour + 3*time.Minute)
	if st := reg.Promote("h", "reboot", w, []Boot{{At: boot2}}, true, sunday3am.Add(8*24*time.Hour)); st != SchLive {
		t.Fatalf("a second distinct boot must accumulate to live, got %v", st)
	}
	s2, _ := reg.Get("h", "reboot")
	if s2.ObservedCount != 2 {
		t.Fatalf("distinct boots must accumulate across runs, got %d", s2.ObservedCount)
	}
}

// A declared, in-scope, active freeze suppresses the expected alert even at critical severity; an
// out-of-scope or inactive window does not (P0-6).
func TestFreezeGate(t *testing.T) {
	now := sunday3am
	fz := &FreezeGate{Windows: []FreezeWindow{{Scope: "h", Start: now.Add(-time.Hour), End: now.Add(time.Hour), Reason: "planned reboot"}}}
	ch := &Chain{Freeze: fz}
	// expected critical HostDown on the frozen host → suppressed (the operator declared the reboot)
	crit := Alert{ExternalRef: "TG-1", Host: "h", AlertRule: "HostDown", Severity: ingest.SeverityCritical}
	if d, _ := ch.Decide(ctx(), crit, now); d.Outcome != OutcomeSuppressed {
		t.Fatalf("an in-scope active freeze must suppress even a critical alert, got %+v", d)
	}
	// a critical on a DIFFERENT host is not in scope → still escalates (freeze is narrow)
	other := Alert{ExternalRef: "TG-2", Host: "other", AlertRule: "HostDown", Severity: ingest.SeverityCritical}
	if d, _ := ch.Decide(ctx(), other, now); d.Outcome == OutcomeSuppressed {
		t.Fatalf("an out-of-scope critical must NOT be frozen, got %+v", d)
	}
	// an EXPIRED window suppresses nothing
	expired := &Chain{Freeze: &FreezeGate{Windows: []FreezeWindow{{Scope: "h", Start: now.Add(-3 * time.Hour), End: now.Add(-2 * time.Hour)}}}}
	if d, _ := expired.Decide(ctx(), Alert{ExternalRef: "TG-3", Host: "h", AlertRule: "R", Severity: ingest.SeverityWarning}, now); d.Outcome == OutcomeSuppressed {
		t.Fatalf("an expired freeze window must suppress nothing, got %+v", d)
	}
}

// Dedup only collapses a re-fire against a still-OPEN prior incident: a suppressed prior is not an anchor,
// and a re-fire after the prior CLOSED is a new incident that escalates (P1-9).
func TestDedupOpenIssueSemantics(t *testing.T) {
	now := sunday3am
	inWin := now.Add(-10 * time.Minute)
	alert := Alert{ExternalRef: "TG-1", Host: "h", AlertRule: "R", Severity: ingest.SeverityWarning}

	// a bare escalated prior in-window still dedups (back-compat)
	base := &DedupStage{Recent: []TriageEntry{{Host: "h", AlertRule: "R", LoggedAt: inWin}}, Window: time.Hour}
	if d, _ := base.Evaluate(ctx(), alert, now); d.Outcome != OutcomeSuppressed {
		t.Fatalf("a bare escalated prior in-window must dedup, got %+v", d)
	}
	// a SUPPRESSED prior is not an anchor → escalate
	sup := &DedupStage{Recent: []TriageEntry{{Host: "h", AlertRule: "R", LoggedAt: inWin, Suppressed: true}}, Window: time.Hour}
	if d, _ := sup.Evaluate(ctx(), alert, now); d.Outcome == OutcomeSuppressed {
		t.Fatalf("a suppressed prior must not be a dedup anchor, got %+v", d)
	}
	// prior escalated into an issue that is now CLOSED → a genuine re-fire, escalate
	closed := &DedupStage{
		Recent:    []TriageEntry{{Host: "h", AlertRule: "R", LoggedAt: inWin, IssueRef: "TG-9"}},
		Window:    time.Hour,
		OpenIssue: func(string) bool { return false },
	}
	if d, _ := closed.Evaluate(ctx(), alert, now); d.Outcome == OutcomeSuppressed {
		t.Fatalf("a re-fire after the prior incident closed must escalate, got %+v", d)
	}
	// prior escalated into a STILL-OPEN issue → dedup
	open := &DedupStage{
		Recent:    []TriageEntry{{Host: "h", AlertRule: "R", LoggedAt: inWin, IssueRef: "TG-9"}},
		Window:    time.Hour,
		OpenIssue: func(string) bool { return true },
	}
	if d, _ := open.Evaluate(ctx(), alert, now); d.Outcome != OutcomeSuppressed {
		t.Fatalf("a re-fire while the prior incident is open must dedup, got %+v", d)
	}
}

// A cross-midnight cron window matches a just-after-midnight boot against the previous day's late fire (P2-19).
func TestCronWindowCrossMidnight(t *testing.T) {
	w := WindowEvaluator{PreBuffer: 30 * time.Minute, PostWindow: 30 * time.Minute}
	// a daily 23:59 reboot; DOW "*" so any day. A boot at 00:03 the next day must match the 23:59 fire.
	sc := Schedule{Host: "h", Kind: "reboot", Cron: "59 23 * * *", Timezone: "UTC"}
	boot := time.Date(2026, 7, 16, 0, 3, 0, 0, time.UTC) // 00:03, four minutes after the 23:59 fire
	if !w.Contains(sc, boot) {
		t.Fatal("a 00:03 boot must match a 23:59 cross-midnight cron")
	}
	// a Sunday 23:59 reboot must match a Monday 00:03 boot (fire's DOW is Sunday)
	scDow := Schedule{Host: "h", Kind: "reboot", Cron: "59 23 * * 0", Timezone: "UTC"} // Sunday
	mon := time.Date(2026, 7, 20, 0, 3, 0, 0, time.UTC) // Monday 00:03 (2026-07-19 is Sunday)
	if !w.Contains(scDow, mon) {
		t.Fatal("a Monday-00:03 boot must match a Sunday-23:59 cron")
	}
	// far from any fire must NOT match
	if w.Contains(sc, time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)) {
		t.Fatal("a midday boot must not match a 23:59 cron")
	}
}

// The active-memory stage suppresses only on an explicit operator glob rule, never a critical/unknown alert,
// and a malformed glob fails open (P2-25).
func TestActiveMemoryOperatorRule(t *testing.T) {
	stage := &ActiveMemoryStage{Rules: []SuppressRule{
		{HostPattern: "dc1ap*", RulePattern: "Device Down*", Reason: "AP fleet firmware rollout"},
		{HostPattern: "*", RulePattern: "SyntheticProbeFlap", Reason: "known synthetic probe"},
	}}
	warn := ingest.SeverityWarning
	// a matching host+rule glob suppresses with the operator reason
	d, _ := stage.Evaluate(context.Background(), Alert{ExternalRef: "TG-1", Host: "dc1ap07", AlertRule: "Device Down (SNMP)", Severity: warn}, time.Time{})
	if d.Outcome != OutcomeSuppressed || d.Phase != PhaseActiveMemory {
		t.Fatalf("a matching operator rule must suppress in active-memory, got %+v", d)
	}
	if !contains(d.Reason, "AP fleet firmware rollout") {
		t.Fatalf("the operator reason must be recorded, got %q", d.Reason)
	}
	// host matches but rule does not → fail open
	if d, _ := stage.Evaluate(context.Background(), Alert{ExternalRef: "TG-2", Host: "dc1ap07", AlertRule: "HighCPU", Severity: warn}, time.Time{}); d.Outcome != OutcomeEscalate {
		t.Fatalf("a non-matching rule must fail open, got %+v", d)
	}
	// a wildcard host rule matches any host
	if d, _ := stage.Evaluate(context.Background(), Alert{ExternalRef: "TG-3", Host: "anything", AlertRule: "SyntheticProbeFlap", Severity: warn}, time.Time{}); d.Outcome != OutcomeSuppressed {
		t.Fatalf("a wildcard-host operator rule must match any host, got %+v", d)
	}
	// a CRITICAL alert is never suppressed even when a rule matches
	if d, _ := stage.Evaluate(context.Background(), Alert{ExternalRef: "TG-4", Host: "dc1ap07", AlertRule: "Device Down (SNMP)", Severity: ingest.SeverityCritical}, time.Time{}); d.Outcome != OutcomeEscalate {
		t.Fatalf("a critical alert must never be suppressed by an operator rule, got %+v", d)
	}
	// a malformed glob matches nothing (fails open), it does not silence everything
	bad := &ActiveMemoryStage{Rules: []SuppressRule{{HostPattern: "[unterminated", RulePattern: "*", Reason: "oops"}}}
	if d, _ := bad.Evaluate(context.Background(), Alert{ExternalRef: "TG-5", Host: "web01", AlertRule: "X", Severity: warn}, time.Time{}); d.Outcome != OutcomeEscalate {
		t.Fatalf("a malformed glob must fail open, got %+v", d)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// The cron parser handles the real crontab grammar: weekday ranges/lists, minute/hour steps, and
// day-of-month (with cron's DOM-or-DOW semantics) — not just single-value M H * * DOW (P2-19).
func TestCronRichGrammar(t *testing.T) {
	w := WindowEvaluator{PreBuffer: 5 * time.Minute, PostWindow: 5 * time.Minute}
	match := func(cron string, y int, mo time.Month, d, h, min int) bool {
		return w.Contains(Schedule{Cron: cron, Timezone: "UTC"}, time.Date(y, mo, d, h, min, 0, 0, time.UTC))
	}
	// weekday RANGE 1-5 (Mon–Fri) at 03:00: 2026-07-16 is a Thursday → match; Saturday 2026-07-18 → no.
	if !match("0 3 * * 1-5", 2026, 7, 16, 3, 0) {
		t.Error("weekday-range cron must match a Thursday 03:00")
	}
	if match("0 3 * * 1-5", 2026, 7, 18, 3, 0) {
		t.Error("weekday-range cron must NOT match a Saturday")
	}
	// weekday LIST 1,3,5 (Mon/Wed/Fri): Wednesday 2026-07-15 → match; Thursday → no.
	if !match("0 3 * * 1,3,5", 2026, 7, 15, 3, 0) {
		t.Error("weekday-list cron must match a Wednesday")
	}
	if match("0 3 * * 1,3,5", 2026, 7, 16, 3, 0) {
		t.Error("weekday-list cron must NOT match a Thursday")
	}
	// hour STEP 2-6/2 (02,04,06): 04:00 → match; 05:00 → no.
	if !match("0 2-6/2 * * *", 2026, 7, 16, 4, 0) {
		t.Error("hour-step cron must match 04:00")
	}
	if match("0 2-6/2 * * *", 2026, 7, 16, 5, 0) {
		t.Error("hour-step cron must NOT match 05:00")
	}
	// day-of-MONTH 1 at 03:00 (monthly reboot): the 1st → match; the 2nd → no.
	if !match("0 3 1 * *", 2026, 8, 1, 3, 0) {
		t.Error("day-of-month cron must match the 1st")
	}
	if match("0 3 1 * *", 2026, 8, 2, 3, 0) {
		t.Error("day-of-month cron must NOT match the 2nd")
	}
	// step minute */30 at 12:30 → match (0 and 30 fire); 12:15 → no.
	if !match("*/30 12 * * *", 2026, 7, 16, 12, 30) {
		t.Error("minute-step cron must match 12:30")
	}
	if match("*/30 12 * * *", 2026, 7, 16, 12, 15) {
		t.Error("minute-step cron must NOT match 12:15")
	}
	// Sunday-as-7 folds to 0: `0 3 * * 7` matches Sunday 2026-07-19.
	if !match("0 3 * * 7", 2026, 7, 19, 3, 0) {
		t.Error("dow=7 must match Sunday")
	}
	// malformed fields fail the parse → no match (fail open).
	for _, bad := range []string{"0 3 * *", "60 3 * * *", "0 24 * * *", "0 3 * * 8", "0 3 5-1 * *", "x 3 * * *"} {
		if w.Contains(Schedule{Cron: bad, Timezone: "UTC"}, time.Date(2026, 7, 16, 3, 0, 0, 0, time.UTC)) {
			t.Errorf("malformed cron %q must not match", bad)
		}
	}
}

// ADVERSARIAL (INV-22, concurrent inputs): the schedule registry is shared across the discovery/promotion
// scheduled activities — concurrent RegisterObserving/Get/Live/Promote must be race-free.
func TestScheduleRegistryConcurrent(t *testing.T) {
	r := NewScheduleRegistry()
	w := WindowEvaluator{PreBuffer: 30 * time.Minute, PostWindow: 30 * time.Minute}
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			h := "h" + string(rune('a'+n%26))
			r.RegisterObserving(Schedule{Host: h, Kind: "reboot", Cron: "0 3 * * *", ValidFrom: time.Time{}, ValidUntil: time.Time{}})
			r.Promote(h, "reboot", w, nil, true, time.Now())
			_, _ = r.Get(h, "reboot")
			_ = r.Live()
		}(i)
	}
	wg.Wait()
}
