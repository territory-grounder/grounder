package librenms

import (
	"fmt"
	"strings"
	"time"

	coreingest "github.com/territory-grounder/grounder/core/ingest"
)

// payload is the subset of the LibreNMS alert-transport JSON this module consumes. LibreNMS posts these
// fields for device-down and device/service/port up-down transitions; the `site` field is the config-row
// discriminator each instance is templated to send (INV-18).
type payload struct {
	Site      string `json:"site"`      // which configured deployment posted this (e.g. "NL", "GR")
	ID        string `json:"id"`        // LibreNMS alert id — the source-side correlation ref
	Rule      string `json:"rule"`      // rule name (e.g. "Device Down"); slugified for the envelope
	Severity  string `json:"severity"`  // "critical" | "warning" | "ok"
	Host      string `json:"host"`      // IP address of the device
	Hostname  string `json:"hostname"`  // resolved hostname
	SysName   string `json:"sysName"`   // sysName fallback for hostname
	Title     string `json:"title"`     // human summary
	Timestamp string `json:"timestamp"` // "2006-01-02 15:04:05" in UTC (LibreNMS alert-transport format)
	State     int    `json:"state"`     // 1 = alert (fault), 0 = recovery/up
}

// librenmsTimeLayout is LibreNMS's alert-transport timestamp format — a NAIVE local-time datetime with no
// zone suffix. It is interpreted in the posting deployment's configured Timezone (LibreNMS renders it in the
// server's local time), NOT UTC.
const librenmsTimeLayout = "2006-01-02 15:04:05"

// pushFutureGrace is how far ahead of receipt a LibreNMS push timestamp may sit before it is treated as a
// mislabeled-timezone / ahead-of-clock artifact and clamped to receive time (rather than dropped by the
// core freshness window). A push arrives in real time, so a correct timestamp is always at or before now;
// the small grace only absorbs benign sub-minute clock differences.
const pushFutureGrace = 90 * time.Second

// mapSeverity maps a LibreNMS severity + state onto a provider-severity string the core validator folds
// into the typed enum. A recovery (state 0) is informational regardless of the rule's configured
// severity; anything unrecognized is passed through for core/ingest to reject (fail closed), never
// defaulted to a low severity.
func mapSeverity(sev string, state int) string {
	if state == 0 {
		return "info" // an up/recovery transition
	}
	return sev
}

// SlugifyRule makes a LibreNMS rule name join-safe (the core grammar forbids whitespace): internal
// whitespace runs collapse to single hyphens. A rule that is empty or has no slug-safe content is left
// as-is for the validator to reject.
//
// EXPORTED so the seed-knowledge tool (tools/seed-knowledge) and the seed round-trip guard
// (core/knowledge/seed_roundtrip_test.go) key the precedent corpus through the EXACT SAME function the
// live ingester uses — proving the stored alert_rule slug equals the env.AlertRule the novelty gate
// (knowledge.Count) reads. If the seed slugged differently than this, the corpus would populate but Count
// would stay 0: a silent no-op. One function, one slug, by construction (TG-125).
func SlugifyRule(rule string) string {
	// The core ingest grammar (core/ingest.slugRe = ^[A-Za-z0-9._:@/+-]+$) is the join-safe key charset.
	// Standard LibreNMS rule names carry characters OUTSIDE it — "Space on / is >= 90% and < 95% in use",
	// "Processor usage over 85%", "Linux High Memory Usage, >= 90% in use" all contain % > < = ',' — so a
	// whitespace-only collapse produced an invalid slug and the core validator 400'd the alert, silently
	// DROPPING the most common infra alerts (disk/CPU/memory/sensor) at the gate. Map every out-of-charset
	// rune to a single '-', collapse runs, and trim, so any rule name becomes a valid, stable slug.
	var b strings.Builder
	prevSep := true // leading state = separator, so the result never starts with '-'
	for _, r := range rule {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || strings.ContainsRune("._:@/+-", r) {
			b.WriteRune(r)
			prevSep = false
		} else if !prevSep {
			b.WriteByte('-')
			prevSep = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// resolveDeployment picks the config row for an event. Real LibreNMS does NOT send a site field by default
// (the predecessor ran one receiver URL per site); so when the payload names no site AND exactly one
// deployment is configured, that sole deployment is used — the common single-site case works without any
// template customization. With multiple deployments the event MUST name its site (a "site" field added to
// the LibreNMS alert template, or injected by the ingress from the authenticated route), otherwise it is
// refused as ambiguous. A named site that is not configured is refused (INV-18).
func (m *Module) resolveDeployment(site string) (Deployment, error) {
	if site == "" {
		if len(m.deployments) == 1 {
			for _, d := range m.deployments {
				return d, nil
			}
		}
		return Deployment{}, fmt.Errorf("librenms: event names no site and %d deployments are configured — the site must be named (LibreNMS template tag or ingress route)", len(m.deployments))
	}
	d, ok := m.deployments[site]
	if !ok {
		return Deployment{}, fmt.Errorf("librenms: event from unconfigured site %q", site)
	}
	return d, nil
}

func (m *Module) toEnvelope(p payload) (coreingest.IncidentEnvelope, error) {
	dep, err := m.resolveDeployment(p.Site)
	if err != nil {
		return coreingest.IncidentEnvelope{}, err
	}

	host := p.Hostname
	if host == "" {
		host = p.SysName
	}

	now := m.now()
	observed := time.Time{}
	if p.Timestamp != "" {
		loc := dep.loc
		if loc == nil {
			loc = time.UTC // deployment built without New (e.g. a zero-value struct in a test) — safe default
		}
		t, err := time.ParseInLocation(librenmsTimeLayout, p.Timestamp, loc)
		if err != nil {
			return coreingest.IncidentEnvelope{}, fmt.Errorf("librenms: malformed timestamp %q: %w", p.Timestamp, err)
		}
		observed = t
		// Safety net: a LibreNMS push arrives in real time, so a timestamp still landing in the FUTURE means
		// the server's timezone is mislabeled (LibreNMS emits naive local time) or its clock is ahead. Do NOT
		// drop a real, just-arrived alert on a timestamp technicality — clamp to receive time so precision
		// degrades instead of the whole alert being rejected by the freshness window. With the deployment's
		// Timezone set correctly, observed is already at/before now and this never fires.
		if observed.After(now.Add(pushFutureGrace)) {
			observed = now
		}
	}

	// Alert-rule identity: prefer the explicit `rule` field, but FALL BACK to `title`. Real LibreNMS posts
	// the rule name in `title` and often sends no `rule` field at all (the predecessor derives the rule name
	// as `title || rule || name`), so without the fallback those alerts have an empty alert_rule and are
	// rejected before triage. rule-first (vs the predecessor's title-first) keeps a host-detailed title out of
	// the dedup/novelty key when both are present.
	ruleName := p.Rule
	if ruleName == "" {
		ruleName = p.Title
	}
	raw := coreingest.NewRawEvent(SourceType+"-"+dep.Site, nil)
	raw.ExternalRef = SourceType + "-" + dep.Site + "-" + p.ID // unique, slug-safe correlation ref
	raw.AlertRule = SlugifyRule(ruleName)
	raw.Severity = mapSeverity(p.Severity, p.State)
	raw.Host = host
	raw.IP = p.Host // LibreNMS "host" carries the device IP
	raw.Site = dep.Site
	raw.Summary = p.Title
	raw.ObservedAt = observed
	// A state-0 push is a RECOVERY transition (the alert row went back UP), carrying the SAME external_ref
	// as the fault that opened it (LibreNMS reuses the alert id across its lifecycle). Label it so the front
	// door can route it as positive clear-evidence (spec/012 clear-confirm) instead of re-triaging it — a
	// recovery is not a new incident. Data, not control (INV-08); the envelope grammar is unchanged. The pull
	// path (FetchActive) fetches state=1 only and is unaffected.
	if p.State == 0 {
		raw.Labels = map[string]string{coreingest.LabelTransition: coreingest.TransitionRecovery}
	}

	return coreingest.Normalize(raw, now)
}
