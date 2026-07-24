package acceptance

import (
	"context"
	"fmt"
	"testing"
	"time"

	_ "time/tzdata"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/suppression"
)

// a Sunday, for the weekly cron window "0 3 * * 0" in UTC.
var sun3am = time.Date(2026, 7, 12, 3, 0, 0, 0, time.UTC)

// panicStage always panics — proves a phase failure fails OPEN.
type panicStage struct{}

func (panicStage) Name() suppression.Phase { return suppression.PhaseKnownPattern }
func (panicStage) Evaluate(context.Context, suppression.Alert, time.Time) (suppression.Decision, error) {
	panic("phase boom")
}

type fakeReopen struct{ n int }

func (f *fakeReopen) Reopen(context.Context, string) error { f.n++; return nil }

type fakePager struct{ n int }

func (f *fakePager) Page(context.Context, string, string) error { f.n++; return nil }

func TestTier1SuppressionAcceptance(t *testing.T) {
	suite := godog.TestSuite{
		Name:                "spec/005 tier1-suppression",
		ScenarioInitializer: initializeScenario,
		Options:             &godog.Options{Format: "pretty", Paths: []string{"."}, Tags: "~@pending", Strict: true, TestingT: t},
	}
	if suite.Run() != 0 {
		t.Fatal("spec/005 acceptance scenarios failed")
	}
}

type world struct {
	now      time.Time
	stages   []suppression.Stage
	alert    suppression.Alert
	decision suppression.Decision

	dedup     *suppression.DedupStage
	entry     suppression.TriageEntry
	accepted  bool
	acceptErr error

	policy    suppression.SuppressionPolicy
	freshness time.Duration

	verifier   *suppression.TwoPhaseVerifier
	reopen     *fakeReopen
	pager      *fakePager
	bootReason string
	verifyRes  suppression.VerifyResult

	reg         *suppression.ScheduleRegistry
	boots       []suppression.Boot
	cronPresent bool
	promo       suppression.SchedStatus
}

func liveSchedule() suppression.Schedule {
	return suppression.Schedule{Host: "h", Kind: "reboot", Cron: "0 3 * * 0", Timezone: "UTC", Status: suppression.SchLive, ValidUntil: sun3am.Add(30 * 24 * time.Hour), LastVerifiedAt: sun3am}
}

func initializeScenario(sc *godog.ScenarioContext) {
	w := &world{now: sun3am, cronPresent: true, freshness: time.Hour}
	ctx := context.Background()
	window := suppression.WindowEvaluator{PreBuffer: 30 * time.Minute, PostWindow: 30 * time.Minute}
	rebootIn := func() suppression.Alert {
		return suppression.Alert{ExternalRef: "TG-1", Host: "h", IsReboot: true, Severity: ingest.SeverityWarning, ObservedAt: sun3am.Add(10 * time.Minute)}
	}

	// panic stage for the fail-open scenario.
	sc.Step(`^a suppression phase raises an error while evaluating an alert$`, func() error {
		w.stages = []suppression.Stage{panicStage{}}
		w.alert = suppression.Alert{ExternalRef: "TG-1", Severity: ingest.SeverityWarning}
		return nil
	})
	sc.Step(`^a candidate suppression match that is not confirmed against the live registry$`, func() error {
		// an expired policy that matches ⇒ unconfirmed ⇒ fail open
		w.policy = suppression.SuppressionPolicy{HostScope: "*", RuleScope: "R", ValidFrom: sun3am.Add(-time.Hour), ValidUntil: sun3am.Add(-time.Minute), LastVerifiedAt: sun3am}
		w.stages = []suppression.Stage{&suppression.BlastRadiusStage{Policies: []suppression.SuppressionPolicy{w.policy}, Freshness: w.freshness}}
		w.alert = suppression.Alert{ExternalRef: "TG-1", Host: "web01", AlertRule: "R", Severity: ingest.SeverityWarning}
		return nil
	})
	sc.Step(`^a reboot-class alert with severity "critical" on a host with a live matching schedule$`, func() error {
		w.stages = []suppression.Stage{&suppression.ScheduledStage{Schedules: []suppression.Schedule{liveSchedule()}, Window: window}}
		w.alert = suppression.Alert{ExternalRef: "TG-1", Host: "h", IsReboot: true, Severity: ingest.SeverityCritical, ObservedAt: sun3am.Add(10 * time.Minute)}
		return nil
	})
	sc.Step(`^an alert whose severity is not a recognised severity value$`, func() error {
		w.stages = []suppression.Stage{&suppression.KnownPatternStage{Patterns: []suppression.TransientPattern{{AlertRule: "R"}}}}
		w.alert = suppression.Alert{ExternalRef: "TG-1", AlertRule: "R", Severity: ingest.SeverityUnknown}
		return nil
	})

	// --- dedup boundary ---
	setupDedup := func(loggedAt time.Time) {
		w.entry = suppression.TriageEntry{Host: "h", AlertRule: "R", LoggedAt: loggedAt}
		w.dedup = &suppression.DedupStage{Recent: []suppression.TriageEntry{w.entry}, Window: time.Hour}
		w.stages = []suppression.Stage{w.dedup}
		w.alert = suppression.Alert{ExternalRef: "TG-1", Host: "h", AlertRule: "R", Severity: ingest.SeverityWarning}
	}
	sc.Step(`^a prior triage-log entry timestamped after the current time$`, func() error { setupDedup(w.now.Add(time.Minute)); return nil })
	sc.Step(`^a prior triage-log entry whose age relative to now is negative$`, func() error { setupDedup(w.now.Add(5 * time.Minute)); return nil })
	sc.Step(`^a prior triage-log entry timestamped inside the window before now$`, func() error { setupDedup(w.now.Add(-10 * time.Minute)); return nil })
	sc.Step(`^the dedup stage evaluates the entry$`, func() error {
		w.accepted, w.acceptErr = w.dedup.AcceptEntry(w.entry, w.now)
		w.decision, _ = (&suppression.Chain{Stages: w.stages}).Decide(ctx, w.alert, w.now)
		return nil
	})
	sc.Step(`^the entry is rejected at the envelope boundary$`, func() error {
		if w.acceptErr == nil {
			return fmt.Errorf("malformed entry must be rejected at the boundary")
		}
		return nil
	})
	sc.Step(`^the entry is accepted as a dedup candidate$`, func() error {
		if !w.accepted || w.acceptErr != nil {
			return fmt.Errorf("a within-window entry must be accepted, got ok=%v err=%v", w.accepted, w.acceptErr)
		}
		return nil
	})

	// --- blast-radius policy ---
	sc.Step(`^a suppression-policy record whose valid_from and valid_until bracket now and whose last_verified_at is fresh$`, func() error {
		w.policy = suppression.SuppressionPolicy{HostScope: "*", RuleScope: "R", ValidFrom: w.now.Add(-time.Hour), ValidUntil: w.now.Add(time.Hour), LastVerifiedAt: w.now.Add(-time.Minute)}
		return nil
	})
	sc.Step(`^a suppression-policy record whose valid_until is before now$`, func() error {
		w.policy = suppression.SuppressionPolicy{HostScope: "*", RuleScope: "R", ValidFrom: w.now.Add(-time.Hour), ValidUntil: w.now.Add(-time.Minute), LastVerifiedAt: w.now}
		return nil
	})
	sc.Step(`^a suppression-policy record whose last_verified_at is past its freshness bound$`, func() error {
		w.policy = suppression.SuppressionPolicy{HostScope: "*", RuleScope: "R", ValidFrom: w.now.Add(-time.Hour), ValidUntil: w.now.Add(time.Hour), LastVerifiedAt: w.now.Add(-48 * time.Hour)}
		return nil
	})
	setChildAlert := func() {
		w.stages = []suppression.Stage{&suppression.BlastRadiusStage{Policies: []suppression.SuppressionPolicy{w.policy}, Freshness: w.freshness}}
		w.alert = suppression.Alert{ExternalRef: "TG-1", Host: "web01", AlertRule: "R", Severity: ingest.SeverityWarning}
	}
	sc.Step(`^a child alert within the record's declared host and rule scope$`, func() error { setChildAlert(); return nil })
	sc.Step(`^a child alert within the record's declared scope$`, func() error { setChildAlert(); return nil })
	sc.Step(`^an active blast-radius fold for a matched child alert$`, func() error {
		w.policy = suppression.SuppressionPolicy{HostScope: "*", RuleScope: "R", ValidFrom: w.now.Add(-time.Hour), ValidUntil: w.now.Add(time.Hour), LastVerifiedAt: w.now.Add(-time.Minute)}
		setChildAlert()
		return nil
	})

	// --- scheduled reboot ---
	sc.Step(`^a live un-killed un-expired schedule whose DST-correct window contains the alert time$`, func() error {
		w.stages = []suppression.Stage{&suppression.ScheduledStage{Schedules: []suppression.Schedule{liveSchedule()}, Window: window}}
		return nil
	})
	sc.Step(`^a schedule in the observing state whose window contains the alert time$`, func() error {
		s := liveSchedule()
		s.Status = suppression.SchObserving
		w.stages = []suppression.Stage{&suppression.ScheduledStage{Schedules: []suppression.Schedule{s}, Window: window}}
		return nil
	})
	sc.Step(`^a live schedule whose window does not contain the alert time$`, func() error {
		w.stages = []suppression.Stage{&suppression.ScheduledStage{Schedules: []suppression.Schedule{liveSchedule()}, Window: window}}
		w.alert = suppression.Alert{ExternalRef: "TG-1", Host: "h", IsReboot: true, Severity: ingest.SeverityWarning, ObservedAt: sun3am.Add(3 * time.Hour)}
		return nil
	})
	sc.Step(`^a live schedule whose window contains the alert time$`, func() error {
		w.stages = []suppression.Stage{&suppression.ScheduledStage{Schedules: []suppression.Schedule{liveSchedule()}, Window: window}}
		return nil
	})
	sc.Step(`^a reboot-class alert on that host$`, func() error {
		if w.alert.ExternalRef == "" {
			w.alert = rebootIn()
		}
		return nil
	})
	sc.Step(`^a reboot-class alert with severity "critical" on that host$`, func() error {
		w.alert = suppression.Alert{ExternalRef: "TG-1", Host: "h", IsReboot: true, Severity: ingest.SeverityCritical, ObservedAt: sun3am.Add(10 * time.Minute)}
		return nil
	})

	// --- known pattern ---
	sc.Step(`^a host-agnostic transient pattern keyed on the alert rule within the estate$`, func() error {
		w.stages = []suppression.Stage{&suppression.KnownPatternStage{Patterns: []suppression.TransientPattern{{AlertRule: "FlappyLink", Estate: "dc1", Confidence: 0.9}}}}
		return nil
	})
	sc.Step(`^an alert carrying that alert rule on a host with no host-specific row$`, func() error {
		w.alert = suppression.Alert{ExternalRef: "TG-1", Host: "anyhost", AlertRule: "FlappyLink", Severity: ingest.SeverityWarning}
		return nil
	})
	sc.Step(`^a host-agnostic transient pattern keyed on one alert rule$`, func() error {
		w.stages = []suppression.Stage{&suppression.KnownPatternStage{Patterns: []suppression.TransientPattern{{AlertRule: "RuleA"}}}}
		return nil
	})
	sc.Step(`^an alert carrying a different alert rule$`, func() error {
		w.alert = suppression.Alert{ExternalRef: "TG-1", AlertRule: "RuleB", Severity: ingest.SeverityWarning}
		return nil
	})

	// --- the shared chain When ---
	sc.Step(`^the suppression chain decides the alert$`, func() error {
		if w.alert.ExternalRef == "" {
			w.alert = rebootIn()
		}
		var err error
		w.decision, err = (&suppression.Chain{Stages: w.stages, Ledger: audit.NewLedger()}).Decide(ctx, w.alert, w.now)
		return err
	})

	// --- two-phase verify ---
	sc.Step(`^a suppressed scheduled reboot whose recorded boot reason is not a clean systemd-reboot$`, func() error {
		w.reopen, w.pager = &fakeReopen{}, &fakePager{}
		w.verifier = &suppression.TwoPhaseVerifier{Reopen: w.reopen, Pager: w.pager}
		w.bootReason = "kernel-panic"
		return nil
	})
	sc.Step(`^a suppressed scheduled reboot whose recorded boot reason is a clean systemd-reboot$`, func() error {
		w.reopen, w.pager = &fakeReopen{}, &fakePager{}
		w.verifier = &suppression.TwoPhaseVerifier{Reopen: w.reopen, Pager: w.pager}
		w.bootReason = suppression.CleanBootReason
		return nil
	})
	sc.Step(`^the two-phase verifier runs$`, func() error {
		var err error
		w.verifyRes, err = w.verifier.Verify(ctx, "TG-1", w.bootReason)
		return err
	})
	sc.Step(`^the incident is reopened$`, func() error {
		if !w.verifyRes.Reopened || w.reopen.n != 1 {
			return fmt.Errorf("a dirty boot must reopen the incident")
		}
		return nil
	})
	sc.Step(`^the approver graph is paged$`, func() error {
		if w.pager.n != 1 {
			return fmt.Errorf("a dirty boot must page the approver graph")
		}
		return nil
	})
	sc.Step(`^the suppression is confirmed and the incident stays closed$`, func() error {
		if !w.verifyRes.Confirmed || w.reopen.n != 0 {
			return fmt.Errorf("a clean systemd-reboot must confirm and stay closed")
		}
		return nil
	})

	// --- promotion ---
	sc.Step(`^an observing schedule with two recorded boots inside its window$`, func() error {
		w.reg = suppression.NewScheduleRegistry()
		w.reg.RegisterObserving(suppression.Schedule{Host: "h", Kind: "reboot", Cron: "0 3 * * 0", Timezone: "UTC", ValidUntil: sun3am.Add(60 * 24 * time.Hour)})
		w.boots = []suppression.Boot{{At: sun3am.Add(5 * time.Minute)}, {At: sun3am.Add(7*24*time.Hour + 3*time.Minute)}}
		return nil
	})
	sc.Step(`^an observing schedule with one recorded boot inside its window$`, func() error {
		w.reg = suppression.NewScheduleRegistry()
		w.reg.RegisterObserving(suppression.Schedule{Host: "h", Kind: "reboot", Cron: "0 3 * * 0", Timezone: "UTC", ValidUntil: sun3am.Add(60 * 24 * time.Hour)})
		w.boots = []suppression.Boot{{At: sun3am.Add(5 * time.Minute)}}
		return nil
	})
	sc.Step(`^a live schedule whose cron is no longer present on the host$`, func() error {
		w.reg = suppression.NewScheduleRegistry()
		s := liveSchedule()
		w.reg.RegisterObserving(s)
		w.cronPresent = false
		return nil
	})
	sc.Step(`^the promotion writer runs$`, func() error {
		w.promo = w.reg.Promote("h", "reboot", window, w.boots, w.cronPresent, sun3am.Add(8*24*time.Hour))
		return nil
	})
	sc.Step(`^the schedule status becomes live$`, func() error {
		if w.promo != suppression.SchLive {
			return fmt.Errorf("two in-window boots must promote to live, got %v", w.promo)
		}
		return nil
	})
	sc.Step(`^the schedule status stays observing$`, func() error {
		if w.promo != suppression.SchObserving {
			return fmt.Errorf("a single boot must stay observing, got %v", w.promo)
		}
		return nil
	})
	sc.Step(`^the schedule status becomes disabled$`, func() error {
		if w.promo != suppression.SchDisabled {
			return fmt.Errorf("a removed cron must drift to disabled, got %v", w.promo)
		}
		return nil
	})

	// --- shared outcome assertions ---
	sc.Step(`^the outcome is escalate$`, func() error {
		if w.decision.Outcome != suppression.OutcomeEscalate {
			return fmt.Errorf("outcome must be escalate, got %s (phase %s)", w.decision.Outcome, w.decision.Phase)
		}
		return nil
	})
	sc.Step(`^the blast-radius fold is activated$`, func() error {
		if w.decision.Outcome != suppression.OutcomeNotice || w.decision.Phase != suppression.PhaseBlastRadius {
			return fmt.Errorf("a valid policy must activate the fold, got %+v", w.decision)
		}
		return nil
	})
	sc.Step(`^the alert is posted as a notice$`, func() error {
		if w.decision.Outcome != suppression.OutcomeNotice {
			return fmt.Errorf("an active fold must post a notice, got %s", w.decision.Outcome)
		}
		return nil
	})
	sc.Step(`^no remediation session is spawned$`, func() error {
		if !w.decision.Outcome.Suppressing() {
			return fmt.Errorf("a notice must not spawn a remediation session")
		}
		return nil
	})
	sc.Step(`^the outcome is suppressed in phase SR$`, func() error {
		if w.decision.Outcome != suppression.OutcomeSuppressed || w.decision.Phase != suppression.PhaseScheduledReboot {
			return fmt.Errorf("an on-schedule reboot must suppress in phase SR, got %+v", w.decision)
		}
		return nil
	})
	sc.Step(`^the outcome is suppressed as a known transient pattern$`, func() error {
		if w.decision.Outcome != suppression.OutcomeSuppressed || w.decision.Phase != suppression.PhaseKnownPattern {
			return fmt.Errorf("a known pattern must suppress, got %+v", w.decision)
		}
		return nil
	})
}
