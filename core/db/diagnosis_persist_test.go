package db

import (
	"context"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/proposal"
)

// TG-201 — THE TYPED CLAIM MUST SURVIVE THE TRIP TO THE JUDGE, `cited` FLAGS AND ALL.
//
// The judge is ASYNCHRONOUS: it runs hours later off session_triage, when the transcript and the captured
// ToolResults are long gone. So the diagnosis dimension can only ever score what the WRITE put in the row
// and the READ took back out — and the `cited` flag in particular cannot be recomputed downstream, because
// the only authority for "this id names an observation the orchestrator really captured" was the
// orchestrator, at bind time. A round-trip that dropped the flag would silently convert every grounded
// citation into an ungrounded assertion and score honest sessions as sloppy ones.
//
// Exercised through the STORE'S OWN pair — RecordTriage then UnjudgedSince (the judge cron's real read
// path) — never hand-written SQL, so it fails if either half drifts and cannot pass by agreeing with a
// query written in the same file. Against a REAL Postgres: a fake round-trips a column missing from the
// INSERT perfectly, which is exactly the defect this file exists to catch.
//
// KILLING MUTATION: drop `diagnosis` from the INSERT column list (or from the SELECT). RED — the claim
// comes back empty and DiagnosisRecorded false, so every live session scores N/A and the dimension grades
// nothing, forever, with no other test noticing.
func TestDiagnosisPersistsOnTheTriageRow(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()

	st := &TriageStore{p: p}
	const ref = "gold-diagnosis-persist-1"
	_, _ = p.Exec(ctx, `DELETE FROM session_triage WHERE external_ref = $1`, ref)
	defer func() { _, _ = p.Exec(ctx, `DELETE FROM session_triage WHERE external_ref = $1`, ref) }()

	// The A2 case verbatim: the agent states the guest crashed while HOLDING the observation that the stop
	// was a deliberate operator task.
	if err := st.RecordTriage(ctx, judge.TriageRow{
		ExternalRef: ref, Host: "dc1pve01", AlertRule: "HostDown", Band: "POLL_PAUSE",
		Outcome: "proposed", Proposed: true, Op: "start-guest", CreatedAt: time.Now().UTC(),
		DiagnosisRecorded: true,
		Diagnosis: proposal.Diagnosis{
			RootCause:     "the guest crashed and needs restarting",
			Mechanism:     "an unclean shutdown left the guest stopped",
			Supporting:    []proposal.EvidenceRef{{ID: "lnms-1", Claim: "the guest is not running", Cited: true}},
			Contradicting: []proposal.EvidenceRef{{ID: "pve-tasks-101", Claim: "the stop was a DELIBERATE operator task", Cited: true}},
			RuledOut:      []proposal.RuledOut{{Cause: "host down", Reason: "the hypervisor is up", ID: "lnms-2", Cited: true}},
		},
	}); err != nil {
		t.Fatalf("record: %v", err)
	}

	got := readTriageRow(t, st, ref)
	if !got.DiagnosisRecorded {
		t.Fatal("the row came back with no diagnosis recorded — the judge would score every live session N/A " +
			"and the dimension would grade nothing")
	}
	if got.Diagnosis.RootCause != "the guest crashed and needs restarting" || got.Diagnosis.Mechanism == "" {
		t.Fatalf("the stated claim did not survive: %+v", got.Diagnosis)
	}
	if !got.Diagnosis.HasContradiction() {
		t.Fatal("the CONTRADICTION did not survive the round-trip — that is the one signal this whole type " +
			"exists to carry, and losing it re-creates the A2 failure with a record that looks clean")
	}
	if n := got.Diagnosis.CitedAssertions(); n != 3 {
		t.Fatalf("cited assertions read back as %d, wrote 3 — `cited` is decided by the ORCHESTRATOR at bind "+
			"time and cannot be recomputed later, so losing it turns grounded citations into bare assertions", n)
	}
	// The scorer must reach the floor THROUGH the durable path — that is the path the live cron scores.
	if score, _, ok := judge.ScoreDiagnosis(got.Facts()); !ok || score != 1 {
		t.Fatalf("the persisted contradicted diagnosis scored %d (applicable=%v) — the agent asserted a cause "+
			"its own captured evidence refutes and paid nothing", score, ok)
	}
}

// AN EMPTY CLAIM AND A PRE-MIGRATION ROW ARE DIFFERENT FACTS, and the column's NULL-ness is what carries
// the difference. "The agent bound nothing" is gradeable; "the field did not exist when this session ran"
// must never be graded, or migration 0056 retroactively fails every historical session — the TG-61
// global-floor class of defect.
func TestAnEmptyDiagnosisIsRecordedAsAValueNotAsAbsence(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()

	st := &TriageStore{p: p}
	const ref = "gold-diagnosis-persist-2"
	_, _ = p.Exec(ctx, `DELETE FROM session_triage WHERE external_ref = $1`, ref)
	defer func() { _, _ = p.Exec(ctx, `DELETE FROM session_triage WHERE external_ref = $1`, ref) }()

	// A proposing session that bound NO diagnosis — the write still records the field.
	if err := st.RecordTriage(ctx, judge.TriageRow{
		ExternalRef: ref, Host: "h", AlertRule: "r", Band: "AUTO", Outcome: "proposed", Proposed: true,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	got := readTriageRow(t, st, ref)
	if !got.DiagnosisRecorded {
		t.Fatal("an empty diagnosis was written as SQL NULL — indistinguishable from a pre-migration row, so " +
			"a proposal that bound no claim would be excused as 'the column did not exist'")
	}
	if score, _, ok := judge.ScoreDiagnosis(got.Facts()); !ok || score > 2 {
		t.Fatalf("a proposal with an explicitly empty diagnosis scored %d (applicable=%v) — a remedy asserts "+
			"what it remedies, and the absent-claim case is the one the dimension has to price", score, ok)
	}

	// The pre-migration shape, forced: NULL means the field did not exist for that session.
	if _, err := p.Exec(ctx, `UPDATE session_triage SET diagnosis = NULL WHERE external_ref = $1`, ref); err != nil {
		t.Fatalf("null the column: %v", err)
	}
	old := readTriageRow(t, st, ref)
	if old.DiagnosisRecorded {
		t.Fatal("a NULL diagnosis column read back as RECORDED — every session that ran before migration 0056 " +
			"would then be graded against a rule it was never offered")
	}
	if _, _, ok := judge.ScoreDiagnosis(old.Facts()); ok {
		t.Fatal("a pre-migration row was scored on the diagnosis axis — a dimension floored across a whole " +
			"population is what fired the flywheel's Regressed trigger for every skill at once (TG-61)")
	}
}

// readTriageRow returns the row through the JUDGE'S OWN read path, failing if it is invisible there — a row
// the judge cannot see is a row that is never scored.
func readTriageRow(t *testing.T, st *TriageStore, ref string) judge.TriageRow {
	t.Helper()
	rows, err := st.UnjudgedSince(context.Background(), time.Hour, 500)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("the judge's read path returned NO unjudged sessions at all — the assertions below would pass " +
			"vacuously over an empty set")
	}
	for i := range rows {
		if rows[i].ExternalRef == ref {
			return rows[i]
		}
	}
	t.Fatalf("the recorded triage row %q is not visible to the judge's own read path (%d row(s) read)", ref, len(rows))
	return judge.TriageRow{}
}
