package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/proposal"
)

// theA2Diagnosis is the recorded failure in its own shape: the model proposes a restart while holding a
// GROUNDED observation that an operator stopped the guest deliberately, and offers one supporting assertion
// whose cited id the orchestrator never captured.
func theA2Diagnosis() proposal.Diagnosis {
	return proposal.Diagnosis{
		RootCause: "guest 101 is down because its unit failed to start",
		Mechanism: "systemd gave up after 3 restart attempts inside 60s",
		Supporting: []proposal.EvidenceRef{
			{ID: "incident-history-101", Claim: "two prior unclean shutdowns", Cited: true},
			{ID: "unit-config-101", Claim: "the unit restarts on boot", Cited: false},
		},
		Contradicting: []proposal.EvidenceRef{
			{ID: "pve-task-history-101", Claim: "root@pam ran vzstop on 101 four minutes before the alert", Cited: true},
		},
		RuledOut: []proposal.RuledOut{
			{Cause: "host out of memory", Reason: "the node reports 41% in use", ID: "host-metrics", Cited: true},
		},
	}
}

// TG-201 — THE CLAIM IS SCREENED BEFORE IT IS PERSISTED, ON BOTH TERMINAL PATHS (REQ-2606, INV-13).
//
// The diagnosis is model-authored prose quoting TOOL OUTPUT: untrusted host content that can carry a leaked
// credential. Every other model-derived field on this row passes screen.Scrub before the write; `diagnosis`
// arrived on the row without joining that list, and the omission was invisible while the only reader was the
// judge cron. The console surface reads this column straight onto an operator's screen.
//
// IT DRIVES THE REAL ACTIVITIES, not scrubDiagnosis directly — the function existing is worth nothing if the
// write path does not call it, and "screens the claim" is a property of the persisted row. Both terminal
// paths are covered because they are separate code with separate screen lists, which is exactly how
// RecordTriageActivity went unscreened for months while ShadowProposalActivity did not.
//
// KILLING MUTATION (executed): drop the `row.Diagnosis = scrubDiagnosis(row.Diagnosis)` line from
// RecordTriageActivity. RED — "the CLAIM reached the persisted row UNSCREENED at root_cause". Same line
// dropped from ShadowProposalActivity: RED on the shadow subtest.
func TestTheClaimIsScreenedBeforeItReachesThePersistedRow(t *testing.T) {
	const secret = "password=hunter2correcthorse"
	claim := func() proposal.Diagnosis {
		return proposal.Diagnosis{
			RootCause: "sshd rejected the key; the log line held " + secret,
			Mechanism: "the agent was handed " + secret + " in the tool output",
			Supporting: []proposal.EvidenceRef{
				{ID: "ev-1", Claim: "the banner quoted " + secret + " verbatim", Cited: true},
			},
			Contradicting: []proposal.EvidenceRef{
				{ID: "ev-2", Claim: "an earlier line already carried " + secret, Cited: true},
			},
			RuledOut: []proposal.RuledOut{
				{Cause: "expired key", Reason: "the rotation log names " + secret, ID: "ev-3", Cited: true},
			},
		}
	}
	// assertScreened is shared by both paths so neither can be quietly held to a weaker bar than the other.
	assertScreened := func(t *testing.T, got proposal.Diagnosis) {
		t.Helper()
		for _, f := range []struct{ where, val string }{
			{"root_cause", got.RootCause},
			{"mechanism", got.Mechanism},
			{"supporting[0].claim", firstRefClaim(got.Supporting)},
			{"contradicting[0].claim", firstRefClaim(got.Contradicting)},
			{"ruled_out[0].reason", firstAltReason(got.RuledOut)},
		} {
			if strings.Contains(f.val, "hunter2correcthorse") {
				t.Errorf("the CLAIM reached the persisted row UNSCREENED at %s (%q). REQ-2606 requires screen.Scrub "+
					"before persist — and this column is read onto an operator's screen by the #reasoning surface "+
					"and into the judge's diagnosis_grounded dimension (INV-13)", f.where, f.val)
			}
		}
		// VACUITY FLOOR: a scrub that blanked the fields would satisfy every check above while destroying the
		// claim. The surrounding text and the structure must survive.
		if !strings.Contains(got.RootCause, "sshd rejected the key") {
			t.Fatalf("the root cause lost its non-secret text (%q) — the checks above would then pass on empty "+
				"fields and prove nothing", got.RootCause)
		}
		if len(got.Supporting) != 1 || len(got.Contradicting) != 1 || len(got.RuledOut) != 1 {
			t.Fatalf("screening changed the SHAPE of the claim (%d/%d/%d refs, want 1/1/1) — a lane dropped on the "+
				"way to the row is a contradiction an operator never sees",
				len(got.Supporting), len(got.Contradicting), len(got.RuledOut))
		}
		if !got.HasContradiction() {
			t.Error("the screened claim no longer reports a grounded contradiction — the console's marker and the " +
				"judge's dimension both read that answer off these flags")
		}
	}

	t.Run("the ordinary terminal path", func(t *testing.T) {
		var got judge.TriageRow
		a := &Activities{D: Deps{TriageRecord: func(_ context.Context, r judge.TriageRow) error {
			got = r
			return nil
		}}}
		res, err := a.RecordTriageActivity(context.Background(), judge.TriageRow{
			ExternalRef: "librenms-1", Host: "dc1mealie01", AlertRule: "Service up/down",
			Op: "restart nginx", OpClass: "restart-service", Diagnosis: claim(),
		})
		if err != nil || !res.Recorded {
			t.Fatalf("record: err=%v res=%+v", err, res)
		}
		assertScreened(t, got.Diagnosis)
	})

	t.Run("the shadow proposal path", func(t *testing.T) {
		deps, sinks, _ := shadowDeps(t)
		acts := NewActivities(deps)
		if _, err := acts.ShadowProposalActivity(context.Background(), ShadowProposalInput{
			ActionID: "act-1", Target: "svc01",
			Row: judge.TriageRow{
				ExternalRef: "TG-shadow-diagnosis", Host: "svc01", AlertRule: "FluxDrift",
				Outcome: "proposed:shadow", Proposed: true,
				Op: "rotate", OpClass: "rotate-flux-capacitor", Diagnosis: claim(),
			},
		}); err != nil {
			t.Fatalf("shadow activity: %v", err)
		}
		if len(sinks.rows) != 1 {
			t.Fatalf("row must land, got %d", len(sinks.rows))
		}
		assertScreened(t, sinks.rows[0].Diagnosis)
	})
}

func firstRefClaim(in []proposal.EvidenceRef) string {
	if len(in) == 0 {
		return ""
	}
	return in[0].Claim
}

func firstAltReason(in []proposal.RuledOut) string {
	if len(in) == 0 {
		return ""
	}
	return in[0].Reason
}

// The pure projection's own contract, held without an activity: structure preserved, nothing filtered.
func TestScrubDiagnosisPreservesEveryLane(t *testing.T) {
	d := scrubDiagnosis(proposal.Diagnosis{
		RootCause: "sshd rejected the key; the log line held AKIAIOSFODNN7EXAMPLE as the account id",
		Mechanism: "the agent was handed AKIAIOSFODNN7EXAMPLE in the tool output",
		Supporting: []proposal.EvidenceRef{
			{ID: "ev-1", Claim: "the banner quoted AKIAIOSFODNN7EXAMPLE verbatim", Cited: true},
		},
		Contradicting: []proposal.EvidenceRef{
			{ID: "ev-2", Claim: "an earlier line already carried AKIAIOSFODNN7EXAMPLE", Cited: true},
		},
		RuledOut: []proposal.RuledOut{
			{Cause: "expired key", Reason: "the rotation log names AKIAIOSFODNN7EXAMPLE", ID: "ev-3", Cited: true},
		},
	})

	const secret = "AKIAIOSFODNN7EXAMPLE"
	// EVERY lane, not just the first: a scrub applied to root_cause alone would pass a one-field check while
	// leaving the secret in the contradicting lane, which is the lane an operator is most likely to read.
	for _, f := range []struct{ where, got string }{
		{"root_cause", d.RootCause},
		{"mechanism", d.Mechanism},
		{"supporting[0].claim", d.Supporting[0].Claim},
		{"contradicting[0].claim", d.Contradicting[0].Claim},
		{"ruled_out[0].reason", d.RuledOut[0].Reason},
	} {
		if strings.Contains(f.got, secret) {
			t.Errorf("a secret-shaped value survived into the stored claim at %s (%q) — the claim quotes untrusted "+
				"host output, and this column is read straight onto an operator's screen and into the judge (INV-13)",
				f.where, f.got)
		}
	}

	// VACUITY FLOOR for the scan above: if screen.Scrub stopped recognising this pattern — or if scrubDiagnosis
	// simply blanked the fields — every assertion would pass by matching nothing. The surrounding text must
	// survive, and the structure must be intact.
	if !strings.Contains(d.RootCause, "sshd rejected the key") {
		t.Fatalf("the root cause lost its non-secret text (%q) — the checks above would then pass on empty fields "+
			"and prove nothing", d.RootCause)
	}
	if len(d.Supporting) != 1 || len(d.Contradicting) != 1 || len(d.RuledOut) != 1 {
		t.Fatalf("the scrub changed the SHAPE of the claim (%d/%d/%d refs, want 1/1/1) — a lane dropped on the way "+
			"to the row is a contradiction an operator never sees",
			len(d.Supporting), len(d.Contradicting), len(d.RuledOut))
	}
}

// CITED SURVIVES THE SCRUB UNTOUCHED. It is the orchestrator's decision, made in agent/loop.go against the
// ToolResults it actually captured; the screen has no business re-deciding it, and an uncited assertion must
// stay in the row and stay marked rather than be filtered out on the way past.
//
// KILLING MUTATION (executed): set Cited from `ID != ""` inside scrubDiagnosis. RED — "an assertion whose id
// the orchestrator never captured was stored as CITED".
func TestTheScrubNeverTouchesTheOrchestratorsCitationDecision(t *testing.T) {
	d := scrubDiagnosis(theA2Diagnosis())

	if len(d.Supporting) != 2 {
		t.Fatalf("supporting refs = %d, want 2 — an ungrounded assertion was dropped rather than kept and marked",
			len(d.Supporting))
	}
	if d.Supporting[1].Cited {
		t.Fatal("an assertion whose id the orchestrator never captured was stored as CITED — a plausible, " +
			"well-formed, fabricated citation is now indistinguishable from a real one (INV-11)")
	}
	if d.Supporting[1].ID == "" {
		t.Fatal("the ungrounded citation id was blanked — \"asserted with no citation\" and \"cited an id nobody " +
			"captured\" are different failures and the console tells them apart by this field")
	}
	if !d.HasContradiction() {
		t.Fatal("the scrubbed claim no longer reports a grounded contradiction — the console's marker and the " +
			"judge's diagnosis_grounded dimension both read that answer off this value")
	}
	if d.UncitedAssertions() != 1 {
		t.Fatalf("uncited assertions = %d, want 1 — the count the console renders is derived from these flags",
			d.UncitedAssertions())
	}
}
