package skillcorpus

import (
	"strings"
	"testing"
)

// The corpus must parse WHOLE — and the two packs whose unreachability was TG-529's finding must be in it.
// Vacuity floor: the tree held 32 runbooks when this shipped; a parse that returns a handful means the
// embed glob or the parser broke, not that the corpus shrank.
func TestTG529CorpusParsesWholeWithBothPacks(t *testing.T) {
	rbs, err := Runbooks()
	if err != nil {
		t.Fatalf("Runbooks: %v", err)
	}
	if len(rbs) < 30 {
		t.Fatalf("only %d runbooks parsed — the corpus embed or parser is broken (vacuity floor 30)", len(rbs))
	}
	byName := map[string]Runbook{}
	for _, r := range rbs {
		if r.Version == "" || r.Body == "" || r.Name == "" {
			t.Fatalf("runbook %+v has an empty invariant field", r)
		}
		byName[r.Name] = r
	}
	for _, name := range []string{
		// TG-85 Cisco pack (batch 4)
		"cisco-acl-nat-triage", "cisco-asa-failover-context-triage", "cisco-asa-vpn-ipsec-triage",
		"cisco-bgp-ospf-adjacency-triage", "cisco-high-cpu-memory-triage", "cisco-interface-line-protocol-triage",
		// TG-78 K8s pack (batch 5)
		"k8s-pod-crashloop-oom-triage", "k8s-pod-pending-scheduling-triage", "k8s-node-notready-triage",
		"k8s-pvc-storageclass-triage", "k8s-service-ingress-dns-triage", "k8s-control-plane-degradation-triage",
	} {
		if _, ok := byName[name]; !ok {
			t.Errorf("pack runbook %q missing from the embedded corpus — the exact unreachability TG-529 exists to end", name)
		}
	}
}

// A malformed entry refuses the WHOLE corpus — a silent subset-seed would read as delivered while
// dropping packs invisibly.
func TestTG529CorpusRefusesMalformed(t *testing.T) {
	for _, tc := range []struct{ name, raw, wantErr string }{
		{"no-frontmatter", "just a body", "no frontmatter"},
		{"unterminated", "---\nname: x\n", "unterminated"},
		{"wrong-dir", "---\nname: other\nclass: runbook\nversion: 1\n---\nbody", "does not match"},
		{"wrong-class", "---\nname: wrong-class\nclass: skill\nversion: 1\n---\nbody", "ONLY the runbook corpus"},
		{"no-version", "---\nname: no-version\nclass: runbook\n---\nbody", "no version"},
		{"empty-body", "---\nname: empty-body\nclass: runbook\nversion: 1\n---\n  ", "empty body"},
	} {
		if _, err := parse(tc.name, tc.raw); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: want error containing %q, got %v", tc.name, tc.wantErr, err)
		}
	}
}
