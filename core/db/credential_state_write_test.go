package db

// ORACLE for the credential-state projection: driving a REAL SyncEngine over a source whose Bundle carries a
// SecretRef, then publishing the resulting coverage + sync state, records ONLY non-secret fields — no bundle
// material (not even the SecretRef reference) ever reaches a published row. The SyncRun/coverage types are
// secret-free by construction; this test proves the end-to-end publish path preserves that.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/credential"
)

// the secret-bearing reference a fake source's bundle carries — it must NEVER appear in any published row.
const secretRefMarker = "vault:secret/data/hosts/h1#ssh_key"

// fakeMachineSource is a read-only credential.CredentialSource whose one entry carries a SecretRef bundle.
type fakeMachineSource struct{ id string }

func (f fakeMachineSource) ID() string              { return f.id }
func (f fakeMachineSource) Plane() credential.Plane { return credential.PlaneMachine }
func (f fakeMachineSource) Sync(context.Context) ([]credential.SourceEntry, error) {
	b, err := credential.NewBundle(credential.BundleSpec{
		User: "svc-actuator", Port: 22, Scheme: credential.SchemeSSH,
		SSHKeyRef: config.SecretRef(secretRefMarker),
	})
	if err != nil {
		panic(err)
	}
	return []credential.SourceEntry{{
		NativeID: "h1",
		Selector: credential.Selector{Kind: credential.KindHost, Pattern: "h1"},
		Bundle:   b,
	}}, nil
}

func TestPublishCredentialState_NoSecretLeaks(t *testing.T) {
	se := credential.NewSyncEngine(nil)
	if err := se.RegisterSource(fakeMachineSource{id: "fake-bao"}, 10); err != nil {
		t.Fatalf("register source: %v", err)
	}
	runs, err := se.SyncAll(context.Background())
	if err != nil {
		t.Fatalf("SyncAll: %v", err)
	}

	// Reconstruct coverage from the drift exactly as the worker does.
	cov := make([]CredentialCoverage, 0, len(runs))
	for _, r := range runs {
		cov = append(cov, CredentialCoverage{SourceID: r.SourceID, Plane: r.Plane, Targets: r.Added - r.Removed})
	}

	store := NewMemCredentialStateStore()
	if err := store.Publish(context.Background(), runs, cov); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// The non-secret projection is faithful.
	if len(store.Runs) != 1 {
		t.Fatalf("recorded %d runs, want 1", len(store.Runs))
	}
	run := store.Runs[0]
	if run.SourceID != "fake-bao" || run.Plane != credential.PlaneMachine || run.Outcome != credential.SyncOK {
		t.Errorf("unexpected run projection: %+v", run)
	}
	if run.Added != 1 || run.Err != "" {
		t.Errorf("expected added=1, empty err; got added=%d err=%q", run.Added, run.Err)
	}
	if c := store.Coverage["fake-bao"]; c.Targets != 1 || c.Plane != credential.PlaneMachine {
		t.Errorf("unexpected coverage: %+v", c)
	}

	// No published row carries ANY secret-bearing bundle material — not the ref, not the user.
	blob, err := json.Marshal(struct {
		Runs     []credential.SyncRun
		Coverage map[string]CredentialCoverage
	}{store.Runs, store.Coverage})
	if err != nil {
		t.Fatalf("marshal recorded state: %v", err)
	}
	for _, leak := range []string{secretRefMarker, "vault:", "svc-actuator", "ssh_key"} {
		if strings.Contains(string(blob), leak) {
			t.Errorf("SECRET LEAK: published credential state contains %q\n%s", leak, blob)
		}
	}
}
