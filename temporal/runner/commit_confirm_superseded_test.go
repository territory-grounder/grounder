package runner

// TG-664 (G4A F6): an armed commit-confirm inverse must never undo a LATER action on the same resource. The deep
// review's counterexample: a stale start-guest window fires its stop-guest after another session healed a unit
// inside that guest. The window now resolves `superseded` instead — no inverse, no credit, no breaker trip, a page.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/safety"
)

// supersedingExecs adds the ExecutionsSince capability to a window execution reader.
type supersedingExecs struct {
	CommitConfirmExecutionReader
	later []db.ExecutionSince
	err   error
	since time.Time
	host  string
}

func (s *supersedingExecs) ExecutionsSince(_ context.Context, after time.Time, host string, _ int) ([]db.ExecutionSince, error) {
	s.since, s.host = after, host
	return s.later, s.err
}

// tg664SealDeps arms a start-guest window on guest web01 (proposed as Target=pve03 + params.guest — the divergent
// spelling) and seals its forward manifest. The guest reads running, so the class-inverse precondition passes and
// the superseded check is the only variable.
func tg664SealDeps(t *testing.T, execs *supersedingExecs) (Deps, string, time.Time) {
	t.Helper()
	armed := time.Date(2026, 11, 8, 4, 0, 0, 0, time.UTC)
	fake := newCCT0292Fake()
	sink := &fakeManifestSink{}
	m, err := manifest.New(manifest.Action{Op: "start", OpClass: "start-guest", Target: "pve03",
		Params: map[string]string{"guest": "web01"}, Reversible: true}, safety.BandAuto, "ph-tg664", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Seal(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if err := fake.ArmCommitConfirm(context.Background(), db.CommitConfirmRow{ActionID: m.ActionID, ExternalRef: "TG-664-w",
		OpClass: "start-guest", TargetHost: "pve03", WindowSeconds: 600, ForwardBand: safety.BandAuto.String(), ArmedAt: armed}); err != nil {
		t.Fatal(err)
	}
	execs.CommitConfirmExecutionReader = newCCT0293FakeExecs()
	return Deps{CommitConfirm: fake, Manifests: sink, ManifestSink: sink, Ledger: audit.NewLedger(), Executions: execs,
			CausalityEvidence: func(context.Context, CausalityQuery) (CausalityEvidence, error) {
				return CausalityEvidence{ActorsKnown: true, ActorCovered: true}, nil
			},
			Gate: &predict.PredictionGate{GuestRunning: func(context.Context, string) (bool, string, bool) { return true, "drill: running", true }}},
		m.ActionID, armed
}

// KILLING MUTATION: drop the supersededBy call in SealCommitConfirmInverseActivity — RED here (the stale inverse seals).
func TestAStaleInverseIsSupersededByALaterActionOnTheSameResource(t *testing.T) {
	cases := []struct {
		name           string
		later          []db.ExecutionSince
		wantSuperseded bool
	}{
		{"a unit heal inside the guest after the window armed", []db.ExecutionSince{
			{ID: 7, ExternalRef: "TG-664-w", Action: manifest.Action{OpClass: "start-guest", Target: "pve03", Params: map[string]string{"guest": "web01"}}}, // its own forward
			{ID: 42, ExternalRef: "TG-other", Action: manifest.Action{OpClass: "restart-service", Target: "web01", Params: map[string]string{"unit": "nginx"}}},
		}, true},
		{"the same guest spelled by its own name", []db.ExecutionSince{
			{ID: 43, ExternalRef: "TG-other", Action: manifest.Action{OpClass: "start-guest", Target: "web01"}},
		}, true},
		{"an execution with no readable manifest counts as its whole host", []db.ExecutionSince{
			{ID: 44, ExternalRef: "TG-other", Action: manifest.Action{Target: "web01"}},
		}, true},
		{"only the window's own forward", []db.ExecutionSince{
			{ID: 7, ExternalRef: "TG-664-w", Action: manifest.Action{OpClass: "start-guest", Target: "pve03", Params: map[string]string{"guest": "web01"}}},
		}, false},
		{"a later action on another host", []db.ExecutionSince{
			{ID: 45, ExternalRef: "TG-other", Action: manifest.Action{OpClass: "restart-service", Target: "web02", Params: map[string]string{"unit": "nginx"}}},
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			execs := &supersedingExecs{later: tc.later}
			deps, fwd, armed := tg664SealDeps(t, execs)
			res, err := NewActivities(deps).SealCommitConfirmInverseActivity(context.Background(),
				SealCommitConfirmInverseInput{ActionID: fwd, ExternalRef: "TG-664-w"})
			if err != nil {
				t.Fatal(err)
			}
			if !execs.since.Equal(armed) || execs.host != "web01" {
				t.Fatalf("the read must start at the window's ARM time and scope to its resource host, got %v / %q", execs.since, execs.host)
			}
			if res.Superseded != tc.wantSuperseded {
				t.Fatalf("superseded=%v, want %v (reason %q)", res.Superseded, tc.wantSuperseded, res.Reason)
			}
			if tc.wantSuperseded && (res.Sealed || !strings.Contains(res.Reason, "superseded")) {
				t.Fatalf("a superseded window seals nothing and says why, got %+v", res)
			}
			if !tc.wantSuperseded && (!res.Sealed || res.HoldReason != "") {
				t.Fatalf("an unsuperseded window seals its inverse as before, got %+v", res)
			}
		})
	}
}

// An unreadable later-execution record HOLDS: the inverse is not fired blind. KILLING MUTATION: ignore the read
// error (treat it as "nothing later") — RED here.
func TestAnUnreadableSupersededCheckHolds(t *testing.T) {
	execs := &supersedingExecs{err: errors.New("db: connection refused")}
	deps, fwd, _ := tg664SealDeps(t, execs)
	res, err := NewActivities(deps).SealCommitConfirmInverseActivity(context.Background(),
		SealCommitConfirmInverseInput{ActionID: fwd, ExternalRef: "TG-664-w"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Sealed || res.Superseded || res.HoldReason == "" {
		t.Fatalf("an unreadable check must hold, never seal, got %+v", res)
	}
}

// The window itself: a deviation fires the inverse path, the seal reports superseded, and the window resolves
// `superseded` — the inverse never executes and the breaker is NOT tripped (it is not a revert failure).
// KILLING MUTATION: drop the seal.Superseded branch in commitConfirmFireInverse — RED here (revert_failed + trip).
func TestASupersededWindowNeverFiresAndNeverTrips(t *testing.T) {
	later := []db.ExecutionSince{{ID: 42, ExternalRef: "TG-other",
		Action: manifest.Action{OpClass: "restart-service", Target: "web01", Params: map[string]string{"unit": "nginx"}}}}
	sink := &fakeManifestSink{}
	m, err := manifest.New(manifest.Action{Op: "restart", OpClass: "restart-service", Target: "web01",
		Params: map[string]string{"unit": "nginx"}, Reversible: true}, safety.BandAuto, "ph-tg664-wf", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Seal(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	env, d := ccT0293ChildEnv(t, func(deps *Deps) {
		deps.Executions = &supersedingExecs{CommitConfirmExecutionReader: deps.Executions, later: later}
		deps.Manifests, deps.ManifestSink = sink, sink
	})
	d.execs.set(db.ForwardExecution{Verdict: "deviation"})
	if err := d.fake.ArmCommitConfirm(context.Background(), db.CommitConfirmRow{ActionID: m.ActionID, ExternalRef: "TG-cc-1",
		OpClass: "restart-service", TargetHost: "web01", WindowSeconds: 600}); err != nil {
		t.Fatal(err)
	}
	in := ccT0292ChildInput()
	in.ActionID = m.ActionID
	env.ExecuteWorkflow(CommitConfirmWorkflow, in)
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("child: %v", env.GetWorkflowError())
	}
	_, resolves := d.fake.snapshot()
	if len(resolves) != 1 || resolves[0].State != db.CommitConfirmSuperseded || !strings.Contains(resolves[0].Detail, "#42") {
		t.Fatalf("the window must resolve superseded, naming the later execution, got %+v", resolves)
	}
	if len(*d.trips) != 0 {
		t.Fatalf("a superseded window is not a revert failure — the breaker must not trip, got %v", *d.trips)
	}
}
