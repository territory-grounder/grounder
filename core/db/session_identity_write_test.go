package db

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/territory-grounder/grounder/core/judge"
)

// TestSessionIdentityRoundTrip drives the REAL pgx path (INSERT -> SELECT) for the session's prompt/seed/model
// provenance (spec/020 T-020-9, REQ-2009). Like the confidence round-trip it guards the exact failure the
// in-memory twin hides: a struct field the SQL forgets to carry (the pgx-fake-hides-field-drop lesson) — it
// writes via the real RecordTriage INSERT and reads back via the real SessionIdentity SELECT, so a dropped
// column fails here even though MemTriageStore would pass. Gated on TG_TEST_POSTGRES_DSN (CI has no Postgres).
func TestSessionIdentityRoundTrip(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the session-identity round-trip test")
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
	s := NewTriageStore(p)

	uniq := fmt.Sprintf("sess-id-it-%d", os.Getpid())
	withProv, bare := uniq+"-prov", uniq+"-bare"
	defer func() {
		_, _ = p.Exec(ctx, "DELETE FROM session_triage WHERE external_ref = ANY($1)", []string{withProv, bare})
	}()

	// A composed session carries the prompt/seed/model provenance; a pre-fix / bare row reads back empty.
	if err := s.RecordTriage(ctx, judge.TriageRow{
		ExternalRef: withProv, Host: "web01", AlertRule: "HostDown", Outcome: "proposal", Proposed: true,
		PromptVersion: "preamble/1", SeedHash: "0f1e2d3c", ModelTier: "fast",
	}); err != nil {
		t.Fatalf("record with provenance: %v", err)
	}
	if err := s.RecordTriage(ctx, judge.TriageRow{
		ExternalRef: bare, Host: "web02", AlertRule: "HostDown", Outcome: "no-proposal:stop", Proposed: false,
	}); err != nil {
		t.Fatalf("record bare: %v", err)
	}

	id, ok, err := s.SessionIdentity(ctx, withProv)
	if err != nil {
		t.Fatalf("read identity: %v", err)
	}
	if !ok {
		t.Fatalf("provenance row not found — the INSERT or SELECT dropped the session-identity join")
	}
	want := SessionIdentity{PromptVersion: "preamble/1", SeedHash: "0f1e2d3c", ModelTier: "fast"}
	if id != want {
		t.Fatalf("session identity round-trip: got %+v, want %+v — the pgx INSERT/SELECT dropped a column", id, want)
	}

	// Backward-compatible: a row written without provenance reads back the empty defaults, never an error.
	bareID, ok, err := s.SessionIdentity(ctx, bare)
	if err != nil {
		t.Fatalf("read bare identity: %v", err)
	}
	if !ok || bareID != (SessionIdentity{}) {
		t.Fatalf("bare identity: got %+v (present=%v), want all-empty present row", bareID, ok)
	}
}
