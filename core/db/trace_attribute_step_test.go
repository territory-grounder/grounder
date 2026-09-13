package db

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/trace"
)

// spec/023 REQ-2311 — the attribute step must reach the operator through the REAL pgx read, not just
// through a hand-built SpineRecords.
//
// WHY THIS EXISTS SEPARATELY FROM core/trace/attribute_step_test.go. Those oracles construct a
// TriageRecord in memory with Attribution already populated and assert the assembler emits the step.
// Every one of them passes while the SELECT never names actor_attribution — the assembler is correct
// and the column never arrives, so the step is absent in production and present in the tests. That is
// the exact shape this whole ticket is about (a determination that exists and never reaches the
// surface), reproduced one layer down, and only a real database can fail it.
//
// It is also the lesson from TG-408 the same day: the guard covering the pure function passed while the
// mutation removing the value's SOURCE survived, because nothing exercised the seam where the value is
// actually obtained.
//
// Gated on TG_TEST_POSTGRES_DSN (CI has no Postgres).
func TestAttributionReachesTheTraceThroughTheRealRead(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the attribute-step read test")
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

	uniq := fmt.Sprintf("trace-attr-it-%d", os.Getpid())
	withAttr, without := uniq+"-with", uniq+"-without"
	defer func() {
		_, _ = p.Exec(ctx, "DELETE FROM session_triage WHERE external_ref = ANY($1)", []string{withAttr, without})
	}()

	// The evidence blob is written in the shape the activity boundary marshals: domain-native records.
	// The REFERENCES are what the step may carry; the actor identity must NOT survive the projection.
	evidence := []byte(`[
	  {"domain":"pve","actor":"mallory@pam","action_kind":"vzstop","target":"web01",
	   "observed_at":"2026-08-07T09:00:00Z","ref":"UPID:dc1pve01:0029D107","covered":true},
	  {"domain":"journal","actor":"root","action_kind":"sudo","target":"web01",
	   "observed_at":"2026-08-07T09:01:00Z","ref":"c-4821","covered":true}
	]`)

	if err := NewTriageStore(p).RecordTriage(ctx, judge.TriageRow{
		ExternalRef: withAttr, Host: "web01", AlertRule: "NginxDown", Band: "POLL_PAUSE",
		Outcome: "proposed", Proposed: true, Op: "restart-service", Conclusion: "nginx down",
		Attribution: "attributed-suspicious", ActorEvidence: evidence,
	}); err != nil {
		t.Fatalf("record triage with attribution: %v", err)
	}
	// The mirror: a session that recorded NO taxonomy, written through the same writer.
	if err := NewTriageStore(p).RecordTriage(ctx, judge.TriageRow{
		ExternalRef: without, Host: "web02", AlertRule: "NginxDown", Band: "AUTO",
		Outcome: "proposed", Proposed: true, Op: "restart-service", Conclusion: "nginx down",
	}); err != nil {
		t.Fatalf("record triage without attribution: %v", err)
	}

	store := NewTraceSpineStore(p)

	// ── the session WITH a taxonomy ──────────────────────────────────────────────────────────────────
	rec, err := store.Load(ctx, withAttr)
	if err != nil {
		t.Fatalf("load %s: %v", withAttr, err)
	}
	if rec.Triage.Attribution != "attributed-suspicious" {
		t.Fatalf("the SELECT dropped actor_attribution — the assembler is correct and the column never "+
			"arrives, so the step is absent in production and present in the unit tests. Got %q",
			rec.Triage.Attribution)
	}
	if len(rec.Triage.AttributionEvidence) != 2 {
		t.Fatalf("want 2 evidence references from the projection, got %v", rec.Triage.AttributionEvidence)
	}
	for _, want := range []string{"pve:UPID:dc1pve01:0029D107", "journal:c-4821"} {
		var found bool
		for _, got := range rec.Triage.AttributionEvidence {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("evidence reference %q missing: %v", want, rec.Triage.AttributionEvidence)
		}
	}
	// INV-13: the projection carries POINTERS, never the actor identity. The stored blob holds
	// "mallory@pam" and "root"; a rendered trace step is exactly the value that reaches a screenshot,
	// a paste, and a ticket.
	for _, ref := range rec.Triage.AttributionEvidence {
		for _, leaked := range []string{"mallory@pam", "root", "vzstop", "sudo"} {
			if containsFold(ref, leaked) {
				t.Errorf("the evidence projection leaked %q into %q — the taxonomy is the finding, a "+
					"reference is where to check it", leaked, ref)
			}
		}
	}

	// And the ASSEMBLED walk carries the step — the property an operator actually sees.
	tr := trace.Assemble(withAttr, rec)
	var got *trace.Step
	for i := range tr.Steps {
		if tr.Steps[i].Kind == trace.StepAttribute {
			got = &tr.Steps[i]
		}
	}
	if got == nil {
		var kinds []string
		for _, s := range tr.Steps {
			kinds = append(kinds, string(s.Kind))
		}
		t.Fatalf("no attribute step in the walk assembled from the REAL read. Kinds: %v", kinds)
	}
	if got.Verdict != "attributed-suspicious" {
		t.Errorf("the assembled step must carry the taxonomy, got %q", got.Verdict)
	}

	// ── the session WITHOUT one: absent, never fabricated ────────────────────────────────────────────
	recNo, err := store.Load(ctx, without)
	if err != nil {
		t.Fatalf("load %s: %v", without, err)
	}
	if recNo.Triage.Attribution != "" {
		t.Errorf("a session with no taxonomy read back %q — COALESCE must yield empty, not a verdict",
			recNo.Triage.Attribution)
	}
	for _, s := range trace.Assemble(without, recNo).Steps {
		if s.Kind == trace.StepAttribute {
			t.Fatalf("a session that recorded no taxonomy produced an attribute step (%q) through the real "+
				"read — absent must not render as reached (INV-15)", s.Label)
		}
	}
}

// containsFold is a local case-insensitive substring check; the leak assertion must not depend on how a
// reader happened to case an actor principal.
func containsFold(haystack, needle string) bool {
	h, n := []rune(haystack), []rune(needle)
	if len(n) == 0 || len(n) > len(h) {
		return false
	}
	lower := func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 32
		}
		return r
	}
	for i := 0; i+len(n) <= len(h); i++ {
		match := true
		for j := range n {
			if lower(h[i+j]) != lower(n[j]) {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
