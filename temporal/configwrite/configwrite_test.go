package configwrite

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/cpconfig"
	"github.com/territory-grounder/grounder/core/seal"
)

// fakes — the activity is a plain method, so the single-writer semantics are oracle-testable
// without a Temporal server.

type memConfig struct {
	rows map[string]string
	fail bool
	seq  []int64
}

func (m *memConfig) Upsert(_ context.Context, key, value, _, _ string, ledgerSeq int64, sv int) error {
	if m.fail {
		return errors.New("store down")
	}
	if sv <= 0 {
		return errors.New("unstamped row")
	}
	if m.rows == nil {
		m.rows = map[string]string{}
	}
	m.rows[key] = value
	m.seq = append(m.seq, ledgerSeq)
	return nil
}

type memSecrets struct {
	blobs map[string]seal.Sealed
}

func (m *memSecrets) Put(_ context.Context, name string, blob seal.Sealed, _, _ string, _ int64, sv int) error {
	if sv <= 0 {
		return errors.New("unstamped row")
	}
	if m.blobs == nil {
		m.blobs = map[string]seal.Sealed{}
	}
	m.blobs[name] = blob
	return nil
}

func rig() (*Activities, *audit.Ledger, *memConfig, *memSecrets) {
	lg := audit.NewLedger()
	cfg := &memConfig{}
	sec := &memSecrets{}
	return &Activities{D: Deps{Ledger: lg, Config: cfg, Secrets: sec}}, lg, cfg, sec
}

func TestApplyConfigLedgersBeforeCommit(t *testing.T) {
	acts, lg, cfg, _ := rig()
	res, err := acts.ApplyConfigActivity(context.Background(),
		ConfigRequest{Key: "session.ttl", Value: "8h", Rationale: "shorter operator sessions", Operator: "kyriakos"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if cfg.rows["session.ttl"] != "8h" {
		t.Fatalf("row not committed: %+v", cfg.rows)
	}
	entries := lg.Entries()
	if len(entries) != 1 || res.LedgerSeq != entries[0].Seq {
		t.Fatalf("ledger entry/seq mismatch: %d entries, res seq %d", len(entries), res.LedgerSeq)
	}
	if entries[0].Decision != "config:set" {
		t.Fatalf("decision = %q", entries[0].Decision)
	}
}

func TestApplyConfigRefusesLawKeyWithNoLedgerEntry(t *testing.T) {
	acts, lg, cfg, _ := rig()
	_, err := acts.ApplyConfigActivity(context.Background(),
		ConfigRequest{Key: "safety.mutation_enabled", Value: "on", Rationale: "flip it", Operator: "mallory"})
	if !errors.Is(err, cpconfig.ErrLawPinned) {
		t.Fatalf("law write: got %v, want ErrLawPinned", err)
	}
	if entries := lg.Entries(); len(entries) != 0 {
		t.Fatalf("a refused LAW write reached the ledger: %d entries", len(entries))
	}
	if len(cfg.rows) != 0 {
		t.Fatalf("a refused LAW write reached the store")
	}
}

func TestApplyConfigStoreFailureLeavesOverRecordedLedger(t *testing.T) {
	// Ledger-BEFORE-commit: a store failure after the append is an over-recorded ledger, never an
	// unrecorded state change.
	acts, lg, cfg, _ := rig()
	cfg.fail = true
	_, err := acts.ApplyConfigActivity(context.Background(),
		ConfigRequest{Key: "session.ttl", Value: "8h", Rationale: "r", Operator: "kyriakos"})
	if err == nil {
		t.Fatalf("store failure did not surface")
	}
	if entries := lg.Entries(); len(entries) != 1 {
		t.Fatalf("ledger must hold the appended decision: %d entries", len(entries))
	}
}

func TestApplyConfigRequiresRationale(t *testing.T) {
	acts, _, _, _ := rig()
	_, err := acts.ApplyConfigActivity(context.Background(),
		ConfigRequest{Key: "session.ttl", Value: "8h", Rationale: "   ", Operator: "kyriakos"})
	if !errors.Is(err, ErrRationaleRequired) {
		t.Fatalf("got %v, want ErrRationaleRequired", err)
	}
}

func TestPutSecretCarriesOnlyCiphertextAndLedgers(t *testing.T) {
	acts, lg, _, sec := rig()
	master := make([]byte, seal.KeySize)
	if _, err := rand.Read(master); err != nil {
		t.Fatalf("rand: %v", err)
	}
	blob, err := seal.Seal(master, "librenms.token", []byte("value-generated-in-test"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	res, err := acts.PutSecretActivity(context.Background(), SecretRequest{
		Name: "librenms.token", Ciphertext: blob.Ciphertext, Nonce: blob.Nonce,
		WrappedDEK: blob.WrappedDEK, DEKNonce: blob.DEKNonce,
		Purpose: "librenms API", Rationale: "rotate the poller token", Operator: "kyriakos",
	})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if res.Ref != "store:librenms.token" {
		t.Fatalf("ref = %q", res.Ref)
	}
	got, err := seal.Open(master, "librenms.token", sec.blobs["librenms.token"])
	if err != nil || string(got) != "value-generated-in-test" {
		t.Fatalf("stored blob does not round-trip: %q %v", got, err)
	}
	entries := lg.Entries()
	if len(entries) != 1 || entries[0].Decision != "secret:put" {
		t.Fatalf("secret write not ledgered: %+v", entries)
	}
	// The ledger reason/action never carries the material.
	if e := entries[0]; strings.Contains(e.Reason, "value-generated-in-test") || strings.Contains(e.ActionID, "value-generated-in-test") {
		t.Fatalf("secret material leaked into the ledger: %+v", e)
	}
}
