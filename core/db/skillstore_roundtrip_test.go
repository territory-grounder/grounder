package db

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/skillstore"
)

// TestSkillVersionRoundTrip drives the REAL pgx path for skill_version — PutSkill + CreateVersion +
// SetOfflineEval → GetVersion — so a struct field the SQL forgets to carry (as the TriageRow prediction was,
// TG-61 seq B) fails HERE instead of silently dropping in prod. It guards OfflineEval especially: TG-65 found
// it silently degenerating to id-order for want of a COALESCE — exactly the field-handling class the in-memory
// fake sink cannot catch. Gated on TG_TEST_POSTGRES_DSN (CI has no Postgres); part of the pgx round-trip
// coverage backfill the persistence audit recommended.
func TestSkillVersionRoundTrip(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the skill_version round-trip test")
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
	s := NewSkillStore(p)

	name := fmt.Sprintf("rt-skill-%d", os.Getpid())
	defer func() {
		_, _ = p.Exec(ctx, "DELETE FROM skill_version WHERE skill_name = $1", name)
		_, _ = p.Exec(ctx, "DELETE FROM skill WHERE name = $1", name)
	}()

	if err := s.PutSkill(ctx, skillstore.Skill{Name: name, Kind: "behavioral", Position: 99}); err != nil {
		t.Fatalf("put skill: %v", err)
	}
	body := "Test skill body — verify the pgx round-trip carries every persisted field."
	aw := skillstore.AppliesWhen{} // empty predicate is valid (ValidatePredicate)
	v := skillstore.Version{
		SkillName: name, Version: "1.0.0-rt", Body: body, AppliesWhen: aw,
		ContentHash: skillstore.ContentHash(body, aw),
		Author:      "roundtrip-test", Source: "flywheel:discovery falsifiable_prediction",
		Rationale:   "round-trip guard",
	}
	created, err := s.CreateVersion(ctx, v)
	if err != nil {
		t.Fatalf("create version: %v", err)
	}

	// OfflineEval is written by a SEPARATE UPDATE (the admission path) — the field TG-65 found degenerating.
	evalBlob := json.RawMessage(`{"detail":"discovery falsifiable_prediction delta +1.500; regression set held"}`)
	if err := s.SetOfflineEval(ctx, created.ID, evalBlob); err != nil {
		t.Fatalf("set offline eval: %v", err)
	}

	got, err := s.GetVersion(ctx, created.ID)
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	if got.SkillName != name || got.Version != "1.0.0-rt" || got.Body != body ||
		got.ContentHash != v.ContentHash || got.Author != "roundtrip-test" || got.Source != v.Source ||
		got.Rationale != "round-trip guard" {
		t.Fatalf("version fields must round-trip: %+v", got)
	}
	// Load-bearing assertion: OfflineEval must survive the round-trip (compare semantically — jsonb normalizes
	// whitespace, so a byte compare is wrong). A silent drop would return nil here (the TG-65 degeneration shape).
	var gotEval map[string]string
	if err := json.Unmarshal(got.OfflineEval, &gotEval); err != nil {
		t.Fatalf("OfflineEval must round-trip as parseable JSON, got %q: %v", got.OfflineEval, err)
	}
	if !strings.Contains(gotEval["detail"], "delta +1.500") {
		t.Fatalf("OfflineEval detail must survive the round-trip, got %q", got.OfflineEval)
	}
}
