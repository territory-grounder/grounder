package alertmanager

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"

	coreingest "github.com/territory-grounder/grounder/core/ingest"
)

// webhook is the subset of the Alertmanager v4 webhook this module consumes.
type webhook struct {
	Status string  `json:"status"` // group status: "firing" | "resolved"
	Alerts []alert `json:"alerts"`
}

// alert is one Alertmanager alert. The per-alert status distinguishes a firing from a resolved
// transition of the SAME series (same labels ⇒ same correlation key).
type alert struct {
	Status      string            `json:"status"` // "firing" | "resolved"
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    string            `json:"startsAt"` // RFC3339; stable across firing→resolved of a series
	Fingerprint string            `json:"fingerprint"`
}

// slugify makes a label value join-safe (the core grammar forbids whitespace).
func slugify(s string) string { return strings.Join(strings.Fields(s), "-") }

// skipAlert drops the always-firing meta-alerts and info-severity noise at the ingest boundary, mirroring the
// predecessor receiver's `alertname === 'Watchdog' || 'InfoInhibitor' || severity === 'info'` guard. Watchdog
// is Prometheus's dead-man's-switch (fires forever by design) and InfoInhibitor is a routing meta-alert;
// neither is ever an incident. The check reads the RAW severity label (not the resolved→info remap in
// toEnvelope), so a RESOLVED transition of a real warning/critical alert still survives to correlate with its
// firing.
func skipAlert(a alert) bool {
	switch a.Labels["alertname"] {
	case "Watchdog", "InfoInhibitor":
		return true
	}
	return strings.EqualFold(a.Labels["severity"], "info")
}

// hostFromInstance splits a Prometheus instance label ("host:port" or "host") into its host/IP part.
func hostFromInstance(instance string) string {
	if instance == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(instance); err == nil {
		return h
	}
	return instance
}

// firstNonEmpty returns the first non-blank value among labels[keys...], in preference order.
func firstNonEmpty(labels map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(labels[k]); v != "" {
			return v
		}
	}
	return ""
}

// siteRe mirrors core/ingest's join-safe slug grammar for the site field.
var siteRe = regexp.MustCompile(`^[A-Za-z0-9._:@/+-]+$`)

// safeSite returns the site label if it satisfies the envelope's site grammar (slug charset, <=64), else
// empty — an unusable site VALUE is dropped, never the alert carrying it.
func safeSite(s string) string {
	if s != "" && len(s) <= 64 && siteRe.MatchString(s) {
		return s
	}
	return ""
}

// boundedLabels filters a label set to the envelope grammar's bounds (<=64 entries, key<=128 bytes,
// value<=1024 bytes), dropping only the offending ENTRIES — deterministically (sorted keys) when the
// cardinality cap truncates — so the label set can never fail the whole alert's normalization.
func boundedLabels(in map[string]string) map[string]string {
	keys := make([]string, 0, len(in))
	for k, v := range in {
		if k == "" || len(k) > 128 || len(v) > 1024 {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > 64 {
		keys = keys[:64]
	}
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[k] = in[k]
	}
	return out
}

func (m *Module) toEnvelope(a alert) (coreingest.IncidentEnvelope, error) {
	alertname := a.Labels["alertname"]
	if alertname == "" {
		return coreingest.IncidentEnvelope{}, fmt.Errorf("alertmanager: alert missing alertname label")
	}
	// Target identity: prefer the node/host `instance`, else fall back through the kube workload labels. A
	// kube-state-metrics alert (KubePodCrashLooping, KubeDeploymentReplicasMismatch, …) carries no `instance`,
	// so without this fallback every such alert collapses onto the same target-less correlation key.
	target := hostFromInstance(a.Labels["instance"])
	if target == "" {
		target = firstNonEmpty(a.Labels, "pod", "node", "deployment", "statefulset", "daemonset", "job", "container")
	}

	sev := a.Labels["severity"]
	if strings.EqualFold(a.Status, "resolved") {
		sev = "info" // a resolved transition is informational regardless of the firing severity
	}

	summary := a.Annotations["summary"]
	if summary == "" {
		summary = a.Annotations["description"]
	}

	observed := time.Time{}
	if a.StartsAt != "" {
		t, err := time.Parse(time.RFC3339, a.StartsAt)
		if err != nil {
			return coreingest.IncidentEnvelope{}, fmt.Errorf("alertmanager: malformed startsAt %q: %w", a.StartsAt, err)
		}
		observed = t
	}

	// Correlation key: alertname + target. A firing and its later resolved transition for the same series
	// share this key, so PublishTriage collapses them to ONE incident (REQ-802).
	ref := "am-" + slugify(alertname)
	if target != "" {
		ref += "-" + slugify(target)
	}

	raw := coreingest.NewRawEvent(SourceType, nil)
	raw.ExternalRef = ref
	raw.AlertRule = slugify(alertname)
	raw.Severity = sev
	raw.Summary = summary
	raw.ObservedAt = observed
	// Propagate site + labels SANITIZED to the envelope grammar's bounds: a site value outside the slug
	// charset is dropped (empty), an oversize/oversized-entry label set is trimmed entry-by-entry — the
	// ALERT itself always survives. Propagating raw values made normalization stricter than before the
	// feature existed (legal Prometheus label data — a site with a space, >64 labels — tripped the core
	// grammar and the alert vanished silently); sanitizing keeps the feature lossless for the incident.
	raw.Site = safeSite(a.Labels["site"])
	raw.Labels = boundedLabels(a.Labels) // the risk classifier keys off labels["category"]; RAG keeps k8s context
	// A target that parses as an IP is carried as IP; otherwise it is a hostname.
	if ip := net.ParseIP(target); ip != nil {
		raw.IP = target
	} else {
		raw.Host = target
	}
	return coreingest.Normalize(raw, m.now())
}
