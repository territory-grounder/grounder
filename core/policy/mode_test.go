package policy

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
)

// --- test seams (fakes) ---------------------------------------------------------------------------------

// allowAuthz admits every actor whose id is non-empty; an empty id (unauthenticated) is rejected.
type allowAuthz struct{}

func (allowAuthz) HasModeChangeAuthority(_ context.Context, actor string) error {
	if actor == "" {
		return errors.New("unauthenticated: empty actor")
	}
	return nil
}

// denyAuthz rejects every actor (unauthorized).
type denyAuthz struct{}

func (denyAuthz) HasModeChangeAuthority(_ context.Context, _ string) error {
	return errors.New("no mode-change authority")
}

// greenPreflight / redPreflight drive the escalation gate.
type greenPreflight struct{}

func (greenPreflight) PreflightGreen(_ context.Context) error { return nil }

type redPreflight struct{}

func (redPreflight) PreflightGreen(_ context.Context) error { return errors.New("preflight not green") }

// failingLedger fails every Append (to exercise the audit-fail-closed path).
type failingLedger struct{}

func (failingLedger) Append(audit.GovDecision) (audit.LedgerEntry, error) {
	return audit.LedgerEntry{}, errors.New("ledger down")
}

func newLedger() *audit.Ledger { return audit.NewLedger() }

// spyRearmer records BreakerRearmer.Rearm calls (and can inject a failure) — the seam for REQ-1523.
type spyRearmer struct {
	calls int
	err   error
}

func (s *spyRearmer) Rearm(_ context.Context) error { s.calls++; return s.err }

// --- Mode enum + zero-value-is-Shadow -------------------------------------------------------------------

func TestModeZeroValueIsShadow(t *testing.T) {
	var m Mode // zero value
	if m != ModeShadow {
		t.Fatalf("zero Mode = %v, want ModeShadow", m)
	}
	if m.String() != "Shadow" {
		t.Fatalf("zero Mode string = %q, want Shadow", m.String())
	}
	if m.MayAutoActuate() {
		t.Fatal("zero Mode (Shadow) must never auto-actuate")
	}
}

func TestExactlyFourModes(t *testing.T) {
	names := map[Mode]string{
		ModeShadow:   "Shadow",
		ModeHITL:     "HITL",
		ModeSemiAuto: "Semi-auto",
		ModeFullAuto: "Full-auto",
	}
	for m, want := range names {
		if got := m.String(); got != want {
			t.Errorf("Mode(%d).String() = %q, want %q", int(m), got, want)
		}
		if !m.valid() {
			t.Errorf("Mode %q reported invalid", want)
		}
		rt, err := ParseMode(want)
		if err != nil || rt != m {
			t.Errorf("ParseMode(%q) = %v,%v want %v,nil", want, rt, err, m)
		}
	}
	// A fifth value is invalid and fails closed to Shadow in String()/ParseMode.
	if bogus := Mode(99); bogus.valid() || bogus.String() != "Shadow" {
		t.Fatalf("Mode(99) must be invalid and render Shadow, got valid=%v str=%q", bogus.valid(), bogus.String())
	}
	if _, err := ParseMode("Turbo"); !errors.Is(err, ErrUnknownMode) {
		t.Fatalf("ParseMode(Turbo) err = %v, want ErrUnknownMode", err)
	}
}

func TestMayAutoActuatePerMode(t *testing.T) {
	cases := []struct {
		m    Mode
		auto bool
		vote bool
	}{
		{ModeShadow, false, false},
		{ModeHITL, false, true},
		{ModeSemiAuto, true, false},
		{ModeFullAuto, true, false},
	}
	for _, tc := range cases {
		if got := tc.m.MayAutoActuate(); got != tc.auto {
			t.Errorf("%s.MayAutoActuate() = %v, want %v", tc.m, got, tc.auto)
		}
		if got := tc.m.RequiresHumanVote(); got != tc.vote {
			t.Errorf("%s.RequiresHumanVote() = %v, want %v", tc.m, got, tc.vote)
		}
	}
}

// --- transitions: gated + audited (ledger record BEFORE activation) -------------------------------------

func TestLegalTransitionAuditsThenActivates(t *testing.T) {
	ctx := context.Background()
	led := newLedger()
	store := NewMemModeStore()
	c := NewModeController(ctx, store, led, allowAuthz{}, greenPreflight{}, nil)

	if c.Current() != ModeShadow {
		t.Fatalf("fresh controller (absent store) = %v, want Shadow", c.Current())
	}
	if led.Len() != 0 {
		t.Fatalf("ledger not empty before any transition: %d", led.Len())
	}

	if err := c.Transition(ctx, ModeShadow, ModeSemiAuto, "op-1", "canary window open"); err != nil {
		t.Fatalf("legal Shadow->Semi transition: %v", err)
	}
	// The mode is now active AND persisted, and exactly one ledger record was appended.
	if c.Current() != ModeSemiAuto {
		t.Fatalf("active mode after transition = %v, want Semi-auto", c.Current())
	}
	if got, _ := store.Load(ctx); got != ModeSemiAuto {
		t.Fatalf("persisted mode = %v, want Semi-auto", got)
	}
	if led.Len() != 1 {
		t.Fatalf("ledger has %d records, want exactly 1 mode-transition record", led.Len())
	}
	e := led.Entries()[0]
	if e.Decision != "mode-transition:Shadow->Semi-auto" {
		t.Fatalf("ledger decision = %q, want the mode-transition record", e.Decision)
	}
	if err := led.Verify(); err != nil {
		t.Fatalf("ledger chain broken: %v", err)
	}
}

// --- REQ-1523: breaker re-arm on an owner-gated escalation into an actuating mode -----------------------

func TestEscalationIntoActuatingModeRearmsBreaker(t *testing.T) {
	ctx := context.Background()
	for _, to := range []Mode{ModeSemiAuto, ModeFullAuto} {
		spy := &spyRearmer{}
		c := NewModeController(ctx, NewMemModeStore(), newLedger(), allowAuthz{}, greenPreflight{}, nil).BindBreakerRearmer(spy)
		if err := c.Transition(ctx, ModeShadow, to, "owner", "resume actuation"); err != nil {
			t.Fatalf("Shadow->%s: %v", to, err)
		}
		if spy.calls != 1 {
			t.Fatalf("escalation Shadow->%s re-armed %d times, want exactly 1", to, spy.calls)
		}
	}
}

func TestNonActuatingTransitionDoesNotRearmBreaker(t *testing.T) {
	ctx := context.Background()
	// A transition whose TARGET does not auto-actuate (Shadow, HITL) must never clear the breaker — only the
	// deliberate escalation into an actuating mode is the operator's "resume actuation" decision.
	cases := []struct{ from, to Mode }{
		{ModeSemiAuto, ModeShadow}, // de-escalation
		{ModeShadow, ModeHITL},     // into a non-auto mode
	}
	for _, tc := range cases {
		spy := &spyRearmer{}
		store := NewMemModeStore()
		if err := store.Save(ctx, tc.from); err != nil {
			t.Fatalf("seed store %s: %v", tc.from, err)
		}
		c := NewModeController(ctx, store, newLedger(), allowAuthz{}, greenPreflight{}, nil).BindBreakerRearmer(spy)
		if err := c.Transition(ctx, tc.from, tc.to, "owner", "de-escalate"); err != nil {
			t.Fatalf("%s->%s: %v", tc.from, tc.to, err)
		}
		if spy.calls != 0 {
			t.Fatalf("%s->%s re-armed %d times, want 0 (only actuating escalations re-arm)", tc.from, tc.to, spy.calls)
		}
	}
}

func TestRearmFailureDoesNotUnwindActivatedTransition(t *testing.T) {
	ctx := context.Background()
	spy := &spyRearmer{err: errors.New("breaker store down")}
	store := NewMemModeStore()
	c := NewModeController(ctx, store, newLedger(), allowAuthz{}, greenPreflight{}, nil).BindBreakerRearmer(spy)
	// The re-arm runs AFTER the mode is audited + persisted; a re-arm failure must NOT unwind it (the breaker
	// simply stays open — fail-safe, actuation still halted — never a rolled-back, unrecorded transition).
	if err := c.Transition(ctx, ModeShadow, ModeSemiAuto, "owner", "resume"); err != nil {
		t.Fatalf("transition must succeed despite a re-arm failure: %v", err)
	}
	if spy.calls != 1 {
		t.Fatalf("re-armer called %d times, want 1", spy.calls)
	}
	if c.Current() != ModeSemiAuto {
		t.Fatalf("mode after re-arm failure = %v, want Semi-auto (transition stands)", c.Current())
	}
	if got, _ := store.Load(ctx); got != ModeSemiAuto {
		t.Fatalf("persisted mode = %v, want Semi-auto (transition committed before re-arm)", got)
	}
}

func TestUnboundRearmerTransitionsNormally(t *testing.T) {
	ctx := context.Background()
	// No rearmer bound (the read-only console-side controller, or a boot with no armed breaker): the escalation
	// must transition exactly as before — the re-arm is optional and its absence changes nothing.
	c := NewModeController(ctx, NewMemModeStore(), newLedger(), allowAuthz{}, greenPreflight{}, nil)
	if err := c.Transition(ctx, ModeShadow, ModeFullAuto, "owner", "resume"); err != nil {
		t.Fatalf("escalation with no rearmer bound: %v", err)
	}
	if c.Current() != ModeFullAuto {
		t.Fatalf("mode = %v, want Full-auto", c.Current())
	}
}

func TestRefusedEscalationDoesNotRearm(t *testing.T) {
	ctx := context.Background()
	// A red preflight refuses the escalation BEFORE the mode activates; the breaker must NOT be re-armed on a
	// transition that never took effect (no unaudited/partial recovery).
	spy := &spyRearmer{}
	c := NewModeController(ctx, NewMemModeStore(), newLedger(), allowAuthz{}, redPreflight{}, nil).BindBreakerRearmer(spy)
	if err := c.Transition(ctx, ModeShadow, ModeSemiAuto, "owner", "resume"); !errors.Is(err, ErrPreflightNotGreen) {
		t.Fatalf("red-preflight escalation err = %v, want ErrPreflightNotGreen", err)
	}
	if spy.calls != 0 {
		t.Fatalf("refused escalation re-armed %d times, want 0", spy.calls)
	}
	if c.Current() != ModeShadow {
		t.Fatalf("mode after refused escalation = %v, want Shadow", c.Current())
	}
}

func TestExactlyOneActiveMode(t *testing.T) {
	ctx := context.Background()
	c := NewModeController(ctx, NewMemModeStore(), newLedger(), allowAuthz{}, greenPreflight{}, nil)
	// Walk the ladder; Current() always returns exactly one of the four modes.
	seq := []Mode{ModeHITL, ModeSemiAuto, ModeFullAuto, ModeShadow}
	from := ModeShadow
	for _, to := range seq {
		if err := c.Transition(ctx, from, to, "op", "walk"); err != nil {
			t.Fatalf("transition %s->%s: %v", from, to, err)
		}
		if got := c.Current(); got != to || !got.valid() {
			t.Fatalf("active = %v (valid=%v), want single active %v", got, got.valid(), to)
		}
		from = to
	}
}

// --- fail-closed paths ----------------------------------------------------------------------------------

func TestRedPreflightRefusesEscalation(t *testing.T) {
	ctx := context.Background()
	led := newLedger()
	c := NewModeController(ctx, NewMemModeStore(), led, allowAuthz{}, redPreflight{}, nil)

	err := c.Transition(ctx, ModeShadow, ModeFullAuto, "op-1", "flip")
	if !errors.Is(err, ErrPreflightNotGreen) {
		t.Fatalf("escalation with red preflight err = %v, want ErrPreflightNotGreen", err)
	}
	if c.Current() != ModeShadow {
		t.Fatalf("mode changed on refused escalation: %v", c.Current())
	}
	if led.Len() != 0 {
		t.Fatalf("refused escalation wrote %d ledger records, want 0", led.Len())
	}
	// A NON-actuating transition (Shadow->HITL) needs no preflight and is allowed even when red.
	if err := c.Transition(ctx, ModeShadow, ModeHITL, "op-1", "de-escalate"); err != nil {
		t.Fatalf("Shadow->HITL should not require preflight: %v", err)
	}
}

func TestUnauthorizedActorRefused(t *testing.T) {
	ctx := context.Background()
	led := newLedger()
	c := NewModeController(ctx, NewMemModeStore(), led, denyAuthz{}, greenPreflight{}, nil)
	err := c.Transition(ctx, ModeShadow, ModeSemiAuto, "intruder", "nope")
	if !errors.Is(err, ErrUnauthorizedModeChange) {
		t.Fatalf("unauthorized actor err = %v, want ErrUnauthorizedModeChange", err)
	}
	if c.Current() != ModeShadow || led.Len() != 0 {
		t.Fatalf("unauthorized transition changed state: mode=%v ledger=%d", c.Current(), led.Len())
	}
}

func TestLedgerFailureLeavesModeUnchanged(t *testing.T) {
	ctx := context.Background()
	store := NewMemModeStore()
	c := NewModeController(ctx, store, failingLedger{}, allowAuthz{}, greenPreflight{}, nil)
	err := c.Transition(ctx, ModeShadow, ModeSemiAuto, "op-1", "audit will fail")
	if err == nil {
		t.Fatal("transition succeeded despite a failing ledger")
	}
	if c.Current() != ModeShadow {
		t.Fatalf("mode advanced despite audit failure: %v", c.Current())
	}
	if _, lerr := store.Load(ctx); !errors.Is(lerr, ErrModeAbsent) {
		t.Fatalf("mode was persisted despite audit failure (store load err = %v)", lerr)
	}
}

// --- fail-closed load: absent / unreadable / corrupt → Shadow -------------------------------------------

func TestLoadAbsentFailsClosedToShadow(t *testing.T) {
	ctx := context.Background()
	// Absent (never saved) store.
	c := NewModeController(ctx, NewMemModeStore(), newLedger(), allowAuthz{}, greenPreflight{}, nil)
	if c.Current() != ModeShadow || c.ResolveMode(ctx) != ModeShadow {
		t.Fatalf("absent persisted mode did not fail closed to Shadow: %v", c.Current())
	}
	// Unreadable store.
	bad := NewMemModeStore().WithLoadError(errors.New("db unreachable"))
	c2 := NewModeController(ctx, bad, newLedger(), allowAuthz{}, greenPreflight{}, nil)
	if c2.ResolveMode(ctx) != ModeShadow {
		t.Fatalf("unreadable persisted mode did not fail closed to Shadow")
	}
}

func TestStaleFromRefused(t *testing.T) {
	ctx := context.Background()
	c := NewModeController(ctx, NewMemModeStore(), newLedger(), allowAuthz{}, greenPreflight{}, nil)
	// active is Shadow; declaring from=HITL is stale.
	if err := c.Transition(ctx, ModeHITL, ModeSemiAuto, "op", "stale"); !errors.Is(err, ErrStaleMode) {
		t.Fatalf("stale from err = %v, want ErrStaleMode", err)
	}
}

// --- concurrency: transitions serialize, no torn read under -race ---------------------------------------

func TestConcurrentTransitionsSerialize(t *testing.T) {
	ctx := context.Background()
	led := newLedger()
	c := NewModeController(ctx, NewMemModeStore(), led, allowAuthz{}, greenPreflight{}, nil)

	const n = 32
	var wg sync.WaitGroup
	var success int64
	var mu sync.Mutex
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// All race to move Shadow->Semi-auto; the compare-and-swap admits exactly one.
			if err := c.Transition(ctx, ModeShadow, ModeSemiAuto, "op", "race"); err == nil {
				mu.Lock()
				success++
				mu.Unlock()
			}
			_ = c.Current() // concurrent reads must never observe a torn state (-race asserts this)
		}()
	}
	wg.Wait()

	if success != 1 {
		t.Fatalf("%d concurrent transitions succeeded, want exactly 1 (CAS serialization)", success)
	}
	if c.Current() != ModeSemiAuto {
		t.Fatalf("final mode = %v, want Semi-auto", c.Current())
	}
	if led.Len() != 1 {
		t.Fatalf("ledger has %d records, want exactly 1 (only the winning transition audits)", led.Len())
	}
	if err := led.Verify(); err != nil {
		t.Fatalf("ledger chain broken after concurrent transitions: %v", err)
	}
}

// --- SeedInitialMode (deploy-time initial mode, TG-140) -------------------------------------------------

func TestSeedInitialMode(t *testing.T) {
	ctx := context.Background()

	t.Run("fresh deploy + actuating config + green preflight → seeds", func(t *testing.T) {
		store := NewMemModeStore()
		c := NewModeController(ctx, store, newLedger(), allowAuthz{}, greenPreflight{}, nil)
		if err := c.SeedInitialMode(ctx, ModeSemiAuto, "TG_INITIAL_MODE"); err != nil {
			t.Fatalf("seed returned error: %v", err)
		}
		if c.Current() != ModeSemiAuto {
			t.Fatalf("current = %s, want Semi-auto", c.Current())
		}
		if got, _ := store.Load(ctx); got != ModeSemiAuto {
			t.Fatalf("persisted = %s, want Semi-auto", got)
		}
	})

	t.Run("fresh deploy + actuating config + RED preflight → refused, stays Shadow", func(t *testing.T) {
		store := NewMemModeStore()
		c := NewModeController(ctx, store, newLedger(), allowAuthz{}, redPreflight{}, nil)
		if err := c.SeedInitialMode(ctx, ModeFullAuto, "TG_INITIAL_MODE"); err == nil {
			t.Fatal("expected an error seeding an actuating mode under a red preflight")
		}
		if c.Current() != ModeShadow {
			t.Fatalf("current = %s, want Shadow (fail closed)", c.Current())
		}
		if _, lerr := store.Load(ctx); !errors.Is(lerr, ErrModeAbsent) {
			t.Fatal("a refused seed must not persist a mode")
		}
	})

	t.Run("non-actuating config (HITL) does NOT need preflight", func(t *testing.T) {
		c := NewModeController(ctx, NewMemModeStore(), newLedger(), allowAuthz{}, redPreflight{}, nil)
		if err := c.SeedInitialMode(ctx, ModeHITL, "TG_INITIAL_MODE"); err != nil {
			t.Fatalf("HITL seed under red preflight should succeed (HITL never actuates): %v", err)
		}
		if c.Current() != ModeHITL {
			t.Fatalf("current = %s, want HITL", c.Current())
		}
	})

	t.Run("Shadow config is a no-op (already the default)", func(t *testing.T) {
		store := NewMemModeStore()
		c := NewModeController(ctx, store, newLedger(), allowAuthz{}, greenPreflight{}, nil)
		if err := c.SeedInitialMode(ctx, ModeShadow, "TG_INITIAL_MODE"); err != nil {
			t.Fatalf("Shadow seed should be a clean no-op: %v", err)
		}
		if _, lerr := store.Load(ctx); !errors.Is(lerr, ErrModeAbsent) {
			t.Fatal("a Shadow seed should not persist anything (leave absent → resolves Shadow)")
		}
	})

	t.Run("ABSENT-ONLY: never overrides an already-persisted mode", func(t *testing.T) {
		store := NewMemModeStore()
		if err := store.Save(ctx, ModeFullAuto); err != nil { // operator already set Full-auto
			t.Fatal(err)
		}
		c := NewModeController(ctx, store, newLedger(), allowAuthz{}, greenPreflight{}, nil)
		if err := c.SeedInitialMode(ctx, ModeHITL, "TG_INITIAL_MODE"); err != nil {
			t.Fatalf("seed over an existing mode should be a clean no-op: %v", err)
		}
		if got, _ := store.Load(ctx); got != ModeFullAuto {
			t.Fatalf("existing mode was overridden: got %s, want Full-auto", got)
		}
		if c.Current() != ModeFullAuto {
			t.Fatalf("current = %s, want the un-overridden Full-auto", c.Current())
		}
	})

	t.Run("ledger failure fails the seed closed", func(t *testing.T) {
		store := NewMemModeStore()
		c := NewModeController(ctx, store, failingLedger{}, allowAuthz{}, greenPreflight{}, nil)
		if err := c.SeedInitialMode(ctx, ModeSemiAuto, "TG_INITIAL_MODE"); err == nil {
			t.Fatal("expected an error when the audit ledger append fails")
		}
		if c.Current() != ModeShadow {
			t.Fatalf("current = %s, want Shadow after an unaudited seed is refused", c.Current())
		}
		if _, lerr := store.Load(ctx); !errors.Is(lerr, ErrModeAbsent) {
			t.Fatal("an unaudited seed must not persist a mode")
		}
	})

	t.Run("nil store (in-memory) is a safe no-op", func(t *testing.T) {
		c := NewModeController(ctx, nil, newLedger(), allowAuthz{}, greenPreflight{}, nil)
		if err := c.SeedInitialMode(ctx, ModeSemiAuto, "TG_INITIAL_MODE"); err != nil {
			t.Fatalf("nil-store seed should be a clean no-op: %v", err)
		}
		if c.Current() != ModeShadow {
			t.Fatalf("current = %s, want Shadow", c.Current())
		}
	})
}
