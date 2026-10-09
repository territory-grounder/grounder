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
// site/category. The CORRELATION TARGET must fall back to the pod (so distinct pods de-correlate instead of
// collapsing onto one target-less ref), and the site + full label set must propagate so the risk classifier
// sees labels["category"] and downstream RAG keeps the k8s context.
//
// THE HOST ASSERTION CHANGED (TG-373), and it was this test that encoded the defect. It used to require
// Host == "prometheus-0" — a POD name in the host field. That conflates two different questions:
//
//	correlation target — what makes this incident distinct from the next one   -> pod is right
//	host subject       — which MACHINE this is about                           -> a pod is not one
//
// A pod name in Host resolves to nothing in the estate graph (guests and hypervisors, 1,864 edges), so
// BlastRadiusWide, SiblingsOf and every host-match gate return their empty answer while the field LOOKS
// like a successful attribution. Measured 2026-08-06: TG recorded
// host=my-awx-task-756d768868-xs8gc for a KubePodNotReady — for its own AWX outage. An empty host is
// legible as missing; a pod name is a plausible wrong answer, and it churns on every redeploy besides.
//
// So the ref assertion below is UNCHANGED (the pod still de-correlates) and Host must now be empty. Nothing
// is lost: pod and namespace remain on Labels, where the RAG and the classifier already read them.
func TestKubeAlertTargetFallbackAndLabels(t *testing.T) {
	body := `{"status":"firing","alerts":[{"status":"firing",
	  "labels":{"alertname":"KubePodCrashLooping","namespace":"monitoring","pod":"prometheus-0","severity":"warning","site":"nl","category":"agentic-platform"},
	  "annotations":{"summary":"pod crashlooping"},"startsAt":"2026-07-15T12:00:00Z"}]}`
	env, err := mod().Normalize(context.Background(), []byte(body))
	if err != nil {
		t.Fatalf("a kube-state-metrics alert (no instance) must still normalize: %v", err)
	}
	if env.Host != "" {
		t.Fatalf("a POD name reached the host field (%q). A pod is a workload, not a machine: it resolves "+
			"to nothing in the estate graph, so every host-match gate returns its empty answer while this "+
			"field looks like a successful attribution (TG-373)", env.Host)
	}
	if env.Labels["pod"] != "prometheus-0" {
		t.Fatalf("the pod must survive on Labels — the workload identity is not lost, it just stops "+
			"claiming to be a host; got %v", env.Labels)
	}
	if env.Site != "dc1" {
		t.Fatalf("the site label must propagate into the envelope, canonicalized to the deployment-key form "+
			"(the 'nl' label folds to 'dc1', TG-456); got Site=%q", env.Site)
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

// THE `node` LABEL IS A MACHINE AND MUST REACH Host (TG-373). A kube alert that names the node it is about
// is attributable — the estate graph holds hypervisor and guest names, so this one can actually resolve.
//
// KILLING MUTATION: drop `node` from hostSubject, leaving only `instance`. RED — every node-scoped kube
// alert becomes unattributed, which is over-correcting the pod fix into the opposite defect.
func TestTheNodeLabelStillReachesTheHostField(t *testing.T) {
	body := `{"status":"firing","alerts":[{"status":"firing",
	  "labels":{"alertname":"KubeNodeNotReady","node":"dc1k8s-node01","pod":"kube-state-metrics-0","severity":"warning"},
	  "annotations":{"summary":"node not ready"},"startsAt":"2026-07-15T12:00:00Z"}]}`
	env, err := mod().Normalize(context.Background(), []byte(body))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if env.Host != "dc1k8s-node01" {
		t.Fatalf("Host = %q, want the node — a node IS a machine and the estate graph can resolve it. "+
			"Suppressing it too would trade a wrong answer for no answer", env.Host)
	}
	// And the correlation target still prefers the more specific pod, so two pods on one node stay distinct.
	if env.ExternalRef != "am-KubeNodeNotReady-kube-state-metrics-0" {
		t.Fatalf("ExternalRef = %q — the correlation target must still prefer the pod; the host subject and "+
			"the correlation key are different questions", env.ExternalRef)
	}
}

// AN `instance` THAT IS AN IP GOES TO IP, NOT Host — unchanged behaviour, pinned because the switch that
// decides it was rewritten. A bare address in Host would look resolvable to the estate graph and never match.
//
// KILLING MUTATION: assign the IP to raw.Host. RED.
func TestAnInstanceIPGoesToTheIPFieldNotHost(t *testing.T) {
	body := `{"status":"firing","alerts":[{"status":"firing",
	  "labels":{"alertname":"KubeJobFailed","instance":"10.0.2.193:8080","severity":"warning"},
	  "annotations":{"summary":"job failed"},"startsAt":"2026-07-15T12:00:00Z"}]}`
	env, err := mod().Normalize(context.Background(), []byte(body))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if env.Host != "" {
		t.Errorf("Host = %q — an address is not a hostname and must not occupy the host field", env.Host)
	}
	if env.IP == nil || env.IP.String() != "10.0.2.193" {
		t.Errorf("IP = %v, want 10.0.2.193 (the port stripped) — this is the field 40 of 48 host-less "+
			"Alertmanager incidents identify their subject by", env.IP)
	}
}

// NO MACHINE NAMED AT ALL: both fields stay empty. The honest gap.
//
// KILLING MUTATION: fall back to the workload labels again. RED.
func TestAWorkloadOnlyAlertNamesNoMachine(t *testing.T) {
	body := `{"status":"firing","alerts":[{"status":"firing",
	  "labels":{"alertname":"KubeDeploymentReplicasMismatch","deployment":"my-awx-web","namespace":"awx","severity":"warning"},
	  "annotations":{"summary":"replicas mismatch"},"startsAt":"2026-07-15T12:00:00Z"}]}`
	env, err := mod().Normalize(context.Background(), []byte(body))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if env.Host != "" || env.IP != nil {
		t.Fatalf("a workload-only alert claimed a machine: Host=%q IP=%v", env.Host, env.IP)
	}
	if env.ExternalRef != "am-KubeDeploymentReplicasMismatch-my-awx-web" {
		t.Fatalf("ExternalRef = %q — the correlation target must still use the deployment, or every "+
			"workload alert of this class collapses onto one ref", env.ExternalRef)
	}
}

// THE ALERTMANAGER FINGERPRINT REACHES THE ENVELOPE (TG-373 / TG-354).
//
// It arrives on every alert — verified live on 2026-08-06: 21 of 21 alerts held by this deployment's
// Alertmanager carry one — and it was parsed into alert.Fingerprint and read by nothing. It is the stable
// join key TG-354 needs: a hash of the alert's label set, identical across firing->resolved and across
// Alertmanager restarts, unlike TG's own am-<alertname>-<target> ref which the estate's tickets have never
// contained.
//
// KILLING MUTATION: drop the withFingerprint call (the state this shipped in). RED.
func TestTheAlertmanagerFingerprintIsCarried(t *testing.T) {
	body := `{"status":"firing","alerts":[{"status":"firing","fingerprint":"10411ea66d359711",
	  "labels":{"alertname":"KubePodCrashLooping","pod":"prometheus-0","severity":"warning"},
	  "annotations":{"summary":"pod crashlooping"},"startsAt":"2026-07-15T12:00:00Z"}]}`
	env, err := mod().Normalize(context.Background(), []byte(body))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if got := env.Labels[FingerprintLabel]; got != "10411ea66d359711" {
		t.Fatalf("%s = %q, want 10411ea66d359711 — Alertmanager sends this on every alert and TG parsed it "+
			"into a field nothing read. It is the only stable identity an incident and its estate ticket "+
			"can share", FingerprintLabel, got)
	}
}

// A SOURCE THAT SENDS NO FINGERPRINT MUST NOT GAIN AN EMPTY ONE. An empty label looks like an identity and
// would match every other empty one on a join.
//
// KILLING MUTATION: drop withFingerprint's empty check. RED.
func TestNoFingerprintMeansNoLabel(t *testing.T) {
	body := `{"status":"firing","alerts":[{"status":"firing",
	  "labels":{"alertname":"KubePodCrashLooping","pod":"prometheus-0","severity":"warning"},
	  "annotations":{"summary":"pod crashlooping"},"startsAt":"2026-07-15T12:00:00Z"}]}`
	env, err := mod().Normalize(context.Background(), []byte(body))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if v, present := env.Labels[FingerprintLabel]; present {
		t.Fatalf("%s present as %q on an alert that sent none — an empty identity joins to every other "+
			"empty identity", FingerprintLabel, v)
	}
}

// THE FINGERPRINT SURVIVES THE 64-LABEL CAP. boundedLabels sorts and truncates; a join key that vanishes on
// a label-heavy alert is a join key that fails exactly where the data is richest.
//
// KILLING MUTATION: inject the fingerprint AFTER boundedLabels instead of before, or rename the label so it
// sorts late (e.g. "zz_fingerprint"). The second RED here; the first would silently push the label set to 65
// and trip the core grammar, which is the failure boundedLabels was written to prevent.
func TestTheFingerprintSurvivesTheLabelCap(t *testing.T) {
	labels := map[string]string{"alertname": "RealDown", "severity": "critical"}
	for i := 0; i < 80; i++ {
		labels["extra_"+strconv.Itoa(i)] = "x"
	}
	b, _ := json.Marshal(map[string]any{"status": "firing", "alerts": []map[string]any{{
		"status": "firing", "fingerprint": "deadbeefcafe0001", "labels": labels,
		"annotations": map[string]string{"summary": "s"}, "startsAt": "2026-07-15T12:00:00Z",
	}}})
	env, err := mod().Normalize(context.Background(), b)
	if err != nil {
		t.Fatalf("a label-heavy alert must still normalize: %v", err)
	}
	if got := env.Labels[FingerprintLabel]; got != "deadbeefcafe0001" {
		t.Fatalf("%s = %q on an 82-label alert — the join key was truncated away by the label cap, which is "+
			"exactly where a rich alert most needs one", FingerprintLabel, got)
	}
	if len(env.Labels) > 64 {
		t.Fatalf("label set is %d, over the cap — injecting the fingerprint after bounding would push it "+
			"past the envelope grammar and the alert would vanish silently", len(env.Labels))
	}
}

// The caller's parsed webhook must not be mutated: toEnvelope reads a.Labels again after this point.
//
// KILLING MUTATION: write the fingerprint into `in` instead of a copy. RED.
func TestWithFingerprintDoesNotMutateItsInput(t *testing.T) {
	in := map[string]string{"alertname": "X"}
	out := withFingerprint(in, "abc123")
	if _, present := in[FingerprintLabel]; present {
		t.Fatal("withFingerprint mutated the caller's label map — toEnvelope reads it again for " +
			"alertname/severity/site after this point")
	}
	if out[FingerprintLabel] != "abc123" {
		t.Fatalf("the copy did not receive the fingerprint: %v", out)
	}
}
