package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/judge"
)

// TG-190a — the leak count against a REAL Postgres, because every risk here is SQL semantics.
//
// The in-process oracles in cmd/worker drive collectSyntheticLeak with a stub and prove the four zeros are
// distinguishable. They cannot fail if the COLUMN does not exist, if the FILTER clause counts the wrong
// rows, or if migration 0069's default lets an existing row read as synthetic — and those are the three
// ways this tripwire would silently certify a contaminated corpus.
//
// Gated on TG_TEST_POSTGRES_DSN (CI has no Postgres).
func TestSyntheticLeakCountsOnlyMarkedRows(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the synthetic-leak count test")
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

	uniq := fmt.Sprintf("synleak-it-%d", os.Getpid())
	real1, real2, canary := uniq+"-real1", uniq+"-real2", uniq+"-canary"
	defer func() {
		_, _ = p.Exec(ctx, "DELETE FROM session_triage WHERE external_ref = ANY($1)",
			[]string{real1, real2, canary})
	}()

	base, err := s.LeakCount(ctx)
	if err != nil {
		t.Fatalf("baseline leak count: %v", err)
	}

	// Two REAL sessions through the real writer. Nothing in the runner writes the marker, so migration
	// 0069's DEFAULT false is what makes these read as real — the property that keeps the tripwire at 0.
	for _, ref := range []string{real1, real2} {
		if err := s.RecordTriage(ctx, judge.TriageRow{
			ExternalRef: ref, Host: "web01", AlertRule: "NginxDown", Band: "AUTO",
			Outcome: "proposed", Proposed: true, Op: "restart-service",
		}); err != nil {
			t.Fatalf("record real triage %s: %v", ref, err)
		}
	}

	afterReal, err := s.LeakCount(ctx)
	if err != nil {
		t.Fatalf("leak count after real rows: %v", err)
	}
	if afterReal.Leaked != base.Leaked {
		t.Fatalf("REAL sessions moved the leak count %d -> %d. Migration 0069's DEFAULT false is what "+
			"keeps every existing and newly-written row out of this counter; if the default changed, the "+
			"tripwire now fires on healthy traffic forever", base.Leaked, afterReal.Leaked)
	}
	if afterReal.Total != base.Total+2 {
		t.Errorf("the DENOMINATOR must grow with real traffic: %d -> %d, want +2", base.Total, afterReal.Total)
	}
	if !afterReal.Clean() {
		t.Errorf("a populated database with no marked rows must report Clean(): %+v", afterReal)
	}

	// Now a LEAKED canary — a row that reached the live database carrying the marker. Written by UPDATE
	// rather than through RecordTriage on purpose: no production writer sets this column, and a test that
	// needed one would be asserting a writer that must not exist.
	if err := s.RecordTriage(ctx, judge.TriageRow{
		ExternalRef: canary, Host: "web01", AlertRule: "NginxDown", Band: "AUTO",
		Outcome: "proposed", Proposed: true, Op: "restart-service",
	}); err != nil {
		t.Fatalf("record canary row: %v", err)
	}
	if _, err := p.Exec(ctx, "UPDATE session_triage SET synthetic = true WHERE external_ref = $1", canary); err != nil {
		t.Fatalf("mark canary: %v", err)
	}

	leaked, err := s.LeakCount(ctx)
	if err != nil {
		t.Fatalf("leak count after canary: %v", err)
	}
	if leaked.Leaked != base.Leaked+1 {
		t.Fatalf("a leaked canary row must be counted: %d -> %d, want +1", base.Leaked, leaked.Leaked)
	}
	if leaked.Clean() {
		t.Error("Clean() must be FALSE while a marked row sits in the live database")
	}
}

// An EMPTY population is not a clean bill of health, and this is the assertion that keeps the tripwire
// from certifying a database it has not actually examined.
func TestAnEmptyPopulationIsNotClean(t *testing.T) {
	if (SyntheticLeak{Leaked: 0, Total: 0}).Clean() {
		t.Error("0 leaked of 0 rows is an unmeasured database, not a clean one — a canary's safety " +
			"argument may not rest on a population that does not exist")
	}
	if !(SyntheticLeak{Leaked: 0, Total: 1}).Clean() {
		t.Error("0 leaked of a non-empty population IS clean")
	}
	if (SyntheticLeak{Leaked: 1, Total: 1000}).Clean() {
		t.Error("one leaked row is not clean, however large the denominator")
	}
}

// The liveness reader must carry the marker through, or judge_liveness's Synthetic exclusion stays dead.
// This is the consumer TG-190 was filed on: it has reserved the flag since it was written.
func TestRecentlyEndedCarriesTheSyntheticMarker(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database")
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

	uniq := fmt.Sprintf("synmark-it-%d", os.Getpid())
	realRef, canaryRef := uniq+"-real", uniq+"-canary"
	defer func() {
		_, _ = p.Exec(ctx, "DELETE FROM session_triage WHERE external_ref = ANY($1)", []string{realRef, canaryRef})
	}()

	ts := NewTriageStore(p)
	for _, ref := range []string{realRef, canaryRef} {
		if err := ts.RecordTriage(ctx, judge.TriageRow{
			ExternalRef: ref, Host: "web01", AlertRule: "NginxDown", Band: "AUTO",
			Outcome: "proposed", Proposed: true, Op: "restart-service",
		}); err != nil {
			t.Fatalf("record %s: %v", ref, err)
		}
	}
	if _, err := p.Exec(ctx, "UPDATE session_triage SET synthetic = true WHERE external_ref = $1", canaryRef); err != nil {
		t.Fatalf("mark canary: %v", err)
	}

	g := NewGovernanceReadStore(p, time.Hour, 500)
	sessions, err := g.RecentlyEnded(ctx)
	if err != nil {
		t.Fatalf("recently ended: %v", err)
	}
	var sawReal, sawCanary bool
	for _, s := range sessions {
		switch s.SessionID {
		case realRef:
			sawReal = true
			if s.Synthetic {
				t.Error("a REAL session read back as synthetic — it would drop out of the judge-liveness " +
					"denominator, inflating the judged fraction and hiding a dead judge")
			}
		case canaryRef:
			sawCanary = true
			if !s.Synthetic {
				t.Error("a MARKED session read back as real — judge_liveness has reserved this flag since " +
					"it was written, and it stays dead until the reader carries it")
			}
		}
	}
	if !sawReal || !sawCanary {
		t.Fatalf("precondition: both rows must be inside the read window (real=%v canary=%v) — otherwise "+
			"this oracle passed over an empty set", sawReal, sawCanary)
	}
}
