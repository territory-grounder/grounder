package db

import (
	"context"
	"testing"

	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/proposal"
)

// TestListShadowProposalsCarriesTheDiagnosisSignal is the end-to-end SQL half of TG-307: the `diagnosis`
// column the judge scores and the #reasoning walk renders reaches the proposals lane through the real
// query — read, scanned and reduced to the operator's signal — not merely through a Go helper an oracle
// stubs. It seeds session_triage the way the runner does (RecordTriage marshals TriageRow.Diagnosis whole)
// and reads it back through ListShadowProposals, so a column rename, a scan-order slip, or a dropped SELECT
// term reddens here rather than in production.
//
// Three rows, three states an operator must be told apart:
//   - a claim that cited GROUNDED evidence AGAINST its own root cause — the recorded A2 failure, the signal;
//   - a grounded claim with nothing against it — recorded, but NO false alarm;
//   - a session that bound no claim at all — recorded=false, rendered as honest silence, never a green light.
func TestListShadowProposalsCarriesTheDiagnosisSignal(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer p.Close()

	s := NewTriageStore(p)
	const pfx = "shadowdiag-"
	cleanup := func() { _, _ = p.Exec(ctx, `DELETE FROM session_triage WHERE external_ref LIKE $1`, pfx+"%") }
	cleanup()
	defer cleanup()

	// The A2 case: the model proposes a restart while HOLDING the observation that the guest was stopped
	// deliberately (Cited=true, a real gathered id) — plus one supporting assertion it could not ground.
	contradicted := proposal.Diagnosis{
		RootCause: "guest 101 down after an unclean shutdown",
		Mechanism: "systemd gave up after three restarts in 60s",
		Supporting: []proposal.EvidenceRef{
			{ID: "incident-history-101", Claim: "two prior unclean shutdowns", Cited: true},
			{ID: "unit-config-101", Claim: "restarts on boot", Cited: false}, // uncited assertion
		},
		Contradicting: []proposal.EvidenceRef{
			{ID: "pve-task-history-101", Claim: "root@pam ran vzstop deliberately", Cited: true},
		},
	}
	clean := proposal.Diagnosis{
		RootCause:  "journald grew unbounded because vacuuming is disabled",
		Supporting: []proposal.EvidenceRef{{ID: "df-h", Claim: "root at 96%", Cited: true}},
	}

	seed := []struct {
		ref  string
		diag proposal.Diagnosis
	}{
		{pfx + "con", contradicted},
		{pfx + "clean", clean},
		{pfx + "none", proposal.Diagnosis{}}, // marshals to '{}' — Present() is false, recorded must be false
	}
	for _, r := range seed {
		if err := s.RecordTriage(ctx, judge.TriageRow{
			ExternalRef: r.ref, Host: "h1", AlertRule: "Service up/down", Band: "POLL_PAUSE",
			Outcome: "proposed", Proposed: true, Op: "restart guest", OpClass: "restart-guest",
			Conclusion: "seeded", Diagnosis: r.diag,
		}); err != nil {
			t.Fatalf("seed %s: %v", r.ref, err)
		}
	}

	rows, err := s.ListShadowProposals(ctx, 500)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byRef := map[string]ShadowProposalRow{}
	for _, r := range rows {
		byRef[r.ExternalRef] = r
	}

	con, ok := byRef[pfx+"con"]
	if !ok {
		t.Fatalf("the contradicted proposal never reached the shadow plane at all")
	}
	if !con.DiagnosisRecorded || !con.DiagnosisContradicted {
		t.Errorf("the A2 signal did not survive the SQL round-trip: recorded=%v contradicted=%v (want true/true) — "+
			"an operator reviewing this proposal would not learn the agent held evidence against its own root cause",
			con.DiagnosisRecorded, con.DiagnosisContradicted)
	}
	if con.DiagnosisUncited != 1 {
		t.Errorf("uncited = %d, want 1 (the one supporting assertion bound to no gathered observation)", con.DiagnosisUncited)
	}

	cln := byRef[pfx+"clean"]
	if !cln.DiagnosisRecorded || cln.DiagnosisContradicted {
		t.Errorf("a grounded, uncontradicted claim must read recorded=true contradicted=false, got %v/%v — a "+
			"false contradiction alarm is as corrosive as a missed one", cln.DiagnosisRecorded, cln.DiagnosisContradicted)
	}

	non := byRef[pfx+"none"]
	if non.DiagnosisRecorded || non.DiagnosisContradicted {
		t.Errorf("a session that bound no claim must read recorded=false (got %v): an empty claim served as a "+
			"grounded all-clear is the fabrication the honest-empty state exists to prevent", non.DiagnosisRecorded)
	}
}
