package db

import (
	"fmt"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/trace"
)

// TG-580: the pgx lister returns every evidence row of ONE session in cycle order and nothing from another
// session; a session with no rows is an empty list, not an error. DSN-gated (two-DB recipe); rows are LEFT.
func TestAgentStepEvidenceForListsOneSessionInCycleOrder(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()
	st := NewAgentStepEvidenceStore(p)
	ref := fmt.Sprintf("tg580-%d", time.Now().UnixNano())
	for _, e := range []trace.AgentStepEvidence{
		{ExternalRef: ref, Cycle: 3, EvidenceID: "lnms-alerts-", Tool: "get-active-alerts", Payload: "c3"},
		{ExternalRef: ref, Cycle: 1, EvidenceID: "lnms-dev-web01", Tool: "get-device-status", Payload: "c1"},
		{ExternalRef: ref, Cycle: 2, EvidenceID: "check-host-disk-web01", Tool: "check-host-disk", Payload: "c2"},
		{ExternalRef: ref + "-other", Cycle: 1, EvidenceID: "lnms-dev-db01", Tool: "get-device-status", Payload: "other"},
	} {
		if err := st.EmitEvidence(ctx, e); err != nil {
			t.Fatalf("EmitEvidence: %v", err)
		}
	}
	got, err := st.EvidenceFor(ctx, ref)
	if err != nil {
		t.Fatalf("EvidenceFor: %v", err)
	}
	// The honesty fields are DERIVED at emit (Truncate bounds the payload and records what it did), so the
	// listing must carry what the store wrote: an unclipped 2-byte payload reads full_bytes=2, truncated=false.
	if len(got) != 3 || got[0].Cycle != 1 || got[1].Cycle != 2 || got[2].Cycle != 3 || got[0].Payload != "c1" || got[0].Truncated || got[0].FullBytes != 2 {
		t.Fatalf("want the session's three rows in cycle order with the emit-time honesty fields, got %+v", got)
	}
	for _, e := range got {
		if e.ExternalRef != ref {
			t.Fatalf("another session's row leaked into the listing: %+v", e)
		}
	}
	if none, err := st.EvidenceFor(ctx, ref+"-never"); err != nil || len(none) != 0 {
		t.Fatalf("a session with no evidence must list empty without error, got %d rows err=%v", len(none), err)
	}
}
