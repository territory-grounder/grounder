package alertmanager

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// TestKubeAlertTargetFallbackAndLabels proves the normalizer handles kube-prometheus-stack alerts: a
// kube-state-metrics alert (KubePodCrashLooping) carries NO `instance` label but does carry pod/namespace/
// site/category. The target must fall back to the pod (so distinct pods de-correlate instead of collapsing
// onto one target-less ref), and the site + full label set must propagate so the risk classifier sees
// labels["category"] and downstream RAG keeps the k8s context.
func TestKubeAlertTargetFallbackAndLabels(t *testing.T) {
	body := `{"status":"firing","alerts":[{"status":"firing",
	  "labels":{"alertname":"KubePodCrashLooping","namespace":"monitoring","pod":"prometheus-0","severity":"warning","site":"nl","category":"agentic-platform"},
	  "annotations":{"summary":"pod crashlooping"},"startsAt":"2026-07-15T12:00:00Z"}]}`
	env, err := mod().Normalize(context.Background(), []byte(body))
	if err != nil {
		t.Fatalf("a kube-state-metrics alert (no instance) must still normalize: %v", err)
	}
	if env.Host != "prometheus-0" {
		t.Fatalf("target must fall back to the pod label when instance is absent; got Host=%q", env.Host)
	}
	if env.Site != "nl" {
		t.Fatalf("the site label must propagate into the envelope; got Site=%q", env.Site)
	}
	if env.Labels["category"] != "agentic-platform" {
		t.Fatalf("labels must propagate (the risk classifier reads labels[category]); got %v", env.Labels)
	}
	if env.ExternalRef != "am-KubePodCrashLooping-prometheus-0" {
		t.Fatalf("external_ref must include the resolved target to de-correlate distinct pods; got %q", env.ExternalRef)
	}
}

// TestHostileSiteAndLabelsDontDropAlert reproduces the review's confirmed major: LEGAL Prometheus label
// data (a site with a space, an oversize label set/value) must never fail the alert's normalization — the
// offending VALUES are sanitized away (site dropped, entries trimmed) and the incident survives.
func TestHostileSiteAndLabelsDontDropAlert(t *testing.T) {
	labels := map[string]string{
		"alertname": "RealDown", "instance": "h1:9100", "severity": "critical",
		"site": "nl east",                 // space — outside the envelope's slug grammar
		"big":  strings.Repeat("v", 2000), // value beyond the 1024-byte bound
	}
	for i := 0; i < 70; i++ { // beyond the 64-label cardinality cap
		labels["extra_"+strconv.Itoa(i)] = "x"
	}
	b, _ := json.Marshal(map[string]any{"status": "firing", "alerts": []map[string]any{{
		"status": "firing", "labels": labels,
		"annotations": map[string]string{"summary": "down"}, "startsAt": "2026-07-15T12:00:00Z",
	}}})
	env, err := mod().Normalize(context.Background(), b)
	if err != nil {
		t.Fatalf("hostile-but-legal label data must not drop the alert: %v", err)
	}
	if env.Site != "" {
		t.Fatalf("an out-of-grammar site value must be dropped, got %q", env.Site)
	}
	if len(env.Labels) > 64 {
		t.Fatalf("the label set must be bounded to 64 entries, got %d", len(env.Labels))
	}
	if _, ok := env.Labels["big"]; ok {
		t.Fatal("an oversize label value must be trimmed from the set")
	}
	if env.AlertRule != "RealDown" || env.Host != "h1" {
		t.Fatalf("the incident identity must survive sanitization: %+v", env)
	}
}
