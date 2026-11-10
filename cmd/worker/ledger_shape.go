package main

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/metrics"
)

// ledger_shape.go — IS THERE CREDENTIAL-SHAPED TEXT IN THE GOVERNANCE LEDGER? (TG-57 item 1, ledger half)
//
// The tool-output half of TG-57 item 1 is done: agent/loop.go screenToolOutput runs the input screen over
// every tool RESULT before it re-enters the model prompt, redacting secrets to [REDACTED:<kind>]. The
// LEDGER half is not — core/audit/ledger.go writes `reason` with no screen on the path.
//
// Measured on the live ledger 2026-08-06: 9,417 rows, 0 on every shape, `reason` up to 4,886 characters.
// Clean, unscreened, and the longest column is model-influenced free text. That is TG-302's situation one
// table over, and TG-345's answer transfers: watch the premise, because it is a property of what happened
// to be written rather than of the design.
//
// Two things make the ledger a worse place to find out late. It is APPEND-ONLY and hash-chained, so a
// leaked row cannot be redacted afterwards without breaking the chain — detection is the only control
// available for anything already written. And the ledger is the audit surface, so it is the table most
// likely to be exported, quoted in a review, or read by someone who is not thinking about secrets.
//
// This deliberately mirrors startEvidenceShapeJob rather than generalising it. The two watchers share the
// SHAPE REGEXES (one definition, in core/db) but keep separate log lines and metric names, because the
// operator action differs: a hit on the evidence corpus re-opens TG-302's sealing decision, while a hit
// here means the write path needs a screen it never had.

type ledgerShapeReader interface {
	CountLedgerShapes(ctx context.Context) (db.LedgerShapeCount, error)
}

func ledgerShapeSamples(c db.LedgerShapeCount) []metrics.Sample {
	out := []metrics.Sample{
		{
			Name: "tg_ledger_rows", Kind: metrics.Gauge, Value: float64(c.Rows),
			Help: "rows in governance_ledger — the DENOMINATOR for the shape count below. Always emitted, " +
				"including at zero, so an absent series means the watcher is gone rather than the ledger " +
				"being clean.",
		},
		{
			Name: "tg_ledger_secret_shaped_rows", Kind: metrics.Gauge, Value: float64(c.SecretShaped()),
			Help: "governance_ledger rows whose reason/decision text matches a credential shape. The ledger " +
				"is written WITHOUT a screen and is hash-chained append-only, so a leak here cannot be " +
				"redacted afterwards. Measured 0 of 9417 on 2026-08-06 (TG-57).",
		},
	}
	// The per-shape breakdown, so a non-zero total says WHICH shape without a database query — an operator
	// told only that the premise broke learns nothing about where to look.
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
			Name: "tg_ledger_secret_shaped_rows_by_shape", Kind: metrics.Gauge, Value: float64(s.n),
			Labels: map[string]string{"shape": s.shape},
			Help:   "governance_ledger rows matching one credential shape",
		})
	}
	return out
}

// startLedgerShapeJob publishes the ledger hygiene gauges off the scrape path — a scrape must never
// trigger a database query — handing samples through an atomic pointer, like the evidence and
// ingest-freshness jobs.
func startLedgerShapeJob(ctx context.Context, store ledgerShapeReader, every time.Duration) func() []metrics.Sample {
	var held atomic.Pointer[[]metrics.Sample]
	reader := func() []metrics.Sample {
		if p := held.Load(); p != nil {
			return *p
		}
		return nil
	}
	if store == nil {
		log.Print("ledger shape: no store — the governance ledger is written WITHOUT a redaction screen " +
			"(TG-57 item 1) and nothing is watching whether credential-shaped text has reached it. The " +
			"ledger is hash-chained append-only, so a row that leaks cannot be redacted afterwards.")
		return reader
	}

	refresh := func() {
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		c, err := store.CountLedgerShapes(rctx)
		if err != nil {
			// Deliberately does NOT clear the held samples. A transient DB error must not zero the gauges:
			// tg_ledger_secret_shaped_rows dropping to 0 is indistinguishable from the ledger being clean,
			// which is the exact distinction this watcher exists to make.
			log.Printf("ledger shape: read failed, keeping the previous reading: %v", err)
			return
		}
		s := ledgerShapeSamples(c)
		held.Store(&s)
		if n := c.SecretShaped(); n > 0 {
			log.Printf("ledger shape: %d of %d governance_ledger row(s) match a credential shape "+
				"(redaction=%d pem=%d provider=%d assigned=%d). The ledger write path has NO screen, and the "+
				"chain is append-only — these rows cannot be redacted without breaking it. Screen the write "+
				"path (TG-57 item 1) and treat the existing rows as disclosed.",
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
	return reader
}

// ledgerShapeStoreOrNil mirrors evidenceShapeStoreOrNil: a nil pool means no watcher, and the caller logs
// that the premise is unwatched rather than silently publishing nothing.
func ledgerShapeStoreOrNil(pool *db.Pool) ledgerShapeReader {
	if pool == nil {
		return nil
	}
	return db.NewLedgerStore(pool)
}
