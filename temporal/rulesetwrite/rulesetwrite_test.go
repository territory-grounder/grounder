package rulesetwrite

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/policy"
)

// validDoc / validDoc2 are two well-formed rules-as-data documents with DIFFERENT content (so their bundle
// versions differ — the compare-and-swap has something to move between).
const (
	validDoc   = `{"rules":[{"id":"r1","match":{"op_class":"restart-service"},"verdict":"approve"}]}`
	validDoc2  = `{"rules":[{"id":"r1","match":{"op_class":"start-guest"},"verdict":"auto"}]}`
	malformedD = `{"rules":[{"id":"r1","match":{"op_class":"x"},"verdict":"nuke"}]}` // unknown verdict — a bad ruleset
)

// spyStore is a policy.RulesetStore that RECORDS whether Save/Load were reached, so a test can assert the
// killing property: a malformed ruleset is rejected with NO persist (Save is never called). It mirrors the
// real store's fail-closed Save (validate before persisting) so the fake cannot pass where the pgx store
// would fail.
type spyStore struct {
	mu            sync.Mutex
	active        []byte
	activeRS      policy.RuleSet
	saveCalls     int
	loadCalls     int
	lastUpdatedBy string
	saveErr       error // optional: force a persist failure AFTER validation
}

func (s *spyStore) Load(context.Context) (policy.RuleSet, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadCalls++
	if s.active == nil {
		return policy.RuleSet{}, nil, policy.ErrRulesetAbsent
	}
	return s.activeRS, append([]byte(nil), s.active...), nil
}

func (s *spyStore) Save(_ context.Context, document []byte, updatedBy string) (policy.RuleSet, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveCalls++
	rs, err := policy.ParseRuleSet(document) // the real store validates before persisting (fail closed).
	if err != nil {
		return policy.RuleSet{}, err
	}
	if s.saveErr != nil {
		return policy.RuleSet{}, s.saveErr
	}
	s.active = append([]byte(nil), document...)
	s.activeRS, s.lastUpdatedBy = rs, updatedBy
	return rs, nil
}

var _ policy.RulesetStore = (*spyStore)(nil)

func newActs(store policy.RulesetStore, led Ledger) *Activities {
	return &Activities{D: Deps{Store: store, Ledger: led}}
}

// A valid ruleset persists, records updated_by, and appends exactly ONE governance record bound to the new
// bundle version.
func TestActivity_ValidPersistsAndLedgers(t *testing.T) {
	ctx := context.Background()
	store := &spyStore{}
	led := audit.NewLedger()
	a := newActs(store, led)

	res, err := a.ApplyRulesetWriteActivity(ctx, Request{
		Document: []byte(validDoc), Operator: "operator:kyriakosp", Rationale: "tighten restart policy", AdminAuthorized: true,
	})
	if err != nil {
		t.Fatalf("valid ruleset refused: %v", err)
	}
	if res.Version == "" || res.RuleCount != 1 || res.UpdatedBy != "operator:kyriakosp" || res.LedgerSeq == 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if store.saveCalls != 1 || store.active == nil {
		t.Fatalf("valid ruleset not persisted exactly once: saveCalls=%d active=%v", store.saveCalls, store.active != nil)
	}
	if store.lastUpdatedBy != "operator:kyriakosp" {
		t.Fatalf("updated_by must be the authenticated operator, got %q", store.lastUpdatedBy)
	}
	if led.Len() != 1 {
		t.Fatalf("ruleset write not audited exactly once: len=%d", led.Len())
	}
	e := led.Entries()[0]
	if e.Decision != "policy:ruleset-write" || e.ActionID != "ruleset:"+res.Version {
		t.Fatalf("audit record wrong: %+v (want decision=policy:ruleset-write action=ruleset:%s)", e, res.Version)
	}
	if !strings.Contains(e.Reason, "operator:kyriakosp") || !strings.Contains(e.Reason, "tighten restart policy") {
		t.Fatalf("audit reason must carry the operator + rationale: %q", e.Reason)
	}
}

// THE KILLING CHECK: a malformed ruleset is REJECTED with the parse error and NOTHING is persisted or
// audited — Save is never reached, the active document is untouched, the ledger stays empty. A bad ruleset
// governs actuation, so it must never become the active policy, not even transiently (INV-09, fail closed).
// TG-437: OnParsed runs on a write that WILL land (valid + CAS-passing), with the PARSED ruleset, and never
// on a rejected write — so the ruleset-write Matrix-approver cross-check fires exactly when a new ruleset
// takes effect. A nil hook is a no-op.
func TestActivity_OnParsedRunsOnSuccessNotOnReject(t *testing.T) {
	ctx := context.Background()
	var parsed []policy.RuleSet
	a := &Activities{D: Deps{Store: &spyStore{}, Ledger: audit.NewLedger(),
		OnParsed: func(rs policy.RuleSet) { parsed = append(parsed, rs) }}}

	if _, err := a.ApplyRulesetWriteActivity(ctx, Request{Document: []byte(validDoc), Operator: "op", AdminAuthorized: true}); err != nil {
		t.Fatalf("valid write: %v", err)
	}
	if len(parsed) != 1 {
		t.Fatalf("OnParsed must run exactly once on a valid write, ran %d", len(parsed))
	}
	if len(parsed[0].Rules) != 1 || parsed[0].Rules[0].ID != "r1" {
		t.Fatalf("OnParsed must receive the PARSED ruleset, got %+v", parsed[0])
	}

	parsed = nil
	if _, err := a.ApplyRulesetWriteActivity(ctx, Request{Document: []byte(malformedD), Operator: "op", AdminAuthorized: true}); err == nil {
		t.Fatal("a malformed ruleset must be rejected")
	}
	if len(parsed) != 0 {
		t.Fatalf("OnParsed must NOT run when the ruleset is rejected (it fires only for a landing write), ran %d", len(parsed))
	}

	// A nil OnParsed is a documented no-op — the write still succeeds.
	if _, err := newActs(&spyStore{}, audit.NewLedger()).ApplyRulesetWriteActivity(ctx,
		Request{Document: []byte(validDoc), Operator: "op", AdminAuthorized: true}); err != nil {
		t.Fatalf("nil OnParsed must be a no-op: %v", err)
	}
}

func TestActivity_InvalidRejectedNoPersistNoLedger(t *testing.T) {
	ctx := context.Background()
	store := &spyStore{}
	led := audit.NewLedger()
	a := newActs(store, led)

	_, err := a.ApplyRulesetWriteActivity(ctx, Request{
		Document: []byte(malformedD), Operator: "operator:kyriakosp", AdminAuthorized: true,
	})
	if !errors.Is(err, policy.ErrMalformedRule) {
		t.Fatalf("malformed ruleset err = %v, want policy.ErrMalformedRule", err)
	}
	if store.saveCalls != 0 {
		t.Fatalf("a malformed ruleset reached Save %d time(s) — it must be rejected BEFORE any persist", store.saveCalls)
	}
	if store.active != nil {
		t.Fatalf("a malformed ruleset was persisted as the active policy — fail-closed violated")
	}
	if led.Len() != 0 {
		t.Fatalf("a rejected ruleset write appended %d audit record(s) — a refused write ledgers nothing", led.Len())
	}
}

// Optimistic concurrency: a stale ExpectedVersion is refused (nothing persisted, nothing audited beyond the
// prior write); the correct ExpectedVersion swaps the active ruleset.
func TestActivity_ExpectedVersionCompareAndSwap(t *testing.T) {
	ctx := context.Background()
	store := &spyStore{}
	led := audit.NewLedger()
	a := newActs(store, led)

	// Seed v1.
	v1, err := a.ApplyRulesetWriteActivity(ctx, Request{Document: []byte(validDoc), Operator: "op:seed", AdminAuthorized: true})
	if err != nil {
		t.Fatalf("seed write: %v", err)
	}
	firstActive := append([]byte(nil), store.active...)

	// A STALE expected_version is refused — no overwrite, no new audit record.
	if _, err := a.ApplyRulesetWriteActivity(ctx, Request{
		Document: []byte(validDoc2), ExpectedVersion: "deadbeef-not-the-active-version", Operator: "op:stale", AdminAuthorized: true,
	}); !errors.Is(err, ErrStaleRuleset) {
		t.Fatalf("stale expected_version err = %v, want ErrStaleRuleset", err)
	}
	if string(store.active) != string(firstActive) {
		t.Fatalf("a stale-CAS write overwrote the active ruleset")
	}
	if led.Len() != 1 {
		t.Fatalf("a stale-CAS refusal appended an audit record: len=%d", led.Len())
	}

	// The CORRECT expected_version swaps the ruleset.
	v2, err := a.ApplyRulesetWriteActivity(ctx, Request{
		Document: []byte(validDoc2), ExpectedVersion: v1.Version, Operator: "op:swap", AdminAuthorized: true,
	})
	if err != nil {
		t.Fatalf("correct-CAS write refused: %v", err)
	}
	if v2.Version == v1.Version {
		t.Fatalf("the new version must differ from the swapped-out version %q", v1.Version)
	}
	if string(store.active) == string(firstActive) {
		t.Fatalf("the correct-CAS write did not replace the active ruleset")
	}
	if led.Len() != 2 {
		t.Fatalf("the swap was not audited: len=%d, want 2", led.Len())
	}
}

// Fail-closed: a nil store/ledger and a non-admin request are both refused with typed errors and touch
// nothing.
func TestActivity_FailClosed(t *testing.T) {
	ctx := context.Background()

	// nil deps.
	if _, err := (&Activities{}).ApplyRulesetWriteActivity(ctx, Request{Document: []byte(validDoc), Operator: "op", AdminAuthorized: true}); !errors.Is(err, ErrNoStore) {
		t.Fatalf("nil deps err = %v, want ErrNoStore", err)
	}

	// non-admin: refused BEFORE any validation/persist/audit.
	store := &spyStore{}
	led := audit.NewLedger()
	a := newActs(store, led)
	if _, err := a.ApplyRulesetWriteActivity(ctx, Request{Document: []byte(validDoc), Operator: "op:notadmin", AdminAuthorized: false}); !errors.Is(err, ErrNotAdmin) {
		t.Fatalf("non-admin err = %v, want ErrNotAdmin", err)
	}
	if store.saveCalls != 0 || store.active != nil || led.Len() != 0 {
		t.Fatalf("a non-admin write touched state: saveCalls=%d active=%v ledger=%d", store.saveCalls, store.active != nil, led.Len())
	}
}
