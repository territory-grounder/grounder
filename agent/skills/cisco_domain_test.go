package skills

import (
	"testing"

	"github.com/territory-grounder/grounder/core/execclass"
)

// TG-85 — the Cisco domain skill, on TG-36's mechanism (adapted to the 3-arg DomainOf that landed with
// TG-78's host-domain routing). The load-bearing property is the SAME strict selection, plus the
// deliberate NON-classification of the ambiguous mesh-routing alerts: guessing a domain is exactly the
// noise the signal exists to prevent.

func TestDomainOfClassifiesAsaButNotAmbiguousMeshAlerts(t *testing.T) {
	// ASA* is unambiguously the Cisco firewall (real production rule: ASABindingDrift).
	for _, r := range []string{"ASABindingDrift", "ASATunnelDown", "asafailover"} {
		if got := DomainOf(r, "dc1fw01", HostSignals{}); got != DomainCisco {
			t.Errorf("DomainOf(%q) = %q, want cisco", r, got)
		}
	}
	// The Mesh(BGP|BFD|IPsec) routing alerts are DELIBERATELY unclassified — they may be Cisco IOS or a
	// non-Cisco fabric, and a cisco skill on a non-cisco incident is noise. They must stay unknown until
	// the estate topology decides, NOT be guessed into cisco.
	for _, r := range []string{"MeshiBGPPeerDown", "MeshBFDSessionDown", "MeshIPsecTunnelDown"} {
		if got := DomainOf(r, "h", HostSignals{}); got != DomainUnknown {
			t.Errorf("DomainOf(%q) = %q — a mesh-routing alert must NOT be guessed into cisco; that is the "+
				"exact domain-noise the strict signal prevents", r, got)
		}
	}
	// It must not misclassify a k8s alert as cisco, and a guest-down stays Proxmox even on an asa-ish host
	// name (the RULE is the identity, never the host string).
	if DomainOf("KubePodNotReady", "h", HostSignals{}) == DomainCisco {
		t.Error("a k8s alert classified as cisco")
	}
	if got := DomainOf("Devices up/down", "dc1asamirror01", HostSignals{Guest: true}); got != DomainProxmox {
		t.Errorf("a guest device-down must stay proxmox regardless of host spelling, got %q", got)
	}
}

// cisco-triage loads ONLY on a Cisco incident at a non-fast class, and never on another platform.
func TestCiscoSkillLoadsOnlyOnCiscoDomain(t *testing.T) {
	reg := Default()
	has := func(ctx Context) bool {
		_, loaded := reg.Compose(ctx)
		for _, n := range loaded {
			if n == "cisco-triage" {
				return true
			}
		}
		return false
	}
	if !has(Context{Phase: PhaseInvestigate, ExecClass: execclass.StandardAgent, Domain: DomainCisco}) {
		t.Error("cisco-triage must load on a STANDARD cisco investigation")
	}
	if !has(Context{Phase: PhaseInvestigate, ExecClass: execclass.DeepInvestigation, Domain: DomainCisco}) {
		t.Error("cisco-triage must load on a DEEP cisco investigation")
	}
	if has(Context{Phase: PhaseInvestigate, ExecClass: execclass.FastAgent, Domain: DomainCisco}) {
		t.Error("a FAST cisco triage must stay compact — no domain skill")
	}
	for _, d := range []Domain{DomainUnknown, DomainKubernetes, DomainProxmox, DomainLinux, DomainStorage} {
		if has(Context{Phase: PhaseInvestigate, ExecClass: execclass.StandardAgent, Domain: d}) {
			t.Errorf("cisco-triage leaked onto domain %q", d)
		}
	}
}
