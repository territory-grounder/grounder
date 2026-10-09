package runner

// TG-483 drills — the terminus collateral re-check. The ticket's oracle, executed at the workflow level:
// a forward heal that CLEARS the incident host but whose blast-radius sibling lights up within the settle
// window must NOT grade clean — the frozen execute-time MATCH and the incident-host-only ConfirmedClear
// both miss it, so only the terminus re-check can catch it. The counterfactual arms are load-bearing too:
// a quiet radius and an UNOBSERVABLE radius must both keep today's clean grade (the fix must not convert
// every terminus into a demotion — "what does the fixed rule do to today's data").
//
// EXECUTED KILLING MUTATIONS (2026-08-15, both witnessed red then restored green):
//   1. workflow.go: the terminus block's `collateralObserved = true` assignment deleted (the freeze the
//      ticket names — the verdict stays execute-time-only) → TestTerminusCollateralDemotesTheCleanRun's
//      demote arm went red ("collateral hit must demote the ladder feed"), quiet/unknown arms stayed
//      green. Restored, re-ran, green.
//   2. core/db alert_log.go: the CollateralOpenedSince pre-existing NOT EXISTS exclusion deleted → the
//      gated DB drill's already-firing row counted as collateral and its arm went red. Restored, green.
//      (See core/db/collateral_db_test.go — runs only under the real-database harness.)

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"

	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// tg483Script proposes start-service — an executing class WITHOUT commit_confirmed, so the terminus feeds
// the graduation ladder directly (a commit-confirmed class would withhold per REQ-2907 and hide the bit
// this drill asserts).
func tg483Script() []string {
	return []string{
		`{"action":"tool","tool":"get-logs","args":{"host":"web01"},"confidence":0.8}`,
		`{"action":"propose","confidence":0.85,"proposal":{"external_ref":"TG-483-e2e","target":"web01","op_class":"start-service","op":"start","params":{"unit":"nginx"},"reversible":true,"confidence":0.85,"evidence_ids":["tr-1"]}}`,
	}
}

func tg483Envelope() ingest.IncidentEnvelope {
	return ingest.IncidentEnvelope{ExternalRef: "TG-483-e2e", Host: "web01", AlertRule: "NginxDown",
		Severity: ingest.SeverityWarning, Site: "dc1"}
}

// The decision table of the activity itself: unknown is UNKNOWN (nil), never a fabricated reading.
func TestObserveCollateralActivityDecisionTable(t *testing.T) {
	ctx := context.Background()
	members := func(string) []string { return []string{"web01", "db01"} }
	quiet := func(context.Context, []string, string, string, time.Time) ([]CollateralHit, error) { return nil, nil }
	hit := func(context.Context, []string, string, string, time.Time) ([]CollateralHit, error) {
		return []CollateralHit{{Host: "db01", AlertRule: "DiskFull"}}, nil
	}
	broken := func(context.Context, []string, string, string, time.Time) ([]CollateralHit, error) {
		return nil, errors.New("reader down")
	}
	in := ObserveCollateralInput{Anchor: "web01", ExcludeHost: "web01", ExcludeRule: "NginxDown", Since: time.Now()}

	t.Run("no anchor is unknown", func(t *testing.T) {
		res, err := NewActivities(Deps{BlastMembers: members, CollateralOpenedSince: quiet}).
			ObserveCollateralActivity(ctx, ObserveCollateralInput{Anchor: "  "})
		if err != nil || res.Observed != nil {
			t.Fatalf("an anchorless scan must report UNKNOWN (nil), got %+v err=%v", res.Observed, err)
		}
	})
	t.Run("nil seams are unknown", func(t *testing.T) {
		res, err := NewActivities(Deps{}).ObserveCollateralActivity(ctx, in)
		if err != nil || res.Observed != nil {
			t.Fatalf("no graph/reader must report UNKNOWN, got %+v err=%v", res.Observed, err)
		}
	})
	t.Run("unenumerable radius is unknown", func(t *testing.T) {
		res, err := NewActivities(Deps{BlastMembers: func(string) []string { return nil }, CollateralOpenedSince: quiet}).
			ObserveCollateralActivity(ctx, in)
		if err != nil || res.Observed != nil {
			t.Fatalf("an unseeded graph must report UNKNOWN — never an all-clear, got %+v err=%v", res.Observed, err)
		}
	})
	t.Run("surveyed and quiet is an earned false", func(t *testing.T) {
		res, err := NewActivities(Deps{BlastMembers: members, CollateralOpenedSince: quiet}).
			ObserveCollateralActivity(ctx, in)
		if err != nil || res.Observed == nil || *res.Observed {
			t.Fatalf("a surveyed quiet radius must read &false, got %+v err=%v", res.Observed, err)
		}
	})
	t.Run("a first-surfaced sibling is an earned true with evidence", func(t *testing.T) {
		res, err := NewActivities(Deps{BlastMembers: members, CollateralOpenedSince: hit}).
			ObserveCollateralActivity(ctx, in)
		if err != nil || res.Observed == nil || !*res.Observed || len(res.Hits) != 1 || res.Hits[0].Host != "db01" {
			t.Fatalf("a hit must read &true and carry the evidence, got %+v err=%v", res, err)
		}
	})
	t.Run("a reader error is an error, not a reading", func(t *testing.T) {
		if _, err := NewActivities(Deps{BlastMembers: members, CollateralOpenedSince: broken}).
			ObserveCollateralActivity(ctx, in); err == nil {
			t.Fatal("a broken reader must surface the error (the workflow's error path treats it as unknown)")
		}
	})
}

// The ticket's oracle end to end, plus the two counterfactual arms that keep the healthy case healthy.
func TestTerminusCollateralDemotesTheCleanRun(t *testing.T) {
	run := func(t *testing.T, blast func(string) []string, scan func(context.Context, []string, string, string, time.Time) ([]CollateralHit, error)) (*gradSpy, []string) {
		var ts testsuite.WorkflowTestSuite
		env := ts.NewTestWorkflowEnvironment()
		gate := safety.NewActuatingChokepoint()
		act := &recordingActuator{}
		sink := &fakeManifestSink{}
		deps := testDeps(tg483Script()...)
		deps.Mutation = gate
		deps.Interceptor = withPermissivePolicy(actuate.NewInterceptor(gate, act, audit.NewLedger()))
		deps.Manifests = sink
		deps.ManifestSink = sink
		deps.PostStateObserve = func(context.Context, string, string) ([]verify.ObservedAlert, bool) { return nil, true }
		// The incident host CLEARS — ConfirmedClear confirms. The collateral, if any, is elsewhere.
		deps.ClearObserve = faultedUntilHealed("web01", "NginxDown", act)
		g := &gradSpy{}
		deps.RecordGraduation = g.record
		deps.BlastMembers = blast
		deps.CollateralOpenedSince = scan
		acts := NewActivities(deps)
		registerAll(env, acts)
		env.RegisterActivity(acts.BackfillManifestActivity)
		env.RegisterActivity(acts.ObserveClearedActivity)
		env.RegisterActivity(acts.RecoveredSinceActivity)
		env.RegisterActivity(acts.ReconcileActivity)
		env.RegisterActivity(acts.GraduationActivity)
		var order []string
		env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
			order = append(order, info.ActivityType.Name)
		})
		env.ExecuteWorkflow(RunnerWorkflow, tg483Envelope())
		if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
			t.Fatalf("workflow must complete: %v", env.GetWorkflowError())
		}
		if act.execs != 1 {
			t.Fatalf("precondition: the heal must actually execute (execs=%d)", act.execs)
		}
		if g.calls != 1 {
			t.Fatalf("the terminus must feed the ladder exactly once, got %d", g.calls)
		}
		return g, order
	}
	members := func(string) []string { return []string{"web01", "db01"} }
	quiet := func(context.Context, []string, string, string, time.Time) ([]CollateralHit, error) { return nil, nil }

	t.Run("a sibling cascade within the window demotes", func(t *testing.T) {
		g, order := run(t, members, func(_ context.Context, hosts []string, exHost, exRule string, _ time.Time) ([]CollateralHit, error) {
			if len(hosts) != 2 || exHost != "web01" || exRule != "NginxDown" {
				t.Fatalf("the scan must cover the radius and exclude the incident's own identity, got hosts=%v ex=(%s,%s)", hosts, exHost, exRule)
			}
			return []CollateralHit{{Host: "db01", AlertRule: "DiskFull"}}, nil
		})
		if g.clean {
			t.Fatal("TG-483: a collateral hit must demote the ladder feed — the frozen execute-time MATCH graded a damaging heal clean")
		}
		colAt, execAt := -1, -1
		for i, n := range order {
			if n == "ObserveCollateralActivity" && colAt == -1 {
				colAt = i
			}
			if n == "ExecuteActivity" && execAt == -1 {
				execAt = i
			}
		}
		if colAt == -1 || execAt == -1 || colAt <= execAt {
			t.Fatalf("the collateral re-check must run at the TERMINUS, after execution (execute@%d collateral@%d in %v)", execAt, colAt, order)
		}
	})
	t.Run("a quiet radius keeps the clean grade", func(t *testing.T) {
		if g, _ := run(t, members, quiet); !g.clean {
			t.Fatal("a surveyed-quiet radius must stay CLEAN — the fix must not demote the healthy case")
		}
	})
	t.Run("an unobservable radius keeps the clean grade", func(t *testing.T) {
		if g, _ := run(t, func(string) []string { return nil }, quiet); !g.clean {
			t.Fatal("UNKNOWN must change nothing — an unseeded graph is not evidence of collateral")
		}
	})
}
