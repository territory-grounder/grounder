package objectgroup

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
)

// ogSpyStore RECORDS whether Insert/Delete were reached, so a test can assert the killing property: a refused
// write never touches the store.
type ogSpyStore struct {
	mu          sync.Mutex
	nextID      int64
	rows        map[int64]string // id → name
	insertCalls int
	deleteCalls int
	lastName    string
	lastPats    []string
	lastPrec    string
	lastBy      string
}

func newOGSpyStore() *ogSpyStore { return &ogSpyStore{nextID: 100, rows: map[int64]string{}} }

func (s *ogSpyStore) Insert(_ context.Context, name string, patterns []string, precedence, createdBy string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.insertCalls++
	s.nextID++
	s.rows[s.nextID] = name
	s.lastName, s.lastPats, s.lastPrec, s.lastBy = name, patterns, precedence, createdBy
	return s.nextID, nil
}

func (s *ogSpyStore) Delete(_ context.Context, id int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteCalls++
	if _, ok := s.rows[id]; !ok {
		return false, nil
	}
	delete(s.rows, id)
	return true, nil
}

// fakeLedger records appends and hands back an incrementing seq.
type fakeLedger struct {
	mu      sync.Mutex
	appends int
	seq     int64
}

func (l *fakeLedger) AppendContext(_ context.Context, _ audit.GovDecision) (audit.LedgerEntry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.appends++
	l.seq++
	return audit.LedgerEntry{Seq: l.seq}, nil
}

func ogApply(store Store, ledger Ledger, req Request) (Result, error) {
	a := &Activities{D: Deps{Store: store, Ledger: ledger}}
	return a.ApplyObjectGroupWriteActivity(context.Background(), req)
}

func TestObjectGroupWrite_AddValid_LedgersThenPersists(t *testing.T) {
	st, lg := newOGSpyStore(), &fakeLedger{}
	res, err := ogApply(st, lg, Request{
		Verb: "add", Name: "webservers", Patterns: []string{" dc1demo-web* ", ""}, // trims + drops the empty
		Rationale: "cover the web tier", Operator: "operator:test", AdminAuthorized: true,
	})
	if err != nil {
		t.Fatalf("valid add refused: %v", err)
	}
	if res.ID == 0 || res.LedgerSeq == 0 {
		t.Fatalf("add result missing id/seq: %+v", res)
	}
	if st.insertCalls != 1 || lg.appends != 1 {
		t.Fatalf("expected 1 insert + 1 ledger append, got insert=%d append=%d", st.insertCalls, lg.appends)
	}
	if st.lastName != "webservers" || st.lastPrec != "union" { // precedence defaults to union
		t.Errorf("persisted name/precedence = %q/%q", st.lastName, st.lastPrec)
	}
	if len(st.lastPats) != 1 || st.lastPats[0] != "dc1demo-web*" {
		t.Errorf("patterns not trimmed/filtered: %v", st.lastPats)
	}
}

func TestObjectGroupWrite_AddRefusals_NeverLedgerOrPersist(t *testing.T) {
	base := Request{Verb: "add", Name: "g", Patterns: []string{"h*"}, Rationale: "r", Operator: "op", AdminAuthorized: true}
	mut := func(f func(*Request)) Request { r := base; f(&r); return r }
	cases := map[string]Request{
		"empty name":     mut(func(r *Request) { r.Name = "  " }),
		"no patterns":    mut(func(r *Request) { r.Patterns = nil }),
		"empty patterns": mut(func(r *Request) { r.Patterns = []string{"", "  "} }),
		"bad precedence": mut(func(r *Request) { r.Precedence = "override" }),
		"no rationale":   mut(func(r *Request) { r.Rationale = " " }),
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			st, lg := newOGSpyStore(), &fakeLedger{}
			_, err := ogApply(st, lg, req)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("expected ErrInvalid, got %v", err)
			}
			if st.insertCalls != 0 || lg.appends != 0 {
				t.Fatalf("a refused add must not ledger or persist: insert=%d append=%d", st.insertCalls, lg.appends)
			}
		})
	}
}

func TestObjectGroupWrite_Delete_FoundAndMissing(t *testing.T) {
	st, lg := newOGSpyStore(), &fakeLedger{}
	add, err := ogApply(st, lg, Request{Verb: "add", Name: "g", Patterns: []string{"h*"}, Rationale: "r", Operator: "op", AdminAuthorized: true})
	if err != nil {
		t.Fatalf("setup add: %v", err)
	}
	// delete the row → found.
	if _, err := ogApply(st, lg, Request{Verb: "delete", RowID: add.ID, Operator: "op", AdminAuthorized: true}); err != nil {
		t.Fatalf("delete of an existing row: %v", err)
	}
	// delete again → ErrNoSuchGroup, and the ledger was appended BEFORE the store discovered the miss
	// (ledger-first: the over-recorded side).
	appendsBefore := lg.appends
	_, err = ogApply(st, lg, Request{Verb: "delete", RowID: add.ID, Operator: "op", AdminAuthorized: true})
	if !errors.Is(err, ErrNoSuchGroup) {
		t.Fatalf("expected ErrNoSuchGroup, got %v", err)
	}
	if lg.appends != appendsBefore+1 {
		t.Errorf("ledger-first: a missing-row delete still appends (over-recorded), appends %d→%d", appendsBefore, lg.appends)
	}
}

func TestObjectGroupWrite_VerbAdminDeps_FailClosed(t *testing.T) {
	st, lg := newOGSpyStore(), &fakeLedger{}
	if _, err := ogApply(st, lg, Request{Verb: "mutate", AdminAuthorized: true}); !errors.Is(err, ErrUnknownVerb) {
		t.Errorf("unknown verb: got %v", err)
	}
	if _, err := ogApply(st, lg, Request{Verb: "add", Name: "g", Patterns: []string{"h*"}, Rationale: "r", AdminAuthorized: false}); !errors.Is(err, ErrNotAdmin) {
		t.Errorf("non-admin: got %v", err)
	}
	if _, err := ogApply(nil, nil, Request{Verb: "add", AdminAuthorized: true}); !errors.Is(err, ErrNoStore) {
		t.Errorf("no store/ledger: got %v", err)
	}
	if st.insertCalls != 0 {
		t.Errorf("no refusal should reach the store, insertCalls=%d", st.insertCalls)
	}
}
