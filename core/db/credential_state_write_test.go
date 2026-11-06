package db

// ORACLE for the credential-state projection: driving a REAL SyncEngine over a source whose Bundle carries a
// SecretRef, then publishing the resulting coverage + sync state, records ONLY non-secret fields — no bundle
// material (not even the SecretRef reference) ever reaches a published row. The SyncRun/coverage types are
// secret-free by construction; this test proves the end-to-end publish path preserves that.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

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
		cov = append(cov, CredentialCoverage{SourceID: r.SourceID, Plane: r.Plane, Targets: r.Added - r.Removed, Precedence: 10})
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
	if c := store.Coverage["fake-bao"]; c.Targets != 1 || c.Plane != credential.PlaneMachine || c.Precedence != 10 {
		t.Errorf("unexpected coverage (precedence must survive the projection, TG-109): %+v", c)
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

// TG-109: the published precedence survives the REAL table round-trip (Publish → credential_coverage →
// Sources). DSN-gated; Migrate brings in 0087. Killing mutation: drop `precedence = EXCLUDED.precedence`
// from the upsert and the second publish's changed rank stops landing.
func TestCredentialStatePrecedenceRoundTrip(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the precedence round-trip")
	}
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer p.Close()
	src := fmt.Sprintf("prectest-%d", os.Getpid())
	defer func() {
		_, _ = p.Exec(ctx, `DELETE FROM credential_coverage WHERE source_id = $1`, src)
		_, _ = p.Exec(ctx, `DELETE FROM credential_sync_run WHERE source_id = $1`, src)
	}()

	store := NewCredentialStateWriteStore(p)
	run := credential.SyncRun{SourceID: src, Plane: credential.PlaneMachine, StartedAt: time.Now().UTC(),
		LastSyncedAt: time.Now().UTC(), Added: 3, Entries: 3, Outcome: credential.SyncOK}
	if err := store.Publish(ctx, []credential.SyncRun{run},
		[]CredentialCoverage{{SourceID: src, Plane: credential.PlaneMachine, Targets: 3, Precedence: 20}}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	rows, err := NewCredentialReadStore(p).Sources(ctx)
	if err != nil {
		t.Fatalf("sources: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.SourceID == src {
			found = true
			if r.Precedence != 20 {
				t.Errorf("read precedence = %d, want 20", r.Precedence)
			}
		}
	}
	if !found {
		t.Fatal("published source missing from Sources()")
	}
	// The upsert refreshes the rank (a re-registered source at a new precedence must not serve the old one).
	if err := store.Publish(ctx, nil,
		[]CredentialCoverage{{SourceID: src, Plane: credential.PlaneMachine, Targets: 3, Precedence: 35}}); err != nil {
		t.Fatalf("re-publish: %v", err)
	}
	rows2, _ := NewCredentialReadStore(p).Sources(ctx)
	for _, r := range rows2 {
		if r.SourceID == src && r.Precedence != 35 {
			t.Errorf("re-published precedence = %d, want 35 (the upsert must refresh the rank)", r.Precedence)
		}
	}
}
