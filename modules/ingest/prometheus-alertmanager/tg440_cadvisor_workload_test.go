package alertmanager

import (
	"strings"
	"testing"
	"time"
)

// tg440StartsAt renders a StartsAt inside the ingest freshness window against the REAL clock these tests
// run under (toEnvelope is called directly on New(), no injected clock). The original hardcoded stamps
// were in-window when authored and rotted out of the 720h bound on 2026-08-30, redding the suite by
// calendar alone — a fixed date in a real-clock test is a time bomb, not a fixture.
func tg440StartsAt() string { return time.Now().Add(-time.Hour).UTC().Format(time.RFC3339) }

// TG-440 — a Docker/cAdvisor container alert names its container in `name` / the docker-compose service
// label, NEVER in a k8s container/pod label. TG's summary was the upstream annotation verbatim (a k8s
// "Container {{.container}} in {{.namespace}}" template renders "Container  in  ()" on such an alert) and its
// correlation target was the host — so every cAdvisor container alert was unidentifiable AND collapsed onto
// the host (measured: am-ContainerMemoryNearLimit-notrf01dmz01, container omoikane-trafilatura). workloadSubject
// resolves the container from BOTH vocabularies; the summary now NAMES it and the ref is workload-qualified.

func TestWorkloadSubjectSpansK8sAndDockerButNoHostOrScrapeLabel(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{"k8s container", map[string]string{"container": "api"}, "api"},
		{"docker compose service (self-identifying, no gate)", map[string]string{"container_label_com_docker_compose_service": "trafilatura"}, "trafilatura"},
		{"bare name is GATED — overloaded label, no container signal", map[string]string{"name": "omoikane-trafilatura"}, ""},
		{"bare name WITH a cadvisor job resolves", map[string]string{"name": "omoikane-trafilatura", "job": "omoikane-cadvisor"}, "omoikane-trafilatura"},
		{"bare name WITH a container_label_ resolves", map[string]string{"name": "omoikane-trafilatura", "container_label_com_docker_compose_project": "x"}, "omoikane-trafilatura"},
		{"OVERLOADED name on a host alert is IGNORED (systemd collector)", map[string]string{"name": "sshd.service", "instance": "notrf01dmz01", "job": "node-exporter"}, ""},
		{"container wins over docker labels", map[string]string{"container": "api", "name": "docker-name"}, "api"},
		{"pod is NOT a workload subject (TG-373 exporter-pod ambiguity)", map[string]string{"pod": "node-exporter-abc"}, ""},
		{"deployment excluded", map[string]string{"deployment": "api"}, ""},
		{"job alone (scrape job) resolves nothing", map[string]string{"job": "omoikane-cadvisor"}, ""},
		{"host-only alert carries no workload", map[string]string{"instance": "notrf01dmz01"}, ""},
	}
	for _, c := range cases {
		if got := workloadSubject(c.labels); got != c.want {
			t.Errorf("%s: workloadSubject(%v) = %q, want %q", c.name, c.labels, got, c.want)
		}
	}
}

// cadvisorAlert builds the exact shape TG measured: a firing ContainerMemoryNearLimit from cAdvisor whose
// container identity is in `name` / the compose-service label, with the upstream annotation already blank.
func cadvisorAlert(service, name string) alert {
	return alert{
		Status: "firing",
		Labels: map[string]string{
			"alertname": "ContainerMemoryNearLimit", "severity": "warning",
			"instance": "notrf01dmz01", "job": "omoikane-cadvisor",
			"name": name, "container_label_com_docker_compose_service": service,
		},
		Annotations: map[string]string{"summary": "Container  in  () using >90% memory limit"},
		StartsAt:    tg440StartsAt(),
	}
}

func TestCadvisorContainerAlertIsNamedAndWorkloadScoped(t *testing.T) {
	m := New()

	env, err := m.toEnvelope(cadvisorAlert("trafilatura", "omoikane-trafilatura"))
	if err != nil {
		t.Fatalf("toEnvelope: %v", err)
	}
	// The container is NAMED in the summary (was the identity-blank "Container  in  ()").
	if !strings.Contains(env.Summary, "trafilatura") {
		t.Errorf("summary does not name the container: %q — a Docker/cAdvisor container alert must not render "+
			"identity-blank when the identity is right there in the labels (TG-440)", env.Summary)
	}
	// The ref carries BOTH the host and the workload, so it is container-scoped, not host-collapsed.
	if !strings.Contains(env.ExternalRef, "notrf01dmz01") || !strings.Contains(env.ExternalRef, "trafilatura") {
		t.Errorf("ref %q is not workload-scoped — it must carry the host AND the container, or two containers "+
			"on one host collapse onto one incident (TG-440)", env.ExternalRef)
	}

	// A SECOND container on the SAME host is a DISTINCT incident.
	env2, err := m.toEnvelope(cadvisorAlert("mealie", "omoikane-mealie"))
	if err != nil {
		t.Fatalf("toEnvelope (second container): %v", err)
	}
	if env.ExternalRef == env2.ExternalRef {
		t.Errorf("two different containers on notrf01dmz01 got the SAME ref %q — the host-collapse defect (TG-440)", env.ExternalRef)
	}
}

// TestHostOnlyAlertRefIsUnchanged — the safety floor: a genuine host/node alert (no workload labels) must NOT
// gain a workload qualifier. Its ref stays host-scoped exactly as before, so this change cannot re-key the
// node/disk alerts that share notrf01dmz01.
func TestHostOnlyAlertRefIsUnchanged(t *testing.T) {
	m := New()
	env, err := m.toEnvelope(alert{
		Status:      "firing",
		Labels:      map[string]string{"alertname": "OmoikaneNodeDiskSpaceLow", "severity": "warning", "instance": "notrf01dmz01"},
		Annotations: map[string]string{"summary": "notrf01dmz01 / is 86.5% full"},
		StartsAt:    tg440StartsAt(),
	})
	if err != nil {
		t.Fatalf("toEnvelope: %v", err)
	}
	if env.ExternalRef != "am-OmoikaneNodeDiskSpaceLow-notrf01dmz01" {
		t.Errorf("host-only alert ref changed to %q — a workload qualifier leaked onto a node alert", env.ExternalRef)
	}
}

// TestOverloadedNameLabelOnAHostAlertIsNotReKeyed is the fresh-eyes-review counterexample (TG-440): the `name`
// label is overloaded — node_exporter's systemd collector emits name="sshd.service" on a genuine HOST alert
// with no cAdvisor/Docker signal. Bare `name` is gated (isContainerScrape) precisely so such an alert is NOT
// mis-read as a container: its ref stays host-scoped and its summary is not prefixed with the unit name.
func TestOverloadedNameLabelOnAHostAlertIsNotReKeyed(t *testing.T) {
	m := New()
	env, err := m.toEnvelope(alert{
		Status: "firing",
		Labels: map[string]string{
			"alertname": "SystemdUnitFailed", "severity": "warning",
			"instance": "notrf01dmz01:9100", "job": "node-exporter", "name": "sshd.service",
		},
		Annotations: map[string]string{"summary": "unit sshd.service failed on notrf01dmz01"},
		StartsAt:    tg440StartsAt(),
	})
	if err != nil {
		t.Fatalf("toEnvelope: %v", err)
	}
	if env.ExternalRef != "am-SystemdUnitFailed-notrf01dmz01" {
		t.Errorf("an overloaded `name` label re-keyed a host alert: ref=%q, want am-SystemdUnitFailed-notrf01dmz01 "+
			"— bare `name` must be gated on a genuine container signal (TG-440 review finding)", env.ExternalRef)
	}
	if strings.HasPrefix(env.Summary, "sshd.service") {
		t.Errorf("an overloaded `name` label prefixed a host alert's summary: %q", env.Summary)
	}
}
