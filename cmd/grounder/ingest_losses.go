package main

import (
	"sort"
	"sync"

	ingestadapter "github.com/territory-grounder/grounder/adapters/ingest"
	"github.com/territory-grounder/grounder/core/httpapi"
	"github.com/territory-grounder/grounder/core/metrics"
)

// ingestAlertLosses tallies ALERTS a grouped delivery carried, or was meant to carry, that never became an
// incident (TG-708). It counts alerts, not deliveries, and it is distinct from ingestRefusals (a delivery turned
// away) and ingestPredrop (an alert accepted that minted no new work): an alert here is lost. alert_rejected is
// one alert inside an accepted delivery that failed the grammar; sender_truncated is one alert the sender says it
// cut from a delivery before sending (Alertmanager's max_alerts). The reason vocabulary is closed.
type ingestAlertLosses struct {
	mu sync.Mutex
	n  map[[2]string]int64
}

func newIngestAlertLosses() *ingestAlertLosses {
	return &ingestAlertLosses{n: map[[2]string]int64{}}
}

const (
	lossAlertRejected   = "alert_rejected"
	lossSenderTruncated = "sender_truncated"
)

var lossReasonSet = map[string]bool{lossAlertRejected: true, lossSenderTruncated: true}

func (c *ingestAlertLosses) add(sourceType, reason string, n int) {
	if c == nil || n <= 0 {
		return
	}
	if !lossReasonSet[reason] {
		reason = "other"
	}
	if sourceType == "" {
		sourceType = "unknown"
	}
	c.mu.Lock()
	c.n[[2]string{sourceType, reason}] += int64(n)
	c.mu.Unlock()
}

// seedBatchSources starts every (source, reason) series at zero for each named source the registry resolves as a
// batch ingester. Prometheus' increase() needs a sample before the first loss: a series that first appears at 1
// reads as no increase, so AlertsLostAtIngest would miss the first loss after every restart (measured on the
// TG-708 live check, 2026-11-10: the counter read 1 and increase() read 0).
func (c *ingestAlertLosses) seedBatchSources(r httpapi.IngesterResolver, sourceTypes ...string) {
	if c == nil || r == nil {
		return
	}
	for _, st := range sourceTypes {
		ing, err := r.ResolveIngester(st)
		if err != nil {
			continue
		}
		if _, ok := ing.(ingestadapter.BatchIngester); !ok {
			continue
		}
		c.mu.Lock()
		for reason := range lossReasonSet {
			if _, ok := c.n[[2]string{st, reason}]; !ok {
				c.n[[2]string{st, reason}] = 0
			}
		}
		c.mu.Unlock()
	}
}

func (c *ingestAlertLosses) samples() []metrics.Sample {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	keys := make([][2]string, 0, len(c.n))
	vals := make(map[[2]string]int64, len(c.n))
	for k, v := range c.n {
		keys = append(keys, k)
		vals[k] = v
	}
	c.mu.Unlock()
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	out := make([]metrics.Sample, 0, len(keys))
	for _, k := range keys {
		out = append(out, metrics.Sample{
			Name: "tg_ingest_alerts_lost_total", Kind: metrics.Counter,
			Help: "ALERTS a grouped ingest delivery carried, or was meant to carry, that never became an incident, by " +
				"source and reason: alert_rejected (one alert failed the grammar; the reason is in the grounder log), " +
				"sender_truncated (the sender says it cut this many from a delivery, e.g. Alertmanager max_alerts). " +
				"Until TG-708 both vanished behind a 202.",
			Value:  float64(vals[k]),
			Labels: map[string]string{"source_type": k[0], "reason": k[1]},
		})
	}
	return out
}
