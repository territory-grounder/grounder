// Package prometheus is the loadable Prometheus observability module (spec/008 REQ-816).
//
// It implements adapters/observability.Exporter as a *scrape target*: there is no outbound HTTP and no
// secret — Prometheus pulls the exposition, so the module simply holds the freshness-stamped series a
// scrape reads back via Gather. Every held Sample is stamped with the time it was produced (INV-15): if a
// caller omits Stamped, Export sets it from the clock, so no series can be exposed without a freshness
// timestamp.
//
// Gather also materialises the load-bearing property — an absent()-guarded staleness gauge. For every
// distinct series IDENTITY (the metric name together with its full label set, not the bare name) it emits a
// "<name>_stale" gauge — carrying that identity's own distinguishing labels (e.g. the per-connector
// "connector" label) — whose value is 1 when that identity's newest sample is older than StalenessWindow,
// else 0. A writer that emits stale data trips its gauge to 1; a writer that dies entirely stops emitting
// its series at all, so its gauge goes ABSENT — which is why the paging rule is `absent(<name>_stale) or
// <name>_stale == 1`. Because staleness is per identity, one dead per-connector writer surfaces on its own
// "<name>_stale{connector=…}" series and can no longer be masked by a healthy sibling that merely shares
// the metric name. Either way a dead writer pages rather than reading as healthy.
//
// Provenance: [O] INV-15, spec/008.
package prometheus

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	observability "github.com/territory-grounder/grounder/adapters/observability"
)

// SourceType is the vendor slug this module serves.
const SourceType = "prometheus"

// DefaultStalenessWindow is the age past which a series' newest sample is considered stale (and its
// "<name>_stale" gauge trips to 1) when no explicit window is configured.
const DefaultStalenessWindow = 2 * time.Minute

// Module is the Prometheus observability exporter/scrape target. Construct with New.
type Module struct {
	// StalenessWindow is the age past which the newest sample of a series is stale; the zero value falls
	// back to DefaultStalenessWindow. Set once at construction via WithStalenessWindow.
	StalenessWindow time.Duration

	mu   sync.Mutex
	now  func() time.Time
	held []observability.Sample
}

// Option configures a Module.
type Option func(*Module)

// WithClock overrides the wall clock so the staleness-window check is deterministic under test.
func WithClock(now func() time.Time) Option { return func(m *Module) { m.now = now } }

// WithStalenessWindow sets the staleness window (see StalenessWindow / DefaultStalenessWindow).
func WithStalenessWindow(d time.Duration) Option { return func(m *Module) { m.StalenessWindow = d } }

// New builds a Prometheus observability module.
func New(opts ...Option) *Module {
	m := &Module{now: time.Now, StalenessWindow: DefaultStalenessWindow}
	for _, o := range opts {
		o(m)
	}
	if m.now == nil {
		m.now = time.Now
	}
	return m
}

// SourceType implements adapters/observability.Exporter.
func (m *Module) SourceType() string { return SourceType }

// compile-time proof the module satisfies the stable observability interface.
var _ observability.Exporter = (*Module)(nil)

// window returns the effective staleness window, defaulting a zero field to DefaultStalenessWindow.
func (m *Module) window() time.Duration {
	if m.StalenessWindow > 0 {
		return m.StalenessWindow
	}
	return DefaultStalenessWindow
}

// Export stamps each sample with a freshness time (INV-15) and holds it for the next scrape. A sample that
// arrives without a Stamped value is stamped from the clock, so no series can be exposed unstamped; an
// already-stamped sample propagates its own freshness unchanged. Prometheus scrapes the held series via
// Gather; there is no outbound request.
func (m *Module) Export(ctx context.Context, samples []observability.Sample) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range samples {
		if s.Stamped.IsZero() {
			s.Stamped = m.now()
		}
		m.held = append(m.held, s)
	}
	return nil
}

// Gather returns the held freshness-stamped series plus one absent()-guarded staleness gauge per distinct
// series IDENTITY — the metric name together with its full (sorted) label set, not the bare name. For each
// identity it takes that identity's newest sample; the emitted "<name>_stale" gauge is 1 when
// now − newest > StalenessWindow, else 0, and carries the identity's own distinguishing labels (e.g. the
// per-connector "connector" label) plus a "series" label naming the base metric. A live-but-stale writer
// trips its gauge to 1; a dead writer stops appearing here entirely, so its gauge is ABSENT — hence the
// paging rule wraps it in absent(). Because staleness is computed per identity, a single dead per-connector
// writer surfaces on its OWN "<name>_stale{connector=…}" series and is no longer masked by a healthy
// sibling that merely shares the metric name (INV-15). The held series are returned in insertion order,
// followed by the staleness gauges in (name, labels) order.
func (m *Module) Gather() []observability.Sample {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	out := make([]observability.Sample, len(m.held))
	copy(out, m.held)

	// Aggregate the newest freshness stamp per distinct SERIES IDENTITY (name + sorted label set), keeping
	// the identity's own labels so its staleness gauge stays distinguishable. Grouping by bare name would
	// let a live sibling mask a dead per-connector writer (INV-15).
	type agg struct {
		name   string
		labels map[string]string
		newest time.Time
	}
	byID := make(map[string]*agg, len(m.held))
	ids := make([]string, 0, len(m.held))
	for _, s := range m.held {
		id := seriesIdentity(s.Name, s.Labels)
		a, ok := byID[id]
		if !ok {
			labels := make(map[string]string, len(s.Labels))
			for k, v := range s.Labels {
				labels[k] = v
			}
			byID[id] = &agg{name: s.Name, labels: labels, newest: s.Stamped}
			ids = append(ids, id)
			continue
		}
		if s.Stamped.After(a.newest) {
			a.newest = s.Stamped
		}
	}
	sort.Strings(ids)

	for _, id := range ids {
		a := byID[id]
		stale := 0.0
		if now.Sub(a.newest) > m.window() {
			stale = 1.0
		}
		labels := make(map[string]string, len(a.labels)+1)
		for k, v := range a.labels {
			labels[k] = v
		}
		if _, ok := labels["series"]; !ok {
			labels["series"] = a.name
		}
		out = append(out, observability.Sample{
			Name:    a.name + "_stale",
			Value:   stale,
			Stamped: now,
			Labels:  labels,
		})
	}
	return out
}

// seriesIdentity is the canonical key for a distinct exported series: its name plus its label set in sorted
// key order. Two samples share an identity iff they would occupy the same line in the exposition, so
// staleness is aggregated per identity (name + labels) rather than per bare metric name — otherwise a live
// sibling that shares the name masks a dead per-connector writer (INV-15). The 0x1e/0x1f control bytes
// separate fields so no combination of ordinary label names/values can collide two distinct identities.
func seriesIdentity(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(name)
	for _, k := range keys {
		b.WriteByte(0x1f)
		b.WriteString(k)
		b.WriteByte(0x1e)
		b.WriteString(labels[k])
	}
	return b.String()
}
