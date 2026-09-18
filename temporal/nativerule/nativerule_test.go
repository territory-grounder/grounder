package nativerule

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
)

const (
	validEntry = "host-glob:web-*|deploy|22|ssh|env:TG_TEST_KEY_A"
	twoRules   = "host:a|root|22|ssh|env:K; host:b|root|22|ssh|env:K"
	malformed  = "host:broken|root" // too few fields — ParseRules refuses
)

// spyStore RECORDS whether Insert/Delete were reached, so a test can assert the killing property: a
// refused write never touches the store. rows maps id→entry, mirroring the real table's shape.
type spyStore struct {
	mu          sync.Mutex
	nextID      int64
	rows        map[int64]string
	insertCalls int
	deleteCalls int
	lastRat     string
	lastBy      string
}

func newSpyStore() *spyStore { return &spyStore{nextID: 100, rows: map[int64]string{}} }

func (s *spyStore) Insert(_ context.Context, entry, rationale, createdBy string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.insertCalls++
	s.nextID++
	s.rows[s.nextID] = entry
	s.lastRat, s.lastBy = rationale, createdBy
	return s.nextID, nil
}

func (s *spyStore) Delete(_ context.Context, id int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteCalls++
	if _, ok := s.rows[id]; !ok {
		return false, nil
	}
	delete(s.rows, id)
	return true, nil
}

var _ Store = (*spyStore)(nil)

func newActs(store Store, led Ledger) *Activities {
	return &Activities{D: Deps{Store: store, Ledger: led}}
}

// A valid add persists exactly once, records the operator as created_by, and appends exactly ONE
// governance record whose reason carries the selector token + rationale + operator — and NOT the packed
// entry (no SecretRef string may enter the audit spine, INV-13).
func TestActivity_ValidAddPersistsAndLedgers(t *testing.T) {
	ctx := context.Background()
	store := newSpyStore()
	led := audit.NewLedger()
	a := newActs(store, led)

	res, err := a.ApplyNativeRuleWriteActivity(ctx, Request{
		Verb: "add", Entry: validEntry, Rationale: "cover the web tier", Operator: "operator:kyriakosp", AdminAuthorized: true,
	})
	if err != nil {
		t.Fatalf("valid add refused: %v", err)
	}
	if res.ID == 0 || res.Entry != validEntry || res.LedgerSeq == 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if store.insertCalls != 1 || store.rows[res.ID] != validEntry {
		t.Fatalf("valid add not persisted exactly once: calls=%d rows=%v", store.insertCalls, store.rows)
	}
	if store.lastBy != "operator:kyriakosp" || store.lastRat != "cover the web tier" {
		t.Fatalf("created_by/rationale must be the authenticated operator's: by=%q rat=%q", store.lastBy, store.lastRat)
	}
	if led.Len() != 1 {
		t.Fatalf("add not audited exactly once: len=%d", led.Len())
	}
	e := led.Entries()[0]
	if e.Decision != "credential:native-rule-write" || e.ActionID != "native-rule:add:host-glob:web-*" {
		t.Fatalf("audit record wrong: %+v", e)
	}
	if !strings.Contains(e.Reason, "operator:kyriakosp") || !strings.Contains(e.Reason, "cover the web tier") || !strings.Contains(e.Reason, "host-glob:web-*") {
		t.Fatalf("audit reason must carry operator + rationale + selector token: %q", e.Reason)
	}
	if strings.Contains(e.Reason, "env:TG_TEST_KEY_A") {
		t.Fatalf("the ledger reason carries a SecretRef string — the spine must record the decision, not the map: %q", e.Reason)
	}
}

// THE KILLING CHECK: a malformed entry is REJECTED with the parser's error and NOTHING is persisted or
// audited — Insert is never reached, the ledger stays empty. A stored unparseable row would fail every
// subsequent sync of the whole source (it fails closed on any bad row), so it must never land.
func TestActivity_MalformedEntryRefusedBeforeLedger(t *testing.T) {
	ctx := context.Background()
	store := newSpyStore()
	led := audit.NewLedger()
	a := newActs(store, led)

	_, err := a.ApplyNativeRuleWriteActivity(ctx, Request{
		Verb: "add", Entry: malformed, Rationale: "r", Operator: "op", AdminAuthorized: true,
	})
	if err == nil || !strings.Contains(err.Error(), "malformed rule") {
		t.Fatalf("malformed entry err = %v, want the ParseRules refusal", err)
	}
	if store.insertCalls != 0 {
		t.Fatalf("a malformed entry reached Insert %d time(s) — it must be rejected BEFORE any persist", store.insertCalls)
	}
	if led.Len() != 0 {
		t.Fatalf("a rejected add appended %d audit record(s) — a refused write ledgers nothing", led.Len())
	}
}

// An entry packing TWO rules is refused the same way (one row, one rule — deletes stay precise), before
// any ledger append or persist.
func TestActivity_TwoRuleEntryRefused(t *testing.T) {
	ctx := context.Background()
	store := newSpyStore()
	led := audit.NewLedger()
	a := newActs(store, led)

	_, err := a.ApplyNativeRuleWriteActivity(ctx, Request{
		Verb: "add", Entry: twoRules, Rationale: "r", Operator: "op", AdminAuthorized: true,
	})
	if err == nil || !strings.Contains(err.Error(), "one row, one rule") {
		t.Fatalf("two-rule entry err = %v, want the one-row-one-rule refusal", err)
	}
	if store.insertCalls != 0 || led.Len() != 0 {
		t.Fatalf("a refused two-rule add touched state: inserts=%d ledger=%d", store.insertCalls, led.Len())
	}
}

// A missing rationale on add is refused before any ledger append or persist.
func TestActivity_AddWithoutRationaleRefused(t *testing.T) {
	ctx := context.Background()
	store := newSpyStore()
	led := audit.NewLedger()
	a := newActs(store, led)

	_, err := a.ApplyNativeRuleWriteActivity(ctx, Request{
		Verb: "add", Entry: validEntry, Operator: "op", AdminAuthorized: true,
	})
	if err == nil || !strings.Contains(err.Error(), "rationale required") {
		t.Fatalf("missing rationale err = %v", err)
	}
	if store.insertCalls != 0 || led.Len() != 0 {
		t.Fatalf("a rationale-less add touched state: inserts=%d ledger=%d", store.insertCalls, led.Len())
	}
}

// The verb table is CLOSED: anything but add/delete is refused before any state is touched.
func TestActivity_UnknownVerbRefused(t *testing.T) {
	ctx := context.Background()
	store := newSpyStore()
	led := audit.NewLedger()
	a := newActs(store, led)

	_, err := a.ApplyNativeRuleWriteActivity(ctx, Request{
		Verb: "replace", Entry: validEntry, Rationale: "r", Operator: "op", AdminAuthorized: true,
	})
	if !errors.Is(err, ErrUnknownVerb) {
		t.Fatalf("unknown verb err = %v, want ErrUnknownVerb", err)
	}
	if store.insertCalls != 0 || store.deleteCalls != 0 || led.Len() != 0 {
		t.Fatalf("an unknown verb touched state: inserts=%d deletes=%d ledger=%d", store.insertCalls, store.deleteCalls, led.Len())
	}
}

// A delete of an existing row removes it and ledgers once; a delete of a MISSING row surfaces the typed
// ErrNoSuchRule (the surface's 404). The not-found is discovered at the store write — after the append —
// so the refusal trails one ledger entry: over-recorded, never unrecorded (the ledger-first trade).
func TestActivity_DeleteAndDeleteMissing(t *testing.T) {
	ctx := context.Background()
	store := newSpyStore()
	led := audit.NewLedger()
	a := newActs(store, led)

	added, err := a.ApplyNativeRuleWriteActivity(ctx, Request{
		Verb: "add", Entry: validEntry, Rationale: "seed", Operator: "op", AdminAuthorized: true,
	})
	if err != nil {
		t.Fatalf("seed add: %v", err)
	}

	res, err := a.ApplyNativeRuleWriteActivity(ctx, Request{
		Verb: "delete", RowID: added.ID, Rationale: "retired host", Operator: "op", AdminAuthorized: true,
	})
	if err != nil {
		t.Fatalf("delete refused: %v", err)
	}
	if res.ID != added.ID || res.LedgerSeq == 0 {
		t.Fatalf("unexpected delete result: %+v", res)
	}
	if _, still := store.rows[added.ID]; still {
		t.Fatalf("the row survived its delete")
	}
	if led.Len() != 2 {
		t.Fatalf("delete not audited: ledger len=%d, want 2", led.Len())
	}
	if e := led.Entries()[1]; !strings.Contains(e.Reason, "retired host") || !strings.Contains(e.Reason, "delete row-") {
		t.Fatalf("delete audit reason wrong: %q", e.Reason)
	}

	// The missing row: typed refusal the surface maps to 404.
	if _, err := a.ApplyNativeRuleWriteActivity(ctx, Request{
		Verb: "delete", RowID: added.ID, Rationale: "again", Operator: "op", AdminAuthorized: true,
	}); !errors.Is(err, ErrNoSuchRule) {
		t.Fatalf("delete-missing err = %v, want ErrNoSuchRule", err)
	}

	// A non-positive row id is refused before any append.
	before := led.Len()
	if _, err := a.ApplyNativeRuleWriteActivity(ctx, Request{
		Verb: "delete", RowID: 0, Rationale: "r", Operator: "op", AdminAuthorized: true,
	}); err == nil || !strings.Contains(err.Error(), "positive row id") {
		t.Fatalf("zero row id err = %v", err)
	}
	if led.Len() != before {
		t.Fatalf("a refused zero-id delete appended an audit record")
	}
}

// Fail-closed: nil deps and a non-admin request are both refused with typed errors and touch nothing.
func TestActivity_FailClosed(t *testing.T) {
	ctx := context.Background()

	// nil deps.
	if _, err := (&Activities{}).ApplyNativeRuleWriteActivity(ctx, Request{
		Verb: "add", Entry: validEntry, Rationale: "r", Operator: "op", AdminAuthorized: true,
	}); !errors.Is(err, ErrNoStore) {
		t.Fatalf("nil deps err = %v, want ErrNoStore", err)
	}

	// non-admin: refused BEFORE any validation/persist/audit.
	store := newSpyStore()
	led := audit.NewLedger()
	a := newActs(store, led)
	if _, err := a.ApplyNativeRuleWriteActivity(ctx, Request{
		Verb: "add", Entry: validEntry, Rationale: "r", Operator: "op:notadmin", AdminAuthorized: false,
	}); !errors.Is(err, ErrNotAdmin) {
		t.Fatalf("non-admin err = %v, want ErrNotAdmin", err)
	}
	if store.insertCalls != 0 || led.Len() != 0 {
		t.Fatalf("a non-admin write touched state: inserts=%d ledger=%d", store.insertCalls, led.Len())
	}
}
