// Package crowdsec is the loadable CrowdSec ingest module (spec/008 REQ-803, T-008-3).
//
// It implements adapters/ingest.Ingester: it parses a CrowdSec LAPI alert (scenario + source +
// decisions), validates it against the explicit grammar (INV-04), and maps it to the one canonical triage
// envelope so it routes through the SAME in-code dedup → flap → burst admission path as every other ingest
// source before any model dispatch (INV-20). Nothing in the payload becomes control flow.
//
// Provenance: [O] INV-04/INV-20, spec/008.
package crowdsec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"

	adaptingest "github.com/territory-grounder/grounder/adapters/ingest"
	coreingest "github.com/territory-grounder/grounder/core/ingest"
)

// SourceType is the vendor slug this module serves.
const SourceType = "crowdsec"

// Module is the CrowdSec ingest adapter. Construct with New.
type Module struct {
	now func() time.Time
}

// Option configures a Module.
type Option func(*Module)

// WithClock overrides the wall clock, so the observed-timestamp window check is deterministic under test.
func WithClock(now func() time.Time) Option { return func(m *Module) { m.now = now } }

// New builds a CrowdSec ingest module.
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

// alert is the subset of a CrowdSec LAPI alert this module consumes.
type alert struct {
	Scenario  string     `json:"scenario"`
	Message   string     `json:"message"`
	StartAt   string     `json:"start_at"` // RFC3339
	Simulated bool       `json:"simulated"` // a simulation-mode alert is not real enforcement (never critical)
	Source    source     `json:"source"`
	Decisions []decision `json:"decisions"`
}

type source struct {
	Scope string `json:"scope"` // "Ip" | "Range" | "Username" | ...
	Value string `json:"value"` // the offending value (an IP for scope Ip, a CIDR for Range, a name for Username)
	IP    string `json:"ip"`
	Range string `json:"range"`
}

type decision struct {
	Type     string `json:"type"`  // e.g. "ban", "captcha"
	Value    string `json:"value"` // the target value
	Scenario string `json:"scenario"`
	Origin   string `json:"origin"`
}

// slugify makes a value join-safe (the core grammar forbids whitespace); '/' is already grammar-legal so a
// CrowdSec scenario like "crowdsecurity/ssh-bf" passes unchanged.
func slugify(s string) string { return strings.Join(strings.Fields(s), "-") }

// scenario→severity classifiers, mirroring the predecessor receiver's scenario matching
// (`/CVE|exploit|backdoor|log4j|rce/i` → critical; `/bf|brute|ssh-bf|http-generic-bf/i` → high). TG's
// severity grammar is three-level (Critical/Warning/Info), so the predecessor's high AND medium tiers
// collapse to Warning/Info per the escalation boundary (critical/high escalate; medium/low do not).
var (
	scenarioCriticalRE = regexp.MustCompile(`(?i)CVE|exploit|backdoor|log4j|rce`)
	scenarioHighRE     = regexp.MustCompile(`(?i)bf|brute|ssh-bf|http-generic-bf`)
)

// severityFor classifies a CrowdSec alert's severity FROM THE SCENARIO NAME (the predecessor's approach), not
// merely from the decision type: an exploit/CVE/RCE scenario is critical even when the only enforcement is a
// ban. An enforcement decision (ban) still FLOORS a lower-tier scenario at warning-class visibility, so a
// banned reconnaissance signal is not silently dropped to info. The core validator folds the string into the
// typed enum and rejects anything unrecognized (fail closed).
func severityFor(scenario string, decisions []decision) string {
	switch {
	case scenarioCriticalRE.MatchString(scenario):
		return "critical"
	case scenarioHighRE.MatchString(scenario):
		return "warning" // predecessor "high" → TG Warning
	}
	// Lower-tier scenarios (scan/probe/enum → predecessor "medium"; else → "low") are informational, UNLESS
	// CrowdSec issued an enforcement decision, which floors visibility at warning.
	for _, d := range decisions {
		if strings.EqualFold(d.Type, "ban") {
			return "warning"
		}
	}
	return "info"
}

// Normalize handles a single CrowdSec alert object — the one-envelope case the base Ingester interface
// serves (e.g. a custom `{{ range . }}`-per-request format, or a unit oracle). The real CrowdSec http
// notification plugin groups every alert from one bucket overflow into a JSON ARRAY (its default
// `format: {{ .|toJson }}` serializes the whole []models.Alert), so the front door prefers NormalizeBatch
// via the BatchIngester assertion; a raw array reaching THIS method is rejected as not-exactly-one.
func (m *Module) Normalize(ctx context.Context, raw []byte) (coreingest.IncidentEnvelope, error) {
	envs, err := m.NormalizeBatch(ctx, raw)
	if err != nil {
		return coreingest.IncidentEnvelope{}, err
	}
	if len(envs) != 1 {
		return coreingest.IncidentEnvelope{}, fmt.Errorf("crowdsec: Normalize expects exactly one alert, got %d (grouped pushes go through NormalizeBatch)", len(envs))
	}
	return envs[0], nil
}

// NormalizeBatch validates every alert in a (grouped) CrowdSec http-notification body and maps each to the
// canonical triage envelope. It accepts EITHER the plugin's default array body ({{ .|toJson }} over
// []models.Alert) OR a single bare alert object, so both the real transport and a hand-crafted per-alert
// format normalize. A malformed body is rejected whole (INV-04); a single alert failing the grammar is
// rejected INDIVIDUALLY — dropped — so one bad alert cannot suppress its well-formed siblings in the same
// push (INV-04 per alert; availability for the rest). Nothing in the payload becomes control flow.
func (m *Module) NormalizeBatch(_ context.Context, raw []byte) ([]coreingest.IncidentEnvelope, error) {
	alerts, err := decodeAlerts(raw)
	if err != nil {
		return nil, err
	}
	out := make([]coreingest.IncidentEnvelope, 0, len(alerts))
	for _, a := range alerts {
		env, err := m.normalizeOne(a)
		if err != nil {
			continue // this alert fails the grammar — rejected individually; its siblings still normalize
		}
		out = append(out, env)
	}
	return out, nil
}

// decodeAlerts accepts the CrowdSec http-notification default body — a JSON ARRAY of alerts — OR a single
// bare alert object, returning a uniform slice. This is the whole reason the module is a BatchIngester: the
// real transport groups every alert from one overflow into ONE array body (`{{ .|toJson }}` over
// []models.Alert); TG's earlier single-object Normalize 400'd every real push.
func decodeAlerts(raw []byte) ([]alert, error) {
	if trimmed := bytes.TrimLeft(raw, " \t\r\n"); len(trimmed) > 0 && trimmed[0] == '[' {
		var as []alert
		if err := json.Unmarshal(raw, &as); err != nil {
			return nil, fmt.Errorf("crowdsec: malformed alert array: %w", err)
		}
		return as, nil
	}
	var a alert
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("crowdsec: malformed alert: %w", err)
	}
	return []alert{a}, nil
}

// normalizeOne validates one parsed CrowdSec alert into the canonical triage envelope. Every field is mapped
// by name (INV-04); a simulation-mode alert is capped below critical (a what-if, not real enforcement); a
// non-IP-scoped source (Range/Username/Country/AS) leaves the IP field empty rather than force-fitting a
// CIDR/name/code into it — which the core IP grammar rejects, and which used to 400 an otherwise valid
// security signal for every non-Ip scope.
func (m *Module) normalizeOne(a alert) (coreingest.IncidentEnvelope, error) {
	if a.Scenario == "" {
		return coreingest.IncidentEnvelope{}, fmt.Errorf("crowdsec: alert missing scenario")
	}
	if a.Source.Value == "" {
		return coreingest.IncidentEnvelope{}, fmt.Errorf("crowdsec: alert missing source value")
	}

	observed := time.Time{}
	if a.StartAt != "" {
		t, err := time.Parse(time.RFC3339, a.StartAt)
		if err != nil {
			return coreingest.IncidentEnvelope{}, fmt.Errorf("crowdsec: malformed start_at %q: %w", a.StartAt, err)
		}
		observed = t
	}

	// Only a GENUINE address goes in the IP field. CrowdSec's scope names the KIND (Ip/Range/Username/
	// Country/AS), but rather than trust the string we validate the candidate: a CIDR, username, or country
	// code is not an address and must not be force-fit into the IP grammar. The scenario+value still ride in
	// ExternalRef/AlertRule/Summary, so a non-IP signal is triaged, just without a bogus host.
	ipVal := ""
	for _, cand := range []string{a.Source.IP, a.Source.Value} {
		if cand != "" && net.ParseIP(cand) != nil {
			ipVal = cand
			break
		}
	}

	sev := severityFor(a.Scenario, a.Decisions)
	if a.Simulated && sev == "critical" {
		sev = "warning" // a simulation-mode alert is a what-if, never a real critical enforcement
	}

	raw2 := coreingest.NewRawEvent(SourceType, nil)
	raw2.ExternalRef = SourceType + "-" + slugify(a.Scenario) + "-" + slugify(a.Source.Value)
	raw2.AlertRule = slugify(a.Scenario)
	raw2.Severity = sev
	raw2.IP = ipVal // security signals are IP-scoped; core validates it (empty stays empty)
	raw2.Summary = a.Message
	raw2.ObservedAt = observed
	return coreingest.Normalize(raw2, m.now())
}
