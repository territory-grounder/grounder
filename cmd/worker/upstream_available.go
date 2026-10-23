package main

// WHAT THE UPSTREAM HAD, BESIDE WHAT ARRIVED (TG-344).
//
// Every ingest gauge TG publishes counts what ARRIVED. None counted what was AVAILABLE, so two opposite
// states rendered identically:
//
//	upstream 0, ingested 0   → a quiet estate. Healthy.
//	upstream 50, ingested 0  → the connector is broken and TG is deaf.
//
// Measured 2026-08-06: the intake had read zero for five days, tg_ingest_last_seen_seconds was ~24h, and
// the freshness pair (TG-336) correctly reported "a source that used to deliver has stopped" — which is
// what a broken connector looks like AND what a quiet estate looks like. Settling it took a hand-run
// `GET /api/v0/alerts?state=1` against the upstream, which returned 0 and proved TG was behaving
// correctly. That query is now a gauge.
//
// DELIBERATELY NOT AN ADMISSION INPUT. This is a second witness for the OPERATOR. Nothing gates ingest on
// it: a connector that started refusing work because a comparison disagreed would be a worse failure than
// the blindness it replaces.

import (
	"context"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/territory-grounder/grounder/core/metrics"
	"github.com/territory-grounder/grounder/modules/ingest/librenms"
)

// upstreamProber reports, per source_id, how many alerts the upstream currently has — and, separately,
// which sources could not be read at all. The split is the whole point: an unreadable upstream must never
// publish as "0 available".
type upstreamProber interface {
	CountActive(ctx context.Context) (map[string]int, map[string]error)
}

// upstreamSamples renders one probe reading.
func upstreamSamples(counts map[string]int, errs map[string]error) []metrics.Sample {
	out := make([]metrics.Sample, 0, len(counts)*2+len(errs)*2+1)

	for id, n := range counts {
		lbl := map[string]string{"source_id": id}
		out = append(out,
			metrics.Sample{
				Name: "tg_ingest_upstream_available", Kind: metrics.Gauge,
				Help: "alerts the UPSTREAM currently has firing for this source. Read against " +
					"tg_ingest_source_recent_total (what ARRIVED): both zero is a quiet estate; this " +
					"non-zero with nothing arriving is a broken connector. Before this gauge those two " +
					"published identically and only a hand-run API call could tell them apart.",
				Value: float64(n), Labels: lbl,
			},
			metrics.Sample{
				Name: "tg_ingest_upstream_readable", Kind: metrics.Gauge,
				Help: "1 when this source's upstream count was actually obtained. ALWAYS emitted. An " +
					"unreadable upstream publishes readable=0 and NO available count — reporting it as " +
					"'0 available' would recreate the exact conflation this family exists to close.",
				Value: 1, Labels: lbl,
			},
		)
	}

	// A source that could not be read publishes readable=0 and DELIBERATELY no available count. Emitting a
	// zero there would say "the upstream has nothing", which is precisely what is not known.
	for id := range errs {
		out = append(out, metrics.Sample{
			Name: "tg_ingest_upstream_readable", Kind: metrics.Gauge,
			Help: "1 when this source's upstream count was actually obtained; 0 when the probe failed " +
				"(credential, transport, or the source refused). At 0 there is NO available count for " +
				"this source, by design.",
			Value: 0, Labels: map[string]string{"source_id": id},
		})
	}

	// VACUITY FLOOR. With no sources probed every per-source series is absent and a rule over them goes
	// quiet — silence that reads as health, which is the failure this whole family exists to prevent.
	out = append(out, metrics.Sample{
		Name: "tg_ingest_upstream_probed", Kind: metrics.Gauge,
		Help: "how many sources the upstream probe attempted. ZERO means nothing is being probed at all — " +
			"every tg_ingest_upstream_* series is then absent, and absent must not read as healthy.",
		Value: float64(len(counts) + len(errs)),
	})
	return out
}

// startUpstreamProbeJob polls the upstream on a cadence and hands samples to the admin surface through an
// atomic pointer — a /metrics scrape must never trigger a network call to the estate.
//
// A nil prober yields a reader that emits nothing and says so at boot: with no probe, "quiet estate" and
// "broken connector" are indistinguishable again, and that is worth a line in the log.
func startUpstreamProbeJob(ctx context.Context, p upstreamProber, every time.Duration) func() []metrics.Sample {
	var held atomic.Pointer[[]metrics.Sample]
	reader := func() []metrics.Sample {
		if s := held.Load(); s != nil {
			return *s
		}
		return nil
	}
	if p == nil {
		log.Print("upstream probe: no prober wired — TG counts what ARRIVED and nothing counts what was " +
			"AVAILABLE, so a quiet estate and a broken connector remain indistinguishable (TG-344)")
		return reader
	}

	refresh := func() {
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		counts, errs := p.CountActive(rctx)
		s := upstreamSamples(counts, errs)
		held.Store(&s)
		for id, err := range errs {
			log.Printf("upstream probe: %s unreadable (%v) — publishing readable=0 and NO available count", id, err)
		}
	}
	refresh() // publish immediately so the gauges exist before the first tick
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
	log.Printf("upstream probe: reading what each source HAS every %s — a quiet estate and a broken "+
		"connector no longer publish identically", every)
	return reader
}

// upstreamProbeSourceFor resolves WHICH LibreNMS deployments the probe counts against, and it deliberately
// does not require the pull poller to be running.
//
// THIS IS THE BUG TG-344 SHIPPED WITH, found live on 2026-08-06. The prober was hoisted out of the
// `TG_LIBRENMS_ALERT_POLL_INTERVAL != ""` branch and read `TG_LIBRENMS_DEPLOYMENTS_AGENT_TOOLS`. Production
// runs push-only — that interval is empty and that key is not set at all — so both conditions were false and
// every boot logged "no prober wired". The code was merged, guarded, CI-green and deployed, and the
// denominator did not exist on the one deployment that had it.
//
// The coupling was backwards. A PULL deployment already has an independent read of the upstream: the poller
// itself fetches the alert list, so a broken connector shows up as a fetch error. A PUSH deployment has no
// such read — silence is its only signal — which is exactly the deployment that needs a denominator, and
// exactly the one the old condition excluded.
//
// So the probe resolves from `TG_LIBRENMS_DEPLOYMENTS`, the same list the ingest registration and the estate
// graph read. The set TG counts against is the set TG ingests from. A polled source is reused when present
// rather than opening a second client against the same upstream.
// ONLY THE INGESTING PLANE PROBES. Observed within minutes of the fix above reaching production: the
// actuation worker published tg_ingest_upstream_readable=0 for both sites, and UpstreamProbeUnreadable
// would have fired on it within 30 minutes. That zero is CORRECT and deliberate — TG-337 scoped the
// actuation plane's LibreNMS token to device reads, so it 403s on /alerts by design — which makes it a
// security posture working as intended, reported as a fault. Probing from there also spends a 403 against
// every LibreNMS every two minutes for a number that plane has no use for.
//
// The denominator is an INGEST measurement and ingest is triage-plane (TG-153), so it is gated on the same
// predicate the rest of the split uses rather than on a new list that could disagree with it.
func upstreamProbeSourceFor(holdsTriage bool, polled *librenms.AlertSource, deps []librenms.Deployment, client *http.Client) *librenms.AlertSource {
	if !holdsTriage {
		return nil
	}
	if polled != nil {
		return polled
	}
	if len(deps) == 0 {
		return nil
	}
	return librenms.NewAlertSource(deps, librenms.WithAlertHTTPClient(client))
}

// upstreamProberOrNil keeps a TYPED NIL out of the interface: a nil *librenms.AlertSource assigned to
// upstreamProber yields a NON-nil interface holding a nil pointer, so the `p == nil` guard in
// startUpstreamProbeJob would not fire and the first refresh would panic on a deployment that configures
// no alert source. Takes the CONCRETE type for exactly that reason — converting after the fact cannot
// undo the wrapping.
func upstreamProberOrNil(s *librenms.AlertSource) upstreamProber {
	if s == nil {
		return nil
	}
	return s
}
