package nativedb

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/credential"
)

func bindRows(s *Source, rows []RuleRow) {
	s.Bind(func(context.Context) ([]RuleRow, error) { return rows, nil })
}

// Two well-formed rows sync to two machine-plane entries, each keyed by its row id and carrying the
// parsed selector + SecretRef-only bundle.
func TestSyncMapsRowsToEntries(t *testing.T) {
	s := New()
	bindRows(s, []RuleRow{
		{ID: 7, Entry: "host-glob:web-*|deploy|22|ssh|env:TG_TEST_KEY_A"},
		{ID: 9, Entry: "host:db01|postgres|2222|ssh|store:db01.key"},
	})
	entries, err := s.Sync(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].NativeID != "row-7" || entries[1].NativeID != "row-9" {
		t.Fatalf("native ids must be the row ids: %q / %q", entries[0].NativeID, entries[1].NativeID)
	}
	if entries[0].Selector.Kind != credential.KindHostGlob || entries[0].Selector.Pattern != "web-*" {
		t.Fatalf("selector 0 = %+v", entries[0].Selector)
	}
	if entries[1].Bundle.User() != "postgres" || entries[1].Bundle.Port() != 2222 {
		t.Fatalf("bundle 1 = user %q port %d", entries[1].Bundle.User(), entries[1].Bundle.Port())
	}
}

// A malformed row fails the WHOLE sync and the error NAMES the row id — never a partial set (the
// CredentialSource contract: an error retains the prior converged state; a silent skip would orphan
// nothing visibly and resolve the estate against half a rule table).
func TestSyncMalformedRowFailsWholeSyncNamingRow(t *testing.T) {
	s := New()
	bindRows(s, []RuleRow{
		{ID: 1, Entry: "host:ok|root|22|ssh|env:K"},
		{ID: 42, Entry: "host:broken|root"}, // too few fields — ParseRules refuses
	})
	entries, err := s.Sync(context.Background())
	if err == nil {
		t.Fatalf("a malformed row synced: %d entries returned", len(entries))
	}
	if !strings.Contains(err.Error(), "42") {
		t.Fatalf("the refusal must name the offending row id 42, got: %v", err)
	}
	if entries != nil {
		t.Fatalf("a failed sync returned a partial set: %+v", entries)
	}
}

// A row packing MORE than one rule (';'-joined) is refused the same way: one row, one rule — the shape
// that keeps deletes precise (a row id maps to exactly one resolver rule).
func TestSyncMultiRuleRowRefused(t *testing.T) {
	s := New()
	bindRows(s, []RuleRow{
		{ID: 5, Entry: "host:a|root|22|ssh|env:K; host:b|root|22|ssh|env:K"},
	})
	if _, err := s.Sync(context.Background()); err == nil || !strings.Contains(err.Error(), "row 5") {
		t.Fatalf("a two-rule row must fail the sync naming row 5, got: %v", err)
	}
}

// Until Bind lands (the pool has not connected), Sync fails closed with the pool-not-yet-connected error —
// prior converged state retained, never an empty set.
func TestSyncUnboundLoaderFailsClosed(t *testing.T) {
	s := New()
	_, err := s.Sync(context.Background())
	if err == nil {
		t.Fatalf("an unbound source synced successfully — it must fail closed")
	}
	if !strings.Contains(err.Error(), "not yet connected") {
		t.Fatalf("unbound error = %v, want the pool-not-yet-connected refusal", err)
	}
}

// A loader read failure surfaces as the sync's error (prior state retained), wrapped with the source name.
func TestSyncLoaderErrorSurfaces(t *testing.T) {
	s := New()
	boom := errors.New("relation does not exist")
	s.Bind(func(context.Context) ([]RuleRow, error) { return nil, boom })
	if _, err := s.Sync(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("loader error must surface, got: %v", err)
	}
}

// The source declares the machine plane and the stable id the composition root registers.
func TestIdentity(t *testing.T) {
	s := New()
	if s.ID() != "native-db" || s.Plane() != credential.PlaneMachine {
		t.Fatalf("identity = %q/%q, want native-db/machine", s.ID(), s.Plane())
	}
}

// The TG-451 atomic handoff under -race: Bind lands from the post-connect path while a sync loop is
// already reading. Reverting Source.load to a plain field reddens this test under the race detector —
// the same killing mutation cmd/worker's estate_loaders_test.go documents.
func TestBindSyncConcurrent(t *testing.T) {
	s := New()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			_, _ = s.Sync(context.Background())
		}
	}()
	for i := 0; i < 200; i++ {
		bindRows(s, []RuleRow{{ID: 1, Entry: "host:h|root|22|ssh|env:K"}})
	}
	<-done
	entries, err := s.Sync(context.Background())
	if err != nil || len(entries) != 1 {
		t.Fatalf("post-bind sync: %v (%d entries)", err, len(entries))
	}
}
