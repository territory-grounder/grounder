package main

import (
	"strings"
	"testing"
)

// TG-343 follow-through (found live 2026-08-07 while verifying TG-346).
//
// `tg_estate_sources_failed` promises, in its own Help string, "estate sources that failed to seed on the
// LAST REFRESH". It was fed `func() int { return len(estateErrs) }` — the boot-time estate.Build error
// slice, captured once and never reassigned. The periodic refresh computes its own srcErrs, LOGS them,
// and threw them away.
//
// Both directions are wrong, and the second is the dangerous one:
//   - a source that failed at boot and recovered reads as failing FOREVER (measured: the actuation plane
//     read 1 for its whole uptime because the TG-346 relay fails once at boot, by design, before the
//     database pool connects — while every 5-minute refresh after that succeeded);
//   - a source that dies AFTER boot never moves the gauge at all, so the one loud signal in this family
//     is deaf to every failure that is not present at process start.
//
// This is the TG-365 class: a signal whose value cannot distinguish the states it claims to report.
//
// These are comment-stripped source assertions on the composition root because the counter is a local in
// main() with no seam to call — the same limitation TG-112's guards state, and the reason the assertions
// below pin the WIRING rather than a returned value.

func estateFailedCounterSource(t *testing.T) string {
	t.Helper()
	src := stripGoComments(readWorkerMain(t))
	if len(src) < 10_000 {
		t.Fatalf("VACUITY FLOOR: main.go stripped to %d bytes", len(src))
	}
	return src
}

// TestTheFailedSourceGaugeReadsALiveCounter is the finding.
func TestTheFailedSourceGaugeReadsALiveCounter(t *testing.T) {
	src := estateFailedCounterSource(t)

	if strings.Contains(src, "func() int { return len(estateErrs) }") {
		t.Fatal("tg_estate_sources_failed is still fed the BOOT build's error count. Its Help says \"on " +
			"the last refresh\"; a source that dies after boot never moves it, and a source that failed " +
			"at boot and recovered reads as failing forever.")
	}
	if !strings.Contains(src, "estateSourcesFailed.Load()") {
		t.Fatal("the gauge no longer reads the shared live counter (estateSourcesFailed.Load()) — whatever " +
			"it reads now, the refresh path does not write it")
	}
}

// TestEveryGraphRebuildWritesTheCounter. A live counter that only ONE path updates is the same defect
// wearing a different type: the periodic refresh is the path that runs forever, and the relay prime is
// the one that clears the by-design boot failure.
func TestEveryGraphRebuildWritesTheCounter(t *testing.T) {
	src := estateFailedCounterSource(t)

	// Every Refresh call site must be followed closely by a Store — otherwise that path rebuilds the
	// graph and leaves the gauge describing a graph that no longer exists.
	var refreshes, stores int
	for i := 0; ; {
		k := strings.Index(src[i:], "estateHolder.Refresh(")
		if k < 0 {
			break
		}
		i += k + 1
		refreshes++
		window := src[i:min(i+400, len(src))]
		if strings.Contains(window, "estateSourcesFailed.Store(") {
			stores++
		}
	}
	if refreshes == 0 {
		t.Fatal("VACUITY FLOOR: no estateHolder.Refresh call site found in main.go — this test would " +
			"otherwise pass by having nothing to check, which is the exact defect class it guards")
	}
	if stores != refreshes {
		t.Errorf("%d of %d estateHolder.Refresh call sites update estateSourcesFailed. A rebuild that does "+
			"not write the counter leaves tg_estate_sources_failed describing a graph that no longer "+
			"exists.", stores, refreshes)
	}
	// And the boot build must seed it, or the gauge reads 0 until the first refresh tick — announcing a
	// healthy seed during exactly the boot in which a source failed.
	// Match the call PREFIX, not the exact arg list — the boot build carries extra Options now
	// (estate.WithDefaultEdgeSchema(), TG-207) and the guard is about it seeding the counter, not its arity.
	b := strings.Index(src, "estate.Build(context.Background(), estateSources")
	if b < 0 {
		t.Fatal("the boot estate.Build call is gone from main.go")
	}
	if !strings.Contains(src[b:min(b+300, len(src))], "estateSourcesFailed.Store(") {
		t.Error("the boot build does not seed estateSourcesFailed — the gauge would read 0 until the first " +
			"refresh, which is a false all-clear across the whole boot window")
	}
}
