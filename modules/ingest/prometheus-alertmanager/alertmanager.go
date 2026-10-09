// Package alertmanager is the loadable Prometheus/Alertmanager ingest module (spec/008 REQ-802, T-008-2).
//
// It implements adapters/ingest.Ingester: it validates each Alertmanager webhook alert against an explicit
// grammar (INV-04), maps it to the one canonical triage envelope keyed by alertname + target, and derives
// a correlation key so a firing alert and its later resolved transition for the same series collapse to
// ONE incident. Nothing in the payload becomes control flow.
//
// The package is named alertmanager though its import path is .../prometheus-alertmanager (Go package name
// need not match the last path element). Provenance: [O] INV-04/INV-06, spec/008.
package alertmanager

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	adaptingest "github.com/territory-grounder/grounder/adapters/ingest"
	coreingest "github.com/territory-grounder/grounder/core/ingest"
)

// SourceType is the vendor slug this module serves.
const SourceType = "prometheus-alertmanager"

// Module is the Alertmanager ingest adapter. Construct with New.
type Module struct {
	now func() time.Time
}

// Option configures a Module.
type Option func(*Module)

// WithClock overrides the wall clock, so the observed-timestamp window check is deterministic under test.
func WithClock(now func() time.Time) Option { return func(m *Module) { m.now = now } }

// New builds an Alertmanager ingest module.
func New(opts ...Option) *Module {
	m := &Module{now: time.Now}
	for _, o := range opts {
		o(m)
	}
	return m
}

// SourceType implements adapters/ingest.Ingester.
func (m *Module) SourceType() string { return SourceType }

// compile-time proof the module satisfies the stable ingest interfaces (single + batch).
var (
	_ adaptingest.Ingester      = (*Module)(nil)
	_ adaptingest.BatchIngester = (*Module)(nil)
)

// Normalize handles a single-alert Alertmanager webhook — the transport case the one-envelope Ingester
// interface serves. A webhook that does not yield exactly one envelope is rejected; grouped webhooks go
// through NormalizeBatch (the front door prefers it via the BatchIngester assertion).
func (m *Module) Normalize(ctx context.Context, raw []byte) (coreingest.IncidentEnvelope, error) {
	envs, err := m.NormalizeBatch(ctx, raw)
	if err != nil {
		return coreingest.IncidentEnvelope{}, err
	}
	if len(envs) != 1 {
		return coreingest.IncidentEnvelope{}, fmt.Errorf("alertmanager: Normalize expects exactly one NORMALIZABLE alert, got %d (grammar-failing and noise alerts are dropped; grouped webhooks go through NormalizeBatch)", len(envs))
	}
	return envs[0], nil
}

// NormalizeBatch validates every alert in a (possibly grouped) Alertmanager webhook against the explicit
// grammar and maps each to the canonical triage envelope keyed by alertname + target. It never emits
// control flow from the payload. Grammar discipline is per ALERT: a malformed webhook (or one carrying no
// alerts) is rejected whole, while a single alert failing the grammar is rejected individually — dropped —
// without discarding its well-formed siblings, so one bad series in an Alertmanager group cannot suppress
// the rest of the group's incidents (INV-04 per alert; availability for the siblings).
func (m *Module) NormalizeBatch(_ context.Context, raw []byte) ([]coreingest.IncidentEnvelope, error) {
	var wh webhook
	if err := json.Unmarshal(raw, &wh); err != nil {
		return nil, fmt.Errorf("alertmanager: malformed webhook: %w", err)
	}
	if len(wh.Alerts) == 0 {
		return nil, fmt.Errorf("alertmanager: webhook carries no alerts")
	}
	out := make([]coreingest.IncidentEnvelope, 0, len(wh.Alerts))
	for _, a := range wh.Alerts {
		if skipAlert(a) {
			continue // always-firing meta-alert (Watchdog/InfoInhibitor) or info-severity noise — never an incident
		}
		env, err := m.toEnvelope(a)
		if err != nil {
			continue // this alert fails the grammar — rejected individually; its siblings still normalize
		}
		out = append(out, env)
	}
	return out, nil
}
