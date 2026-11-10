package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/safety"
)

var tg702Armed = time.Date(2026, 11, 8, 18, 54, 48, 0, time.UTC)

func tg702Evidence(taxonomy string) CausalityEvidence {
	return CausalityEvidence{ActorsKnown: true, ActorCovered: true, ActorTaxonomy: taxonomy, ActorActions: 2}
}

func tg702Deps(t *testing.T, running, stateKnown bool, read func(context.Context, CausalityQuery) (CausalityEvidence, error)) (Deps, string) {
	t.Helper()
	fake := newCCT0292Fake()
	sink := &fakeManifestSink{}
	m, err := manifest.New(manifest.Action{Op: "start", OpClass: "start-guest", Target: "pve03",
		Params: map[string]string{"guest": "web01"}, Reversible: true}, safety.BandAuto, "ph-tg702", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Seal(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if err := fake.ArmCommitConfirm(context.Background(), db.CommitConfirmRow{ActionID: m.ActionID, ExternalRef: "TG-702-w",
		OpClass: "start-guest", TargetHost: "pve03", WindowSeconds: 600, ForwardBand: safety.BandAuto.String(), ArmedAt: tg702Armed}); err != nil {
		t.Fatal(err)
	}
	return Deps{CommitConfirm: fake, Manifests: sink, ManifestSink: sink, Ledger: audit.NewLedger(), Executions: &supersedingExecs{CommitConfirmExecutionReader: newCCT0293FakeExecs()},
		CausalityEvidence: read,
		Gate: &predict.PredictionGate{GuestRunning: func(context.Context, string) (bool, string, bool) {
			return running, "drill: guest_liveness", stateKnown
		}}}, m.ActionID
}

func TestTheInverseDecisionSeparatesWhoMovedTheGuest(t *testing.T) {
	authorized := attribution.AttributedAuthorized.String()
	test := attribution.AuthorizedTest.String()
	self := attribution.AttributedSelf.String()
	suspicious := attribution.AttributedSuspicious.String()
	failure := errors.New("pve: 596 connection timed out")
	cases := []struct {
		name       string
		running    bool
		stateKnown bool
		ev         CausalityEvidence
		err        error
		want       string
	}{
		{"already stopped by a sanctioned person", false, true, tg702Evidence(authorized), nil, "superseded"},
		{"already stopped by the test carve-out", false, true, tg702Evidence(test), nil, "superseded"},
		{"restarted by a sanctioned person after the window armed", true, true, tg702Evidence(authorized), nil, "superseded"},
		{"already stopped with only TG on the record", false, true, tg702Evidence(self), nil, "failed"},
		{"already stopped with no actor on the record", false, true, tg702Evidence(""), nil, "failed"},
		{"already stopped by an unsanctioned actor", false, true, tg702Evidence(suspicious), nil, "failed"},
		{"already stopped and the task log failed", false, true, CausalityEvidence{}, failure, "failed"},
		{"already stopped and the subject was not covered", false, true, CausalityEvidence{ActorsKnown: true, ActorTaxonomy: authorized}, nil, "failed"},
		{"already stopped and one record names no host", false, true, CausalityEvidence{ActorsKnown: true, ActorCovered: true, ActorTaxonomy: authorized, ActorUnattributed: 1}, nil, "failed"},
		{"already stopped and one reader failed despite an authorized record", false, true, CausalityEvidence{ActorCovered: true, ActorTaxonomy: authorized, ActorActions: 1}, nil, "failed"},
		{"running and one reader failed despite an authorized record", true, true, CausalityEvidence{ActorCovered: true, ActorTaxonomy: authorized, ActorActions: 1}, nil, "held"},
		{"state unreadable with an authorized actor on the record", true, false, tg702Evidence(authorized), nil, "failed"},
		{"running with only TG on the record", true, true, tg702Evidence(self), nil, "sealed"},
		{"running with no actor on the record", true, true, tg702Evidence(""), nil, "sealed"},
		{"running and an unsanctioned actor acted", true, true, tg702Evidence(suspicious), nil, "held"},
		{"running and the task log failed", true, true, CausalityEvidence{}, failure, "held"},
		{"running and the subject was not covered", true, true, CausalityEvidence{ActorsKnown: true}, nil, "held"},
		{"running and the reading is unrecognised", true, true, tg702Evidence("attributed-sideways"), nil, "held"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps, fwd := tg702Deps(t, tc.running, tc.stateKnown, func(_ context.Context, q CausalityQuery) (CausalityEvidence, error) {
				if q.Host != "web01" || q.TargetHost != "web01" || !q.ExecutedAt.Equal(tg702Armed) {
					t.Fatalf("the read must cover the guest itself since the window armed, got %+v", q)
				}
				return tc.ev, tc.err
			})
			res, err := NewActivities(deps).SealCommitConfirmInverseActivity(context.Background(),
				SealCommitConfirmInverseInput{ActionID: fwd, ExternalRef: "TG-702-w"})
			if err != nil {
				t.Fatal(err)
			}
			got := "failed"
			switch {
			case res.Sealed:
				got = "sealed"
			case res.Superseded:
				got = "superseded"
			case res.HoldReason != "":
				got = "held"
			}
			if got != tc.want {
				t.Fatalf("got %s, want %s: %+v", got, tc.want, res)
			}
			if tc.want == "failed" && res.Reason == "" {
				t.Fatalf("a refusal must say why, got %+v", res)
			}
		})
	}
}

func TestAWiredReaderIsRequiredBeforeAnInverseMayFire(t *testing.T) {
	deps, fwd := tg702Deps(t, true, true, nil)
	res, err := NewActivities(deps).SealCommitConfirmInverseActivity(context.Background(),
		SealCommitConfirmInverseInput{ActionID: fwd, ExternalRef: "TG-702-w"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Sealed || res.HoldReason == "" {
		t.Fatalf("with no actor reader the inverse must hold, got %+v", res)
	}
}

func TestAPersonStoppingAHeldGuestResolvesSupersededWithoutATrip(t *testing.T) {
	sink := &fakeManifestSink{}
	m, err := manifest.New(manifest.Action{Op: "start", OpClass: "start-guest", Target: "web01",
		Params: map[string]string{"guest": "web01"}, Reversible: true}, safety.BandAuto, "ph-tg702-wf", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Seal(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	env, d := ccT0293ChildEnv(t, func(deps *Deps) {
		deps.Manifests, deps.ManifestSink = sink, sink
		deps.CausalityEvidence = func(context.Context, CausalityQuery) (CausalityEvidence, error) {
			return tg702Evidence(attribution.AttributedAuthorized.String()), nil
		}
		deps.Gate = &predict.PredictionGate{GuestRunning: func(context.Context, string) (bool, string, bool) {
			return false, "drill: guest_liveness", true
		}}
	})
	d.execs.set(db.ForwardExecution{Verdict: "deviation"})
	if err := d.fake.ArmCommitConfirm(context.Background(), db.CommitConfirmRow{ActionID: m.ActionID, ExternalRef: "TG-cc-1",
		OpClass: "start-guest", TargetHost: "web01", WindowSeconds: 600}); err != nil {
		t.Fatal(err)
	}
	in := ccT0292ChildInput()
	in.ActionID = m.ActionID
	env.ExecuteWorkflow(CommitConfirmWorkflow, in)
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("child: %v", env.GetWorkflowError())
	}
	_, resolves := d.fake.snapshot()
	if len(resolves) != 1 || resolves[0].State != db.CommitConfirmSuperseded || !strings.Contains(resolves[0].Detail, "authorized actor") {
		t.Fatalf("the window must resolve superseded and name the actor class, got %+v", resolves)
	}
	if len(*d.trips) != 0 {
		t.Fatalf("an authorized actor's stop is not a revert failure, got %v", *d.trips)
	}
}

func TestAnUnexplainedAlreadyStoppedGuestStillTripsTheBreaker(t *testing.T) {
	sink := &fakeManifestSink{}
	m, err := manifest.New(manifest.Action{Op: "start", OpClass: "start-guest", Target: "web01",
		Params: map[string]string{"guest": "web01"}, Reversible: true}, safety.BandAuto, "ph-tg702-kill", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Seal(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	env, d := ccT0293ChildEnv(t, func(deps *Deps) {
		deps.Manifests, deps.ManifestSink = sink, sink
		deps.CausalityEvidence = func(context.Context, CausalityQuery) (CausalityEvidence, error) {
			return CausalityEvidence{}, errors.New("pve: unreadable")
		}
		deps.Gate = &predict.PredictionGate{GuestRunning: func(context.Context, string) (bool, string, bool) {
			return false, "drill: guest_liveness", true
		}}
	})
	d.execs.set(db.ForwardExecution{Verdict: "deviation"})
	if err := d.fake.ArmCommitConfirm(context.Background(), db.CommitConfirmRow{ActionID: m.ActionID, ExternalRef: "TG-cc-1",
		OpClass: "start-guest", TargetHost: "web01", WindowSeconds: 600}); err != nil {
		t.Fatal(err)
	}
	in := ccT0292ChildInput()
	in.ActionID = m.ActionID
	env.ExecuteWorkflow(CommitConfirmWorkflow, in)
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("child: %v", env.GetWorkflowError())
	}
	_, resolves := d.fake.snapshot()
	if len(resolves) != 1 || resolves[0].State != db.CommitConfirmRevertFailed {
		t.Fatalf("ambiguity keeps the kill path, got %+v", resolves)
	}
	if len(*d.trips) != 1 {
		t.Fatalf("the breaker must trip on an unexplained revert failure, got %v", *d.trips)
	}
}
