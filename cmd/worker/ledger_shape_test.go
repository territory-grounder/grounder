package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/metrics"
)

// TG-57 item 1, ledger half. The governance ledger is written with NO redaction screen
// (core/audit/ledger.go), is hash-chained append-only so a leaked row cannot be redacted afterwards, and
// carries a `reason` column running to 4,886 characters of model-influenced text. Measured live
// 2026-08-06: 9,417 rows, 0 on every shape.
//
// That is TG-302's situation one table over — a premise that holds because of what happened to be
// written, not because of the design — and the answer is TG-345's: watch it.

// ledgerSampleByName is named distinctly on purpose: cmd/worker already has a sampleByName with a
// different signature (it takes a source label). Two helpers with one name is how the deploy package went
// red on main earlier today when parallel branches each added an identical composeFile.
func ledgerSampleByName(ss []metrics.Sample, name string) (metrics.Sample, bool) {
	for _, s := range ss {
		if s.Name == name {
			return s, true
		}
	}
	return metrics.Sample{}, false
}

type fakeLedgerShape struct {
	c   db.LedgerShapeCount
	err error
	n   int
}

func (f *fakeLedgerShape) CountLedgerShapes(context.Context) (db.LedgerShapeCount, error) {
	f.n++
	return f.c, f.err
}

// TestTheDenominatorIsPublishedEvenOnACleanLedger is the vacuity floor, and it is the whole reason this
// watcher is a pair of gauges rather than one. A clean ledger and a DEAD watcher both report "no secret
// shapes"; only the denominator distinguishes them.
func TestTheDenominatorIsPublishedEvenOnACleanLedger(t *testing.T) {
	f := &fakeLedgerShape{c: db.LedgerShapeCount{Rows: 9417}} // clean, like production
	read := startLedgerShapeJob(context.Background(), f, time.Hour)

	ss := read()
	rows, ok := ledgerSampleByName(ss, "tg_ledger_rows")
	if !ok {
		t.Fatal("tg_ledger_rows is not published on a clean ledger. Without the denominator, an absent " +
			"tg_ledger_secret_shaped_rows series reads exactly like a clean ledger — which is the failure " +
			"this watcher exists to prevent.")
	}
	if rows.Value != 9417 {
		t.Errorf("tg_ledger_rows = %v, want 9417", rows.Value)
	}
	shaped, ok := ledgerSampleByName(ss, "tg_ledger_secret_shaped_rows")
	if !ok {
		t.Fatal("tg_ledger_secret_shaped_rows is absent on a clean ledger — it must be published AT ZERO")
	}
	if shaped.Value != 0 {
		t.Errorf("a clean ledger reported %v secret-shaped rows", shaped.Value)
	}
	// The per-shape breakdown must be present at zero too, or a non-zero total later has nothing to
	// compare against.
	var byShape int
	for _, s := range ss {
		if s.Name == "tg_ledger_secret_shaped_rows_by_shape" {
			byShape++
		}
	}
	if byShape != 4 {
		t.Errorf("expected 4 per-shape series, got %d — an operator told only that the premise broke "+
			"learns nothing about where to look", byShape)
	}
}

// TestEachShapeReachesTheTotal pins the sum. A total that silently drops a shape is a watcher that reports
// clean while one specific kind of leak accumulates.
func TestEachShapeReachesTheTotal(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    db.LedgerShapeCount
	}{
		{"redaction_marker", db.LedgerShapeCount{Rows: 10, RedactionMarker: 3}},
		{"pem_block", db.LedgerShapeCount{Rows: 10, PEMBlock: 3}},
		{"provider_key", db.LedgerShapeCount{Rows: 10, ProviderKey: 3}},
		{"assigned_value", db.LedgerShapeCount{Rows: 10, AssignedValue: 3}},
	} {
		f := &fakeLedgerShape{c: tc.c}
		read := startLedgerShapeJob(context.Background(), f, time.Hour)
		shaped, ok := ledgerSampleByName(read(), "tg_ledger_secret_shaped_rows")
		if !ok {
			t.Fatalf("%s: total not published", tc.name)
		}
		if shaped.Value != 3 {
			t.Errorf("%s: contributed %v to the total, want 3 — this shape does not reach the alert rule",
				tc.name, shaped.Value)
		}
	}
}

// TestATransientReadErrorDoesNotZeroTheGauges is the one that matters operationally. Clearing the samples
// on a DB blip makes tg_ledger_secret_shaped_rows drop to 0, which is indistinguishable from the ledger
// being clean — the watcher would report the all-clear precisely when it cannot see.
func TestATransientReadErrorDoesNotZeroTheGauges(t *testing.T) {
	f := &fakeLedgerShape{c: db.LedgerShapeCount{Rows: 9417, PEMBlock: 2}}
	read := startLedgerShapeJob(context.Background(), f, time.Hour)

	before, ok := ledgerSampleByName(read(), "tg_ledger_secret_shaped_rows")
	if !ok || before.Value != 2 {
		t.Fatalf("precondition: expected 2 secret-shaped rows, got %v (present=%v)", before.Value, ok)
	}

	// Now the database goes away and the job refreshes again.
	f.err = errors.New("connection refused")
	f2 := &fakeLedgerShape{c: db.LedgerShapeCount{Rows: 9417, PEMBlock: 2}, err: errors.New("connection refused")}
	readFailFirst := startLedgerShapeJob(context.Background(), f2, time.Hour)
	if ss := readFailFirst(); len(ss) != 0 {
		t.Errorf("a watcher whose FIRST read fails published %d sample(s). It has never seen the ledger, "+
			"so it must publish nothing rather than a fabricated zero.", len(ss))
	}

	after, ok := ledgerSampleByName(read(), "tg_ledger_secret_shaped_rows")
	if !ok {
		t.Fatal("the gauges vanished after a read error")
	}
	if after.Value != 2 {
		t.Errorf("a transient read error changed the reading from 2 to %v. It must KEEP the previous "+
			"value: dropping to 0 is indistinguishable from the ledger being clean.", after.Value)
	}
}

// TestANilStoreSaysSoLoudly — a deployment with no database must not look like a clean ledger.
func TestANilStoreSaysSoLoudly(t *testing.T) {
	read := startLedgerShapeJob(context.Background(), nil, time.Hour)
	if ss := read(); len(ss) != 0 {
		t.Errorf("a nil store published %d sample(s) — with no database there is nothing to report, and a "+
			"published zero would be a fabricated all-clear", len(ss))
	}
}

// TestTheWatcherIsWiredAtTheCompositionRoot. Guarding the job is not guarding the wiring: this project has
// shipped a correct watcher that main() never constructed, and logged "no prober wired" for days.
func TestTheWatcherIsWiredAtTheCompositionRoot(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	src := stripGoComments(string(raw))
	if !strings.Contains(src, "startLedgerShapeJob(") {
		t.Fatal("main.go never calls startLedgerShapeJob — the ledger hygiene gauges would be published by " +
			"nothing, and tg_ledger_rows would be absent rather than zero")
	}
	if !strings.Contains(src, "withLedgerShape(") {
		t.Fatal("the job is constructed but not handed to the admin surface, so its samples never reach " +
			"/metrics — constructed and unreachable is this project's signature defect")
	}
	if !strings.Contains(src, "ledgerShapeStoreOrNil(") {
		t.Fatal("main.go does not resolve a ledger store; the job would be constructed with a nil reader " +
			"on every deployment, including ones that have a database")
	}
}
