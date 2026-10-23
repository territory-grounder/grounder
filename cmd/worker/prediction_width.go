package main

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/metrics"
)

// prediction_width.go — publish the blast-radius predictor's WIDTH distribution (TG-352).
//
// There were no prediction metrics at all: `tg_predict*` and `tg_blast*` returned NO SERIES, so the only
// way to say anything about the model the actuation gate reasons over was to query Postgres by hand —
// which is how TG-352 came to quote an average taken over a biased sub-population.

type predictionWidthReader interface {
	CountPredictionWidth(ctx context.Context, wideThreshold int) (db.PredictionWidth, error)
}

func predictionWidthSamples(w db.PredictionWidth, threshold int) []metrics.Sample {
	return []metrics.Sample{
		{Name: "tg_prediction_rows", Kind: metrics.Gauge, Value: float64(w.Rows),
			Help: "blast-radius predictions ever committed — the DENOMINATOR. Every other series here is " +
				"meaningless without it, and every precision figure quoted about this table is computed " +
				"over the SCORED subset, not this one."},
		{Name: "tg_prediction_empty", Kind: metrics.Gauge, Value: float64(w.Empty),
			Help: "predictions naming NO host. NOT an error rate and NOT alertable: measured 2026-08-07, all " +
				"1386 of these were on targets with in-degree ZERO, and an empty blast radius for a leaf is " +
				"the CORRECT answer. Read it as the denominator for tg_prediction_empty_on_connected."},
		{Name: "tg_prediction_empty_on_connected", Kind: metrics.Gauge, Value: float64(w.EmptyOnConnected),
			Help: "the empty prediction that IS wrong: the target has dependents in the estate graph and the " +
				"predictor still said nothing is affected. This is the blind-predictor signal; bare emptiness " +
				"is not. 0 of 1386 on 2026-08-07 (TG-352)."},
		{Name: "tg_prediction_wide", Kind: metrics.Gauge, Value: float64(w.Wide),
			Help: "predictions over the configured wide threshold — the ones that DO force extra review."},
		{Name: "tg_prediction_scored", Kind: metrics.Gauge, Value: float64(w.Scored),
			Help: "predictions carrying an outcome (tp IS NOT NULL). Published so a precision figure is " +
				"never quoted without its base: scoring is biased toward WIDE predictions (avg 43.9 hosts " +
				"scored vs 6.2 unscored), so an average over scored rows is not an average over predictions."},
		{Name: "tg_prediction_wide_threshold", Kind: metrics.Gauge, Value: float64(threshold),
			Help: "the boundary the counts above are taken against — the same value the risk classifier uses."},
	}
}

func startPredictionWidthJob(ctx context.Context, store predictionWidthReader, threshold int, every time.Duration) func() []metrics.Sample {
	var held atomic.Pointer[[]metrics.Sample]
	reader := func() []metrics.Sample {
		if p := held.Load(); p != nil {
			return *p
		}
		return nil
	}
	if store == nil {
		log.Print("prediction width: no store — nothing measures how wide the blast-radius predictions are " +
			"or how often they are EMPTY, and an empty prediction removes a poll-forcing reason (TG-352).")
		return reader
	}

	refresh := func() {
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		w, err := store.CountPredictionWidth(rctx, threshold)
		if err != nil {
			// Keep the previous reading: zeroing would make tg_prediction_empty drop to 0, which reads as
			// the predictor having started answering.
			log.Printf("prediction width: read failed, keeping the previous reading: %v", err)
			return
		}
		s := predictionWidthSamples(w, threshold)
		held.Store(&s)
		if w.EmptyOnConnected > 0 {
			log.Printf("prediction width: %d blast-radius prediction(s) said NOTHING IS AFFECTED about a "+
				"target that HAS dependents in the estate graph. An empty radius on a leaf is correct; this "+
				"is the predictor going blind on a connected host (TG-352). Total empty: %d of %d.",
				w.EmptyOnConnected, w.Empty, w.Rows)
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

func predictionWidthStoreOrNil(pool *db.Pool) predictionWidthReader {
	if pool == nil {
		return nil
	}
	return pool
}
