package db

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/skillstore"
)

// TestNotableIncidentsRoundTrip drives the REAL pgx query for the lesson flywheel's source (spec/014 REQ-1312):
// only ESCALATED resolved incidents are returned, each distilled into a lesson for the configured skill +
// dimension; a no-proposal stand-down is NOT notable; an unconfigured source is dormant. Gated on
// TG_TEST_POSTGRES_DSN (CI has no Postgres).
func TestNotableIncidentsRoundTrip(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the notable-incident round-trip test")
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

	uniq := fmt.Sprintf("lesson-it-%d", os.Getpid())
	escRef, okRef := uniq+"-escalated", uniq+"-resolved"
	defer func() { _, _ = p.Exec(ctx, "DELETE FROM session_triage WHERE external_ref = ANY($1)", []string{escRef, okRef}) }()

	tstore := NewTriageStore(p)
	seed := func(ref, outcome string) {
		if err := tstore.RecordTriage(ctx, judge.TriageRow{ExternalRef: ref, Host: "web01", AlertRule: "Disk-Full", Outcome: outcome}); err != nil {
			t.Fatalf("triage %s: %v", ref, err)
		}
	}
	seed(escRef, "escalated:handoff-limit") // notable — the agent could not resolve it autonomously
	seed(okRef, "no-proposal:stop")         // NOT notable — a stand-down, nothing to learn

	got, err := NewNotableIncidentStore(p, "triage-protocol", "correct_diagnosis", 100).NotableIncidents(ctx, time.Hour)
	if err != nil {
		t.Fatalf("notable incidents: %v", err)
	}
	byRef := map[string]skillstore.NotableIncident{}
	for _, ni := range got {
		byRef[ni.ExternalRef] = ni
	}
	esc, ok := byRef[escRef]
	if !ok {
		t.Fatal("the escalated incident must be a notable lesson source")
	}
	if esc.TargetSkill != "triage-protocol" || esc.TargetDimension != "correct_diagnosis" {
		t.Fatalf("a lesson must target the configured skill+dimension, got %+v", esc)
	}
	if !strings.Contains(esc.Lesson, "Disk-Full") || !strings.Contains(esc.Lesson, "ESCALATED") {
		t.Fatalf("the lesson must template the incident (rule + escalation), got %q", esc.Lesson)
	}
	if _, isNotable := byRef[okRef]; isNotable {
		t.Fatal("a no-proposal:stop stand-down is NOT notable — it must be excluded")
	}
	// An unconfigured source (empty skill) is dormant — returns nothing regardless of escalated incidents.
	if l, _ := NewNotableIncidentStore(p, "", "correct_diagnosis", 100).NotableIncidents(ctx, time.Hour); l != nil {
		t.Fatalf("an empty target skill must yield a dormant source, got %d incident(s)", len(l))
	}
}
