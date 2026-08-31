package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/forensic"
)

// TG-168 — the forensic window reader, against a REAL Postgres.
//
// THIS TEST EXISTS BECAUSE THE FIRST DRAFT OF THE QUERIES WAS WRONG IN FOUR PLACES, and nothing in Go
// could have told me: `credential_resolution` has no `principal` column (the identity is
// `resolved_user`), and `exec_class_decision` has neither `created_at` (it stamps `decided_at`) nor
// `host`. Every one of those compiles perfectly and fails at runtime, in production, on the first real
// question an operator asks. A unit test with an in-memory fake would have passed on all four.
//
// So this is DSN-gated rather than faked: the only assertion worth making about hand-written SQL is that
// the database accepts it.
func forensicTestPool(t *testing.T) *Pool {
	t.Helper()
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the forensic window reader test")
	}
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// EVERY LANE'S SQL MUST EXECUTE. This is the assertion the four schema mistakes would have failed.
func TestEveryForensicLaneQueryIsAcceptedByPostgres(t *testing.T) {
	p := forensicTestPool(t)
	s := NewForensicStore(p)

	// A window in the far past: the point is that each statement PARSES and RUNS, not that it matches rows.
	w := forensic.Window{
		From: time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2001, 1, 2, 0, 0, 0, 0, time.UTC),
	}
	if _, err := s.Window(context.Background(), w, "", 10); err != nil {
		t.Fatalf("a lane query was rejected by Postgres: %v\n\nThis is exactly the failure this test "+
			"exists for — the queries are hand-written against a schema that has drifted, and a Go-level "+
			"fake cannot see a column that does not exist.", err)
	}
	// And with a host filter set, which takes the other branch of every `($3 = '' OR ...)` predicate.
	if _, err := s.Window(context.Background(), w, "web01", 10); err != nil {
		t.Fatalf("the host-filtered form was rejected: %v", err)
	}
}

// A REAL WINDOW RETURNS REAL ROWS, ORDERED. Without this, the query test above passes over an empty
// result set forever and proves only that the SQL parses.
func TestTheForensicWindowReturnsOrderedRowsItActuallyWrote(t *testing.T) {
	p := forensicTestPool(t)
	ctx := context.Background()
	s := NewForensicStore(p)

	base := time.Now().UTC().Truncate(time.Second).Add(-90 * 24 * time.Hour)
	ref := fmt.Sprintf("forensic-it-%d", os.Getpid())
	t.Cleanup(func() {
		_, _ = p.Exec(ctx, "DELETE FROM ingest_alert WHERE external_ref = $1", ref)
		_, _ = p.Exec(ctx, "DELETE FROM agent_step WHERE external_ref = $1", ref)
	})

	// Two corpora, deliberately inserted OUT of chronological order so the ordering assertion is real.
	if _, err := p.Exec(ctx, `INSERT INTO ingest_alert
		(external_ref, source_type, source_id, alert_rule, severity, host, summary, received_at, schema_version)
		VALUES ($1,'test','test','ForensicProbe','warning','web01','probe alert',$2,1)`,
		ref, base.Add(2*time.Minute)); err != nil {
		t.Fatalf("seed ingest_alert: %v", err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO agent_step
		(external_ref, cycle, thought, tool, observation, outcome, created_at, schema_version)
		VALUES ($1,1,'t','probe-tool','o','ok',$2,1)`,
		ref, base.Add(1*time.Minute)); err != nil {
		t.Fatalf("seed agent_step: %v", err)
	}

	got, err := s.Window(ctx, forensic.Window{From: base, To: base.Add(time.Hour)}, "", 100)
	if err != nil {
		t.Fatalf("window: %v", err)
	}

	var mine []forensic.Event
	for _, e := range got.Events {
		if e.SubjectRef == ref {
			mine = append(mine, e)
		}
	}
	if len(mine) != 2 {
		t.Fatalf("got %d of my 2 seeded events — with this failing, the ordering assertion below would "+
			"pass over an empty slice", len(mine))
	}
	// The agent step is EARLIER in time and must come first, despite being inserted second and despite
	// ingest ranking earlier on the causal tie-break. Time dominates.
	if mine[0].Source != forensic.SourceAgentStep {
		t.Errorf("order = [%s, %s]; the earlier event (agent_step at +1m) must precede the later one "+
			"(ingest_alert at +2m) — time dominates the source rank", mine[0].Source, mine[1].Source)
	}
}

// TRUNCATION MUST BE DECLARED. A silently capped narrative and a complete one are the same object, and
// this package exists so an operator can trust completeness.
func TestACappedLaneIsReportedAsTruncated(t *testing.T) {
	p := forensicTestPool(t)
	ctx := context.Background()
	s := NewForensicStore(p)

	base := time.Now().UTC().Truncate(time.Second).Add(-80 * 24 * time.Hour)
	ref := fmt.Sprintf("forensic-trunc-%d", os.Getpid())
	t.Cleanup(func() { _, _ = p.Exec(ctx, "DELETE FROM agent_step WHERE external_ref = $1", ref) })

	for i := 0; i < 3; i++ {
		if _, err := p.Exec(ctx, `INSERT INTO agent_step
			(external_ref, cycle, thought, tool, observation, outcome, created_at, schema_version)
			VALUES ($1,$2,'t','probe','o','ok',$3,1)`, ref, i, base.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	got, err := s.Window(ctx, forensic.Window{From: base, To: base.Add(time.Hour)}, "", 2)
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	var sawAgentStep bool
	for _, n := range got.Truncated {
		if n == "agent_step" {
			sawAgentStep = true
		}
	}
	if !sawAgentStep {
		t.Errorf("a lane hit its cap of 2 with 3 rows available and was NOT reported as truncated "+
			"(truncated=%v) — a partial reconstruction that does not say so reads as a complete one",
			got.Truncated)
	}
}

// An unbounded window is refused rather than widened — answering a different question from the one asked
// is worse than refusing.
func TestAnUnboundedForensicWindowIsRefusedBeforeTouchingTheDatabase(t *testing.T) {
	s := NewForensicStore(nil)
	if _, err := s.Window(context.Background(), forensic.Window{}, "", 10); err == nil {
		t.Error("an unbounded window was accepted")
	}
}
