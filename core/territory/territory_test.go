package territory

import "testing"

func TestClassify(t *testing.T) {
	cases := map[string]Territory{
		"kubectl delete pod":   TerritoryK8s,
		"helm upgrade":         TerritoryK8s,
		"cisco asa acl change": TerritoryNetwork,
		"netplan apply":        TerritoryEdge,
		"pct reboot 101":       TerritoryPVE,
		"restart syno01 iscsi": TerritoryNative,
		"docker compose up":    TerritoryDocker,
		// IaC/GitOps writers are k8s territory (were waved through as benign).
		"tofu apply on atlantis":      TerritoryK8s,
		"terraform apply":             TerritoryK8s,
		"argocd app sync prod":        TerritoryK8s,
		// storage command verbs (iscsiadm/exportfs/seaweedfs) are native territory.
		"iscsiadm --op delete --logout": TerritoryNative,
		"exportfs -ra":                  TerritoryNative,
		"weed volume delete":            TerritoryNative,
		// network/edge command verbs.
		"vtysh -c 'write mem'":  TerritoryNetwork,
		"swanctl --terminate":   TerritoryEdge,
		// a qm reboot of a k8s control-plane GUEST dominates to k8s (not pve) — the stateful target's territory.
		"dc1k8s-ctrlr01 qm reboot 101": TerritoryK8s,
	}
	for op, want := range cases {
		if got, ok := Classify(op); !ok || got != want {
			t.Errorf("Classify(%q) = %q,%v; want %q", op, got, ok, want)
		}
	}
	if _, ok := Classify("restart nginx service"); ok {
		t.Error("a plain service op must not be in a high-stakes territory")
	}
}

func TestGatePermit(t *testing.T) {
	g := Gate{Acknowledged: map[Territory]bool{TerritoryK8s: true}}

	// read-only is never gated
	if r := g.Permit(false, false, "kubectl delete pod"); r.Decision != Allow {
		t.Fatalf("read-only must be allowed, got %+v", r)
	}
	// mutating in an ACKNOWLEDGED territory → allow
	if r := g.Permit(true, true, "kubectl", "rollout restart"); r.Decision != Allow || r.Territory != TerritoryK8s {
		t.Fatalf("acknowledged k8s write must be allowed, got %+v", r)
	}
	// mutating in an UNACKNOWLEDGED territory → block, with the caveat surfaced
	r := g.Permit(true, true, "netplan apply on edge-router")
	if r.Decision != Block || r.Territory != TerritoryEdge || r.Reason == "" {
		t.Fatalf("unacknowledged edge write must be blocked, got %+v", r)
	}
	// mutating, NOT high-stakes, NOT confirmed-infra → benign allow
	if r := g.Permit(true, false, "restart nginx"); r.Decision != Allow {
		t.Fatalf("a benign mutating op must be allowed, got %+v", r)
	}
	// mutating, CONFIRMED infra, but unclassifiable → fail closed (block)
	if r := g.Permit(true, true, "some-opaque-destructive-verb"); r.Decision != Block {
		t.Fatalf("a confirmed infra write the gate can't place must fail closed, got %+v", r)
	}
}
