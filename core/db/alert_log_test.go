package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/httpapi"
	"github.com/territory-grounder/grounder/core/trace"
)

// TestAlertLogDurableRoundTrip drives the REAL pgx durable alert log (ingest_alert, migration 0033): it
// Appends an accepted alert record, reads it back through Recent AND through the decision-tracer spine reader
// (by external_ref), and asserts the assembled walk carries a StepIngest with the non-secret front-door
// fields. This is the write→read→assemble round-trip the in-memory MemAlertLog could not exercise (the tracer
// reads Postgres). DSN-gated (CI has no Postgres).
func TestAlertLogDurableRoundTrip(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to run the durable alert-log round-trip test")
	}
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer p.Close()

	ref := fmt.Sprintf("ingest-it-%d-ref", os.Getpid())
	defer func() { _, _ = p.Exec(ctx, "DELETE FROM ingest_alert WHERE external_ref = $1", ref) }()

	store := NewAlertLogStore(p)
	store.Append(ctx, httpapi.AlertRecord{
		ExternalRef: ref, SourceType: "librenms", SourceID: "42", AlertRule: "Linux High Memory Usage",
		Severity: "warning", Host: "dc1librespeed01", Site: "nl", Summary: "memory 91% >= 90%",
		Labels: map[string]string{"device_id": "42"}, ObservedAt: time.Unix(1_784_000_000, 0),
		ReceivedAt: time.Unix(1_784_000_050, 0), WorkflowID: "tg/" + ref,
	})

	// Idempotency: a re-delivered webhook (same external_ref) must NOT create a duplicate row on this
	// append-only, DELETE-revoked table (ON CONFLICT (external_ref) DO NOTHING). The first acceptance wins.
	store.Append(ctx, httpapi.AlertRecord{
		ExternalRef: ref, SourceType: "librenms", AlertRule: "Linux High Memory Usage", Severity: "critical",
		Host: "dc1librespeed01", Site: "nl", Summary: "RE-DELIVERY should be dropped", ReceivedAt: time.Unix(1_784_000_099, 0),
	})
	var n int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM ingest_alert WHERE external_ref = $1", ref).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("re-delivery created a duplicate ingest_alert row (idempotency broken): %d rows", n)
	}

	// Recent must read the row back (the durable /v1/alerts view).
	recent, err := store.Recent(ctx, auth.Principal{}, 50)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	var found *httpapi.AlertRecord
	for i := range recent {
		if recent[i].ExternalRef == ref {
			found = &recent[i]
		}
	}
	if found == nil {
		t.Fatalf("appended alert did not read back through Recent (durable write failed?)")
	}
	if found.Severity != "warning" || found.Host != "dc1librespeed01" || found.Summary != "memory 91% >= 90%" ||
		found.Labels["device_id"] != "42" || found.ObservedAt.Unix() != 1_784_000_000 {
		t.Fatalf("alert fields dropped on round-trip: %+v", found)
	}

	// The decision-tracer must read the ingest boundary by external_ref and assemble a StepIngest.
	rec, err := NewTraceSpineStore(p).Load(ctx, ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !rec.Ingest.Present || rec.Ingest.AlertRule != "Linux High Memory Usage" || rec.Ingest.SourceType != "librenms" ||
		rec.Ingest.Severity != "warning" || rec.Ingest.Host != "dc1librespeed01" {
		t.Fatalf("ingest boundary not read by external_ref: %+v", rec.Ingest)
	}
	tr := trace.Assemble(ref, rec)
	if len(tr.Steps) == 0 || tr.Steps[0].Kind != trace.StepIngest {
		t.Fatalf("assembled walk did not lead with the ingest boundary: %+v", tr.Steps)
	}
	if tr.Steps[0].Reason == "" {
		t.Errorf("ingest step carries no front-door detail")
	}
}
