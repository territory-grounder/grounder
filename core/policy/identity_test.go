package policy

import (
	"context"
	"errors"
	"testing"

	"github.com/territory-grounder/grounder/core/credential"
)

// fakeHumanSource is a minimal PlaneHuman credential.CredentialSource that seeds approver identities for the
// SHARED human plane — the SAME store approve_by resolves through (no forked identity store in this test).
type fakeHumanSource struct {
	id      string
	entries []credential.SourceEntry
}

func (s *fakeHumanSource) ID() string              { return s.id }
func (s *fakeHumanSource) Plane() credential.Plane { return credential.PlaneHuman }
func (s *fakeHumanSource) Sync(context.Context) ([]credential.SourceEntry, error) {
	return s.entries, nil
}

// humanPlane builds a synced credential.SyncEngine holding the given approvers on the human plane.
func humanPlane(t *testing.T, entries ...credential.SourceEntry) *credential.SyncEngine {
	t.Helper()
	se := credential.NewSyncEngine(nil)
	if err := se.RegisterSource(&fakeHumanSource{id: "ldap", entries: entries}, 0); err != nil {
		t.Fatalf("register human source: %v", err)
	}
	if _, err := se.Sync(context.Background(), "ldap"); err != nil {
		t.Fatalf("human sync: %v", err)
	}
	return se
}

func approver(t *testing.T, nativeID string, kind credential.PrincipalKind, name string, groups ...string) credential.SourceEntry {
	t.Helper()
	a, err := credential.NewApproverIdentity(credential.ApproverIdentitySpec{Kind: kind, Name: name, Groups: groups})
	if err != nil {
		t.Fatalf("build approver identity: %v", err)
	}
	return credential.SourceEntry{NativeID: nativeID, Approver: a}
}

// errResolver always fails — models an identity backend that cannot answer (→ deny, fail closed).
type errResolver struct{}

func (errResolver) Resolve(context.Context, string) (Principal, error) {
	return Principal{}, errors.New("identity backend unreachable")
}

// ---------------------------------------------------------------------------------------------------------

// REQ-1516: a vote from a principal named DIRECTLY in approve_by (a user: entry) is admitted.
// Binds acceptance scenario "A vote from an approve_by member is admitted".
func TestMayApprove_MemberByID_Admitted(t *testing.T) {
	r := NewLocalResolver(nil, LocalPrincipal{ID: "alice"})
	ok, dec := MayApprove(context.Background(), r, "alice", []string{"user:alice", "group:sre-oncall"})
	if !ok || !dec.Allowed {
		t.Fatalf("actor named in approve_by must be admitted, got %+v", dec)
	}
	if dec.MatchedEntry != "user:alice" {
		t.Fatalf("expected matched entry user:alice, got %q", dec.MatchedEntry)
	}
}

// REQ-1516: a vote from an actor in a GROUP named in approve_by is admitted — via the SHARED credential human
// plane's group resolution (ResolveApprovers), not a second store.
func TestMayApprove_MemberByHumanPlaneGroup_Admitted(t *testing.T) {
	hp := humanPlane(t, approver(t, "u-bob", credential.PrincipalUser, "bob", "sre-oncall"))
	r := NewLocalResolver(hp) // bob is NOT in the local registry — only the human plane knows him
	ok, dec := MayApprove(context.Background(), r, "bob", []string{"group:sre-oncall"})
	if !ok || !dec.Allowed {
		t.Fatalf("actor in an approve_by group (human plane) must be admitted, got %+v", dec)
	}
	if dec.MatchedEntry != "group:sre-oncall" {
		t.Fatalf("expected matched entry group:sre-oncall, got %q", dec.MatchedEntry)
	}
}

// REQ-1516: a vote from an actor in a GROUP named in approve_by is admitted via the LOCAL registry too.
func TestMayApprove_MemberByLocalGroup_Admitted(t *testing.T) {
	r := NewLocalResolver(nil, LocalPrincipal{ID: "carol", Groups: []string{"dba"}})
	ok, dec := MayApprove(context.Background(), r, "carol", []string{"group:dba"})
	if !ok || !dec.Allowed {
		t.Fatalf("actor in a local approve_by group must be admitted, got %+v", dec)
	}
}

// REQ-1516: a vote from a principal in NEITHER approve_by (not named, in no named group) is rejected.
// Binds acceptance scenario "A vote from a non-member is rejected".
func TestMayApprove_NonMember_Rejected(t *testing.T) {
	hp := humanPlane(t, approver(t, "u-bob", credential.PrincipalUser, "bob", "sre-oncall"))
	r := NewLocalResolver(hp, LocalPrincipal{ID: "mallory", Groups: []string{"interns"}})
	ok, dec := MayApprove(context.Background(), r, "mallory", []string{"user:alice", "group:sre-oncall"})
	if ok || dec.Allowed {
		t.Fatalf("a non-member must be rejected, got %+v", dec)
	}
	if dec.MatchedEntry != "" {
		t.Fatalf("a rejected vote matches no entry, got %q", dec.MatchedEntry)
	}
}

// REQ-1516: a resolver error → DENY (fail closed).
func TestMayApprove_ResolverError_Denied(t *testing.T) {
	ok, dec := MayApprove(context.Background(), errResolver{}, "alice", []string{"user:alice"})
	if ok || dec.Allowed {
		t.Fatalf("a resolver error must deny (fail closed), got %+v", dec)
	}
}

// REQ-1516: empty approve_by → DENY (no members, so no one may approve — the safer reading, consistent with
// the human plane's "never a default 'anyone may approve'", core/credential/plane.go REQ-1602/1611).
func TestMayApprove_EmptyApproveBy_Denied(t *testing.T) {
	r := NewLocalResolver(nil, LocalPrincipal{ID: "alice", Groups: []string{"sre-oncall"}})
	for _, ab := range [][]string{nil, {}, {"", "  "}} {
		if ok, dec := MayApprove(context.Background(), r, "alice", ab); ok || dec.Allowed {
			t.Fatalf("empty approve_by must deny, got ok=%v %+v (approveBy=%v)", ok, dec, ab)
		}
	}
}

// REQ-1516: a nil resolver → DENY (fail closed).
func TestMayApprove_NilResolver_Denied(t *testing.T) {
	if ok, _ := MayApprove(context.Background(), nil, "alice", []string{"user:alice"}); ok {
		t.Fatal("a nil resolver must deny (fail closed)")
	}
}

// REQ-1516: group-name matching is case-insensitive / trimmed, consistent with the credential human plane
// (plane.go approverMatches uses EqualFold; normalizeGroups trims/dedups/sorts).
func TestMayApprove_GroupNormalizationMatchesHumanPlane(t *testing.T) {
	// local registry: mixed case + whitespace → normalized to match a differently-cased approve_by entry.
	r := NewLocalResolver(nil, LocalPrincipal{ID: "dave", Groups: []string{"  SRE-Oncall "}})
	if ok, _ := MayApprove(context.Background(), r, "dave", []string{"group:sre-oncall"}); !ok {
		t.Fatal("local group match must be case-insensitive / trimmed")
	}
	// human plane: an approver group with different casing resolves for a differently-cased approve_by entry.
	hp := humanPlane(t, approver(t, "u-erin", credential.PrincipalUser, "erin", "Net-Admins"))
	rh := NewLocalResolver(hp)
	if ok, _ := MayApprove(context.Background(), rh, "erin", []string{"group:net-admins"}); !ok {
		t.Fatal("human-plane group match must be case-insensitive, consistent with the human plane")
	}
}

// REQ-1516: an unknown actor (in no registry, no human-plane group) is a member of nothing → DENY.
func TestMayApprove_UnknownActor_Denied(t *testing.T) {
	hp := humanPlane(t, approver(t, "u-bob", credential.PrincipalUser, "bob", "sre-oncall"))
	r := NewLocalResolver(hp)
	if ok, dec := MayApprove(context.Background(), r, "nobody", []string{"user:alice", "group:sre-oncall"}); ok {
		t.Fatalf("an unknown actor must be denied, got %+v", dec)
	}
}

// Negative control (spoof-resistance): a crafted actor id / group cannot forge membership. An actor whose id
// merely resembles a member's, and whose claimed local group is NOT the approve_by group, is rejected — the
// human plane (source of truth) never named it.
func TestMayApprove_SpoofRejected(t *testing.T) {
	hp := humanPlane(t, approver(t, "u-bob", credential.PrincipalUser, "bob", "sre-oncall"))
	// attacker "bob-evil" self-declares a look-alike local group but is not in the real approve_by group,
	// and the human plane knows nothing about it.
	r := NewLocalResolver(hp, LocalPrincipal{ID: "bob-evil", Groups: []string{"sre-oncall-but-not-really"}})
	if ok, dec := MayApprove(context.Background(), r, "bob-evil", []string{"user:bob", "group:sre-oncall"}); ok {
		t.Fatalf("a crafted id/group must not spoof membership, got %+v", dec)
	}
}

// The local resolver composes with the human plane WITHOUT forking a store: a user in the local registry and
// a user only in the human plane both resolve, and Resolve fails closed on an empty actor id.
func TestLocalResolver_ComposesAndFailsClosed(t *testing.T) {
	hp := humanPlane(t, approver(t, "u-bob", credential.PrincipalUser, "bob", "sre-oncall"))
	r := NewLocalResolver(hp, LocalPrincipal{ID: "alice", Groups: []string{"dba"}})

	p, err := r.Resolve(context.Background(), "alice")
	if err != nil || p.ID != "alice" || len(p.Groups) != 1 || p.Groups[0] != "dba" {
		t.Fatalf("local principal resolve wrong: p=%+v err=%v", p, err)
	}
	if _, err := r.Resolve(context.Background(), "  "); err == nil {
		t.Fatal("an empty actor id must fail closed")
	}
	// human-plane membership is resolved through credential.ResolveApprovers (the shared store).
	member, err := r.groupMember(context.Background(), "bob", "sre-oncall")
	if err != nil || !member {
		t.Fatalf("human-plane group membership must resolve via the shared plane, got member=%v err=%v", member, err)
	}
	// a group the human plane names no member for is a fail-closed non-match, not an error.
	member, err = r.groupMember(context.Background(), "bob", "no-such-group")
	if err != nil || member {
		t.Fatalf("unresolved human-plane group must be a non-match (not an error), got member=%v err=%v", member, err)
	}
}
