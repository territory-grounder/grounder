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
	mod         *Module       // shares the one normalization grammar with the push receiver
	minAge      time.Duration // 0 = pull every active alert (air-gapped primary intake); >0 = SAFETY-NET gate
	pageLimit   int           // row cap requested from each list endpoint; a response that REACHES it under-confirms
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

// WithAlertMinAge turns the pull into a missed-push RECONCILIATION SAFETY-NET: only an active alert that has
// been firing at least d (by its LibreNMS observed time) is minted. d<=0 (the default) disables the gate —
// every active alert is pulled, the air-gapped/transport-less PRIMARY-intake mode. A positive d makes the
// pull complementary to PUSH: it re-triages only alerts stale enough that a prompt push should already have
// covered them, so a delayed or dropped push no longer leaves a down host unhealed — WITHOUT reacting to a
// transient that push's anti-flap delay legitimately suppresses (the workflow-id REJECT_DUPLICATE dedup keeps
// a still-firing alert single-fire whether it arrives by push or pull).
func WithAlertMinAge(d time.Duration) AlertOption { return func(s *AlertSource) { s.minAge = d } }

// NewAlertSource builds a pull source over the configured deployments, reusing the push module's grammar.
func NewAlertSource(deployments []Deployment, opts ...AlertOption) *AlertSource {
	s := &AlertSource{deployments: deployments, http: http.DefaultClient, mod: New(deployments), pageLimit: defaultPageLimit}
	for _, o := range opts {
		o(s)
	}
	return s
}

// FetchActive returns the canonical envelope for every active (state=1) alert across all deployments,
// enriched with device hostname and rule name/severity. A per-deployment fetch error aborts THAT
// deployment's contribution (returned) — never a silent partial. A single alert that fails normalization is
// skipped (one bad row must not stall the whole poll), not fatal. When minAge>0 (WithAlertMinAge, the
// safety-net mode) an alert younger than minAge is withheld — it is not stale enough to be a missed push, and
// actuating on a just-fired alert would react to a transient that push's anti-flap delay legitimately holds.
func (s *AlertSource) FetchActive(ctx context.Context) ([]coreingest.IncidentEnvelope, int, error) {
	var out []coreingest.IncidentEnvelope
	withheld := 0 // alerts held back by the minAge gate — RETURNED so the caller logs it (never a silent drop)
	for _, d := range s.deployments {
		token, err := config.SecretRef(d.TokenRef).Resolve()
		if err != nil {
			return nil, 0, fmt.Errorf("librenms[%s]: resolve token: %w", d.Site, err)
		}
		rules, err := s.fetchRules(ctx, d, token)
		if err != nil {
			return nil, 0, fmt.Errorf("librenms[%s]: rules: %w", d.Site, err)
		}
		hosts, err := s.fetchDeviceHosts(ctx, d, token)
		if err != nil {
			return nil, 0, fmt.Errorf("librenms[%s]: devices: %w", d.Site, err)
		}
		alerts, err := s.fetchActiveAlerts(ctx, d, token)
		if err != nil {
			return nil, 0, fmt.Errorf("librenms[%s]: alerts: %w", d.Site, err)
		}
		for _, a := range alerts {
			env, ok := s.mod.envelopeFor(a, rules, hosts, d.Site)
			if !ok {
				continue // a single unresolvable/malformed alert is skipped, never aborts the batch
			}
			if s.minAge > 0 && s.mod.now().Sub(env.ObservedAt) < s.minAge {
				// Safety-net gate: withhold alerts younger than minAge — not stale enough to be a missed push.
				// AGE uses env.ObservedAt, which coreingest.Normalize parses in the deployment's Timezone
				// (dep.loc) and CLAMPS to receipt time if it reads as future. So a blank timestamp (age≈0), and
				// crucially a timestamp a MISCONFIGURED Timezone reads as future, both withhold — a whole
				// deployment with a wrong TZ would withhold every alert. That is why the count is RETURNED: a
				// persistently high withheld surfaces the misconfig (or a minAge set too high) instead of
				// masquerading as a quiet estate. The gate also assumes the LibreNMS timestamp is fault-ONSET
				// (fixed while firing) so a sustained fault ages past minAge.
				withheld++
				continue
			}
			out = append(out, env)
		}
	}
	return out, withheld, nil
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

// defaultPageLimit is the row cap the reader ASKS each LibreNMS list endpoint for. It is set FAR above any
// real estate ON PURPOSE. LibreNMS applies no LIMIT/OFFSET to list_devices or list_alerts (confirmed in the
// upstream api_functions source: the SQL carries no LIMIT clause, and measured 2026-08-06 on both deployments
// running 26.8.0-dev.68 — `?limit=10` returned all 126 rows), so on today's server this value is inert and a
// healthy full read (~367 devices, a handful of active alerts) never reaches it. It matters only the day a
// LibreNMS version HONOURS the limit: a response that comes back filled to exactly this many rows is one we
// cannot prove is complete, and checkComplete refuses it. Chosen ~two orders of magnitude above the estate so
// reaching it always means "genuinely too many to have seen them all", never an artificial cap the way the old
// limit=500 was (whose truncation this reader could not even detect — see checkComplete).
const defaultPageLimit = 100_000

// checkComplete UNDER-CONFIRMS a device/alert list that may be TRUNCATED (TG-146 S2). A missing device/alert is
// not a smaller answer but a WRONG one: an unlisted device resolves Host="" (envelopeFor allows it — not every
// alert is host-scoped), ObserveClearedActivity's host match then never matches, and it returns Cleared=true
// for a host that is STILL alerting — a false auto-close, a false de-novel, and a clean run credited to the
// graduation ladder on post-state evidence nobody observed. So a truncation must surface as an ERROR: it
// reaches FetchActive as an error, the cmd/worker ClearObserve seam maps that to ok=false, and
// ObserveClearedActivity then fails closed (holds the incident To Verify) rather than clearing. Loud and
// wrong-way-safe, versus silent and wrong.
//
// LibreNMS gives a client exactly ONE trustworthy truncation signal, so this checks precisely that one:
//
//	got >= limit — the response REACHED the row cap we requested, so rows may exist beyond it that we did not
//	fetch. This is the load-bearing check. It is NOT redundant with the count check below: LibreNMS's `count`
//	is defined server-side as count(returned_rows) — api_success passes no separate total for list_devices or
//	list_alerts — so `count` EQUALS len on any genuine response and can never, by itself, reveal a short page.
//	A guard that compared only got vs count is therefore VACUOUS; that was the defect this replaces (its tests
//	only ever passed against a {count:N, rows:[<N]} shape the real API cannot emit). Requesting `limit` far
//	above the estate (defaultPageLimit) keeps this from firing on healthy traffic.
//
//	count > got — a DEFENSIVE secondary. On today's LibreNMS it never fires (count==got). It is kept so that a
//	future version which reports a real TOTAL in `count` would expose truncation directly, at zero cost today.
//	Guarded on count>got (not count!=got) so an ordinary response and a count-omitting version both pass.
func checkComplete(path string, got, limit, count int) error {
	if limit > 0 && got >= limit {
		return fmt.Errorf("GET %s returned %d row(s), reaching the requested limit of %d — the list may be "+
			"TRUNCATED and an incomplete device/alert view reads as a CLEAR on hosts that are still alerting; "+
			"refusing to serve a possibly-partial answer", path, got, limit)
	}
	if count > got {
		return fmt.Errorf("GET %s returned %d row(s) but the upstream reports a total of %d — the page is "+
			"TRUNCATED and an incomplete device/alert view reads as a CLEAR on hosts that are still alerting; "+
			"refusing to serve a partial answer", path, got, count)
	}
	return nil
}

func (s *AlertSource) fetchDeviceHosts(ctx context.Context, d Deployment, token string) (map[int]string, error) {
	var wrap struct {
		Devices []apiDevice `json:"devices"`
		Count   int         `json:"count"`
	}
	path := "/api/v0/devices?limit=" + strconv.Itoa(s.pageLimit)
	if err := s.get(ctx, d.BaseURL, token, path, &wrap); err != nil {
		return nil, err
	}
	if err := checkComplete(path, len(wrap.Devices), s.pageLimit, wrap.Count); err != nil {
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

// CountActive reports how many alerts EACH deployment currently has firing, keyed by the same source_id
// the ingest rows carry. It is a READ-ONLY probe: it normalizes nothing, admits nothing, and mints no
// session — so it can run on a monitoring cadence without touching the intake path.
//
// WHY IT EXISTS (TG-344). Every ingest gauge TG has counts what ARRIVED. None counts what was AVAILABLE,
// so these two states publish identically:
//
//	upstream 0, ingested 0   → a quiet estate. Healthy.
//	upstream 50, ingested 0  → the connector is broken and TG is deaf.
//
// On 2026-08-06 that distinction took a hand-run API call to make: the intake had read zero for five days
// and only a manual GET /api/v0/alerts proved TG was behaving correctly rather than failing silently.
//
// A per-deployment ERROR is returned alongside the counts rather than folded into them, because an
// unreadable upstream must never publish as "0 available" — that is the same conflation one level down.
func (s *AlertSource) CountActive(ctx context.Context) (map[string]int, map[string]error) {
	counts := map[string]int{}
	errs := map[string]error{}
	for _, d := range s.deployments {
		// The source_id the ingest rows carry, so the gauge JOINS the arrived-count on the same label.
		// A probe keyed differently from the thing it is compared against is worse than no probe.
		id := SourceType + "-" + d.Site
		token, err := config.SecretRef(d.TokenRef).Resolve()
		if err != nil {
			errs[id] = fmt.Errorf("resolve token: %w", err)
			continue
		}
		var wrap struct {
			Alerts []apiAlert `json:"alerts"`
			Count  int        `json:"count"`
		}
		path := "/api/v0/alerts?state=1&limit=" + strconv.Itoa(s.pageLimit)
		if err := s.get(ctx, d.BaseURL, token, path, &wrap); err != nil {
			errs[id] = err
			continue
		}
		// The upstream-availability denominator (TG-344) must not publish a TRUNCATED page as the number
		// available — that is the same "0 available" conflation this probe exists to end, one row short.
		if err := checkComplete(path, len(wrap.Alerts), s.pageLimit, wrap.Count); err != nil {
			errs[id] = err
			continue
		}
		counts[id] = len(wrap.Alerts)
	}
	return counts, errs
}

func (s *AlertSource) fetchActiveAlerts(ctx context.Context, d Deployment, token string) ([]apiAlert, error) {
	var wrap struct {
		Alerts []apiAlert `json:"alerts"`
		Count  int        `json:"count"`
	}
	path := "/api/v0/alerts?state=1&limit=" + strconv.Itoa(s.pageLimit)
	if err := s.get(ctx, d.BaseURL, token, path, &wrap); err != nil {
		return nil, err
	}
	// Same reasoning as the device page, one step more direct: a truncated ACTIVE-ALERT page IS a set of
	// alerts TG cannot see, and the close-out reader would read their absence as the host being quiet.
	if err := checkComplete(path, len(wrap.Alerts), s.pageLimit, wrap.Count); err != nil {
		return nil, err
	}
	return wrap.Alerts, nil
}
