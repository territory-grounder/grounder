// Package healthchecks is the loadable Healthchecks.io dead-man observability module (spec/008 REQ-820).
//
// It implements adapters/observability.Exporter, but its sink is not a metrics store — it is an EXTERNAL
// dead-man switch. Each control-plane heartbeat pings the configured check URL; if a heartbeat is missed,
// Healthchecks.io raises the alert on its own infrastructure, independent of TG's internal alert path. That
// out-of-band watchdog is the whole point: a wedged or dead control plane can no longer page anyone from the
// inside, so an external observer must notice the silence (INV-15). The HTTP transport is injectable (a
// Doer) so the oracle drives the real code path against a fake server. The check identifier is the secret
// embedded in the ping URL and is a secret reference, resolved per ping, never a literal (INV-13).
//
// Provenance: [O] INV-15 (freshness/liveness is observable from outside), INV-13, spec/008 REQ-820.
package healthchecks

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	observability "github.com/territory-grounder/grounder/adapters/observability"
	"github.com/territory-grounder/grounder/core/config"
)

// SourceType is the vendor slug this module serves.
const SourceType = "healthchecks"

// Doer is the minimal HTTP contract; *http.Client satisfies it, and tests inject a fake ping server.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Module is the Healthchecks.io dead-man exporter. Construct with New.
type Module struct {
	baseURL  string
	checkRef config.SecretRef
	http     Doer
}

// Option configures a Module.
type Option func(*Module)

// WithHTTPClient injects the HTTP transport (a fake in tests, *http.Client in production).
func WithHTTPClient(d Doer) Option { return func(m *Module) { m.http = d } }

// New builds a Healthchecks.io module for a ping host (e.g. "https://hc-ping.com") and the dead-man check
// identifier as a secret reference (e.g. "env:TG_HEALTHCHECKS_CHECK"). The check id uniquely addresses the
// check and is therefore a secret — it is resolved per ping, never stored as a literal (INV-13).
func New(baseURL string, checkRef config.SecretRef, opts ...Option) *Module {
	m := &Module{baseURL: strings.TrimRight(baseURL, "/"), checkRef: checkRef, http: http.DefaultClient}
	for _, o := range opts {
		o(m)
	}
	return m
}

// SourceType implements adapters/observability.Exporter.
func (m *Module) SourceType() string { return SourceType }

// compile-time proof the module satisfies the stable observability interface.
var _ observability.Exporter = (*Module)(nil)

// ping issues a request against the configured dead-man check URL. The check id is resolved from its secret
// reference at call time (INV-13); a non-2xx response is an error. Healthchecks.io accepts GET or POST on
// the check URL, so the method is caller-chosen.
func (m *Module) ping(ctx context.Context, method string) error {
	check, err := m.checkRef.Resolve()
	if err != nil {
		return fmt.Errorf("healthchecks: resolve check: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.baseURL+"/"+check, nil)
	if err != nil {
		return err
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("healthchecks: %s check: status %d: %s", method, resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return nil
}

// Ping fires one heartbeat at the configured dead-man check. A scheduled control-plane heartbeat calls this
// on every tick; the absence of that ping within the check's period is what makes Healthchecks.io raise an
// EXTERNAL alert, independent of TG's internal alert path (INV-15, REQ-820).
func (m *Module) Ping(ctx context.Context) error {
	return m.ping(ctx, http.MethodGet)
}

// Export ships a batch of freshness-stamped samples. Healthchecks.io ingests no series; the meaningful
// export to a dead-man switch is liveness itself — a successfully produced batch is proof the writer is
// alive, so Export stamps freshness on any unstamped sample (INV-15) and then pings the dead-man check. A
// writer that stops exporting stops pinging, and the external watchdog fires.
func (m *Module) Export(ctx context.Context, samples []observability.Sample) error {
	now := time.Now().UTC()
	for i := range samples {
		if samples[i].Stamped.IsZero() {
			samples[i].Stamped = now // freshness: an unstamped sample is stamped at export (INV-15).
		}
	}
	return m.Ping(ctx)
}
