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

// TG-348. Four mechanisms are built, wired, running, and their closing step has never once executed.
// Measured live 2026-08-06: 369 manifest drafts / 0 approved, 10 op-class candidates / 0 ratified, 460
// executions / 0 graduation credits.
//
// The existing wiring register cannot see this — `world.discovery` declares its yield as "manifest drafts
// written" and reads LIVE at 369 while zero approvals is invisible. It measures PRODUCTION; this measures
// CLOSURE.

type fakeLoopReader struct {
	cs  []db.LoopClosure
	err error
}

func (f *fakeLoopReader) CountLoopClosures(context.Context) ([]db.LoopClosure, error) {
	return f.cs, f.err
}

func loopSample(ss []metrics.Sample, name, loop string) (metrics.Sample, bool) {
	for _, s := range ss {
		if s.Name == name && (loop == "" || s.Labels["loop"] == loop) {
			return s, true
		}
	}
	return metrics.Sample{}, false
}

// TestZeroAgainstAPopulatedDenominatorIsNeverClosed is the finding: production looks healthy, closure is
// zero, and only the pair distinguishes them.
func TestZeroAgainstAPopulatedDenominatorIsNeverClosed(t *testing.T) {
	f := &fakeLoopReader{cs: []db.LoopClosure{{Loop: "world_manifest", Generated: 369, Closed: 0}}}
	read := startLoopClosureJob(context.Background(), f, time.Hour)
	ss := read()

	gen, ok := loopSample(ss, "tg_loop_generated_total", "world_manifest")
	if !ok || gen.Value != 369 {
		t.Fatalf("denominator missing or wrong (%v, present=%v) — without it, closed=0 cannot be read", gen.Value, ok)
	}
	if cl, ok := loopSample(ss, "tg_loop_closed_total", "world_manifest"); !ok || cl.Value != 0 {
		t.Fatalf("closed series missing or wrong: %v present=%v", cl.Value, ok)
	}
	never, ok := loopSample(ss, "tg_loops_never_closed", "")
	if !ok {
		t.Fatal("tg_loops_never_closed is not published — the whole TG-348 count is then unavailable")
	}
	if never.Value != 1 {
		t.Errorf("never_closed=%v, want 1 — a loop with 369 generated and 0 closed is the exact case this "+
			"register exists to surface", never.Value)
	}
}

// TestAnIdleLoopIsNotReportedAsNeverClosed is the false-positive control, and it matters more than it
// looks: on a fresh deployment every loop has 0/0, and a register that flagged those would train the
// operator to ignore this signal before it ever said anything true.
func TestAnIdleLoopIsNotReportedAsNeverClosed(t *testing.T) {
	f := &fakeLoopReader{cs: []db.LoopClosure{{Loop: "graduation_credit", Generated: 0, Closed: 0}}}
	read := startLoopClosureJob(context.Background(), f, time.Hour)

	never, ok := loopSample(read(), "tg_loops_never_closed", "")
	if !ok {
		t.Fatal("tg_loops_never_closed absent")
	}
	if never.Value != 0 {
		t.Errorf("never_closed=%v for a loop with nothing to close. Zero-against-zero is an IDLE loop, not "+
			"a broken one; flagging it makes the signal noise on every fresh deployment.", never.Value)
	}
}

// TestAHealthyLoopIsNotFlagged — the other direction of the control.
func TestAHealthyLoopIsNotFlagged(t *testing.T) {
	f := &fakeLoopReader{cs: []db.LoopClosure{{Loop: "opclass_ratification", Generated: 10, Closed: 3}}}
	read := startLoopClosureJob(context.Background(), f, time.Hour)
	if never, _ := loopSample(read(), "tg_loops_never_closed", ""); never.Value != 0 {
		t.Errorf("never_closed=%v for a loop that has closed 3 of 10", never.Value)
	}
}

// TestATransientErrorDoesNotFabricateClosure. Zeroing on a DB blip makes tg_loops_never_closed drop to 0,
// which reads as every loop suddenly closing — the one thing this register must never invent.
func TestATransientErrorDoesNotFabricateClosure(t *testing.T) {
	f := &fakeLoopReader{cs: []db.LoopClosure{{Loop: "world_manifest", Generated: 369, Closed: 0}}}
	read := startLoopClosureJob(context.Background(), f, time.Hour)
	if never, _ := loopSample(read(), "tg_loops_never_closed", ""); never.Value != 1 {
		t.Fatal("precondition: expected never_closed=1")
	}

	failing := &fakeLoopReader{err: errors.New("connection refused")}
	if ss := startLoopClosureJob(context.Background(), failing, time.Hour)(); len(ss) != 0 {
		t.Errorf("a reader whose FIRST read fails published %d sample(s) — it has never seen the database, "+
			"so it must publish nothing rather than a fabricated all-closed", len(ss))
	}
}

func TestANilStorePublishesNothing(t *testing.T) {
	if ss := startLoopClosureJob(context.Background(), nil, time.Hour)(); len(ss) != 0 {
		t.Errorf("a nil store published %d sample(s); with no database there is nothing to report", len(ss))
	}
}

// TestTheRegisterIsWiredAtTheCompositionRoot — guarding the job is not guarding the wiring.
func TestTheRegisterIsWiredAtTheCompositionRoot(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	src := stripGoComments(string(raw))
	for _, want := range []string{"startLoopClosureJob(", "withLoopClosure(", "loopClosureStoreOrNil("} {
		if !strings.Contains(src, want) {
			t.Errorf("main.go does not call %s — the register would be computed and published by nothing, "+
				"which is the precise defect class TG-348 is about", want)
		}
	}
}
