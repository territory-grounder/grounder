package credential

import (
	"context"
	"errors"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

// fakeSource is the in-memory CredentialSource that drives the sync-framework oracles (CI has no external
// platform). Its upstream entry set is mutable so a test can prove re-read-by-id (INV-05) and drift.
type fakeSource struct {
	id      string
	plane   Plane
	entries []SourceEntry
	err     error // when set, Sync fails (unreachable/denied) and the framework must fail closed
	calls   int
}

func (f *fakeSource) ID() string   { return f.id }
func (f *fakeSource) Plane() Plane { return f.plane }
func (f *fakeSource) Sync(ctx context.Context) ([]SourceEntry, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	// return a COPY of the current upstream set (re-read by id, not a shared mutable slice).
	out := make([]SourceEntry, len(f.entries))
	copy(out, f.entries)
	return out, nil
}

func entry(t *testing.T, nativeID string, sel Selector, user, ref string) SourceEntry {
	t.Helper()
	b, err := NewBundle(BundleSpec{User: user, Port: 22, Scheme: SchemeSSH, SSHKeyRef: config.SecretRef(ref)})
	if err != nil {
		t.Fatalf("build entry bundle: %v", err)
	}
	return SourceEntry{NativeID: nativeID, Selector: sel, Bundle: b}
}

// TestSyncResolveConcurrentNoRace drives Sync (writer) and Resolve/LastRun (readers) concurrently. Under
// `go test -race` it fails if the SyncEngine's shared slot state (rules/entries/lastRun) is touched without
// synchronization — the blocker the MR !351 review found (Temporal schedule Syncs while HTTP handlers
// Resolve). It passes only because Sync converges under a write lock and Resolve/LastRun read under RLock.
func TestSyncResolveConcurrentNoRace(t *testing.T) {
	src := &fakeSource{id: "awx", plane: PlaneMachine, entries: []SourceEntry{
		entry(t, "n1", Selector{KindHost, "host-a"}, "ops", "env:K_A"),
	}}
	se := NewSyncEngine(NewEngine(nil))
	if err := se.RegisterSource(src, 0); err != nil {
		t.Fatalf("register: %v", err)
	}
	done := make(chan struct{})
	// One writer: repeatedly re-syncs (rewrites slot.rules/entries/lastRun).
	go func() {
		for i := 0; i < 500; i++ {
			_, _ = se.Sync(context.Background(), "awx")
		}
		close(done)
	}()
	// Several readers: Resolve + LastRun hammer the same slots while the writer converges.
	for r := 0; r < 4; r++ {
		go func() {
			for {
				select {
				case <-done:
					return
				default:
					_, _ = se.Resolve(Target{Host: "host-a"})
					_, _ = se.LastRun("awx")
				}
			}
		}()
	}
	<-done
}

// REQ-1607: a source syncs read-only into the store on demand and its entries become resolvable.
func TestSyncMakesEntriesResolvable(t *testing.T) {
	src := &fakeSource{id: "awx", plane: PlaneMachine, entries: []SourceEntry{
		entry(t, "h-100", Selector{KindHost, "host-a"}, "awxuser", "env:A"),
	}}
	se := NewSyncEngine(NewEngine(nil))
	if err := se.RegisterSource(src, 0); err != nil {
		t.Fatalf("register: %v", err)
	}
	// before sync: nothing resolves (fail closed).
	if _, err := se.Resolve(Target{Host: "host-a"}); !IsRefused(err) {
		t.Fatalf("pre-sync must refuse, got %v", err)
	}
	run, err := se.Sync(context.Background(), "awx")
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if run.Outcome != SyncOK || run.Added != 1 {
		t.Fatalf("expected 1 added ok run, got %+v", run)
	}
	res, err := se.Resolve(Target{Host: "host-a"})
	if err != nil {
		t.Fatalf("resolve after sync: %v", err)
	}
	if res.Bundle.User() != "awxuser" || res.Source != "awx" || res.Native {
		t.Fatalf("resolved wrong: %+v", res)
	}
}

// REQ-1607 (INV-05): a re-sync re-reads the source by id, so an upstream change is reflected.
func TestReSyncReReadsUpstream(t *testing.T) {
	src := &fakeSource{id: "awx", plane: PlaneMachine, entries: []SourceEntry{
		entry(t, "h-100", Selector{KindHost, "host-a"}, "olduser", "env:A"),
	}}
	se := NewSyncEngine(nil)
	_ = se.RegisterSource(src, 0)
	if _, err := se.Sync(context.Background(), "awx"); err != nil {
		t.Fatal(err)
	}
	// upstream changes the user on the SAME native id.
	src.entries[0] = entry(t, "h-100", Selector{KindHost, "host-a"}, "newuser", "env:A")
	run, err := se.Sync(context.Background(), "awx")
	if err != nil {
		t.Fatal(err)
	}
	if run.Changed != 1 || run.Added != 0 || run.Removed != 0 {
		t.Fatalf("expected changed=1, got %+v", run)
	}
	res, _ := se.Resolve(Target{Host: "host-a"})
	if res.Bundle.User() != "newuser" {
		t.Fatalf("re-sync did not re-read upstream: %q", res.Bundle.User())
	}
}

// REQ-1608: a repeated sync of unchanged data converges — no duplicate, no orphan, zero drift.
func TestSyncIdempotent(t *testing.T) {
	src := &fakeSource{id: "awx", plane: PlaneMachine, entries: []SourceEntry{
		entry(t, "h-1", Selector{KindHost, "host-a"}, "u1", "env:A"),
		entry(t, "h-2", Selector{KindHost, "host-b"}, "u2", "env:B"),
	}}
	se := NewSyncEngine(nil)
	_ = se.RegisterSource(src, 0)
	first, _ := se.Sync(context.Background(), "awx")
	if first.Added != 2 {
		t.Fatalf("first sync added=%d, want 2", first.Added)
	}
	second, _ := se.Sync(context.Background(), "awx")
	if second.Added != 0 || second.Changed != 0 || second.Removed != 0 || second.Drifted() {
		t.Fatalf("re-sync of unchanged data must converge with zero drift, got %+v", second)
	}
	// both hosts still resolve exactly once each (no duplication).
	if _, err := se.Resolve(Target{Host: "host-a"}); err != nil {
		t.Fatalf("host-a lost after re-sync: %v", err)
	}
	if _, err := se.Resolve(Target{Host: "host-b"}); err != nil {
		t.Fatalf("host-b lost after re-sync: %v", err)
	}
}

// REQ-1608: an upstream removal removes the local bundle (no orphan).
func TestSyncRemovesOrphan(t *testing.T) {
	src := &fakeSource{id: "awx", plane: PlaneMachine, entries: []SourceEntry{
		entry(t, "h-1", Selector{KindHost, "host-a"}, "u1", "env:A"),
		entry(t, "h-2", Selector{KindHost, "host-b"}, "u2", "env:B"),
	}}
	se := NewSyncEngine(nil)
	_ = se.RegisterSource(src, 0)
	_, _ = se.Sync(context.Background(), "awx")
	// upstream drops host-b.
	src.entries = src.entries[:1]
	run, _ := se.Sync(context.Background(), "awx")
	if run.Removed != 1 {
		t.Fatalf("expected removed=1, got %+v", run)
	}
	if _, err := se.Resolve(Target{Host: "host-b"}); !IsRefused(err) {
		t.Fatalf("removed host-b must now refuse (no orphan), got %v", err)
	}
}

// REQ-1608: a duplicate native id in one pull collapses to a single entry (no duplication).
func TestSyncCollapsesDuplicateNativeID(t *testing.T) {
	src := &fakeSource{id: "awx", plane: PlaneMachine, entries: []SourceEntry{
		entry(t, "dup", Selector{KindHost, "host-a"}, "first", "env:A"),
		entry(t, "dup", Selector{KindHost, "host-a"}, "second", "env:B"),
	}}
	se := NewSyncEngine(nil)
	_ = se.RegisterSource(src, 0)
	run, _ := se.Sync(context.Background(), "awx")
	if run.Added != 1 {
		t.Fatalf("duplicate native id must collapse to one entry, got added=%d", run.Added)
	}
}

// REQ-1609: a target in two sources resolves by declared precedence and records the shadowed source.
func TestCrossSourcePrecedence(t *testing.T) {
	high := &fakeSource{id: "vault", plane: PlaneMachine, entries: []SourceEntry{
		entry(t, "v-1", Selector{KindHost, "host-a"}, "vaultuser", "env:V"),
	}}
	low := &fakeSource{id: "awx", plane: PlaneMachine, entries: []SourceEntry{
		entry(t, "a-1", Selector{KindHost, "host-a"}, "awxuser", "env:A"),
	}}
	se := NewSyncEngine(nil)
	_ = se.RegisterSource(high, 0) // higher precedence (lower value)
	_ = se.RegisterSource(low, 10)
	_, _ = se.SyncAll(context.Background())

	res, err := se.Resolve(Target{Host: "host-a"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Source != "vault" || res.Bundle.User() != "vaultuser" {
		t.Fatalf("expected vault to win, got %+v", res)
	}
	if len(res.Shadowed) != 1 || res.Shadowed[0] != "awx" {
		t.Fatalf("expected awx shadowed, got %v", res.Shadowed)
	}
}

// REQ-1609: equal-precedence sources covering the same target fail closed (precedence didn't disambiguate).
func TestCrossSourceEqualPrecedenceRefuses(t *testing.T) {
	a := &fakeSource{id: "awx", plane: PlaneMachine, entries: []SourceEntry{
		entry(t, "a-1", Selector{KindHost, "host-a"}, "auser", "env:A"),
	}}
	b := &fakeSource{id: "semaphore", plane: PlaneMachine, entries: []SourceEntry{
		entry(t, "b-1", Selector{KindHost, "host-a"}, "buser", "env:B"),
	}}
	se := NewSyncEngine(nil)
	_ = se.RegisterSource(a, 5)
	_ = se.RegisterSource(b, 5) // SAME precedence → ambiguous
	_, _ = se.SyncAll(context.Background())

	_, err := se.Resolve(Target{Host: "host-a"})
	if !errors.Is(err, ErrAmbiguous) || !IsRefused(err) {
		t.Fatalf("equal-precedence conflict must fail closed as ambiguous, got %v", err)
	}
}

// REQ-1610: a target no synced source covers resolves from the native-store fallback.
func TestNativeFallback(t *testing.T) {
	nativeRules, err := ParseRules("host:native-host|nativeuser|22|ssh|env:N")
	if err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{id: "awx", plane: PlaneMachine, entries: []SourceEntry{
		entry(t, "a-1", Selector{KindHost, "synced-host"}, "awxuser", "env:A"),
	}}
	se := NewSyncEngine(NewEngine(nativeRules))
	_ = se.RegisterSource(src, 0)
	_, _ = se.Sync(context.Background(), "awx")

	// covered by native only → native fallback wins.
	res, err := se.Resolve(Target{Host: "native-host"})
	if err != nil {
		t.Fatalf("native fallback resolve: %v", err)
	}
	if !res.Native || res.Source != "" || res.Bundle.User() != "nativeuser" {
		t.Fatalf("expected native fallback, got %+v", res)
	}
	// covered by a source → the source wins over native (native is fallback only).
	res2, _ := se.Resolve(Target{Host: "synced-host"})
	if res2.Native || res2.Source != "awx" {
		t.Fatalf("synced target should resolve from the source, got %+v", res2)
	}
	// covered by neither → fail closed.
	if _, err := se.Resolve(Target{Host: "nowhere"}); !IsRefused(err) {
		t.Fatalf("uncovered target must refuse, got %v", err)
	}
}

// REQ-1615: a failed sync is recorded, leaves the prior converged state intact, and never advances
// last-synced or falls open.
func TestFailedSyncRetainsPriorState(t *testing.T) {
	src := &fakeSource{id: "awx", plane: PlaneMachine, entries: []SourceEntry{
		entry(t, "a-1", Selector{KindHost, "host-a"}, "gooduser", "env:A"),
	}}
	se := NewSyncEngine(nil)
	_ = se.RegisterSource(src, 0)
	okRun, _ := se.Sync(context.Background(), "awx")

	// now the backend goes unreachable.
	src.err = errors.New("dial tcp: connection refused")
	failRun, err := se.Sync(context.Background(), "awx")
	if err == nil || failRun.Outcome != SyncFailed || failRun.Err == "" {
		t.Fatalf("expected a recorded failed run, got run=%+v err=%v", failRun, err)
	}
	// last-synced did not advance past the successful run.
	if !failRun.LastSyncedAt.Equal(okRun.LastSyncedAt) {
		t.Fatalf("failed sync advanced last-synced: ok=%v fail=%v", okRun.LastSyncedAt, failRun.LastSyncedAt)
	}
	// prior converged entry still resolves (state intact, not wiped, not fallen open to a default).
	if res, err := se.Resolve(Target{Host: "host-a"}); err != nil || res.Bundle.User() != "gooduser" {
		t.Fatalf("prior state lost after failed sync: res=%+v err=%v", res, err)
	}
	// LastRun surfaces the failed status for the console.
	lr, ok := se.LastRun("awx")
	if !ok || lr.Outcome != SyncFailed {
		t.Fatalf("LastRun should report the failed run, got %+v ok=%v", lr, ok)
	}
}

// REQ-1608/1615: a malformed entry (empty native id or invalid bundle) fails the sync closed and retains
// prior state.
func TestSyncRejectsMalformedEntry(t *testing.T) {
	src := &fakeSource{id: "awx", plane: PlaneMachine, entries: []SourceEntry{
		{NativeID: "", Selector: Selector{KindHost, "host-a"}, Bundle: mustBundle(t, BundleSpec{User: "u", Port: 22, Scheme: SchemeSSH, SSHKeyRef: "env:A"})},
	}}
	se := NewSyncEngine(nil)
	_ = se.RegisterSource(src, 0)
	if _, err := se.Sync(context.Background(), "awx"); err == nil {
		t.Fatal("expected a malformed entry (empty native id) to fail the sync closed")
	}
	// invalid (zero) bundle is also rejected.
	src.entries = []SourceEntry{{NativeID: "x", Selector: Selector{KindHost, "host-a"}, Bundle: Bundle{}}}
	if _, err := se.Sync(context.Background(), "awx"); err == nil {
		t.Fatal("expected a zero-bundle entry to fail the sync closed")
	}
}

// Registration guards: nil source, empty id, duplicate id, unknown plane are all refused.
func TestRegisterSourceGuards(t *testing.T) {
	se := NewSyncEngine(nil)
	if err := se.RegisterSource(nil, 0); err == nil {
		t.Fatal("nil source must be rejected")
	}
	if err := se.RegisterSource(&fakeSource{id: "", plane: PlaneMachine}, 0); err == nil {
		t.Fatal("empty id must be rejected")
	}
	if err := se.RegisterSource(&fakeSource{id: "x", plane: "bogus"}, 0); err == nil {
		t.Fatal("unknown plane must be rejected")
	}
	if err := se.RegisterSource(&fakeSource{id: "dup", plane: PlaneMachine}, 0); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := se.RegisterSource(&fakeSource{id: "dup", plane: PlaneMachine}, 0); err == nil {
		t.Fatal("duplicate id must be rejected")
	}
}

// A nil SyncEngine and a zero-source engine both fail closed by construction.
func TestSyncEngineFailClosedByConstruction(t *testing.T) {
	var nilEng *SyncEngine
	if _, err := nilEng.Resolve(Target{Host: "x"}); !IsRefused(err) {
		t.Fatalf("nil SyncEngine must refuse, got %v", err)
	}
	empty := NewSyncEngine(nil)
	if _, err := empty.Resolve(Target{Host: "x"}); !IsRefused(err) {
		t.Fatalf("zero-source, no-native engine must refuse, got %v", err)
	}
}
