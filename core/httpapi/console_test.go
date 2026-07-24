package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/auth"
)

type fakeLedger struct {
	entries []audit.LedgerEntry
	err     error
}

func (f fakeLedger) Recent(_ context.Context, _ auth.Principal, _ int) ([]audit.LedgerEntry, error) {
	return f.entries, f.err
}

type fakeStats struct {
	stats Stats
	err   error
}

func (f fakeStats) Stats(_ context.Context, _ auth.Principal) (Stats, error) { return f.stats, f.err }

func TestWhoAmIHandler(t *testing.T) {
	t.Run("echoes source and the live mutation posture", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/v1/whoami", nil)
		Deps{Stats: fakeStats{stats: Stats{MutationEnabled: true}}}.whoamiHandler(w, r, auth.Principal{SourceID: "librenms"})
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		var got WhoAmI
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.Source != "librenms" || !got.MutationEnabled {
			t.Errorf("unexpected whoami: %+v", got)
		}
	})

	t.Run("nil StatsReader reports mutation OFF, never panics", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/v1/whoami", nil)
		Deps{}.whoamiHandler(w, r, auth.Principal{SourceID: "s1"})
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		var got WhoAmI
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if got.Source != "s1" || got.MutationEnabled {
			t.Errorf("nil stats must yield source=s1, mutation OFF; got %+v", got)
		}
	})
}

func TestLedgerHandler(t *testing.T) {
	entries := []audit.LedgerEntry{
		{Seq: 1, Decision: "predicted", ActionID: "a1", Hash: "h1", PrevHash: ""},
		{Seq: 2, Decision: "verified", ActionID: "a1", Hash: "h2", PrevHash: "h1"},
	}

	t.Run("returns entries", func(t *testing.T) {
		w := httptest.NewRecorder()
		Deps{Ledger: fakeLedger{entries: entries}}.ledgerHandler(w, httptest.NewRequest("GET", "/v1/ledger", nil), auth.Principal{})
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		var got LedgerPage
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(got.Entries) != 2 || got.Entries[1].Hash != "h2" {
			t.Errorf("unexpected entries: %+v", got.Entries)
		}
	})

	t.Run("nil reader fails closed to 503", func(t *testing.T) {
		w := httptest.NewRecorder()
		Deps{}.ledgerHandler(w, httptest.NewRequest("GET", "/v1/ledger", nil), auth.Principal{})
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503 (nil ledger must not panic or 200)", w.Code)
		}
	})

	t.Run("store error fails closed to 503", func(t *testing.T) {
		w := httptest.NewRecorder()
		Deps{Ledger: fakeLedger{err: errors.New("db down")}}.ledgerHandler(w, httptest.NewRequest("GET", "/v1/ledger", nil), auth.Principal{})
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", w.Code)
		}
	})

	t.Run("empty ledger returns [] not null", func(t *testing.T) {
		w := httptest.NewRecorder()
		Deps{Ledger: fakeLedger{entries: nil}}.ledgerHandler(w, httptest.NewRequest("GET", "/v1/ledger", nil), auth.Principal{})
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		var got map[string]json.RawMessage
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if string(got["entries"]) != "[]" {
			t.Errorf("entries = %s, want [] (never null)", got["entries"])
		}
	})
}
