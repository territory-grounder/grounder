package suppression

import (
	"context"
	"testing"
	"time"

	_ "time/tzdata"

	"github.com/territory-grounder/grounder/core/ingest"
)

// learnWindow is the production-shaped asymmetric window: [fire−5m, fire+10m].
var learnWindow = WindowEvaluator{PreBuffer: 5 * time.Minute, PostWindow: 10 * time.Minute}

func newLearner() *Learner {
	return &Learner{Registry: NewScheduleRegistry(), Window: learnWindow, Timezone: "UTC"}
}

// A recurring nightly reboot is LEARNED the way the predecessor learns one: the first sighting registers
// nothing to suppress with, the second promotes the row to live, and only then does phase SR suppress.
// Crucially the FIRST occurrence never suppresses (observe-before-live) — REQ-409/410.
func TestLearnedRebootObserveThenPromote(t *testing.T) {
	l := newLearner()
	night1 := time.Date(2026, 7, 12, 3, 0, 0, 0, time.UTC)
	night2 := night1.Add(24 * time.Hour)

	o1 := l.Observe(context.Background(), RebootObservation{Host: "web01", BootReason: "systemd-reboot", At: night1}, night1)
	if o1.Registered {
		t.Fatalf("a single sighting is not a cadence — nothing may be registered yet: %+v", o1)
	}
	if len(l.Live()) != 0 {
		t.Fatal("no learned schedule may be live after ONE occurrence")
	}

	o2 := l.Observe(context.Background(), RebootObservation{Host: "web01", BootReason: "systemd-reboot", At: night2}, night2)
	if !o2.Registered || o2.Status != SchLive || !o2.Promoted {
		t.Fatalf("two verified in-window occurrences must promote to live, got %+v", o2)
	}
	if o2.Key.Cron != "0 3 * * *" {
		t.Fatalf("a nightly cadence must render as a daily cron, got %q", o2.Key.Cron)
	}
	live := l.Live()
	if len(live) != 1 || live[0].Source != SourceLearned {
		t.Fatalf("exactly one LEARNED schedule must be live, got %+v", live)
	}
}

// The learned lane never generalizes an irregular gap into a schedule: two reboots two days apart would
// render as a daily cron that claims fires on days nothing was ever observed.
func TestLearnedRebootRejectsIrregularCadence(t *testing.T) {
	l := newLearner()
	at := time.Date(2026, 7, 12, 3, 0, 0, 0, time.UTC)
	l.Observe(context.Background(), RebootObservation{Host: "web01", BootReason: "systemd-reboot", At: at}, at)
	two := at.Add(48 * time.Hour)
	o := l.Observe(context.Background(), RebootObservation{Host: "web01", BootReason: "systemd-reboot", At: two}, two)
	if o.Registered {
		t.Fatalf("a 2-day gap is not a daily or weekly cadence — nothing may be registered: %+v", o)
	}
}

// #12: a REACTIVE boot (OOM / panic / watchdog) is a symptom, never a schedule. It must never be recorded
// as evidence, so it cannot register a row NOR contribute to a later promotion.
func TestCrashRebootNeverRegisters(t *testing.T) {
	l := newLearner()
	night1 := time.Date(2026, 7, 12, 3, 0, 0, 0, time.UTC)
	night2 := night1.Add(24 * time.Hour)

	if o := l.Observe(context.Background(), RebootObservation{Host: "web01", BootReason: "oom-kill invoked", At: night1}, night1); o.Confirmed || o.Registered {
		t.Fatalf("an OOM reboot must never be confirmed or registered, got %+v", o)
	}
	// A clean boot the next night must NOT be able to pair with the crash to form a cadence.
	if o := l.Observe(context.Background(), RebootObservation{Host: "web01", BootReason: "systemd-reboot", At: night2}, night2); o.Registered {
		t.Fatalf("a crash reboot must not count as the prior occurrence of a cadence, got %+v", o)
	}
	if len(l.Live()) != 0 {
		t.Fatal("a crash reboot must never produce a live learned schedule")
	}
	// An UNKNOWN reason fails the same way (fail-safe: unknown is not clean).
	if o := l.Observe(context.Background(), RebootObservation{Host: "web02", BootReason: "", At: night1}, night1); o.Confirmed {
		t.Fatalf("an unknown boot reason must not confirm, got %+v", o)
	}
}

// #10: a SHIFTED schedule is a different identity, so it re-enters OBSERVING instead of inheriting the
// previous schedule's LIVE promotion. This is the hazard assembling the chain would otherwise revive.
func TestShiftedScheduleDoesNotInheritPromotion(t *testing.T) {
	l := newLearner()
	n1 := time.Date(2026, 7, 12, 3, 0, 0, 0, time.UTC)
	n2 := n1.Add(24 * time.Hour)
	l.Observe(context.Background(), RebootObservation{Host: "web01", BootReason: "systemd-reboot", At: n1}, n1)
	o := l.Observe(context.Background(), RebootObservation{Host: "web01", BootReason: "systemd-reboot", At: n2}, n2)
	if o.Status != SchLive {
		t.Fatalf("setup: the 03:00 schedule must be live, got %+v", o)
	}

	// The operator moves the reboot to 05:00. The first sighting at the NEW time is a single sighting of an
	// unknown schedule: it must not be live, and it must not inherit the 03:00 row's promotion.
	shifted := n2.Add(24*time.Hour + 2*time.Hour)
	s1 := l.Observe(context.Background(), RebootObservation{Host: "web01", BootReason: "systemd-reboot", At: shifted}, shifted)
	if s1.Status == SchLive {
		t.Fatalf("a shifted schedule must not be live on its first sighting, got %+v", s1)
	}
	for _, sc := range l.Live() {
		if sc.Cron != o.Key.Cron {
			t.Fatalf("only the previously-earned 03:00 schedule may be live after ONE sighting at the new time, found %q live", sc.Cron)
		}
	}
	s2 := l.Observe(context.Background(), RebootObservation{Host: "web01", BootReason: "systemd-reboot", At: shifted.Add(24 * time.Hour)}, shifted.Add(24*time.Hour))
	if s2.Key.Cron != "0 5 * * *" {
		t.Fatalf("the shifted schedule must carry its OWN signature, got %q", s2.Key.Cron)
	}
	if s2.Key.Cron == o.Key.Cron {
		t.Fatal("the shifted schedule must not share the previous schedule's identity")
	}
}

// The registry key carries the signature, so registering a DIFFERENT cron on a host that already has a
// live row creates a NEW observing row rather than inheriting LIVE (#10, at the registry level).
func TestRegistryKeyCarriesScheduleIdentity(t *testing.T) {
	reg := NewScheduleRegistry()
	base := time.Date(2026, 7, 12, 3, 0, 0, 0, time.UTC)
	old := ScheduleKey{Host: "h", Kind: LearnedKind, Cron: "0 3 * * *"}
	reg.RegisterObserving(Schedule{Host: "h", Kind: LearnedKind, Cron: "0 3 * * *", Timezone: "UTC", ValidUntil: base.Add(90 * 24 * time.Hour)})
	row, _ := reg.Get(old)
	row.Status = SchLive
	row.ObservedCount = 5

	reg.RegisterObserving(Schedule{Host: "h", Kind: LearnedKind, Cron: "0 5 * * *", Timezone: "UTC", ValidUntil: base.Add(90 * 24 * time.Hour)})
	shifted, ok := reg.Get(ScheduleKey{Host: "h", Kind: LearnedKind, Cron: "0 5 * * *"})
	if !ok {
		t.Fatal("the shifted schedule must be registered as its own row")
	}
	if shifted.Status != SchObserving || shifted.ObservedCount != 0 {
		t.Fatalf("a shifted schedule must re-enter observing with no inherited evidence, got status=%v count=%d", shifted.Status, shifted.ObservedCount)
	}
	if prior, _ := reg.Get(old); prior.Status != SchLive {
		t.Fatal("the previous schedule's row must be untouched by the new registration")
	}
}

// stubDemotions is a DemotionLookup with a fixed answer (and an optional error) — the governance state as
// the stage sees it.
type stubDemotions struct {
	demoted bool
	err     error
}

func (s stubDemotions) Demoted(context.Context, string, string, time.Time) (bool, error) {
	return s.demoted, s.err
}

// REQ-411: a LIVE learned schedule stops suppressing while its tuple carries a governance demotion, and an
// UNREADABLE demotion state also refuses to suppress. A DECLARED schedule is not gated by it.
func TestLearnedScheduleConsultsDemotion(t *testing.T) {
	at := time.Date(2026, 7, 12, 3, 5, 0, 0, time.UTC)
	learned := Schedule{Host: "h", Kind: LearnedKind, Cron: "0 3 * * *", Timezone: "UTC", Source: SourceLearned, Status: SchLive, ValidUntil: at.Add(24 * time.Hour)}
	declared := Schedule{Host: "h", Kind: "declared", Cron: "0 3 * * *", Timezone: "UTC", Source: SourceDeclared, Status: SchLive, ValidUntil: at.Add(24 * time.Hour)}
	alert := Alert{ExternalRef: "TG-1", Host: "h", AlertRule: "HostDown", IsReboot: true, Severity: ingest.SeverityWarning, ObservedAt: at}

	suppressed := func(scheds []Schedule, look DemotionLookup) bool {
		st := &ScheduledStage{Schedules: scheds, Window: learnWindow, Demotions: look}
		d, err := st.Evaluate(context.Background(), alert, at)
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		return d.Outcome.Suppressing()
	}

	if !suppressed([]Schedule{learned}, stubDemotions{}) {
		t.Fatal("an un-demoted live learned schedule must suppress")
	}
	if suppressed([]Schedule{learned}, stubDemotions{demoted: true}) {
		t.Fatal("a governance-demoted learned schedule must NOT suppress")
	}
	if suppressed([]Schedule{learned}, stubDemotions{err: context.DeadlineExceeded}) {
		t.Fatal("an unreadable demotion state must fail toward investigating, never toward suppression")
	}
	if !suppressed([]Schedule{declared}, stubDemotions{demoted: true}) {
		t.Fatal("an operator-DECLARED schedule keeps its behavior — the learned lane's demotion never gates it")
	}
}

// Demotion clears the accumulated evidence, so a demoted lesson must be re-earned from scratch rather than
// re-promoting on the same boots that produced the wrong suppression.
func TestDemoteClearsEvidence(t *testing.T) {
	l := newLearner()
	n1 := time.Date(2026, 7, 12, 3, 0, 0, 0, time.UTC)
	n2 := n1.Add(24 * time.Hour)
	l.Observe(context.Background(), RebootObservation{Host: "web01", BootReason: "systemd-reboot", At: n1}, n1)
	o := l.Observe(context.Background(), RebootObservation{Host: "web01", BootReason: "systemd-reboot", At: n2}, n2)
	if o.Status != SchLive {
		t.Fatalf("setup: must be live, got %+v", o)
	}
	if !l.Demote(o.Key) {
		t.Fatal("a live learned row must be demotable")
	}
	row, _ := l.Registry.Get(o.Key)
	if row.Status != SchObserving || row.ObservedCount != 0 || len(row.ObservedBoots) != 0 {
		t.Fatalf("demotion must return the row to observing with no evidence, got %+v", row)
	}
	if len(l.Live()) != 0 {
		t.Fatal("a demoted row must not be live")
	}
	// A DECLARED row is not TG's to revoke.
	l.Registry.RegisterObserving(Schedule{Host: "d", Kind: "declared", Cron: "0 3 * * *", Source: SourceDeclared, Status: SchLive})
	dk := ScheduleKey{Host: "d", Kind: "declared", Cron: "0 3 * * *"}
	drow, _ := l.Registry.Get(dk)
	drow.Status = SchLive
	if l.Demote(dk) {
		t.Fatal("a declared schedule must never be demoted by the learned lane")
	}
}

// renew-on-match keeps an actively-firing learned schedule from expiring mid-life, without touching its
// promotion state (the predecessor's renew_on_match).
func TestRenewOnMatch(t *testing.T) {
	reg := NewScheduleRegistry()
	at := time.Date(2026, 7, 12, 3, 5, 0, 0, time.UTC)
	k := ScheduleKey{Host: "h", Kind: LearnedKind, Cron: "0 3 * * *"}
	reg.RegisterObserving(Schedule{Host: "h", Kind: LearnedKind, Cron: "0 3 * * *", Timezone: "UTC", Source: SourceLearned, ValidUntil: at.Add(time.Hour)})
	row, _ := reg.Get(k)
	row.Status = SchLive
	row.ObservedCount = 2

	st := &ScheduledStage{Schedules: reg.Live(), Window: learnWindow, Renew: reg, RenewFor: 90 * 24 * time.Hour}
	d, _ := st.Evaluate(context.Background(), Alert{ExternalRef: "TG-1", Host: "h", AlertRule: "HostDown", IsReboot: true, Severity: ingest.SeverityWarning, ObservedAt: at}, at)
	if !d.Outcome.Suppressing() {
		t.Fatalf("setup: the live schedule must suppress, got %+v", d)
	}
	renewed, _ := reg.Get(k)
	if !renewed.ValidUntil.After(at.Add(24 * time.Hour)) {
		t.Fatalf("a matched schedule must have its validity renewed, got %v", renewed.ValidUntil)
	}
	if renewed.Status != SchLive || renewed.ObservedCount != 2 {
		t.Fatal("renewal must not touch the promotion state")
	}
}

// The reboot-class allowlist is DATA: the compiled default still classifies, and operator patterns replace
// it. A malformed pattern matches nothing (⇒ not reboot-class ⇒ investigate).
func TestRebootRulesAllowlist(t *testing.T) {
	def := RebootRules{}
	if !def.IsReboot("HostDown") || !def.IsReboot("NodeReboot") || def.IsReboot("DiskFull") {
		t.Fatal("the compiled default reboot-class set must be unchanged")
	}
	custom := RebootRules{Patterns: []string{"*device rebooted*", "*sysuptime*"}}
	if !custom.IsReboot("LibreNMS: Device rebooted (nl-web01)") {
		t.Fatal("an operator pattern must classify its estate's reboot rule")
	}
	if custom.IsReboot("HostDown") {
		t.Fatal("operator patterns REPLACE the default set")
	}
	if (RebootRules{Patterns: []string{"[bad"}}).IsReboot("anything") {
		t.Fatal("a malformed pattern must match nothing (fail toward investigating)")
	}
}
