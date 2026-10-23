package db

import (
	"context"
	"testing"

	"github.com/territory-grounder/grounder/core/judge"
)

// TG-575: the in-memory twin round-trips the harness identity; the pgx round-trip
// (TestSessionIdentityRoundTrip, DSN-gated) proves the real INSERT/SELECT carry the column.
func TestMemTriageStoreRoundTripsHarnessFP(t *testing.T) {
	m := NewMemTriageStore()
	if err := m.RecordTriage(context.Background(), judge.TriageRow{ExternalRef: "hfp-1", HarnessFP: "abc123"}); err != nil {
		t.Fatal(err)
	}
	id, ok, err := m.SessionIdentity(context.Background(), "hfp-1")
	if err != nil || !ok {
		t.Fatalf("read back: ok=%v err=%v", ok, err)
	}
	if id.HarnessFP != "abc123" {
		t.Fatalf("harness_fp dropped at the write→read seam: %+v", id)
	}
}
