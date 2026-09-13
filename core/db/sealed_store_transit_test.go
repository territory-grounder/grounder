package db

// ORACLE FOR TRANSIT-SEALED PERSISTENCE (TG-276).
//
// THE DEFECT, measured live on 2026-08-04: every OpenBao Transit secret write failed at the database.
//
//	db: sealed put: ERROR: null value in column "dek_nonce" of relation "sealed_secret"
//	violates not-null constraint (SQLSTATE 23502)
//
// A DEK wrapper whose ciphertext is self-describing has no separate nonce, and the Transit wrapper returns
// nil for it — stated outright at core/seal/transit.go:91. pgx renders a nil []byte as SQL NULL, and the
// column is `bytea NOT NULL`. So on a Transit deployment the sealed store could never hold one row, and the
// first SecretPutWorkflow ever executed on the production system is the one that proved it.
//
// Nothing caught this because the LOCAL wrapper always produces a real DEK nonce, so every sealing test in
// the tree passes. The two halves were correct and were never run against each other. This test is the
// missing meeting point: it must run against a REAL Postgres, because the whole failure IS the constraint.

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/seal"
)

func skipWithoutSealDB(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TG_TEST_DSN")
	if dsn == "" {
		t.Skip("TG_TEST_DSN not set — this oracle is ABOUT a NOT NULL constraint, so a fake pool would " +
			"assert nothing. CI supplies a real Postgres with every migration applied.")
	}
	return dsn
}

// KILLING MUTATION: drop bytesOrEmpty from the DEKNonce argument in Put. RED with SQLSTATE 23502 — the
// exact production error. This is the whole bug in one assertion.
func TestATransitSealedBlobWithNoSeparateNoncePersists(t *testing.T) {
	ctx := context.Background()
	p, err := Connect(ctx, skipWithoutSealDB(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()
	s := NewSealedSecretStore(p)
	const name = "tg276-transit-shape"
	defer func() { _, _ = p.Exec(ctx, `DELETE FROM sealed_secret WHERE name = $1`, name) }()
	_, _ = p.Exec(ctx, `DELETE FROM sealed_secret WHERE name = $1`, name)

	// EXACTLY the shape transitWrapper.WrapDEK produces: a self-describing "vault:v1:…" wrapped DEK and a
	// nil DEK nonce. Hand-built rather than taken from a live OpenBao so the oracle needs no network.
	blob := seal.Sealed{
		Ciphertext: []byte("ciphertext-bytes"),
		Nonce:      []byte("aead-nonce-12"),
		WrappedDEK: []byte("vault:v1:Zm9vYmFyYmF6"),
		DEKNonce:   nil, // <- Transit carries its own; this is the field that was NULL
	}
	if err := s.Put(ctx, name, blob, "tg276 oracle", "test", 1, 1); err != nil {
		t.Fatalf("a Transit-shaped blob could not be stored: %v\n\nThis is the production failure: an "+
			"operator writing a credential through the console gets 503 forever, and the sealed store "+
			"stays permanently empty on every Transit deployment.", err)
	}

	got, found, err := s.Get(ctx, name)
	if err != nil || !found {
		t.Fatalf("stored blob did not read back (found=%v err=%v)", found, err)
	}
	// It must come back as EMPTY, not NULL and not invented: the unwrap path passes this field straight to
	// the wrapper, and a wrapper handed unexpected bytes fails closed on a secret that is actually intact.
	if got.DEKNonce == nil || len(got.DEKNonce) != 0 {
		t.Fatalf("DEKNonce round-tripped as %#v, want empty-and-non-nil — 'no separate nonce' is a fact "+
			"about the wrapper, and it must survive the database unchanged", got.DEKNonce)
	}
	if string(got.WrappedDEK) != string(blob.WrappedDEK) || string(got.Ciphertext) != string(blob.Ciphertext) {
		t.Fatalf("the material itself did not round-trip: wrapped=%q ciphertext=%q",
			got.WrappedDEK, got.Ciphertext)
	}
}

// THE CONTROL, so the fix cannot degrade into "write NULLs as empty everywhere". A locally-wrapped blob
// carries a REAL DEK nonce and must be stored byte-for-byte — silently emptying it would make every
// local-master-key secret permanently un-openable, which is worse than the bug being fixed.
func TestALocallyWrappedBlobKeepsItsRealNonce(t *testing.T) {
	ctx := context.Background()
	p, err := Connect(ctx, skipWithoutSealDB(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()
	s := NewSealedSecretStore(p)
	const name = "tg276-local-shape"
	defer func() { _, _ = p.Exec(ctx, `DELETE FROM sealed_secret WHERE name = $1`, name) }()
	_, _ = p.Exec(ctx, `DELETE FROM sealed_secret WHERE name = $1`, name)

	realNonce := []byte("\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c")
	blob := seal.Sealed{
		Ciphertext: []byte("ct"), Nonce: []byte("n"), WrappedDEK: []byte("wrapped"), DEKNonce: realNonce,
	}
	if err := s.Put(ctx, name, blob, "tg276 control", "test", 2, 1); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, _, err := s.Get(ctx, name)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got.DEKNonce) != string(realNonce) {
		t.Fatalf("DEKNonce came back %q, want %q — a mangled nonce means the secret can never be "+
			"unsealed again, and it fails as a corruption error long after the write", got.DEKNonce, realNonce)
	}
}

// VACUITY FLOOR on the whole file: if the schema ever loses the constraint, the oracle above passes for a
// reason that has nothing to do with the fix, and the fix could then be reverted unnoticed.
func TestTheDekNonceConstraintStillExists(t *testing.T) {
	ctx := context.Background()
	p, err := Connect(ctx, skipWithoutSealDB(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()
	var nullable string
	if err := p.Pool.QueryRow(ctx,
		`SELECT is_nullable FROM information_schema.columns
		  WHERE table_name = 'sealed_secret' AND column_name = 'dek_nonce'`).Scan(&nullable); err != nil {
		t.Fatalf("could not read the column definition: %v", err)
	}
	if !strings.EqualFold(nullable, "NO") {
		t.Fatalf("sealed_secret.dek_nonce is nullable — the oracle above no longer proves anything, and " +
			"a blob whose nonce was genuinely forgotten can now be stored as NULL")
	}
}
