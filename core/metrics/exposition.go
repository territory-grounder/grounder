// Package metrics renders a minimal, dependency-free Prometheus text exposition for the read-only
// /metrics surfaces the grounder and worker serve. It publishes gauges/counters the caller collects from
// live, authoritative values (the mutation gate, the mutation breaker) — matching how the predecessor
// exported circuit_breaker_state into Prometheus/Grafana. It has NO side effect and emits NO secret.
//
// go.mod carries no prometheus/client_golang, and the text exposition format is small and stable, so the
// safety plane hand-rolls it rather than adding a dependency (Phase-2 readiness review §2: "Gather() never
// served → /metrics 404"). The output is deterministic: metric groups are emitted in sorted name order,
// each with exactly one HELP and TYPE header.
package metrics

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// Kind is the Prometheus metric type emitted in the # TYPE line.
type Kind string

const (
	// Gauge is a value that can go up or down (mutation_enabled, circuit_breaker_state).
	Gauge Kind = "gauge"
	// Counter is a monotonically non-decreasing total (deviation_count, halt_total).
	Counter Kind = "counter"
)

// Sample is one metric observation: a name, a type, a help string, a float value, and optional labels.
type Sample struct {
	Name   string
	Kind   Kind
	Help   string
	Value  float64
	Labels map[string]string
}

// Render writes samples as a Prometheus text exposition (content type text/plain; version=0.0.4). All
// samples sharing a name are grouped under one HELP/TYPE header (a Prometheus requirement); groups are
// emitted in sorted name order so the output is deterministic and diff-stable.
func Render(samples []Sample) string {
	byName := map[string][]Sample{}
	kind := map[string]Kind{}
	help := map[string]string{}
	var names []string
	for _, s := range samples {
		if _, seen := byName[s.Name]; !seen {
			names = append(names, s.Name)
			kind[s.Name] = s.Kind
			help[s.Name] = s.Help
		}
		byName[s.Name] = append(byName[s.Name], s)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		if h := help[name]; h != "" {
			b.WriteString("# HELP ")
			b.WriteString(name)
			b.WriteByte(' ')
			b.WriteString(escapeHelp(h))
			b.WriteByte('\n')
		}
		k := kind[name]
		if k == "" {
			k = Gauge
		}
		b.WriteString("# TYPE ")
		b.WriteString(name)
		b.WriteByte(' ')
		b.WriteString(string(k))
		b.WriteByte('\n')
		for _, s := range byName[name] {
			b.WriteString(name)
			b.WriteString(renderLabels(s.Labels))
			b.WriteByte(' ')
			b.WriteString(strconv.FormatFloat(s.Value, 'g', -1, 64))
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// renderLabels formats an optional label set as {k="v",k2="v2"} with sorted keys (deterministic).
func renderLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(labels[k]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func escapeLabelValue(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return v
}

func escapeHelp(h string) string {
	h = strings.ReplaceAll(h, `\`, `\\`)
	h = strings.ReplaceAll(h, "\n", `\n`)
	return h
}

// Handler serves the text exposition produced by collect() on each GET. It is read-only and
// unauthenticated-internal by design (a scraper reaches it on the internal network; it emits no secret and
// has no side effect), mirroring the liveness/readiness probes. A non-GET is 405; collect is called per
// request so the gauges are always current.
func Handler(collect func() []Sample) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(Render(collect())))
	}
}
