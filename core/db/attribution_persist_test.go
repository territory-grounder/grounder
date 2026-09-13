package db

import (
	"context"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/judge"
)

// spec/023 REQ-2311 — the attribution OUTCOME and its evidence must PERSIST on session_triage.
//
// Found by spec/023's new acceptance runner: the scenario was marked `present` while naming a migration and a
// function rather than a test, and nothing anywhere asserted the columns survive a write. The runner tests
// cover CLASSIFICATION; the judge tests cover READING the fact; nothing covered the WRITE.
//
// It exercises the STORE'S OWN round-trip — RecordTriage then UnjudgedSince — rather than hand-written SQL,
// because that pair IS the contract: UnjudgedSince is the judge cron's read path, and it derives
// ActorEvidenceCount as len(records). Asserting through it means the test fails if either half drifts, and
// cannot pass by agreeing with a query I wrote myself (the defect REQ-2512 exists to forbid).
//
// Against a REAL Postgres, never a fake. A fake returns whatever it was told to, so a column missing from an
// INSERT round-trips through it perfectly — this repo has already been bitten by exactly that. The real
// database earned its keep here immediately: my first version assumed actor_evidence was an integer count and
// CI answered "cannot unmarshal array into Go value of type int". It is jsonb holding the redacted records.
func TestAttributionPersistsOnTheTriageRow(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()

	st := &TriageStore{p: p}
	const ref = "gold-attr-persist-1"
	_, _ = p.Exec(ctx, `DELETE FROM session_triage WHERE external_ref = $1`, ref)
	defer func() { _, _ = p.Exec(ctx, `DELETE FROM session_triage WHERE external_ref = $1`, ref) }()

	if err := st.RecordTriage(ctx, judge.TriageRow{
		ExternalRef: ref, Host: "dc1mealie01", AlertRule: "Service-up/down",
		Band: "POLL_PAUSE", Outcome: "proposed", Conclusion: "an unsanctioned actor stopped the container",
		CreatedAt: time.Now().UTC(),
		// WRITE-side names: Attribution is the taxonomy; ActorEvidence is the jsonb blob of redacted records.
		// (ActorAttribution/ActorEvidenceCount are the READ model — the count is DERIVED as len(recs), an
		// asymmetry a fake would have let me get wrong.)
		Attribution: "attributed-suspicious",
		ActorEvidence: []byte(`[{"actor":"jimmy","verb":"stop","ref":"pve:task:1"},` +
			`{"actor":"jimmy","verb":"stop","ref":"pve:task:2"},` +
			`{"actor":"jimmy","verb":"stop","ref":"pve:task:3"}]`),
	}); err != nil {
		t.Fatalf("record: %v", err)
	}

	rows, err := st.UnjudgedSince(ctx, time.Hour, 500)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var got *judge.TriageRow
	for i := range rows {
		if rows[i].ExternalRef == ref {
			got = &rows[i]
		}
	}
	if got == nil {
		t.Fatal("the recorded triage row is not visible to the judge's own read path — a row the judge cannot " +
			"see is a row that is never scored")
	}
	// The taxonomy is the SECURITY-relevant half: "attributed-suspicious" is the value that must never be
	// auto-healed, so a write that dropped it would turn a security escalation into an ordinary heal.
	if got.ActorAttribution != "attributed-suspicious" {
		t.Fatalf("actor_attribution did not survive the round-trip: read %q — every downstream decision would "+
			"then read an unattributed incident", got.ActorAttribution)
	}
	// The COUNT is what distinguishes a grounded attribution from an asserted one, and it is derived from the
	// stored array — so a truncating write understates the grounding without erroring.
	if got.ActorEvidenceCount != 3 {
		t.Fatalf("actor_evidence did not survive: wrote 3 record(s), the judge's read path derives %d",
			got.ActorEvidenceCount)
	}
}

// An unattributed incident and an unrecorded one are DIFFERENT FACTS. The zero case must persist explicitly.
func TestUnattributedPersistsAsAValueNotAsAbsence(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()

	st := &TriageStore{p: p}
	const ref = "gold-attr-persist-2"
	_, _ = p.Exec(ctx, `DELETE FROM session_triage WHERE external_ref = $1`, ref)
	defer func() { _, _ = p.Exec(ctx, `DELETE FROM session_triage WHERE external_ref = $1`, ref) }()

	if err := st.RecordTriage(ctx, judge.TriageRow{
		ExternalRef: ref, Host: "h", AlertRule: "r", Band: "AUTO", Outcome: "proposed",
		CreatedAt: time.Now().UTC(), Attribution: "unattributable",
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	rows, err := st.UnjudgedSince(ctx, time.Hour, 500)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	for i := range rows {
		if rows[i].ExternalRef != ref {
			continue
		}
		if rows[i].ActorAttribution != "unattributable" || rows[i].ActorEvidenceCount != 0 {
			t.Fatalf("the unattributed case must persist EXPLICITLY as the taxonomy plus zero records, got %q/%d",
				rows[i].ActorAttribution, rows[i].ActorEvidenceCount)
		}
		return
	}
	t.Fatal("the unattributed row is not visible to the judge's read path")
}
