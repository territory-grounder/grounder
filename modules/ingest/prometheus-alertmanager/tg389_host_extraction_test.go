package alertmanager

import (
	"context"
	"testing"
)

// TG-389 — the Alertmanager normalizer threw away the node identity of kube-state / node-level alerts,
// leaving host='' on 29% of Alertmanager traffic and collapsing seven distinct CiliumAgentNotReady nodes
// onto one empty key. hostSubject now resolves the node from every MACHINE-naming label — instance, node,
// kubernetes_node, nodename — while still refusing workload labels (pod/deployment/...) per TG-373.

func TestHostSubjectResolvesEveryMachineLabelButNoWorkloadLabel(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{"instance strips port", map[string]string{"instance": "192.168.85.10:9090"}, "192.168.85.10"},
		{"node", map[string]string{"node": "nl-node-1"}, "nl-node-1"},
		{"kubernetes_node", map[string]string{"kubernetes_node": "192.168.85.11"}, "192.168.85.11"},
		{"nodename", map[string]string{"nodename": "192.168.85.20"}, "192.168.85.20"},
		{"instance wins over node", map[string]string{"instance": "h1:1", "node": "h2"}, "h1"},
		{"pod is NOT a host (TG-373)", map[string]string{"pod": "cilium-agent-abc"}, ""},
		{"deployment is NOT a host (TG-373)", map[string]string{"deployment": "api"}, ""},
		{"no machine label", map[string]string{"alertname": "X"}, ""},
	}
	for _, c := range cases {
		if got := hostSubject(c.labels); got != c.want {
			t.Errorf("%s: hostSubject(%v) = %q, want %q", c.name, c.labels, got, c.want)
		}
	}
}

// amPayload builds a one-alert Alertmanager webhook naming its node ONLY via kubernetes_node.
func amNodePayload(node, fp string) string {
	return `{"status":"firing","alerts":[{"status":"firing",
	  "labels":{"alertname":"CiliumAgentNotReady","severity":"warning","kubernetes_node":"` + node + `"},
	  "annotations":{"summary":"cilium agent not ready"},"startsAt":"2026-07-15T12:00:00Z","fingerprint":"` + fp + `"}]}`
}

// TestDistinctIPNodesKeepDistinctIdentity — the MEASURED cascade shape: CiliumAgentNotReady on distinct nodes
// addressed by IP, named ONLY by kubernetes_node. Before TG-389 hostSubject checked only instance/node, so
// the IP was thrown away entirely — both alerts had host=” AND ip=” and were indistinguishable. Now the IP
// is preserved (an IP subject routes to the envelope's IP field, hostnames to Host — the normalizer's own
// split), so the two nodes keep distinct identities. host stays "" for the IP case, and the dedup stage's
// empty-host guard (core/suppression, TestEmptyHostIsNeverADedupCandidate) is what stops the collapse there.
// KILLING MUTATION: drop kubernetes_node/nodename from hostSubject → both go empty → RED.
func TestDistinctIPNodesKeepDistinctIdentity(t *testing.T) {
	a, err := mod().Normalize(context.Background(), []byte(amNodePayload("192.168.85.10", "n10")))
	if err != nil {
		t.Fatalf("normalize node .10: %v", err)
	}
	b, err := mod().Normalize(context.Background(), []byte(amNodePayload("192.168.85.11", "n11")))
	if err != nil {
		t.Fatalf("normalize node .11: %v", err)
	}
	if a.IP == nil || b.IP == nil {
		t.Fatalf("an IP-addressed node normalized to NO identity (a.ip=%v b.ip=%v) — the kubernetes_node label "+
			"was thrown away, so distinct nodes were indistinguishable (TG-389)", a.IP, b.IP)
	}
	if a.IP.Equal(b.IP) {
		t.Fatalf("two DIFFERENT nodes both normalized to IP %v — they are indistinguishable to every host-keyed stage", a.IP)
	}
}

// TestDistinctHostnameNodesGetDistinctHosts — the same for nodes named by HOSTNAME: they route to Host (the
// key the dedup stage matches on), so distinct nodes get distinct dedup keys instead of one empty one.
func TestDistinctHostnameNodesGetDistinctHosts(t *testing.T) {
	a, err := mod().Normalize(context.Background(), []byte(amNodePayload("nl-cilium-node-a", "na")))
	if err != nil {
		t.Fatalf("normalize node a: %v", err)
	}
	b, err := mod().Normalize(context.Background(), []byte(amNodePayload("nl-cilium-node-b", "nb")))
	if err != nil {
		t.Fatalf("normalize node b: %v", err)
	}
	if a.Host == "" || b.Host == "" {
		t.Fatalf("a hostname-named node normalized to host='' (a=%q b=%q) — kubernetes_node was thrown away", a.Host, b.Host)
	}
	if a.Host == b.Host {
		t.Fatalf("two DIFFERENT nodes both normalized to host %q — dedup would suppress one as a duplicate", a.Host)
	}
}

// TestAWorkloadOnlyAlertStaysHostlessAndHonest — the vacuity guard. An alert that genuinely names only a
// workload (no machine label) must stay host=” (never borrow a pod name as a host, TG-373) — the dedup
// stage's empty-host guard (core/suppression) is what stops those from collapsing, NOT a fabricated host.
func TestAWorkloadOnlyAlertStaysHostless(t *testing.T) {
	payload := `{"status":"firing","alerts":[{"status":"firing",
	  "labels":{"alertname":"KubePodCrashLooping","severity":"warning","pod":"api-7d9","namespace":"prod"},
	  "annotations":{"summary":"crashloop"},"startsAt":"2026-07-15T12:00:00Z","fingerprint":"wl1"}]}`
	env, err := mod().Normalize(context.Background(), []byte(payload))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if env.Host != "" {
		t.Errorf("a workload-only alert got host=%q — a pod/workload name in the host field is a wrong answer "+
			"that reads like a right one (TG-373); it must stay empty and be caught by the dedup empty-host guard", env.Host)
	}
}
