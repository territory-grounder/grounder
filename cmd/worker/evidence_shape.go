package main

// WATCHING THE PREMISE A DECISION RESTS ON (TG-345, splitting TG-302).
//
// TG-302 decided NOT to seal agent_step_evidence at rest. The argument is entirely empirical: across the
// live corpus there were 0 redaction markers, 0 PEM blocks, 0 provider keys and 0 assigned-secret shapes,
// so what is stored is screened host output with no credential material in it — and encrypting it, onto a
// sealing seam with 2 rows of production exercise, changing the read path the console depends on, was not
// a trade worth making.
//
// That premise is not a property of the design. It is a property of what the estate's hosts happen to
// print, and it stops being true the first time a tool reads a file containing a key. Re-measured
// 2026-08-06: 0 of 354 on every shape, on a corpus that has doubled since TG-302 counted 172. The decision
// still holds — and nothing was watching whether it would tomorrow.
//
// A decision with a measured premise and no watcher is a decision that silently expires.

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/metrics"
)

// evidenceShapeReader is the seam the job depends on — an interface so the job is testable without a
// database, and so a nil store degrades to silence rather than panicking.
type evidenceShapeReader interface {
	CountEvidenceShapes(ctx context.Context) (db.EvidenceShapeCount, error)
}

// evidenceShapeSamples converts one reading into the published gauges.
//
// EVERY series is emitted unconditionally, including at zero. A metric that only appears when something is
// wrong cannot distinguish "healthy" from "the exporter stopped emitting" — the failure this whole family
// exists to avoid, and the reason tg_ingest_sources_known, tg_poll_queue_open and tg_estate_edges are all
// published at rest.
func evidenceShapeSamples(c db.EvidenceShapeCount) []metrics.Sample {
	out := []metrics.Sample{
		{
			Name: "tg_evidence_rows", Kind: metrics.Gauge, Value: float64(c.Rows),
			Help: "rows in agent_step_evidence — the denominator behind TG-302's decision not to seal it at rest",
		},
		{
			Name: "tg_evidence_secret_shaped_rows", Kind: metrics.Gauge, Value: float64(c.SecretShaped()),
			Help: "agent_step_evidence rows matching a credential shape; TG-302's premise is that this is 0",
		},
	}
	// The per-shape breakdown, so a non-zero total says WHICH shape without a database query. A single
	// aggregate would tell an operator that the premise broke and nothing about where to look.
	for _, s := range []struct {
		shape string
		n     int64
	}{
		{"redaction_marker", c.RedactionMarker},
		{"pem_block", c.PEMBlock},
		{"provider_key", c.ProviderKey},
		{"assigned_value", c.AssignedValue},
	} {
		out = append(out, metrics.Sample{
			Name: "tg_evidence_secret_shaped_rows_by_shape", Kind: metrics.Gauge, Value: float64(s.n),
			Labels: map[string]string{"shape": s.shape},
			Help:   "agent_step_evidence rows matching one credential shape",
		})
	}
	return out
}

// startEvidenceShapeJob re-measures the corpus on a cadence and hands the samples to the admin surface
// through an atomic pointer — a /metrics scrape must never trigger a database query.
//
// Returns the reader the admin surface should call. A nil store yields a reader that emits nothing.
func startEvidenceShapeJob(ctx context.Context, store evidenceShapeReader, every time.Duration) func() []metrics.Sample {
	var held atomic.Pointer[[]metrics.Sample]
	reader := func() []metrics.Sample {
		if p := held.Load(); p != nil {
			return *p
		}
		return nil
	}
	if store == nil {
		log.Print("evidence shape: no store — TG-302's premise (agent_step_evidence holds no credential " +
			"material, so it need not be sealed at rest) is UNWATCHED. It stops being true the first time a " +
			"tool reads a file containing a key, and nothing would say so.")
		return reader
	}

	refresh := func() {
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		c, err := store.CountEvidenceShapes(rctx)
		if err != nil {
			// Deliberately does NOT clear the held samples. A transient DB error must not zero the gauges:
			// tg_evidence_secret_shaped_rows dropping to 0 is indistinguishable from the corpus being clean,
			// and this watcher exists precisely because that difference matters.
			log.Printf("evidence shape: read failed, keeping the previous reading: %v", err)
			return
		}
		s := evidenceShapeSamples(c)
		held.Store(&s)
		if n := c.SecretShaped(); n > 0 {
			log.Printf("evidence shape: %d of %d agent_step_evidence row(s) now match a credential shape "+
				"(redaction=%d pem=%d provider=%d assigned=%d) — TG-302 declined to seal this table at rest on "+
				"the measured premise that this count is ZERO. Re-open that decision.",
				n, c.Rows, c.RedactionMarker, c.PEMBlock, c.ProviderKey, c.AssignedValue)
		}
	}

	refresh() // publish immediately, so the gauges exist before the first tick
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
	log.Printf("evidence shape: re-measuring TG-302's premise every %s — the decision not to seal "+
		"agent_step_evidence rests on a count that nothing was checking", every)
	return reader
}

// evidenceShapeStoreOrNil returns the store, or nil when there is no pool — so an un-pooled worker
// degrades to a reader that emits nothing and says so at boot, rather than panicking or silently watching
// nothing.
func evidenceShapeStoreOrNil(pool *db.Pool) evidenceShapeReader {
	if pool == nil {
		return nil
	}
	return db.NewAgentStepEvidenceStore(pool)
}
