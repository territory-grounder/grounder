package librenms

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/config"
	coreingest "github.com/territory-grounder/grounder/core/ingest"
)

// The PULL counterpart to the push receiver (normalize.go): TG's worker periodically fetches ACTIVE LibreNMS
// alerts (state=1) from each configured deployment and normalizes them into the SAME canonical envelope
// through the SAME grammar (INV-04), so a pulled alert and a pushed alert are indistinguishable downstream.
//
// LibreNMS's own alert transport cannot compute TG's mandatory HMAC ingest signature, so a direct
// LibreNMS→/v1/ingest webhook is impossible; native pull is the chosen intake (docs-confirmed). It is
// strictly READ-ONLY (GET only — it never acknowledges or writes an alert) and per-deployment isolated.
//
// The /api/v0/alerts rows are UNENRICHED (device_id, rule_id); the hostname is joined from /api/v0/devices
// and the rule name + severity from /api/v0/rules.

// apiAlert is the subset of a LibreNMS /api/v0/alerts row this puller consumes.
type apiAlert struct {
	ID        int    `json:"id"`
	DeviceID  int    `json:"device_id"`
	RuleID    int    `json:"rule_id"`
	State     int    `json:"state"`
	Timestamp string `json:"timestamp"`
}

// apiRule is the subset of /api/v0/rules this puller consumes (rule_id → name + severity).
type apiRule struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Severity string `json:"severity"`
}

// apiDevice is the subset of /api/v0/devices this puller consumes. It deliberately includes ONLY the
// identity fields. The LibreNMS device row also carries SNMP secrets (community / authpass / cryptopass /
// authname), which this struct MUST NEVER declare — so json.Unmarshal silently drops them and they can
// never be surfaced (logged, persisted, or emitted). Fetch never logs the raw response body.
type apiDevice struct {
	DeviceID int    `json:"device_id"`
	Hostname string `json:"hostname"`
	SysName  string `json:"sysName"`
}

// AlertSource pulls active alerts for each configured deployment and normalizes them into triage envelopes.
type AlertSource struct {
	deployments []Deployment
	http        Doer
	mod         *Module // shares the one normalization grammar with the push receiver
}

// AlertOption configures an AlertSource.
type AlertOption func(*AlertSource)

// WithAlertHTTPClient injects the HTTP transport (a fake in tests, a TLS-configured *http.Client in prod).
func WithAlertHTTPClient(d Doer) AlertOption { return func(s *AlertSource) { s.http = d } }

// WithAlertClock overrides the normalization clock so the observed-timestamp handling is deterministic in
// tests.
func WithAlertClock(now func() time.Time) AlertOption {
	return func(s *AlertSource) { s.mod.now = now }
}

// NewAlertSource builds a pull source over the configured deployments, reusing the push module's grammar.
func NewAlertSource(deployments []Deployment, opts ...AlertOption) *AlertSource {
	s := &AlertSource{deployments: deployments, http: http.DefaultClient, mod: New(deployments)}
	for _, o := range opts {
		o(s)
	}
	return s
}

// FetchActive returns the canonical envelope for every active (state=1) alert across all deployments,
// enriched with device hostname and rule name/severity. A per-deployment fetch error aborts THAT
// deployment's contribution (returned) — never a silent partial. A single alert that fails normalization is
// skipped (one bad row must not stall the whole poll), not fatal.
func (s *AlertSource) FetchActive(ctx context.Context) ([]coreingest.IncidentEnvelope, error) {
	var out []coreingest.IncidentEnvelope
	for _, d := range s.deployments {
		token, err := config.SecretRef(d.TokenRef).Resolve()
		if err != nil {
			return nil, fmt.Errorf("librenms[%s]: resolve token: %w", d.Site, err)
		}
		rules, err := s.fetchRules(ctx, d, token)
		if err != nil {
			return nil, fmt.Errorf("librenms[%s]: rules: %w", d.Site, err)
		}
		hosts, err := s.fetchDeviceHosts(ctx, d, token)
		if err != nil {
			return nil, fmt.Errorf("librenms[%s]: devices: %w", d.Site, err)
		}
		alerts, err := s.fetchActiveAlerts(ctx, d, token)
		if err != nil {
			return nil, fmt.Errorf("librenms[%s]: alerts: %w", d.Site, err)
		}
		for _, a := range alerts {
			env, ok := s.mod.envelopeFor(a, rules, hosts, d.Site)
			if !ok {
				continue // a single unresolvable/malformed alert is skipped, never aborts the batch
			}
			out = append(out, env)
		}
	}
	return out, nil
}

// envelopeFor enriches one raw alert and normalizes it. It returns ok=false if the alert cannot be turned
// into a valid envelope (so the caller skips it). Empty host is allowed (a not-host-scoped alert); a missing
// rule falls back to a stable rule-id label so alert_rule is never empty (the validator requires it).
func (m *Module) envelopeFor(a apiAlert, rules map[int]apiRule, hosts map[int]string, site string) (coreingest.IncidentEnvelope, bool) {
	rule := rules[a.RuleID]
	ruleName := rule.Name
	if ruleName == "" {
		ruleName = "librenms-alert-rule-" + strconv.Itoa(a.RuleID)
	}
	// A firing alert whose rule we could not map has no severity from the rule table. Fail SAFE, not closed:
	// default it to "warning" so it is still triaged (an active alert is at least a warning) rather than
	// dropped for an empty severity — under-triage is the worse failure for intake.
	severity := rule.Severity
	if severity == "" {
		severity = "warning"
	}
	host := hosts[a.DeviceID] // "" if the device is unknown — allowed (not host-scoped)
	p := payload{
		Site:      site,
		ID:        strconv.Itoa(a.ID),
		Rule:      ruleName,
		Severity:  severity,
		Hostname:  host,
		Title:     strings.TrimSpace(ruleName + " on " + host),
		Timestamp: normalizeAlertTimestamp(a.Timestamp),
		State:     a.State,
	}
	env, err := m.toEnvelope(p)
	if err != nil {
		return coreingest.IncidentEnvelope{}, false
	}
	return env, true
}

// normalizeAlertTimestamp keeps a timestamp only if it parses as LibreNMS's alert-transport layout; anything
// else is blanked (toEnvelope then leaves ObservedAt zero) so a format quirk downgrades precision rather than
// dropping the whole alert.
func normalizeAlertTimestamp(ts string) string {
	ts = strings.TrimSpace(ts)
	if ts == "" {
		return ""
	}
	if _, err := time.ParseInLocation(librenmsTimeLayout, ts, time.UTC); err != nil {
		return ""
	}
	return ts
}

// get issues an authenticated read-only GET against a deployment's LibreNMS API and decodes into out. The
// token rides the X-Auth-Token header (LibreNMS convention). The response body is never logged (it may carry
// SNMP secrets for the devices endpoint — apiDevice drops them at unmarshal).
func (s *AlertSource) get(ctx context.Context, base, token, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Auth-Token", token)
	req.Header.Set("Accept", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GET %s: status %d", path, resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("malformed %s response: %w", path, err)
	}
	return nil
}

func (s *AlertSource) fetchRules(ctx context.Context, d Deployment, token string) (map[int]apiRule, error) {
	var wrap struct {
		Rules []apiRule `json:"rules"`
	}
	if err := s.get(ctx, d.BaseURL, token, "/api/v0/rules", &wrap); err != nil {
		return nil, err
	}
	m := make(map[int]apiRule, len(wrap.Rules))
	for _, r := range wrap.Rules {
		m[r.ID] = r
	}
	return m, nil
}

func (s *AlertSource) fetchDeviceHosts(ctx context.Context, d Deployment, token string) (map[int]string, error) {
	var wrap struct {
		Devices []apiDevice `json:"devices"`
	}
	if err := s.get(ctx, d.BaseURL, token, "/api/v0/devices?limit=500", &wrap); err != nil {
		return nil, err
	}
	m := make(map[int]string, len(wrap.Devices))
	for _, dv := range wrap.Devices {
		h := dv.Hostname
		if h == "" {
			h = dv.SysName
		}
		m[dv.DeviceID] = h
	}
	return m, nil
}

func (s *AlertSource) fetchActiveAlerts(ctx context.Context, d Deployment, token string) ([]apiAlert, error) {
	var wrap struct {
		Alerts []apiAlert `json:"alerts"`
	}
	if err := s.get(ctx, d.BaseURL, token, "/api/v0/alerts?state=1", &wrap); err != nil {
		return nil, err
	}
	return wrap.Alerts, nil
}
