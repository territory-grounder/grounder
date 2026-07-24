// Package librenms is the loadable LibreNMS ingest module (spec/008 REQ-801, T-008-1).
//
// It implements adapters/ingest.Ingester: it parses a LibreNMS alert-transport payload, maps a
// device-down / device/service/port up-down transition to candidate fields, and validates it into the one
// canonical triage envelope via core/ingest.Normalize (INV-04 — the payload is a claim validated against
// an explicit grammar, never coerced and never control flow). ONE implementation serves both the NL and
// GR deployments as two configuration rows (INV-18); per-site variance (base URL, API token, site label)
// is configuration behind this module, never a second module or a fork.
//
// Provenance: [O] INV-04/INV-05/INV-18, spec/008. The live poller (auth against BaseURL with the
// configured token ref) is wired by the runner; the oracle-bound core here is the normalization.
package librenms

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	adaptingest "github.com/territory-grounder/grounder/adapters/ingest"
	coreingest "github.com/territory-grounder/grounder/core/ingest"
)

// SourceType is the vendor slug this module serves.
const SourceType = "librenms"

// Deployment is one configured LibreNMS instance — a config row. The NL and GR instances are two of these
// behind ONE Module (INV-18). BaseURL/TokenRef drive the live poller and never enter the envelope; the
// token is a secret reference (env:/file:), never a literal (INV-13).
type Deployment struct {
	Site     string // estate label (descriptive filter, NOT a security boundary — ADR-0010)
	BaseURL  string
	TokenRef string // e.g. "env:LIBRENMS_NL_TOKEN"
	// Timezone is the IANA zone (e.g. "Europe/Athens") this LibreNMS server renders its alert `$timestamp`
	// in. LibreNMS emits a NAIVE local-time timestamp with no offset, so parsing it as UTC made every alert
	// from a UTC+N server look N hours future-dated — and the freshness window then DROPPED it (a UTC+3
	// Athens push at 18:36 local read as 18:36Z, three hours ahead of a correct 15:36Z clock). Empty ⇒ UTC.
	Timezone string
	loc      *time.Location // resolved from Timezone at New (nil ⇒ UTC)
}

// Module is the LibreNMS ingest adapter. Construct with New.
type Module struct {
	deployments map[string]Deployment // keyed by Site
	now         func() time.Time
}

// Option configures a Module.
type Option func(*Module)

// WithClock overrides the wall clock, so the observed-timestamp window check is deterministic under test.
func WithClock(now func() time.Time) Option { return func(m *Module) { m.now = now } }

// New builds a LibreNMS module for one or more site deployments (e.g. NL and GR).
func New(deployments []Deployment, opts ...Option) *Module {
	m := &Module{deployments: make(map[string]Deployment, len(deployments)), now: time.Now}
	for _, d := range deployments {
		d.loc = locationFor(d.Timezone)
		m.deployments[d.Site] = d
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// locationFor resolves a deployment's IANA timezone to a *time.Location, falling back to UTC for an empty or
// unloadable zone. The binaries embed the zoneinfo database via `import _ "time/tzdata"` (the distroless
// images ship no /usr/share/zoneinfo), so LoadLocation resolves reproducibly in prod; the future-timestamp
// safety net in toEnvelope still covers a mis-set zone so no real alert is dropped.
func locationFor(tz string) *time.Location {
	if strings.TrimSpace(tz) == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(strings.TrimSpace(tz))
	if err != nil {
		return time.UTC
	}
	return loc
}

// SourceType implements adapters/ingest.Ingester.
func (m *Module) SourceType() string { return SourceType }

// compile-time proof the module satisfies the stable ingest interface.
var _ adaptingest.Ingester = (*Module)(nil)

// Normalize parses a LibreNMS alert-transport payload and validates it into the canonical triage
// envelope. The payload must declare a site that matches a configured deployment (INV-18) — an event from
// an unconfigured instance has no path. Every field is mapped by name and validated by
// core/ingest.Normalize; nothing in the body becomes control flow (INV-04).
func (m *Module) Normalize(_ context.Context, raw []byte) (coreingest.IncidentEnvelope, error) {
	var p payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return coreingest.IncidentEnvelope{}, fmt.Errorf("librenms: malformed payload: %w", err)
	}
	return m.toEnvelope(p)
}
