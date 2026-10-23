package credential

import (
	"context"
	"errors"
	"testing"
)

// approverEntry builds a human-plane SourceEntry carrying an approver identity (no host Bundle).
func approverEntry(t *testing.T, nativeID string, kind PrincipalKind, name string, groups ...string) SourceEntry {
	t.Helper()
	a, err := NewApproverIdentity(ApproverIdentitySpec{Kind: kind, Name: name, Groups: groups})
	if err != nil {
		t.Fatalf("build approver identity: %v", err)
	}
	return SourceEntry{NativeID: nativeID, Approver: a}
}

// REQ-1611: the human-plane output type is distinct from Bundle, fail-closed by construction (zero invalid),
// carries no secret material, and normalizes/deduplicates its groups.
func TestApproverIdentityConstruction(t *testing.T) {
	if (ApproverIdentity{}).Valid() {
		t.Fatal("zero ApproverIdentity must be invalid (no blank approver)")
	}
	if _, err := NewApproverIdentity(ApproverIdentitySpec{Kind: "bogus", Name: "x"}); err == nil {
		t.Fatal("unknown principal kind must be a construction error")
	}
	if _, err := NewApproverIdentity(ApproverIdentitySpec{Kind: PrincipalUser, Name: "  "}); err == nil {
		t.Fatal("empty name must be a construction error")
	}
	a, err := NewApproverIdentity(ApproverIdentitySpec{Kind: PrincipalUser, Name: "alice", Groups: []string{"SRE", "sre", "", " oncall "}})
	if err != nil {
		t.Fatalf("valid approver: %v", err)
	}
	if !a.Valid() || a.Kind() != PrincipalUser || a.Name() != "alice" {
		t.Fatalf("approver accessors wrong: %+v", a)
	}
	// groups normalized: trimmed, empties dropped, case-deduplicated, sorted.
	got := a.Groups()
	if len(got) != 2 || got[0] != "SRE" || got[1] != "oncall" {
		t.Fatalf("groups not normalized: %v", got)
	}
}

// REQ-1611: an approver-plane source's entries resolve as approvers but NEVER as a host Bundle.
func TestHumanSourceResolvesApproversNotBundle(t *testing.T) {
	human := &fakeSource{id: "ldap", plane: PlaneHuman, entries: []SourceEntry{
		approverEntry(t, "u-alice", PrincipalUser, "alice", "sre-oncall"),
		approverEntry(t, "u-bob", PrincipalUser, "bob", "sre-oncall", "dba"),
		approverEntry(t, "g-oncall", PrincipalGroup, "sre-oncall"),
	}}
	se := NewSyncEngine(NewEngine(nil))
	if err := se.RegisterSource(human, 0); err != nil {
		t.Fatalf("register human source: %v", err)
	}
	if _, err := se.Sync(context.Background(), "ldap"); err != nil {
		t.Fatalf("human sync: %v", err)
	}

	// approvers resolve for the queried group/role.
	set, err := se.ResolveApprovers(ApproverQuery{Group: "sre-oncall"})
	if err != nil {
		t.Fatalf("ResolveApprovers: %v", err)
	}
	if len(set.Users) != 2 || set.Users[0] != "alice" || set.Users[1] != "bob" {
		t.Fatalf("expected users alice,bob, got %v", set.Users)
	}
	if len(set.Groups) != 1 || set.Groups[0] != "sre-oncall" {
		t.Fatalf("expected group sre-oncall, got %v", set.Groups)
	}
	if len(set.Sources) != 1 || set.Sources[0] != "ldap" {
		t.Fatalf("expected source ldap recorded, got %v", set.Sources)
	}

	// the SAME synced human entries NEVER populate host-credential resolution: Resolve refuses.
	if _, err := se.Resolve(Target{Host: "alice"}); !IsRefused(err) {
		t.Fatalf("a human-plane entry must not resolve as a host bundle, got %v", err)
	}
	if _, err := se.Resolve(Target{Host: "sre-oncall"}); !IsRefused(err) {
		t.Fatalf("a human-plane entry must not resolve as a host bundle, got %v", err)
	}
}

// REQ-1611: a machine-plane source's entries resolve as a Bundle but NEVER as approvers.
func TestMachineSourceResolvesBundleNotApprovers(t *testing.T) {
	machine := &fakeSource{id: "awx", plane: PlaneMachine, entries: []SourceEntry{
		entry(t, "h-1", Selector{KindHost, "host-a"}, "ops", "env:K_A"),
	}}
	se := NewSyncEngine(nil)
	if err := se.RegisterSource(machine, 0); err != nil {
		t.Fatalf("register machine source: %v", err)
	}
	if _, err := se.Sync(context.Background(), "awx"); err != nil {
		t.Fatalf("machine sync: %v", err)
	}

	// the host bundle resolves.
	res, err := se.Resolve(Target{Host: "host-a"})
	if err != nil || res.Bundle.User() != "ops" {
		t.Fatalf("machine host bundle should resolve, got res=%+v err=%v", res, err)
	}
	// the machine-plane source contributes NO approver: ResolveApprovers refuses (fail closed).
	if _, err := se.ResolveApprovers(ApproverQuery{Group: "host-a"}); !IsRefused(err) {
		t.Fatalf("a machine-plane source must never resolve an approver, got %v", err)
	}
	if _, err := se.ResolveApprovers(ApproverQuery{Group: "ops"}); !IsRefused(err) {
		t.Fatalf("a machine-plane source must never resolve an approver, got %v", err)
	}
}

// REQ-1611: a machine-plane source whose entry carries an approver identity is a cross-plane leak — the sync
// fails closed and retains prior state.
func TestMachineEntryCarryingApproverRefused(t *testing.T) {
	a, _ := NewApproverIdentity(ApproverIdentitySpec{Kind: PrincipalUser, Name: "eve"})
	leak := entry(t, "h-1", Selector{KindHost, "host-a"}, "ops", "env:K_A")
	leak.Approver = a // machine entry ALSO carries an approver → forbidden
	src := &fakeSource{id: "awx", plane: PlaneMachine, entries: []SourceEntry{leak}}
	se := NewSyncEngine(nil)
	if err := se.RegisterSource(src, 0); err != nil {
		t.Fatalf("register: %v", err)
	}
	run, err := se.Sync(context.Background(), "awx")
	if err == nil || run.Outcome != SyncFailed {
		t.Fatalf("a machine entry carrying an approver must fail closed, got run=%+v err=%v", run, err)
	}
	// nothing leaked into either plane.
	if _, err := se.Resolve(Target{Host: "host-a"}); !IsRefused(err) {
		t.Fatalf("failed cross-plane sync must not populate host resolution, got %v", err)
	}
	if _, err := se.ResolveApprovers(ApproverQuery{Group: "any"}); !IsRefused(err) {
		t.Fatalf("failed cross-plane sync must not populate approver resolution, got %v", err)
	}
}

// REQ-1611: a human-plane source whose entry carries a host Bundle is a cross-plane leak — the sync fails
// closed and retains prior state.
func TestHumanEntryCarryingBundleRefused(t *testing.T) {
	leak := approverEntry(t, "u-1", PrincipalUser, "alice", "sre")
	leak.Bundle = mustBundle(t, BundleSpec{User: "root", Port: 22, Scheme: SchemeSSH, SSHKeyRef: "env:K"}) // forbidden
	src := &fakeSource{id: "ldap", plane: PlaneHuman, entries: []SourceEntry{leak}}
	se := NewSyncEngine(nil)
	if err := se.RegisterSource(src, 0); err != nil {
		t.Fatalf("register: %v", err)
	}
	run, err := se.Sync(context.Background(), "ldap")
	if err == nil || run.Outcome != SyncFailed {
		t.Fatalf("a human entry carrying a bundle must fail closed, got run=%+v err=%v", run, err)
	}
	if _, err := se.ResolveApprovers(ApproverQuery{Group: "sre"}); !IsRefused(err) {
		t.Fatalf("failed cross-plane sync must not populate approver resolution, got %v", err)
	}
}

// REQ-1611: registering BOTH a human and a machine source on one engine keeps the planes isolated — a
// human source can't leak into Resolve(target) and a machine source can't leak into ResolveApprovers.
func TestBothPlanesRegisteredStayIsolated(t *testing.T) {
	machine := &fakeSource{id: "awx", plane: PlaneMachine, entries: []SourceEntry{
		entry(t, "h-1", Selector{KindHost, "shared-name"}, "ops", "env:K_A"),
	}}
	human := &fakeSource{id: "ldap", plane: PlaneHuman, entries: []SourceEntry{
		approverEntry(t, "u-1", PrincipalUser, "shared-name", "shared-name"),
	}}
	se := NewSyncEngine(nil)
	if err := se.RegisterSource(machine, 0); err != nil {
		t.Fatal(err)
	}
	if err := se.RegisterSource(human, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := se.SyncAll(context.Background()); err != nil {
		t.Fatalf("sync all: %v", err)
	}

	// "shared-name" as a HOST resolves ONLY the machine bundle, never the human approver.
	res, err := se.Resolve(Target{Host: "shared-name"})
	if err != nil || res.Source != "awx" || res.Bundle.User() != "ops" {
		t.Fatalf("host resolution must come from the machine plane only, got res=%+v err=%v", res, err)
	}
	// "shared-name" as a GROUP resolves ONLY the human approver, never the machine bundle, and records only
	// the human source.
	set, err := se.ResolveApprovers(ApproverQuery{Group: "shared-name"})
	if err != nil {
		t.Fatalf("approver resolution: %v", err)
	}
	if len(set.Users) != 1 || set.Users[0] != "shared-name" {
		t.Fatalf("expected the human user, got %v", set.Users)
	}
	if len(set.Sources) != 1 || set.Sources[0] != "ldap" {
		t.Fatalf("approver resolution must draw only on the human source, got %v", set.Sources)
	}
}

// REQ-1611/1602: the human plane fails closed — no human source, an empty query, an unmatched group, and a
// nil engine all refuse rather than returning a default "anyone may approve" set.
func TestResolveApproversFailClosed(t *testing.T) {
	var nilEng *SyncEngine
	if _, err := nilEng.ResolveApprovers(ApproverQuery{Group: "x"}); !IsRefused(err) {
		t.Fatalf("nil engine must refuse, got %v", err)
	}
	// engine with a human source but no matching group.
	human := &fakeSource{id: "ldap", plane: PlaneHuman, entries: []SourceEntry{
		approverEntry(t, "u-1", PrincipalUser, "alice", "sre-oncall"),
	}}
	se := NewSyncEngine(nil)
	_ = se.RegisterSource(human, 0)
	_, _ = se.Sync(context.Background(), "ldap")
	if _, err := se.ResolveApprovers(ApproverQuery{Group: ""}); !IsRefused(err) {
		t.Fatalf("empty query must refuse, got %v", err)
	}
	if _, err := se.ResolveApprovers(ApproverQuery{Group: "no-such-role"}); !IsRefused(err) {
		t.Fatalf("unmatched group must fail closed, got %v", err)
	}
	// no synced source at all → refuse.
	if _, err := NewSyncEngine(nil).ResolveApprovers(ApproverQuery{Group: "any"}); !IsRefused(err) {
		t.Fatalf("no human source must refuse, got %v", err)
	}
	// the refusal is the fail-closed sentinel.
	_, err := NewSyncEngine(nil).ResolveApprovers(ApproverQuery{Group: ""})
	if !errors.Is(err, ErrUnresolved) {
		t.Fatalf("approver refusal must wrap ErrUnresolved, got %v", err)
	}
}

// REQ-1608/1615: a human-plane approver-identity change is drift (sameEntry compares the approver).
func TestHumanApproverDriftDetected(t *testing.T) {
	human := &fakeSource{id: "ldap", plane: PlaneHuman, entries: []SourceEntry{
		approverEntry(t, "u-1", PrincipalUser, "alice", "sre-oncall"),
	}}
	se := NewSyncEngine(nil)
	_ = se.RegisterSource(human, 0)
	first, _ := se.Sync(context.Background(), "ldap")
	if first.Added != 1 {
		t.Fatalf("first human sync added=%d, want 1", first.Added)
	}
	// unchanged re-sync → zero drift.
	second, _ := se.Sync(context.Background(), "ldap")
	if second.Drifted() {
		t.Fatalf("unchanged human re-sync must converge with zero drift, got %+v", second)
	}
	// alice's group membership changes upstream → drift.
	human.entries[0] = approverEntry(t, "u-1", PrincipalUser, "alice", "dba")
	third, _ := se.Sync(context.Background(), "ldap")
	if third.Changed != 1 {
		t.Fatalf("an approver membership change must be drift, got %+v", third)
	}
	set, err := se.ResolveApprovers(ApproverQuery{Group: "dba"})
	if err != nil || len(set.Users) != 1 || set.Users[0] != "alice" {
		t.Fatalf("re-synced approver membership not reflected, got set=%+v err=%v", set, err)
	}
}

// TestPlaneConcurrentNoRace drives Sync (writer) and Resolve + ResolveApprovers (readers) concurrently across
// both planes. Under `go test -race` it fails if the human-plane resolution touches shared slot state without
// synchronization — it reads under the same RLock as Resolve.
func TestPlaneConcurrentNoRace(t *testing.T) {
	machine := &fakeSource{id: "awx", plane: PlaneMachine, entries: []SourceEntry{
		entry(t, "h-1", Selector{KindHost, "host-a"}, "ops", "env:K_A"),
	}}
	human := &fakeSource{id: "ldap", plane: PlaneHuman, entries: []SourceEntry{
		approverEntry(t, "u-1", PrincipalUser, "alice", "sre-oncall"),
	}}
	se := NewSyncEngine(nil)
	_ = se.RegisterSource(machine, 0)
	_ = se.RegisterSource(human, 1)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ {
			_, _ = se.Sync(context.Background(), "awx")
			_, _ = se.Sync(context.Background(), "ldap")
		}
		close(done)
	}()
	for r := 0; r < 4; r++ {
		go func() {
			for {
				select {
				case <-done:
					return
				default:
					_, _ = se.Resolve(Target{Host: "host-a"})
					_, _ = se.ResolveApprovers(ApproverQuery{Group: "sre-oncall"})
				}
			}
		}()
	}
	<-done
}
