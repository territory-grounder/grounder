package db

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/instrumentid"
	"github.com/territory-grounder/grounder/core/judge"
)

// TG-672 (migration 0132), REAL Postgres: the identity row is written once with every component and the names of
// the unrecorded ones; the session row and the run row carry the stamp.
func TestInstrumentIdentityIsRecordedOnceAndStampedOnSessionAndRun(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()
	run := fmt.Sprintf("tg672id-%d", time.Now().UnixNano())
	c := instrumentid.Components{BuildSHA: run, HarnessFP: strings.Repeat("a", 64), OpSchemaHash: strings.Repeat("b", 64),
		RubricVersion: judge.RubricVersion(), SkillSetHash: strings.Repeat("c", 64)}
	rec := instrumentid.Record{ID: c.ID(), Components: c}
	st := NewInstrumentIdentityStore(p)
	for i := 0; i < 2; i++ {
		if err := st.Ensure(ctx, rec); err != nil {
			t.Fatalf("ensure %d: %v", i, err)
		}
	}
	if err := NewInstrumentIdentityStore(p).Ensure(ctx, rec); err != nil {
		t.Fatalf("ensure from a second process: %v", err)
	}
	var n int
	var unrec, policy string
	if err := p.QueryRow(ctx, `SELECT count(*), min(unrecorded), min(policy_bundle) FROM instrument_identity WHERE instrument_id = $1`, rec.ID).Scan(&n, &unrec, &policy); err != nil {
		t.Fatalf("read: %v", err)
	}
	if n != 1 || unrec != "policy_bundle,model_alias_hash,knowledge_hash" || policy != "" {
		t.Fatalf("rows=%d unrecorded=%q policy=%q — want one row naming the three missing components", n, unrec, policy)
	}
	if err := st.Ensure(ctx, instrumentid.Record{}); err == nil {
		t.Fatal("an empty id must be refused")
	}

	tri := NewTriageStore(p)
	row := judge.TriageRow{ExternalRef: run + "-s", Host: "h", AlertRule: "r", Band: "AUTO", Outcome: "proposed", Proposed: true,
		HarnessFP: c.HarnessFP, InstrumentID: rec.ID, CreatedAt: time.Now()}
	if err := tri.RecordTriage(ctx, row); err != nil {
		t.Fatalf("record triage: %v", err)
	}
	if err := tri.RecordRun(ctx, run+"-run1", row); err != nil {
		t.Fatalf("record run: %v", err)
	}
	var sid, rid string
	if err := p.QueryRow(ctx, `SELECT instrument_id FROM session_triage WHERE external_ref = $1`, row.ExternalRef).Scan(&sid); err != nil || sid != rec.ID {
		t.Fatalf("session_triage.instrument_id = %q (%v), want the stamp", sid, err)
	}
	if err := p.QueryRow(ctx, `SELECT instrument_id FROM triage_run WHERE external_ref = $1 AND run_id = $2`, row.ExternalRef, run+"-run1").Scan(&rid); err != nil || rid != rec.ID {
		t.Fatalf("triage_run.instrument_id = %q (%v), want the stamp", rid, err)
	}
}
