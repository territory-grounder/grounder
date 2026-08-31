// Package telemetry generates the worker's self-telemetry samples from the module registry — the read side
// of the observability surface. It lets the worker export its own liveness and declared-capability posture
// to the enabled observability exporters (resolve.Exporter), so ops can dashboard/alert on the worker being
// up and on the live capability set changing unexpectedly (a disabled connector going enabled, or vice
// versa). It is a pure function over the registry so it is deterministic and oracle-testable; the periodic
// export loop that ships these samples lives at the composition root (cmd/worker).
//
// Provenance: [O] spec/008 (registry-backed observability surface — the 4th surface made load-bearing).
package telemetry

import (
	"sort"
	"time"

	observability "github.com/territory-grounder/grounder/adapters/observability"
	"github.com/territory-grounder/grounder/modules"
)

// CapabilitySamples renders the registry's declared fleet as observability samples stamped at now: a
// liveness gauge (tg_worker_up=1 — a fresh sample means the worker is exporting), a total-capabilities
// gauge, and one tg_module_capabilities gauge per (surface, state) with the count. Deterministic (sorted).
func CapabilitySamples(reg *modules.Registry, now time.Time) []observability.Sample {
	type key struct {
		surface string
		enabled bool
	}
	counts := map[key]int{}
	total := 0
	for _, c := range reg.Capabilities() {
		counts[key{c.Surface, c.Enabled}]++
		total++
	}
	out := []observability.Sample{
		{Name: "tg_worker_up", Value: 1, Stamped: now},
		{Name: "tg_module_capabilities_total", Value: float64(total), Stamped: now},
	}
	keys := make([]key, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].surface != keys[j].surface {
			return keys[i].surface < keys[j].surface
		}
		return keys[i].enabled && !keys[j].enabled // enabled before disabled, for a stable order
	})
	for _, k := range keys {
		state := "disabled"
		if k.enabled {
			state = "enabled"
		}
		out = append(out, observability.Sample{
			Name:    "tg_module_capabilities",
			Value:   float64(counts[k]),
			Stamped: now,
			Labels:  map[string]string{"surface": k.surface, "state": state},
		})
	}
	return out
}

// SuppressionSamples renders the tier-1 suppression gate's running decision counts as observability samples
// (one tg_suppression_decisions gauge per outcome — escalate / suppressed / notice), so ops can dashboard the
// suppression RATE and catch over-suppression. Deterministic (sorted by outcome).
func SuppressionSamples(counts map[string]int, now time.Time) []observability.Sample {
	outcomes := make([]string, 0, len(counts))
	for k := range counts {
		outcomes = append(outcomes, k)
	}
	sort.Strings(outcomes)
	out := make([]observability.Sample, 0, len(counts))
	for _, o := range outcomes {
		out = append(out, observability.Sample{
			Name:    "tg_suppression_decisions",
			Value:   float64(counts[o]),
			Stamped: now,
			Labels:  map[string]string{"outcome": o},
		})
	}
	return out
}
