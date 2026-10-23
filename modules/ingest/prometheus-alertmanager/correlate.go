package alertmanager

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"

	coreingest "github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/safety"
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

// hostSubject resolves the MACHINE an alert is about, from the labels that name one — and ONLY those.
// `instance` (a scrape target, "host:port" or "host"), then the Kubernetes NODE labels: `node`,
// `kubernetes_node`, `nodename`. It deliberately does NOT fall through to pod/deployment/statefulset/
// daemonset/job/container — those are workloads, and a workload identifier in a host field is a wrong answer
// that reads like a right one (TG-373).
//
// WHY THE EXTRA NODE LABELS (TG-389). kube-state-metrics / node-level alerts (CiliumAgentNotReady,
// NodeMemoryMajorPagesFaults, …) name the node in `kubernetes_node` or `nodename`, not `instance`/`node`.
// Measured 2026-08-06: 48 of 166 Alertmanager rows carried host=” and SEVEN distinct CiliumAgentNotReady
// nodes collapsed onto one empty key — so repairing dedup (TG-377) on (host, rule) would have suppressed
// genuinely different nodes as duplicates. These are MACHINE labels (a node is a machine), so adding them
// keeps hostSubject's "only labels that name a machine" contract; pod stays excluded (TG-373).
//
// Returns "" when no machine is named — an honest gap. A caller MUST NOT treat an empty host as a matchable
// key (an empty host equals every other empty host): the dedup stage refuses to suppress on it, so distinct
// hostless workloads never collapse (core/suppression/dedup.go, the other half of TG-389).
func hostSubject(labels map[string]string) string {
	if h := hostFromInstance(labels["instance"]); h != "" {
		return h
	}
	for _, k := range []string{"node", "kubernetes_node", "nodename"} {
		if h := strings.TrimSpace(labels[k]); h != "" {
			return h
		}
	}
	return ""
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

// workloadSubject resolves the WORKLOAD a container alert is about — the counterpart of hostSubject, from the
// labels that name a workload and ONLY those. In preference order: the k8s `container`; the Docker Compose
// service label `container_label_com_docker_compose_service` (an unambiguous cAdvisor-Docker signal); and,
// ONLY when the alert is genuinely a container scrape, the bare Docker container `name`. TG-440 measured
// am-ContainerMemoryNearLimit-notrf01dmz01 carrying `omoikane-trafilatura` in the compose-service label
// (job=omoikane-cadvisor), while the summary rendered "Container  in  ()" and the incident collapsed onto the
// host notrf01dmz01.
//
// It DELIBERATELY excludes `pod`/`deployment`/`node`/`job`: `pod` is ambiguous (a node-level alert scraped
// from a DaemonSet exporter carries the EXPORTER's pod, not the alert's subject — the TG-373 "workload in a
// host field is a plausible wrong answer" trap), `job` is the scrape job, and `node` is a machine.
//
// `name` is GATED (isContainerScrape) because it is one of the most OVERLOADED label keys in the Prometheus
// ecosystem — node_exporter's systemd/textfile collectors attach a generic `name` (e.g. name="sshd.service")
// that is not a container. Trusting it unconditionally would mis-read a host-level SystemdUnitFailed as a
// container and re-key/rename it (fresh-eyes review of TG-440). The `container` and compose-service labels are
// self-identifying and need no gate; bare `name` is used only alongside a cAdvisor job or a `container_label_*`.
// hostSubject/raw.Host stays machine-only (TG-373): a workload never lands in the host field.
func workloadSubject(labels map[string]string) string {
	if v := firstNonEmpty(labels, "container", "container_label_com_docker_compose_service"); v != "" {
		return v
	}
	if isContainerScrape(labels) {
		return strings.TrimSpace(labels["name"])
	}
	return ""
}

// isContainerScrape reports whether the alert is genuinely a container scrape — a cAdvisor job, or any Docker
// `container_label_*` — so the OVERLOADED bare `name` label may be trusted as a container identity. A host or
// node alert (node_exporter, kube-state-metrics) carries neither. See workloadSubject.
func isContainerScrape(labels map[string]string) bool {
	if strings.Contains(strings.ToLower(labels["job"]), "cadvisor") {
		return true
	}
	for k := range labels {
		if strings.HasPrefix(k, "container_label_") {
			return true
		}
	}
	return false
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
// FingerprintLabel is the label key under which the Alertmanager fingerprint is carried. EXPORTED because
// it is a JOIN KEY: a consumer matching TG's incidents against the estate's own tickets must not re-type
// the string, which is exactly how the two would drift apart.
const FingerprintLabel = "alertmanager_fingerprint"

// withFingerprint returns the label set with the Alertmanager fingerprint added. The input map is never
// mutated — it is the caller's parsed webhook, read again for alertname/severity/site after this point, and
// a normalizer that edits its own input is a bug waiting for the next reader.
//
// An empty fingerprint adds nothing: a source that does not send one must not gain an empty label that
// looks like an identity.
// AlertCategoryLabel holds an operator "category" label whose VALUE collided with TG's safety vocabulary,
// moved off the safety key by demoteCollidingSafetyCategory so it is preserved for RAG/k8s context without
// driving the poll-forcing clamp (TG-405).
const AlertCategoryLabel = "alert_category"

// demoteCollidingSafetyCategory defends TG's safety input from operator label collisions (TG-405).
//
// The risk classifier reads labels["category"] and force-clamps a session to POLL_PAUSE when it is one of
// {maintenance, security-incident, deployment} (safety.HighRiskCategory). But the estate uses "category"
// on its OWN Prometheus rules for SUBSYSTEM names (measured: mesh-bgp, storage-write-path, iac-hygiene,
// ... — 39 of 3,165 alerts, 0 high-risk). The two vocabularies collide on one key. The day an operator
// names a subsystem "maintenance" — a natural topic name that is also a TG high-risk category — every
// alert from it would force POLL_PAUSE forever, for a reason nobody connects to a dashboard label.
//
// TG does not own the operator's "category" key and will not silently trust its value as a safety input.
// So an incoming Alertmanager category that collides with the closed safety set is moved to alert_category:
// kept in full for RAG, removed from the key the safety driver reads. Non-colliding categories (every real
// value ever measured) pass through untouched, so the reachability gauge's collision signal (TG-405,
// cmd/worker/category_coverage.go) is unaffected. Trusted modules (crowdsec, authlog) set "category"
// in their OWN normalizers and never traverse this passthrough, so the only categories that still drive the
// clamp are the ones TG itself hardcoded. The set is read from safety.HighRiskCategory, never copied, so a
// category added there is defended here on the same commit.
func demoteCollidingSafetyCategory(in map[string]string) map[string]string {
	cat, ok := in["category"]
	if !ok || !safety.HighRiskCategory(cat) {
		return in
	}
	out := make(map[string]string, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	delete(out, "category")
	out[AlertCategoryLabel] = cat
	return out
}

func withFingerprint(in map[string]string, fp string) map[string]string {
	fp = strings.TrimSpace(fp)
	if fp == "" {
		return in
	}
	out := make(map[string]string, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	out[FingerprintLabel] = fp
	return out
}

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

	// The WORKLOAD a container alert is about — resolved once, used for the summary AND the correlation ref.
	workload := workloadSubject(a.Labels)

	summary := a.Annotations["summary"]
	if summary == "" {
		summary = a.Annotations["description"]
	}
	// A container alert whose upstream annotation was templated for a DIFFERENT label shape loses the workload
	// identity to blank substitutions: a k8s `Container {{.container}} in {{.namespace}}` template renders
	// "Container  in  ()" on a Docker/cAdvisor alert whose identity is in `name` / the compose-service label.
	// The identity is right there in the labels — surface it so the incident says WHICH workload, not just the
	// host (TG-440). Prefix rather than replace: the upstream text may still carry the threshold/value.
	if workload != "" && !strings.Contains(summary, workload) {
		if strings.TrimSpace(summary) == "" {
			summary = alertname + ": " + workload
		} else {
			summary = workload + " — " + summary
		}
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
	// A container-scoped alert names a workload that lives ON the host (target), and two workloads on one host
	// are DIFFERENT incidents — so when the target is the host, the ref MUST also carry the workload or distinct
	// containers collapse onto one host key (TG-440). Appended only when the workload is DISTINCT from the
	// target, so a pod alert whose target already IS the workload does not double-qualify. workloadSubject only
	// resolves on genuinely container-scoped alerts (a host alert carries none of its labels), so a host-only
	// alert's ref is unchanged.
	if workload != "" && workload != target {
		ref += "-" + slugify(workload)
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
	// THE ALERTMANAGER FINGERPRINT, CARRIED (TG-373). It arrives on every alert — verified live: 21 of 21
	// alerts held by this deployment's Alertmanager have one — and it was parsed into alert.Fingerprint and
	// used by NOTHING. `grep -rn Fingerprint` over the non-test tree returned that declaration and, apart
	// from it, only SSH keys and content hashes. Third declared-but-dead field found today, after
	// ActionManifest.ToolCalls (TG-66) and IncidentEnvelope.IP.
	//
	// It matters because it is the STABLE key TG-354 needs. TG's own external_ref is
	// am-<alertname>-<target>, which the estate's tickets have never contained; the fingerprint is a hash of
	// the alert's label set, identical across firing->resolved and across Alertmanager restarts. A join built
	// on (rule, host, time-window) is guesswork by comparison.
	//
	// Carried as a LABEL rather than an envelope field on purpose: labels_json is already persisted and
	// already queryable, so this needs no new column and no exemption from the ingest spine's
	// declared-but-dead floor. Injected BEFORE bounding so it is subject to the same 64-label cap as
	// everything else — and named alertmanager_fingerprint, which sorts ahead of almost any real Prometheus
	// label, so the cap trims something else first. A user label literally called "fingerprint" would mean
	// something different and is deliberately not overwritten.
	raw.Labels = boundedLabels(demoteCollidingSafetyCategory(withFingerprint(a.Labels, a.Fingerprint))) // the risk classifier keys off labels["category"]; RAG keeps k8s context
	// THE SUBJECT IS NOT THE CORRELATION TARGET (TG-373). `target` above is the correlation identity and
	// deliberately falls through to workload labels — a pod/deployment/job is the right key for collapsing a
	// firing and its resolved transition. It is NOT a host, and writing it into raw.Host was worse than
	// leaving Host empty:
	//
	//   host = my-awx-task-756d768868-xs8gc
	//
	// is what TG recorded for a KubePodNotReady on 2026-08-06. That resolves to nothing in the estate graph
	// (which holds guests and hypervisors, 1,864 edges), so BlastRadiusWide, SiblingsOf and every host-match
	// gate return their empty answer — while the field LOOKS like a successful attribution. An empty host is
	// legible as missing; a pod name is a plausible wrong answer. The pod name also churns on every
	// redeploy, so it cannot key anything across restarts either.
	//
	// So the subject is resolved from the two labels that name a MACHINE — `instance` and `node` — and
	// nothing else. Everything the fallback carries stays on raw.Labels (the RAG and the classifier already
	// read pod/namespace there), so no information is lost; it just stops claiming to be a host.
	switch subject := hostSubject(a.Labels); {
	case subject == "":
		// No machine named this alert. Host and IP stay empty, honestly.
	case net.ParseIP(subject) != nil:
		raw.IP = subject
	default:
		raw.Host = subject
	}
	return coreingest.Normalize(raw, m.now())
}
