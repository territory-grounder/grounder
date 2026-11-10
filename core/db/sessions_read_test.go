package db

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/safety"
)

// TestRecentSurfacesTriageOnlySession drives the REAL pgx path for the sessions list (spec/006 REQ-509) to
// prove the broadened source: an agent run that reasoned and STOPPED leaves a session_triage row but seals no
// classification (no session_risk_audit) — it MUST still surface in the list, else the richest sessions (the
// agent investigations with tool-call cycles) are invisible. Guards the exact regression the in-memory fake
// hides — a query that only reads session_risk_audit drops triage-only refs (the pgx-fake-hides-field-drop
// lesson). It also asserts a classified session still appears (no regression). Gated on TG_TEST_POSTGRES_DSN
// (CI has no Postgres).
func TestRecentSurfacesTriageOnlySession(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the sessions-list round-trip test")
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

	uniq := fmt.Sprintf("sessions-read-it-%d", os.Getpid())
	triageOnlyRef := uniq + "-triage-only" // an agent run that STOPPED: triage row, no classification
	classifiedRef := uniq + "-classified"  // a normal classified session
	actionID := uniq + "-act"
	defer func() {
		_, _ = p.Exec(ctx, "DELETE FROM session_triage WHERE external_ref = ANY($1)", []string{triageOnlyRef, classifiedRef})
		_, _ = p.Exec(ctx, "DELETE FROM session_risk_audit WHERE external_ref = $1", classifiedRef)
	}()

	// triage-only session — the agent investigated and proposed no action (a STOP). No session_risk_audit.
	if err := NewTriageStore(p).RecordTriage(ctx, judge.TriageRow{
		ExternalRef: triageOnlyRef, Host: "librespeed01", AlertRule: "iface_flap", Band: "POLL_PAUSE",
		Outcome: "stop", Proposed: false, Op: "", Conclusion: "no action — flap self-cleared",
	}); err != nil {
		t.Fatalf("record triage-only: %v", err)
	}
	// classified session — a normal admission with a sealed action (regression control).
	if err := NewRiskAuditStore(p).PersistRiskAudit(audit.RiskAudit{
		ExternalRef: classifiedRef, RiskLevel: "low", Band: safety.BandAuto, AutoApproved: true,
		ActionID: actionID, SchemaVersion: 1,
	}); err != nil {
		t.Fatalf("persist classified: %v", err)
	}

	rows, err := NewSessionReadStore(p).Recent(ctx, 200)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	got := map[string]SessionRow{}
	for _, r := range rows {
		got[r.ExternalRef] = r
	}

	// the triage-only agent run MUST surface (the whole point of the broadening) with its triage band and
	// EMPTY classification fields (never fabricated).
	to, ok := got[triageOnlyRef]
	if !ok {
		t.Fatalf("triage-only session %q did not surface in the list — the list still reads session_risk_audit only", triageOnlyRef)
	}
	if to.Band != "POLL_PAUSE" {
		t.Errorf("triage-only band = %q, want the triage band POLL_PAUSE", to.Band)
	}
	if to.RiskLevel != "" || to.ActionID != "" {
		t.Errorf("triage-only session leaked non-empty classification fields (risk=%q action=%q) — must be empty, not fabricated", to.RiskLevel, to.ActionID)
	}
	// the classified session still appears (no regression).
	if _, ok := got[classifiedRef]; !ok {
		t.Fatalf("classified session %q vanished from the broadened list", classifiedRef)
	}
}
