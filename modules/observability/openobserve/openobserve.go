// Package openobserve is the loadable OpenObserve observability module (spec/008 REQ-818, T-008 fleet).
//
// It implements adapters/observability.Exporter: it ships freshness-stamped samples (metrics/logs) as
// OTLP-ish JSON to the configured OpenObserve endpoint, and — with tracing enabled by default — exports the
// per-session span trajectory so a completed session is reconstructable end to end (INV-14). Each sample
// carries its own freshness stamp so an absent()-guarded staleness check pages on a dead writer rather than
// reading as healthy (INV-15). The HTTP transport is injectable (a Doer) so the oracle drives the real code
// path against a fake endpoint. The ingest token is a secret reference, resolved per request, never a
// literal (INV-13).
//
// Provenance: [O] INV-13/INV-14/INV-15, spec/008 REQ-818.
package openobserve

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	observability "github.com/territory-grounder/grounder/adapters/observability"
	"github.com/territory-grounder/grounder/core/config"
)

// SourceType is the vendor slug this module serves.
const SourceType = "openobserve"

// Doer is the minimal HTTP contract; *http.Client satisfies it, and tests inject a fake endpoint.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Module is the OpenObserve observability exporter. Construct with New.
type Module struct {
	endpoint string
	tokenRef config.SecretRef
	tracing  bool
	http     Doer
}

// Option configures a Module.
type Option func(*Module)

// WithHTTPClient injects the HTTP transport (a fake in tests, *http.Client in production).
func WithHTTPClient(d Doer) Option { return func(m *Module) { m.http = d } }

// WithTracing toggles span (trajectory) export. Tracing is default-on; pass WithTracing(false) to withdraw
// the trace export path while keeping metric/log export.
func WithTracing(on bool) Option { return func(m *Module) { m.tracing = on } }

// New builds an OpenObserve module for an OTLP endpoint base URL and an ingest-token secret reference (e.g.
// "env:OPENOBSERVE_TOKEN"). Tracing is enabled by default so the session trajectory is reconstructable.
func New(endpoint string, tokenRef config.SecretRef, opts ...Option) *Module {
	m := &Module{endpoint: strings.TrimRight(endpoint, "/"), tokenRef: tokenRef, tracing: true, http: http.DefaultClient}
	for _, o := range opts {
		o(m)
	}
	return m
}

// SourceType implements adapters/observability.Exporter.
func (m *Module) SourceType() string { return SourceType }

// Tracing reports whether span/trajectory export is enabled (default-on).
func (m *Module) Tracing() bool { return m.tracing }

// compile-time proof the module satisfies the stable observability interface.
var _ observability.Exporter = (*Module)(nil)

// otlpRecord is one OTLP-ish log/metric record carrying its freshness stamp so a dead writer is observable
// from the outside (INV-15).
type otlpRecord struct {
	Name         string            `json:"name"`
	Value        float64           `json:"value"`
	TimeUnixNano int64             `json:"timeUnixNano"`
	Attributes   map[string]string `json:"attributes,omitempty"`
}

// exportEnvelope is the OTLP-ish logs/metrics batch shipped to the endpoint.
type exportEnvelope struct {
	SourceType string       `json:"sourceType"`
	Records    []otlpRecord `json:"records"`
}

// traceEnvelope is the OTLP-ish per-session trace shipped to the endpoint so the trajectory keyed by the
// session id is reconstructable (INV-14).
type traceEnvelope struct {
	SourceType string   `json:"sourceType"`
	SessionID  string   `json:"sessionId"`
	Spans      []string `json:"spans"`
}

// post ships a JSON body to the endpoint path. The ingest token is resolved from its secret reference at
// call time (INV-13); a non-2xx response is an error and is never silently dropped.
func (m *Module) post(ctx context.Context, path string, body any) error {
	token, err := m.tokenRef.Resolve()
	if err != nil {
		return fmt.Errorf("openobserve: resolve token: %w", err)
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoint+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	// OpenObserve ingest (both the native _json bulk route and the OTLP HTTP route) authenticates with HTTP
	// Basic, NOT a Bearer token — a bare Bearer 401s. The ingest token is the base64-encoded credential
	// (base64(user:password)) that OpenObserve issues, presented directly as the Basic credential.
	req.Header.Set("Authorization", "Basic "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("openobserve: POST %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return nil
}

// Export ships a batch of freshness-stamped samples as OTLP-ish logs/metrics. A sample without a stamp is
// stamped at send time so no record leaves undated (INV-15).
func (m *Module) Export(ctx context.Context, samples []observability.Sample) error {
	recs := make([]otlpRecord, 0, len(samples))
	for _, s := range samples {
		stamped := s.Stamped
		if stamped.IsZero() {
			stamped = time.Now()
		}
		recs = append(recs, otlpRecord{
			Name:         s.Name,
			Value:        s.Value,
			TimeUnixNano: stamped.UnixNano(),
			Attributes:   s.Labels,
		})
	}
	return m.post(ctx, "/v1/logs", exportEnvelope{SourceType: SourceType, Records: recs})
}

// ExportSpans POSTs the ordered spans of a completed session to the endpoint so its trajectory is
// reconstructable (INV-14). With tracing disabled it is a no-op — there is no trace-export path.
func (m *Module) ExportSpans(ctx context.Context, sessionID string, spans []string) error {
	if !m.tracing {
		return nil
	}
	if sessionID == "" {
		return fmt.Errorf("openobserve: empty session id")
	}
	return m.post(ctx, "/v1/traces", traceEnvelope{SourceType: SourceType, SessionID: sessionID, Spans: spans})
}
