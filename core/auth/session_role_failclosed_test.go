package auth

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A DATABASE ERROR MUST NOT GRANT A ROLE.
//
// AdminEligible consults the durable RoleStore first and falls through to the process-local map. The
// fall-through is deliberate — a transient DB error must not look identical to a revoked role and downgrade
// a live operator mid-session — but it must never become permissive: an UNREADABLE grant is not a grant.
//
// This oracle exists because a mutation control demanded it and the existing suite could not answer. Flipping
// the durable read to `err != nil || eligible` (fail OPEN on error) left every core/db role test GREEN,
// because a real store against a healthy database never errors, so the branch was untested. The fixture that
// distinguishes the two is a store that FAILS, which only a fake can produce.

type erroringRoleStore struct {
	MemSessionStore
	err error
}

func (e *erroringRoleStore) SetAdminEligible(context.Context, string, bool) error { return e.err }
func (e *erroringRoleStore) AdminEligible(context.Context, string) (bool, error)  { return false, e.err }

func TestAnUnreadableRoleGrantIsNotAGrant(t *testing.T) {
	store := &erroringRoleStore{MemSessionStore: *NewMemSessionStore(), err: errors.New("connection refused")}
	a, err := NewSessionAuthenticator([]byte("0123456789abcdef0123456789abcdef"), store, MemOperators{}, time.Hour)
	if err != nil {
		t.Fatalf("NewSessionAuthenticator: %v", err)
	}
	// No in-memory grant either: this is the post-restart process whose database is also unreachable.
	if a.AdminEligible("some-session-id") {
		t.Error("a FAILED durable role read was treated as a grant — an unreadable grant is not a grant, and " +
			"this hands the elevated trace-read/admin tier to any session whenever the database blinks")
	}
}

// TestAnInMemoryGrantSurvivesAnUnreadableStore — the other half, and the reason the fall-through exists at
// all. Within one process, a session that logged in successfully keeps its role even if the durable read
// later fails; otherwise a momentary DB blip would silently downgrade an operator mid-session, which is the
// same class of invisible failure this whole change is fixing.
func TestAnInMemoryGrantSurvivesAnUnreadableStore(t *testing.T) {
	store := &erroringRoleStore{MemSessionStore: *NewMemSessionStore(), err: errors.New("connection refused")}
	a, err := NewSessionAuthenticator([]byte("0123456789abcdef0123456789abcdef"), store, MemOperators{}, time.Hour)
	if err != nil {
		t.Fatalf("NewSessionAuthenticator: %v", err)
	}
	a.markAdminEligible("live-session") // the login path, whose durable write also failed (best-effort)
	if !a.AdminEligible("live-session") {
		t.Error("a session that legitimately earned the role in THIS process lost it because the durable read " +
			"failed — a DB blip must not revoke a live operator's role")
	}
}
