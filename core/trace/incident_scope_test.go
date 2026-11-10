package trace

import (
	"testing"
	"time"
)

// A SESSION'S LIFECYCLE MUST COME FROM ITS OWN EXECUTION, NOT FROM A SHARED VERDICT ROW.
//
// action_verdict is keyed by the content-hashed action_id alone and is append-only first-wins: one row serves
// every incident that ever proposed the same action shape. deriveStatus keyed StatusExecuted off
// Verdict.Present, so an incident that merely proposed a shape someone else had executed days earlier
// inherited "executed". Measured live 2026-07-29 over 30 consecutive sessions: 22 read "executed" and all 22
// were reading a verdict stamped before their own proposal.
//
// The honest source is this incident's own execute gate — interceptor_gate_verdict carries external_ref, so
// the row belongs to one incident and no other.
//
// WHY THIS IS NOT A TAUTOLOGY: the first case supplies a verdict AND an execute gate and would pass either
// way. It is the second and third that carry the property — an incident with an execute gate and NO verdict
// must still read executed (the old code called that "proposed"), and an incident with a verdict but NO
// execute gate of its own must NOT read executed off a stranger's row. Deleting the new loop turns case 2
// red; deleting the reader's provenance check turns case 3 red.
func TestDeriveStatusUsesThisIncidentsExecuteGate(t *testing.T) {
	at := func(n int) time.Time { return time.Date(2026, 7, 29, 0, 0, n, 0, time.UTC) }

	cases := []struct {
		name string
		rec  SpineRecords
		want Status
		why  string
	}{
		{
			name: "executed with both gate and verdict",
			rec: SpineRecords{
				Triage:       TriageRecord{Present: true, Proposed: true, CreatedAt: at(1)},
				GateVerdicts: []GateVerdictRecord{{Ordinal: 1, Gate: "execute", Verdict: "pass", CreatedAt: at(2)}},
				Verdict:      VerdictRecord{Present: true, Verdict: "match", CreatedAt: at(3)},
			},
			want: StatusExecuted,
			why:  "the uncontroversial case — both signals agree",
		},
		{
			name: "executed on its own gate with no verdict yet",
			rec: SpineRecords{
				Triage:       TriageRecord{Present: true, Proposed: true, CreatedAt: at(1)},
				GateVerdicts: []GateVerdictRecord{{Ordinal: 1, Gate: "execute", Verdict: "pass", CreatedAt: at(2)}},
				// no Verdict: the shared row was claimed by an earlier incident, so this one never wrote its own
			},
			want: StatusExecuted,
			why: "the action DID run — this incident's own execute gate passed. Reporting it as merely " +
				"'proposed' because a first-wins ledger row was already taken understates what TG did",
		},
		{
			name: "not executed when the only evidence is a shared verdict row",
			rec: SpineRecords{
				Triage: TriageRecord{Present: true, Proposed: true, CreatedAt: at(1)},
				// no execute gate of its own; the reader must not have admitted a foreign verdict either
			},
			want: StatusProposed,
			why: "this is the live defect: 22 of 30 sessions read 'executed' on a verdict written before " +
				"their own proposal, one of them six days earlier",
		},
		{
			name: "a refused execute gate is not an execution",
			rec: SpineRecords{
				Triage:       TriageRecord{Present: true, Proposed: true, CreatedAt: at(1)},
				GateVerdicts: []GateVerdictRecord{{Ordinal: 1, Gate: "execute", Verdict: "refuse", CreatedAt: at(2)}},
			},
			want: StatusProposed,
			why:  "the gate ran and said no; 'pass' is load-bearing in the predicate, not decoration",
		},
		{
			name: "a passing NON-execute gate is not an execution",
			rec: SpineRecords{
				Triage:       TriageRecord{Present: true, Proposed: true, CreatedAt: at(1)},
				GateVerdicts: []GateVerdictRecord{{Ordinal: 1, Gate: "admission", Verdict: "pass", CreatedAt: at(2)}},
			},
			want: StatusProposed,
			why:  "every governed action passes admission; only the execute gate means it ran",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := deriveStatus(c.rec); got != c.want {
				t.Errorf("deriveStatus = %q, want %q — %s", got, c.want, c.why)
			}
		})
	}
}

// The verify STEP must not be emitted for a verdict the reader refused to admit. Assemble reads
// Verdict.Present, so this pins that the two stay consistent: a session whose foreign verdict was rejected
// shows no "Mechanical verdict" step at all, rather than a step with a zero timestamp.
func TestAssembleOmitsVerifyStepWhenVerdictNotAdmitted(t *testing.T) {
	at := func(n int) time.Time { return time.Date(2026, 7, 29, 0, 0, n, 0, time.UTC) }
	rec := SpineRecords{
		Triage:       TriageRecord{Present: true, Proposed: true, CreatedAt: at(1), Op: "start-guest"},
		GateVerdicts: []GateVerdictRecord{{Ordinal: 1, Gate: "execute", Verdict: "pass", CreatedAt: at(2)}},
		// Verdict deliberately absent — the reader rejected a row that predated this incident
	}
	tr := Assemble("librenms-dc1-000001", rec)
	for _, s := range tr.Steps {
		if s.Kind == StepVerify {
			t.Fatalf("a Mechanical verdict step was emitted for a session whose verdict was not admitted "+
				"(verdict=%q at=%s) — an absent proof must be visibly absent, never rendered blank",
				s.Verdict, s.At)
		}
	}
	if tr.Status != StatusExecuted {
		t.Errorf("status = %q, want %q — rejecting the foreign verdict must not cost the session the "+
			"execution it genuinely performed", tr.Status, StatusExecuted)
	}
}
