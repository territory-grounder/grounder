package db

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/judge"
)

// TG-394 slice 3 — THE DEGRADED-CAPABILITY SET MUST SURVIVE THE TRIP TO THE DURABLE RECORD (part 4).
//
// The stamp exists so a lexical-only investigation is legible AFTERWARDS: the analyst (and, hours later, the
// judge) reads the row long after the estate graph has recovered, when a degraded-retrieval session is
// otherwise indistinguishable from an ordinary one. So the value is only ever what the INSERT put in and the
// read took back out — exercised through the store's OWN write→read pair (RecordTriage then
// DegradedCapabilities), against a REAL Postgres, because a fake round-trips a column missing from the INSERT
// perfectly, which is exactly the defect this file exists to catch.
//
// KILLING MUTATION: drop `degraded_capabilities` from the INSERT column list in RecordTriage (or from the
// SELECT in DegradedCapabilities). RED — the set comes back empty/absent, so every live session reads
// "nothing degraded" and the lexical-only reason the record exists to carry is lost, silently, forever.
func TestDegradedCapabilitiesPersistOnTheTriageRow(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()

	st := &TriageStore{p: p}
	const ref = "gold-degraded-caps-1"
	_, _ = p.Exec(ctx, `DELETE FROM session_triage WHERE external_ref = $1`, ref)
	defer func() { _, _ = p.Exec(ctx, `DELETE FROM session_triage WHERE external_ref = $1`, ref) }()

	if err := st.RecordTriage(ctx, judge.TriageRow{
		ExternalRef: ref, Host: "goldhost-degraded", AlertRule: "HostDown", Band: "POLL_PAUSE",
		Outcome: "proposed", Proposed: true, Op: "start-guest", CreatedAt: time.Now().UTC(),
		// The pve03-cascade shape: retrieval ran lexical-only (embed degraded) while the journal hosts on the
		// dying hypervisor also fell out of the graph.
		DegradedCapabilities: []string{"embed", "journal-evidence"},
	}); err != nil {
		t.Fatalf("record: %v", err)
	}

	got, ok, err := st.DegradedCapabilities(ctx, ref)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !ok {
		t.Fatal("the recorded triage row is not visible to the degraded-capabilities reader — the stamp would " +
			"never reach the analyst it exists for")
	}
	if !slices.Contains(got, "embed") {
		t.Errorf("the degraded set must carry 'embed' after the round-trip — a lexical-only investigation would " +
			"otherwise read like an ordinary session hours later; got %v", got)
	}
	if !slices.Contains(got, "journal-evidence") {
		t.Errorf("the degraded set lost journal-evidence on the round-trip; got %v", got)
	}
}

// The "recorded, nothing degraded" case must be an explicit EMPTY set, distinct from NULL (a row that predates
// the column) — the same backward-compat distinction diagnosis (0056) keeps. Verified structurally with SQL
// IS NULL rather than the scanned slice's nil-ness, which is a pgx codec detail.
func TestDegradedCapabilitiesEmptyIsRecordedNotNull(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()

	st := &TriageStore{p: p}
	const ref = "gold-degraded-caps-empty-1"
	_, _ = p.Exec(ctx, `DELETE FROM session_triage WHERE external_ref = $1`, ref)
	defer func() { _, _ = p.Exec(ctx, `DELETE FROM session_triage WHERE external_ref = $1`, ref) }()

	// nil DegradedCapabilities on the row → this build writes an explicit '{}' (never NULL).
	if err := st.RecordTriage(ctx, judge.TriageRow{
		ExternalRef: ref, Host: "goldhost-clean", AlertRule: "HostDown", Band: "NOTICE",
		Outcome: "stood-down", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("record: %v", err)
	}

	var isNull bool
	if err := p.QueryRow(ctx,
		`SELECT degraded_capabilities IS NULL FROM session_triage WHERE external_ref = $1`, ref).Scan(&isNull); err != nil {
		t.Fatalf("null check: %v", err)
	}
	if isNull {
		t.Error("a session recorded by THIS build must store a non-NULL degraded set (explicit '{}') — NULL is " +
			"reserved for rows that predate the column; collapsing the two loses the 'checked, nothing degraded' fact")
	}
	got, ok, err := st.DegradedCapabilities(ctx, ref)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !ok || len(got) != 0 {
		t.Errorf("want a recorded, empty degraded set, got %v (ok=%v)", got, ok)
	}
}
