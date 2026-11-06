package db

import (
	"fmt"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/judge"
)

// TG-581: Row reads one session by ref with the judge cron's decoding (skill_loads decoded, unknown ref ⇒ ok=false),
// renders to the same record as the written row, and decodes exactly what the batch read decodes. DSN-gated.
func TestTriageRowByRefUsesTheJudgesDecoding(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()
	ts := NewTriageStore(p)
	ref := fmt.Sprintf("tg581-%d", time.Now().UnixNano())
	row := judge.TriageRow{ExternalRef: ref, Host: "h", AlertRule: "R", Band: "ESCALATE", Outcome: "escalated", Proposed: false,
		Conclusion: "down", EvidenceIDs: []string{"lnms-dev-h"}, SkillLoads: []string{"triage-protocol@1.1.0#290:store", "pack:proxmox"}}
	if err := ts.RecordTriage(ctx, row); err != nil {
		t.Fatalf("RecordTriage: %v", err)
	}
	got, ok, err := ts.Row(ctx, ref)
	if err != nil || !ok {
		t.Fatalf("Row: ok=%v err=%v", ok, err)
	}
	if got.Host != "h" || got.Band != "ESCALATE" || len(got.SkillLoads) != 2 || got.SkillLoads[0] != "triage-protocol@1.1.0#290:store" || len(got.EvidenceIDs) != 1 {
		t.Fatalf("row did not round-trip with the judge's decoding: %+v", got)
	}
	if judge.Record(got.Facts()) != judge.Record(row.Facts()) {
		t.Fatalf("the record rendered from the read row must equal the record of the written row")
	}
	// The batch read decodes the same columns: the row must appear in the unjudged window with the same facts.
	batch, err := ts.UnjudgedSince(ctx, time.Hour, 10000)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range batch {
		if b.ExternalRef == ref {
			found = true
			if judge.Record(b.Facts()) != judge.Record(got.Facts()) {
				t.Fatalf("UnjudgedSince and Row must decode the same row to the same facts")
			}
		}
	}
	if !found {
		t.Fatal("the fresh unjudged row must appear in the batch read")
	}
	if _, ok, err := ts.Row(ctx, ref+"-unknown"); err != nil || ok {
		t.Fatalf("an unknown ref must read ok=false without error, got ok=%v err=%v", ok, err)
	}
}
