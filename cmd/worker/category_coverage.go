package main

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/metrics"
	"github.com/territory-grounder/grounder/core/safety"
)

// category_coverage.go — make the high-risk-category driver's REACHABILITY observable (TG-405).
//
// safety.HighRiskCategory forces a POLL_PAUSE for {maintenance, security-incident, deployment}, read from
// env.Labels["category"]. The Alertmanager module passes every label through raw, and the estate uses that
// same key for SUBSYSTEMS (mesh-bgp, storage-write-path, iac-hygiene, ...). Measured over all 3,165
// ingest_alert rows: 39 carried a category, 0 were high-risk. The driver has never been reachable.
//
// Nothing could see that, because "no alert was high-risk" and "the driver cannot see a high-risk value"
// are the same quiet zero. These gauges separate them.

type categoryCoverageReader interface {
	CountCategoryValues(ctx context.Context) ([]db.CategoryCount, map[string]int64, error)
}

func categoryCoverageSamples(values []db.CategoryCount, totals map[string]int64) []metrics.Sample {
	type agg struct{ present, recognised int64 }
	per := map[string]*agg{}
	for src := range totals {
		per[src] = &agg{}
	}
	for _, v := range values {
		a, ok := per[v.SourceID]
		if !ok {
			a = &agg{}
			per[v.SourceID] = a
		}
		a.present += v.Count
		// Classified through the SAME predicate the safety path enforces with — never a second copy of
		// the vocabulary. A measurement taken against a different list than the one enforcing is worse
		// than none: it would report coverage the classifier does not actually have.
		if safety.HighRiskCategory(v.Category) {
			a.recognised += v.Count
		}
	}

	out := make([]metrics.Sample, 0, len(per)*3+1)
	var totalPresent, totalRecognised int64
	for src, a := range per {
		lbl := map[string]string{"source_id": src}
		out = append(out,
			metrics.Sample{Name: "tg_ingest_alerts_total_by_source", Kind: metrics.Gauge, Value: float64(totals[src]), Labels: lbl,
				Help: "alerts this source has ever delivered — the denominator for the category gauges below"},
			metrics.Sample{Name: "tg_ingest_category_present", Kind: metrics.Gauge, Value: float64(a.present), Labels: lbl,
				Help: "alerts carrying a NON-EMPTY category label. A source at 0 here cannot reach the " +
					"high-risk poll-forcing driver at all, whatever its incidents are."},
			metrics.Sample{Name: "tg_ingest_category_high_risk", Kind: metrics.Gauge, Value: float64(a.recognised), Labels: lbl,
				Help: "alerts whose category is in safety.HighRiskCategory's closed set " +
					"{maintenance, security-incident, deployment} — the only values that force a POLL_PAUSE. " +
					"present > 0 with this at 0 means the source is setting a category from a DIFFERENT " +
					"vocabulary, and the safety driver is unreachable rather than merely quiet (TG-405)."},
		)
		totalPresent += a.present
		totalRecognised += a.recognised
	}
	// Always emitted, including at zero: an ABSENT series is the register being gone, not a clean estate.
	out = append(out, metrics.Sample{
		Name: "tg_ingest_category_unrecognised", Kind: metrics.Gauge,
		Value: float64(totalPresent - totalRecognised),
		Help: "alerts that set a category TG does not recognise. Non-zero with tg_ingest_category_high_risk " +
			"at 0 is the TG-405 collision: the operator uses labels[\"category\"] for one thing and TG reads " +
			"it as a safety input for another.",
	})
	return out
}

func startCategoryCoverageJob(ctx context.Context, store categoryCoverageReader, every time.Duration) func() []metrics.Sample {
	var held atomic.Pointer[[]metrics.Sample]
	reader := func() []metrics.Sample {
		if p := held.Load(); p != nil {
			return *p
		}
		return nil
	}
	if store == nil {
		log.Print("category coverage: no store — nothing measures whether the high-risk poll-forcing " +
			"driver is reachable. It reads labels[\"category\"], which the estate also uses for subsystem " +
			"names, and an unreachable driver is indistinguishable from a calm one (TG-405).")
		return reader
	}

	refresh := func() {
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		values, totals, err := store.CountCategoryValues(rctx)
		if err != nil {
			// Keep the previous reading: zeroing on a DB blip would make the unrecognised count drop to 0,
			// which reads as the collision having resolved itself.
			log.Printf("category coverage: read failed, keeping the previous reading: %v", err)
			return
		}
		s := categoryCoverageSamples(values, totals)
		held.Store(&s)
		var present, recognised int64
		for _, v := range values {
			present += v.Count
			if safety.HighRiskCategory(v.Category) {
				recognised += v.Count
			}
		}
		if present > 0 && recognised == 0 {
			log.Printf("category coverage: %d alert(s) set a category and NONE is in the high-risk set "+
				"{maintenance, security-incident, deployment} — the poll-forcing driver is UNREACHABLE, not "+
				"merely quiet. The estate is using labels[\"category\"] for a different vocabulary (TG-405).",
				present)
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

func categoryCoverageStoreOrNil(pool *db.Pool) categoryCoverageReader {
	if pool == nil {
		return nil
	}
	return pool
}
