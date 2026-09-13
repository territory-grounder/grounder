package db

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/persist"
	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/schema"
	"github.com/territory-grounder/grounder/core/verify"
)

// TG-495: core/schema/version.go promises a reader-side fail-closed guard (schema.CheckRow) — a governed
// reader that decodes a row whose stored schema_version it cannot interpret (a FUTURE version > compiled)
// REFUSES rather than mis-reads. Before this change that guard was wired at ZERO readers; the v1->v2
// skill_version bump merely rode the gap. This oracle proves each adopting reader now:
//   (a) reads a validly-stamped row back UNHARMED (the guard does not false-trip on real v1 data), and
//   (b) FAILS CLOSED with a SchemaVersionError once the row's stored schema_version is bumped past the
//       compiled version out-of-band — the deploy-skew (a bumped writer outrunning an un-recompiled
//       reader) the guard exists to catch.
// Removing any single schema.CheckRow call turns its sub-case red — that is the killing mutation.
// Gated on TG_TEST_POSTGRES_DSN (it Migrates an empty database itself), like the other durable oracles.
func TestSchemaVersionReaderGuardFailsClosedOnFutureVersion(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to an empty database to run the schema-version reader-guard oracle")
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

	// future returns compiled+1 for a table — a version no compiled reader can interpret.
	future := func(tb schema.Table) int {
		cur, err := schema.Current(tb)
		if err != nil {
			t.Fatalf("schema.Current(%s): %v", tb, err)
		}
		return int(cur) + 1
	}
	// isFuture reports whether err is the guard's SchemaVersionError (a value, returned unwrapped by each reader).
	isFuture := func(err error) bool {
		var sve schema.SchemaVersionError
		return errors.As(err, &sve)
	}

	t.Run("escalation_queue/DuePending", func(t *testing.T) {
		s := NewEscalationStore(p)
		const ref = "tg495-escalation-guard"
		t.Cleanup(func() { _, _ = p.Exec(ctx, "DELETE FROM escalation_queue WHERE external_ref = $1", ref) })
		if _, err := s.Enqueue(ctx, ref, 0, time.Now().UTC().Add(-time.Hour)); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if _, err := s.DuePending(ctx, time.Now().UTC()); err != nil {
			t.Fatalf("DuePending on a validly-stamped row must NOT error (guard false-tripped): %v", err)
		}
		if _, err := p.Exec(ctx, "UPDATE escalation_queue SET schema_version = $1 WHERE external_ref = $2",
			future(schema.TableEscalationQueue), ref); err != nil {
			t.Fatalf("bump schema_version: %v", err)
		}
		if _, err := s.DuePending(ctx, time.Now().UTC()); !isFuture(err) {
			t.Fatalf("DuePending must FAIL CLOSED on a future schema_version — got %v, want SchemaVersionError", err)
		}
	})

	t.Run("discovered_scheduled_reboots/Get+List", func(t *testing.T) {
		s := NewScheduledReboots(p)
		const host = "tg495-scheduled-guard"
		t.Cleanup(func() { _, _ = p.Exec(ctx, "DELETE FROM discovered_scheduled_reboots WHERE host = $1", host) })
		sr := persist.ScheduledReboot{
			Host: host, Kind: "reboot", Cron: "0 4 * * *", Timezone: "UTC",
			State: persist.SRLive, Observations: 1,
			ValidFrom:      time.Now().UTC().Add(-time.Hour),
			ValidUntil:     time.Now().UTC().Add(24 * time.Hour),
			LastVerifiedAt: time.Now().UTC().Truncate(time.Second),
		}
		if err := s.Save(ctx, sr); err != nil {
			t.Fatalf("save: %v", err)
		}
		if _, _, err := s.Get(ctx, host, "reboot"); err != nil {
			t.Fatalf("Get on a validly-stamped row must NOT error: %v", err)
		}
		if _, err := s.List(ctx); err != nil {
			t.Fatalf("List on validly-stamped rows must NOT error: %v", err)
		}
		if _, err := p.Exec(ctx, "UPDATE discovered_scheduled_reboots SET schema_version = $1 WHERE host = $2",
			future(schema.TableDiscoveredScheduledReboots), host); err != nil {
			t.Fatalf("bump schema_version: %v", err)
		}
		if _, _, err := s.Get(ctx, host, "reboot"); !isFuture(err) {
			t.Fatalf("Get must FAIL CLOSED on a future schema_version — got %v, want SchemaVersionError", err)
		}
		if _, err := s.List(ctx); !isFuture(err) {
			t.Fatalf("List must FAIL CLOSED on a future schema_version — got %v, want SchemaVersionError", err)
		}
	})

	t.Run("infragraph_prediction/Get+DueForScoring", func(t *testing.T) {
		s := NewPredictionStore(p)
		fs := NewFalsifiabilityStore(p)
		const planHash = "tg495-prediction-guard"
		t.Cleanup(func() { _, _ = p.Exec(ctx, "DELETE FROM infragraph_prediction WHERE plan_hash = $1", planHash) })
		cur, err := schema.Current(schema.TableInfragraphPrediction)
		if err != nil {
			t.Fatalf("schema.Current: %v", err)
		}
		rec := predict.PredictionRecord{
			Prediction: verify.Prediction{
				ActionID: "tg495-act", PlanHash: planHash, TargetHost: "web01", Site: "nl",
				PredictedHosts: map[string]struct{}{"web01": {}},
				PredictedRules: map[string]struct{}{verify.RuleKey("web01", "HostDown"): {}},
			},
			ControlHosts:   map[string]struct{}{"web09": {}},
			SchemaVersion:  cur,
			PredictionHash: "tg495-prh",
			ExternalRef:    "tg495-ext",
		}
		if err := s.Commit(ctx, rec); err != nil {
			t.Fatalf("commit: %v", err)
		}
		// olderThan in the future so the just-committed row (committed_at=now, no verdict) is due for scoring.
		due := time.Now().UTC().Add(time.Minute)
		if _, ok, err := s.Get(ctx, planHash); err != nil || !ok {
			t.Fatalf("Get on a validly-stamped row must return it without error: ok=%v err=%v", ok, err)
		}
		if _, err := fs.DueForScoring(ctx, due, 1000); err != nil {
			t.Fatalf("DueForScoring on validly-stamped rows must NOT error: %v", err)
		}
		if _, err := p.Exec(ctx, "UPDATE infragraph_prediction SET schema_version = $1 WHERE plan_hash = $2",
			int(cur)+1, planHash); err != nil {
			t.Fatalf("bump schema_version: %v", err)
		}
		if _, _, err := s.Get(ctx, planHash); !isFuture(err) {
			t.Fatalf("Get must FAIL CLOSED on a future schema_version — got %v, want SchemaVersionError", err)
		}
		if _, err := fs.DueForScoring(ctx, due, 1000); !isFuture(err) {
			t.Fatalf("DueForScoring must FAIL CLOSED on a future schema_version — got %v, want SchemaVersionError", err)
		}
	})
}
