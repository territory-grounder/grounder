package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/auth"
)

// THE ROLE A SESSION CARRIES MUST SURVIVE A RESTART, EXACTLY AS THE SESSION ITSELF ALREADY DOES.
//
// operator_sessions was made durable (migration 0006) for a stated reason: "browser operator sessions
// persist across grounder restarts/redeploys, so a valid cookie keeps working instead of forcing a re-login
// on every deploy". The LDAP-admin eligibility granting the elevated trace-read (REQ-2014) and admin-write
// (REQ-522) tiers was left in a process-local map, so every restart emptied it while every cookie stayed
// valid. The operator remained logged in, the console rendered normally, and one elevated surface silently
// began refusing them with no signal at all.
//
// Measured live 2026-07-29: the grounder restarted at 00:21:56Z on a routine deploy; the owner's 403s on
// /v1/sessions/{ref} began at 00:21:58Z; no re-authentication in the following six hours. A fresh login on
// the same account returned 200 on the same endpoint, proving the role was grantable and only the in-memory
// record had been lost. Every deploy that night silently downgraded every logged-in operator.
//
// ★ THE ORACLE MUST SIMULATE A RESTART, and that is the whole difficulty. A test that grants the role and
// immediately reads it back passes with the defect fully present — the process-local map answers. The only
// input that distinguishes durable from volatile is a SECOND SessionAuthenticator, built over the SAME
// store, with an empty map: that is what a redeploy is. Every existing session test used one authenticator
// throughout, which is exactly why eight months of green tests never noticed.

func openSessionFixture(t *testing.T) (context.Context, *Pool, func()) {
	t.Helper()
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the durable session-role tests")
	}
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	return ctx, p, func() { p.Close() }
}

// TestTheElevatedRoleSurvivesARestart is the regression for the live defect.
func TestTheElevatedRoleSurvivesARestart(t *testing.T) {
	ctx, p, done := openSessionFixture(t)
	defer done()
	store := NewSessionStore(p)

	const id = "session-role-survives-restart"
	clean := func() {
		if err := store.Revoke(ctx, id); err != nil {
			t.Fatalf("fixture cleanup: %v", err)
		}
	}
	clean()
	defer clean()

	if err := store.Put(ctx, id, "kyriakosp", time.Now().Add(12*time.Hour)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := store.SetAdminEligible(ctx, id, true); err != nil {
		t.Fatalf("SetAdminEligible: %v", err)
	}

	// A FRESH authenticator over the SAME store — this is precisely what a redeploy produces: the cookie and
	// its row are untouched, the process-local map is empty.
	restarted := newAuthOverStore(t, store)
	if !restarted.AdminEligible(id) {
		t.Fatal("after a restart the session lost its elevated role while its cookie stayed valid — the " +
			"operator remains logged in, the console renders normally, and the elevated surface refuses them " +
			"with a bare 403. This is the live 2026-07-29 defect: every deploy downgrades every logged-in operator.")
	}
}

// TestAPlainSessionIsNotElevatedAfterARestart — the fail-closed direction, and the reason the test above
// cannot be satisfied by returning true. A session that never held the role must not acquire one from the
// durable path; the column defaults to false and an unknown id reads false.
func TestAPlainSessionIsNotElevatedAfterARestart(t *testing.T) {
	ctx, p, done := openSessionFixture(t)
	defer done()
	store := NewSessionStore(p)

	const plain, unknown = "session-role-plain", "session-role-never-existed"
	clean := func() {
		if err := store.Revoke(ctx, plain); err != nil {
			t.Fatalf("fixture cleanup: %v", err)
		}
	}
	clean()
	defer clean()

	// Put WITHOUT SetAdminEligible — a read-only console operator.
	if err := store.Put(ctx, plain, "readonly-operator", time.Now().Add(12*time.Hour)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	restarted := newAuthOverStore(t, store)
	if restarted.AdminEligible(plain) {
		t.Error("a plain read-only session was reported as elevated after a restart — the durable column must " +
			"default to false, or every operator inherits the admin tier across a deploy")
	}
	if restarted.AdminEligible(unknown) {
		t.Error("an unknown session id was reported as elevated — an absent row must read false (fail closed), " +
			"never permissive")
	}
}

// TestRevokingASessionRevokesItsRole — logout must be authoritative for the ROLE too, not only the identity.
// A row deleted by Revoke reads false, so a resurrected cookie cannot carry the old grant.
func TestRevokingASessionRevokesItsRole(t *testing.T) {
	ctx, p, done := openSessionFixture(t)
	defer done()
	store := NewSessionStore(p)

	const id = "session-role-revoked"
	defer func() {
		if err := store.Revoke(ctx, id); err != nil {
			t.Fatalf("fixture cleanup: %v", err)
		}
	}()

	if err := store.Put(ctx, id, "kyriakosp", time.Now().Add(12*time.Hour)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := store.SetAdminEligible(ctx, id, true); err != nil {
		t.Fatalf("SetAdminEligible: %v", err)
	}
	if err := store.Revoke(ctx, id); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	eligible, err := store.AdminEligible(ctx, id)
	if err != nil {
		t.Fatalf("AdminEligible: %v", err)
	}
	if eligible {
		t.Error("a REVOKED session still reports its elevated role — logout must be authoritative for the role " +
			"as well as the identity, or the grant outlives the session that earned it")
	}
}

// newAuthOverStore builds a SessionAuthenticator over an existing store — the "process restarted" fixture.
// The key is 32 bytes because the constructor rejects anything shorter (a real guard, not a formality).
func newAuthOverStore(t *testing.T, store auth.SessionStore) *auth.SessionAuthenticator {
	t.Helper()
	a, err := auth.NewSessionAuthenticator([]byte("0123456789abcdef0123456789abcdef"), store, auth.MemOperators{}, 12*time.Hour)
	if err != nil {
		t.Fatalf("NewSessionAuthenticator: %v", err)
	}
	return a
}
