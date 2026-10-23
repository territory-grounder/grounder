package main

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/metrics"
)

// loop_closure.go — publishes whether each built loop has ever completed (TG-348).
//
// The wiring/yield register already reports whether a seam PRODUCES. This reports whether the loop it
// feeds ever CLOSES, which is a different question and the one nobody could answer: `world.discovery`
// declares its yield as "manifest drafts written" and reads LIVE at 369, while zero approvals is
// invisible in every series TG publishes.

type loopClosureReader interface {
	CountLoopClosures(ctx context.Context) ([]db.LoopClosure, error)
}

func loopClosureSamples(cs []db.LoopClosure) []metrics.Sample {
	out := make([]metrics.Sample, 0, len(cs)*2+1)
	var neverClosed int
	for _, c := range cs {
		lbl := map[string]string{"loop": c.Loop}
		out = append(out,
			metrics.Sample{Name: "tg_loop_generated_total", Kind: metrics.Gauge, Value: float64(c.Generated), Labels: lbl,
				Help: "artifacts the loop's PRODUCING half has created. The denominator: closed=0 against " +
					"generated=0 is an idle loop, closed=0 against generated=369 is a loop that has never once completed."},
			metrics.Sample{Name: "tg_loop_closed_total", Kind: metrics.Gauge, Value: float64(c.Closed), Labels: lbl,
				Help: "artifacts that reached the loop's TERMINAL state — an operator approval, a ratification, " +
					"an earned credit. Zero here with a non-zero denominator means the consuming half has never " +
					"been exercised, so any defect in it is undiscovered by construction (TG-348)."},
		)
		if c.NeverClosed() {
			neverClosed++
		}
	}
	// Always emitted, including at zero: an ABSENT series is the register being gone, not every loop
	// closing healthily. Same discipline as tg_ingest_sources_known and tg_evidence_rows.
	out = append(out, metrics.Sample{
		Name: "tg_loops_never_closed", Kind: metrics.Gauge, Value: float64(neverClosed),
		Help: "built loops that have produced work and closed NONE of it. This is the TG-348 count: a loop " +
			"that has never closed is not a working loop with an idle operator, it is an untested path " +
			"presented as a feature.",
	})
	return out
}

func startLoopClosureJob(ctx context.Context, store loopClosureReader, every time.Duration) func() []metrics.Sample {
	var held atomic.Pointer[[]metrics.Sample]
	reader := func() []metrics.Sample {
		if p := held.Load(); p != nil {
			return *p
		}
		return nil
	}
	if store == nil {
		log.Print("loop closure: no store — nothing is measuring whether TG's built loops (world manifest, " +
			"op-class ratification, graduation ladder) have ever completed. Each produces output and looks " +
			"healthy while its closing step goes unexercised (TG-348).")
		return reader
	}

	refresh := func() {
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		cs, err := store.CountLoopClosures(rctx)
		if err != nil {
			// Keep the previous reading. Zeroing on a transient DB error would make tg_loops_never_closed
			// drop to 0 — indistinguishable from every loop suddenly closing, which is the one reading this
			// register must never fabricate.
			log.Printf("loop closure: read failed, keeping the previous reading: %v", err)
			return
		}
		s := loopClosureSamples(cs)
		held.Store(&s)
		for _, c := range cs {
			if c.NeverClosed() {
				log.Printf("loop closure: %q has generated %d artifact(s) and closed NONE. The producing half "+
					"is working; the consuming half has never been exercised, so a defect in it would be "+
					"undiscovered (TG-348).", c.Loop, c.Generated)
			}
		}
	}

	refresh()
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				refresh()
			}
		}
	}()
	return reader
}

func loopClosureStoreOrNil(pool *db.Pool) loopClosureReader {
	if pool == nil {
		return nil
	}
	return pool
}
