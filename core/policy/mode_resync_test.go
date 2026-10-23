package policy

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// ORACLES FOR THE CROSS-PROCESS MODE (TG-570, spec/015 REQ-1526).
//
// Production runs a triage worker and an actuation worker, each with its own ModeController over the ONE
// persisted policy_mode row. Live, 2026-09-30 → 10-09: a recon burst force-Shadowed the triage worker and
// persisted Shadow, and the actuation worker — which had read the row once, at boot — kept serving Semi-auto
// and executed eleven guest starts after the kill. Each test below builds TWO controllers over one store,
// because a single-controller test cannot see the defect at all.

// twoPlanes builds the triage and actuation controllers over one shared store, both in mode m.
func twoPlanes(t *testing.T, m Mode) (store *MemModeStore, triage, actuation *ModeController) {
	t.Helper()
	ctx := context.Background()
	store = NewMemModeStore()
	if err := store.Save(ctx, m); err != nil {
		t.Fatal(err)
	}
	triage = NewModeController(ctx, store, newLedger(), allowAuthz{}, greenPreflight{}, nil)
	actuation = NewModeController(ctx, store, newLedger(), allowAuthz{}, greenPreflight{}, nil)
	if triage.Current() != m || actuation.Current() != m {
		t.Fatalf("vacuity floor: both planes must boot in %s, got triage=%s actuation=%s", m, triage.Current(), actuation.Current())
	}
	return store, triage, actuation
}

// THE LIVE DEFECT. A kill in the triage process must stop the process that actuates.
//
// KILLING MUTATION: make Resync return c.Current() without reading the store (the pre-TG-570 behaviour). RED —
// the actuation plane stays Semi-auto over a stored Shadow, exactly as it did for nine days in production.
func TestResyncCarriesAKillFromAnotherProcessToTheActuatingPlane(t *testing.T) {
	ctx := context.Background()
	_, triage, actuation := twoPlanes(t, ModeSemiAuto)

	triage.ForceShadow("recon burst: 150 estate reads across 32 distinct targets in 5m0s")
	if actuation.Current() != ModeSemiAuto {
		t.Fatal("vacuity floor: before a resync the actuation plane still holds its boot copy — the defect")
	}
	if got := actuation.Resync(ctx); got != ModeShadow || actuation.Current() != ModeShadow {
		t.Fatalf("a kill persisted by the triage plane must reach the actuation plane on its next resync; "+
			"actuation plane is %s (Resync returned %s) — the kill does not stop the process that actuates",
			actuation.Current(), got)
	}
}

// The other half of the same gap: an operator's POST /v1/mode runs on the runner plane only, so a restore (or a
// deliberate Shadow) never reached the actuation plane until it restarted.
//
// KILLING MUTATION: as above. RED — the owner's restore to Semi-auto never reaches the plane that actuates.
func TestResyncCarriesAnOperatorTransitionToEveryPlane(t *testing.T) {
	ctx := context.Background()
	_, triage, actuation := twoPlanes(t, ModeShadow)

	if err := triage.Transition(ctx, ModeShadow, ModeSemiAuto, "owner", "restore after a false recon-burst kill"); err != nil {
		t.Fatal(err)
	}
	if got := actuation.Resync(ctx); got != ModeSemiAuto {
		t.Fatalf("an operator escalation recorded on the runner plane must reach the actuation plane; got %s", got)
	}
	if err := triage.Transition(ctx, ModeSemiAuto, ModeShadow, "owner", "stop"); err != nil {
		t.Fatal(err)
	}
	if got := actuation.Resync(ctx); got != ModeShadow {
		t.Fatalf("an operator's Shadow recorded on the runner plane must reach the actuation plane; got %s", got)
	}
}

// saveFailingStore is a ModeStore whose Save can be made to fail — ForceShadow's persist is best-effort.
type saveFailingStore struct {
	*MemModeStore
	mu      sync.Mutex
	saveErr error
}

func (s *saveFailingStore) Save(ctx context.Context, m Mode) error {
	s.mu.Lock()
	err := s.saveErr
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return s.MemModeStore.Save(ctx, m)
}

func (s *saveFailingStore) failSaves(err error) {
	s.mu.Lock()
	s.saveErr = err
	s.mu.Unlock()
}

// A KILL WHOSE PERSIST FAILED MUST NOT BE UNDONE BY THE STALE ROW. ForceShadow sets Shadow in memory first and
// persists best-effort; if the persist fails, the store still holds the pre-kill actuating mode. Resync must
// not read that row back in. It must also put the kill on record as soon as the store accepts writes, so every
// other process stops too and a later operator resume is adopted rather than mistaken for the stale row.
//
// KILLING MUTATIONS: (a) drop the killLatched case from Resync. RED — the first resync after a failed persist
// resurrects Semi-auto in the very process whose breaker just tripped. (b) make persistLatchedKill a no-op.
// RED — the kill never reaches the store, so the other plane keeps actuating and this one never releases.
func TestResyncNeverResurrectsAnUnpersistedKillAndPersistsItWhenItCan(t *testing.T) {
	ctx := context.Background()
	store := &saveFailingStore{MemModeStore: NewMemModeStore()}
	if err := store.Save(ctx, ModeSemiAuto); err != nil {
		t.Fatal(err)
	}
	actuation := NewModeController(ctx, store, newLedger(), allowAuthz{}, greenPreflight{}, nil)
	triage := NewModeController(ctx, store, newLedger(), allowAuthz{}, greenPreflight{}, nil)

	store.failSaves(errors.New("db write refused"))
	actuation.ForceShadow("deviation breaker tripped")
	if m, _ := store.Load(ctx); m != ModeSemiAuto {
		t.Fatalf("vacuity floor: the failed persist must leave the pre-kill row (Semi-auto) in the store, got %s", m)
	}
	for i := 0; i < 3; i++ {
		if got := actuation.Resync(ctx); got != ModeShadow {
			t.Fatalf("resync %d read the pre-kill Semi-auto row back in over an unpersisted kill (got %s)", i, got)
		}
	}

	// The store accepts writes again: the next resync puts the kill on record...
	store.failSaves(nil)
	if got := actuation.Resync(ctx); got != ModeShadow {
		t.Fatalf("the plane must stay Shadow while its kill is being persisted, got %s", got)
	}
	if m, _ := store.Load(ctx); m != ModeShadow {
		t.Fatalf("resync must re-attempt the failed kill persist once the store accepts writes; store holds %s", m)
	}
	// ...the other plane stops too...
	if got := triage.Resync(ctx); got != ModeShadow {
		t.Fatalf("once persisted, the kill must reach the other plane, got %s", got)
	}
	// ...and an operator escalation made after the kill is on record reaches this plane.
	if err := triage.Transition(ctx, ModeShadow, ModeSemiAuto, "owner", "resume"); err != nil {
		t.Fatal(err)
	}
	if got := actuation.Resync(ctx); got != ModeSemiAuto {
		t.Fatalf("an operator escalation recorded after the kill must reach this plane, got %s — the latch never "+
			"releases, so one failed persist would disarm the actuation plane until a restart", got)
	}
}

// A PERSISTED KILL MUST NOT BLOCK A RESUME FROM ANOTHER PLANE (review finding, TG-570). The actuation plane's
// breaker trips and its Shadow persists normally; the operator resumes on the runner plane BEFORE the actuation
// plane's next tick has read the Shadow row. That tick reads Semi-auto — a NEWER operator write, since this
// plane's kill is already on record — and must adopt it.
//
// KILLING MUTATION: keep killLatched set after ForceShadow's persist succeeds (drop the clear). RED — every tick
// refuses the resume and the actuation plane stays Shadow until a restart.
func TestAPersistedKillDoesNotBlockAResumeFromAnotherPlane(t *testing.T) {
	ctx := context.Background()
	_, triage, actuation := twoPlanes(t, ModeSemiAuto)

	actuation.ForceShadow("deviation breaker tripped")
	if got := triage.Resync(ctx); got != ModeShadow {
		t.Fatalf("vacuity floor: the persisted kill must reach the triage plane, got %s", got)
	}
	if err := triage.Transition(ctx, ModeShadow, ModeSemiAuto, "owner", "false trip — resume"); err != nil {
		t.Fatal(err)
	}
	if got := actuation.Resync(ctx); got != ModeSemiAuto {
		t.Fatalf("the operator's resume, recorded after this plane's kill was persisted, must reach it; got %s", got)
	}
}

// An operator transition on THIS controller is the deliberate decision that supersedes a pending latch.
//
// KILLING MUTATION: drop `c.killLatched = false` from Transition. RED — after a failed-persist kill and an
// operator's restore on this same plane, the next resync re-persists the superseded kill over the restore.
func TestALocalTransitionReleasesTheKillLatch(t *testing.T) {
	ctx := context.Background()
	store := &saveFailingStore{MemModeStore: NewMemModeStore()}
	if err := store.Save(ctx, ModeSemiAuto); err != nil {
		t.Fatal(err)
	}
	c := NewModeController(ctx, store, newLedger(), allowAuthz{}, greenPreflight{}, nil)
	store.failSaves(errors.New("db write refused"))
	c.ForceShadow("breaker")
	store.failSaves(nil)
	if err := c.Transition(ctx, ModeShadow, ModeSemiAuto, "owner", "resume"); err != nil {
		t.Fatal(err)
	}
	if got := c.Resync(ctx); got != ModeSemiAuto {
		t.Fatalf("an operator's audited restore on this controller must release the kill latch; resync gave %s", got)
	}
	if m, _ := store.Load(ctx); m != ModeSemiAuto {
		t.Fatalf("a released latch must not re-persist the superseded kill over the operator's restore; store holds %s", m)
	}
}

// AN UNREADABLE ROW FAILS CLOSED, AND A READ ERROR IS NOT A KILL. REQ-1519: absent/unreadable/corrupt → Shadow.
// But a database blip must not latch the plane off until a restart: the next good read restores the mode.
//
// KILLING MUTATION (a): on a Load error keep c.current. RED — a plane that cannot read the mode keeps
// actuating on a value it can no longer confirm. (b): set killLatched on a read error. RED — one blip disarms
// the actuation plane until the next deploy.
func TestResyncFailsClosedOnAnUnreadableRowAndRecovers(t *testing.T) {
	ctx := context.Background()
	store, _, actuation := twoPlanes(t, ModeSemiAuto)

	store.WithLoadError(errors.New("db unreachable"))
	if got := actuation.Resync(ctx); got != ModeShadow {
		t.Fatalf("an unreadable persisted mode must fail closed to Shadow on resync, got %s", got)
	}
	store.WithLoadError(nil)
	if got := actuation.Resync(ctx); got != ModeSemiAuto {
		t.Fatalf("once the row reads again the stored Semi-auto must be restored, got %s", got)
	}

	if err := store.Save(ctx, Mode(42)); err != nil {
		t.Fatal(err)
	}
	if got := actuation.Resync(ctx); got != ModeShadow {
		t.Fatalf("a corrupt persisted mode must fail closed to Shadow on resync, got %s", got)
	}
}

// racingStore returns a stale value from Load AFTER running a hook — the hook stands in for a Transition that
// lands while Resync's read is in flight (Resync reads outside the lock).
type racingStore struct {
	*MemModeStore
	stale  Mode
	during func()
}

func (s *racingStore) Load(ctx context.Context) (Mode, error) {
	if s.during != nil {
		hook := s.during
		s.during = nil
		hook()
		return s.stale, nil
	}
	return s.MemModeStore.Load(ctx)
}

// A READ THAT RACED A LOCAL WRITE IS DISCARDED. Without it, a resync whose Load began before an operator's
// Transition to Shadow would re-apply the stale Semi-auto over it — undoing an off-switch for one interval.
//
// KILLING MUTATION: drop the `c.gen != gen` check in Resync. RED — the plane is Semi-auto right after the
// operator turned it off.
func TestResyncDiscardsAReadThatRacedALocalTransition(t *testing.T) {
	ctx := context.Background()
	store := &racingStore{MemModeStore: NewMemModeStore(), stale: ModeSemiAuto}
	if err := store.MemModeStore.Save(ctx, ModeSemiAuto); err != nil {
		t.Fatal(err)
	}
	c := NewModeController(ctx, store, newLedger(), allowAuthz{}, greenPreflight{}, nil)
	store.during = func() {
		if err := c.Transition(ctx, ModeSemiAuto, ModeShadow, "owner", "stop now"); err != nil {
			t.Errorf("transition during the read: %v", err)
		}
	}
	if got := c.Resync(ctx); got != ModeShadow {
		t.Fatalf("a resync read taken before the operator's Transition to Shadow re-applied the stale %s", got)
	}
}

// Many resyncs racing transitions and kills on two planes — for -race, and for the invariant that every plane
// converges on the stored mode once the writers stop.
func TestConcurrentResyncConverges(t *testing.T) {
	ctx := context.Background()
	store, triage, actuation := twoPlanes(t, ModeSemiAuto)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); actuation.Resync(ctx) }()
		go func() { defer wg.Done(); triage.Resync(ctx) }()
		go func() { defer wg.Done(); triage.ForceShadow("burst") }()
	}
	wg.Wait()
	triage.Resync(ctx)
	actuation.Resync(ctx)
	stored, _ := store.Load(ctx)
	if stored != ModeShadow || triage.Current() != ModeShadow || actuation.Current() != ModeShadow {
		t.Fatalf("after the writers stop every plane must hold the stored mode: stored=%s triage=%s actuation=%s",
			stored, triage.Current(), actuation.Current())
	}
}

// A nil store is in-memory only: Resync is a no-op, never a panic.
func TestResyncWithoutAStoreIsANoOp(t *testing.T) {
	c := NewModeController(context.Background(), nil, newLedger(), allowAuthz{}, greenPreflight{}, nil)
	if got := c.Resync(context.Background()); got != ModeShadow {
		t.Fatalf("a storeless controller resyncs to its own (Shadow) mode, got %s", got)
	}
}

// orchestratedStore scripts one exact interleaving: ForceShadow's persist blocks until released, Resync's own
// persist retry fails, and Resync's Load lets ForceShadow's persist land (clearing the latch) before handing back
// the stale pre-kill row it read.
type orchestratedStore struct {
	*MemModeStore
	mu        sync.Mutex
	saves     int
	entered   chan struct{} // closed when ForceShadow's persist is in flight
	release   chan struct{} // closed to let it land
	duringLd  func()
	staleLoad Mode
}

func (s *orchestratedStore) Save(ctx context.Context, m Mode) error {
	s.mu.Lock()
	s.saves++
	n := s.saves
	s.mu.Unlock()
	switch n {
	case 1: // ForceShadow's persist
		close(s.entered)
		<-s.release
		return s.MemModeStore.Save(ctx, m)
	case 2: // Resync's retry of it
		return errors.New("db write refused")
	default:
		return s.MemModeStore.Save(ctx, m)
	}
}

func (s *orchestratedStore) Load(ctx context.Context) (Mode, error) {
	if h := s.duringLd; h != nil {
		s.duringLd = nil
		h()
		return s.staleLoad, nil
	}
	return s.MemModeStore.Load(ctx)
}

// A READ TAKEN BEFORE THE KILL'S PERSIST LANDED IS DISCARDED EVEN THOUGH THE LATCH IS GONE BY APPLY TIME. Resync
// reads Semi-auto (the pre-kill row) while ForceShadow's persist is in flight; the persist then lands and clears
// the latch before Resync applies its read. Without the gen bump that accompanies the latch release, the stale
// read would pass both guards and resurrect Semi-auto for one interval.
//
// KILLING MUTATION: drop the `c.gen++` beside the latch release in ForceShadow. RED — Semi-auto resurrected.
func TestAReadThatPredatesTheKillPersistIsDiscarded(t *testing.T) {
	ctx := context.Background()
	store := &orchestratedStore{MemModeStore: NewMemModeStore(), entered: make(chan struct{}), release: make(chan struct{}), staleLoad: ModeSemiAuto}
	if err := store.MemModeStore.Save(ctx, ModeSemiAuto); err != nil {
		t.Fatal(err)
	}
	c := NewModeController(ctx, store, newLedger(), allowAuthz{}, greenPreflight{}, nil)

	killed := make(chan struct{})
	go func() { c.ForceShadow("deviation breaker tripped"); close(killed) }()
	<-store.entered
	store.duringLd = func() { close(store.release); <-killed }

	if got := c.Resync(ctx); got != ModeShadow {
		t.Fatalf("a resync read taken before the kill's persist landed re-applied the pre-kill %s", got)
	}
	if m, _ := store.MemModeStore.Load(ctx); m != ModeShadow {
		t.Fatalf("vacuity floor: the kill's persist must have landed, store holds %s", m)
	}
}

// hookSaveStore persists the first Save, runs a hook, and fails every later Save.
type hookSaveStore struct {
	*MemModeStore
	saves      int
	afterFirst func()
}

func (s *hookSaveStore) Save(ctx context.Context, m Mode) error {
	s.saves++
	if s.saves == 1 {
		err := s.MemModeStore.Save(ctx, m)
		if s.afterFirst != nil {
			s.afterFirst()
		}
		return err
	}
	return errors.New("db write refused")
}

// ONE KILL'S PERSIST MUST NOT RELEASE ANOTHER KILL'S LATCH (review finding, TG-570). Kill A persists; an operator
// resumes on another plane; kill B arrives on this plane and its persist fails; only THEN does A's success branch
// run. If A cleared the latch, B's kill would be on record nowhere and the next resync would adopt the resumed
// Semi-auto over it.
//
// KILLING MUTATION: drop the `c.gen == myGen` guard in ForceShadow. RED — Semi-auto adopted over kill B.
func TestOneKillsPersistDoesNotReleaseAnothersLatch(t *testing.T) {
	ctx := context.Background()
	store := &hookSaveStore{MemModeStore: NewMemModeStore()}
	if err := store.MemModeStore.Save(ctx, ModeSemiAuto); err != nil {
		t.Fatal(err)
	}
	c := NewModeController(ctx, store, newLedger(), allowAuthz{}, greenPreflight{}, nil)
	store.afterFirst = func() {
		if err := store.MemModeStore.Save(ctx, ModeSemiAuto); err != nil { // the operator's resume, elsewhere
			t.Error(err)
		}
		c.ForceShadow("kill B") // its persist fails
	}
	c.ForceShadow("kill A")

	if got := c.Resync(ctx); got != ModeShadow {
		t.Fatalf("kill B is on record nowhere, yet the resync adopted the stored %s — kill A's persist released B's latch", got)
	}
}
