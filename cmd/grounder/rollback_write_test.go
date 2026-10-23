package main

import (
	"context"
	"errors"
	"testing"

	"go.temporal.io/sdk/client"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/httpapi"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/safety"
)

// fakeExecs is KEY-AWARE: InversesOf returns rows ONLY on a correct (inverts_action_id, external_ref) lookup,
// exactly like the real store's `WHERE inverts_action_id=$1 AND external_ref=$2`. A fake that ignored the key
// would pass even when the backend queries the WRONG external_ref — the mismatch that silently disabled the
// idempotency check.
type fakeExecs struct {
	latest        db.ForwardExecution
	found         bool
	inversesByKey map[[2]string][]db.InverseExecution
}

func (f fakeExecs) LatestExecution(context.Context, string) (db.ForwardExecution, bool, error) {
	return f.latest, f.found, nil
}
func (f fakeExecs) InversesOf(_ context.Context, forwardActionID, externalRef string) ([]db.InverseExecution, error) {
	return f.inversesByKey[[2]string{forwardActionID, externalRef}], nil
}

type fakeManifests struct {
	m  *manifest.ActionManifest
	ok bool
}

func (f fakeManifests) Get(context.Context, string) (*manifest.ActionManifest, bool, error) {
	return f.m, f.ok, nil
}

// fakeStarter stands in for Temporal with REJECT_DUPLICATE effectively DISABLED (it always "starts"), so any 409
// the backend returns can only have come from the InversesOf idempotency layer, not from Temporal's dedup.
type fakeStarter struct {
	started int
	err     error
}

func (f *fakeStarter) ExecuteWorkflow(_ context.Context, _ client.StartWorkflowOptions, _ interface{}, _ ...interface{}) (client.WorkflowRun, error) {
	f.started++
	return nil, f.err
}

func reversibleForwardManifest(t *testing.T, reversible bool) *manifest.ActionManifest {
	t.Helper()
	m, err := manifest.New(manifest.Action{
		Target: "app01", OpClass: "start-service", Op: "start",
		Params: map[string]string{"unit": "nginx"}, Reversible: reversible,
	}, safety.BandAuto, "plan-hash", "")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestRollbackBackend_PreCheckRefusals covers the existence / self-inverse / reversibility pre-checks. Each
// refuses BEFORE any workflow starts (the fail-closed direction), proven by fakeStarter.started == 0.
func TestRollbackBackend_PreCheckRefusals(t *testing.T) {
	tests := []struct {
		name      string
		execs     fakeExecs
		manifests fakeManifests
		want      error
	}{
		{
			name:  "never executed ⇒ unknown",
			execs: fakeExecs{found: false},
			want:  httpapi.ErrRollbackUnknownAction,
		},
		{
			name:  "the action is ITSELF a rollback ⇒ no double-undo",
			execs: fakeExecs{found: true, latest: db.ForwardExecution{ExternalRef: "inc-1", InvertsActionID: "some-forward"}},
			want:  httpapi.ErrRollbackAlreadyInverted,
		},
		{
			name:      "forward was not sealed reversible ⇒ not reversible",
			execs:     fakeExecs{found: true, latest: db.ForwardExecution{ExternalRef: "inc-1", TargetHost: "app01"}},
			manifests: fakeManifests{ok: true, m: reversibleForwardManifest(t, false)},
			want:      httpapi.ErrRollbackIrreversible,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeStarter{}
			b := rollbackBackend{tc: fs, execs: tc.execs, manifests: tc.manifests}
			_, err := b.StartRollback(context.Background(), "forward-abc", "root-admin")
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if fs.started != 0 {
				t.Fatalf("a pre-check refusal STARTED the workflow (%d) — it must refuse before any actuation lane", fs.started)
			}
		})
	}
}

// TestRollbackBackend_IdempotencyRefusedByInversesOfLayer is the FIX-2 proof (TG-462 assertion 3): after a
// rollback has executed — its inverse execution row written under external_ref = "rollback:"+forwardID — a SECOND
// rollback request is refused 409 by the InversesOf layer SPECIFICALLY. The starter always succeeds (Temporal's
// REJECT_DUPLICATE "disabled"), so the 409 can only come from InversesOf, and started == 0 proves it refused
// before any workflow start.
//
// RED-CONFIRM: revert the key fix (query InversesOf with fe.ExternalRef = "inc-1" instead of the rollback ref) —
// the key-aware fake returns no row, the check passes, the starter runs (started == 1) and StartRollback returns
// SUCCESS, so BOTH assertions below go RED. That is the dead check the original test could not catch.
func TestRollbackBackend_IdempotencyRefusedByInversesOfLayer(t *testing.T) {
	const fwd = "forward-abc"
	rollbackRef := "rollback:" + fwd // the external_ref the inverse execution row is written under
	execs := fakeExecs{
		found:  true,
		latest: db.ForwardExecution{ExternalRef: "inc-1", TargetHost: "app01"}, // the FORWARD incident ref (a DIFFERENT string)
		inversesByKey: map[[2]string][]db.InverseExecution{
			{fwd, rollbackRef}: {{ActionID: "inverse-1"}}, // an inverse already ran, keyed correctly
		},
	}
	fs := &fakeStarter{}
	b := rollbackBackend{tc: fs, execs: execs, manifests: fakeManifests{ok: true, m: reversibleForwardManifest(t, true)}}

	_, err := b.StartRollback(context.Background(), fwd, "root-admin")
	if !errors.Is(err, httpapi.ErrRollbackAlreadyInverted) {
		t.Fatalf("a second rollback (an inverse already recorded under %q) must be refused 409 by the InversesOf layer; got %v", rollbackRef, err)
	}
	if fs.started != 0 {
		t.Fatalf("the workflow was STARTED (%d) despite an existing inverse — the InversesOf idempotency layer did not "+
			"refuse; it queried the WRONG external_ref key (fe.ExternalRef instead of the rollback ref)", fs.started)
	}
}
