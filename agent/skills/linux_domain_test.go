package skills

import (
	"testing"

	"github.com/territory-grounder/grounder/core/execclass"
)

// TG-78 linux slice — the Debian/Ubuntu guest OS competence on the same strict mechanism as the other
// three domains. The load-bearing properties: a POSITIVE rule allowlist (never a fall-through), both
// rule dialects (provider display + ingest slug) routing identically, and the two precedences that keep
// the never-touch-host and guest-lifecycle frames intact — a PVE node's OS-plane alert stays proxmox,
// and a guest DEVICE-down stays proxmox even though the guest is Linux.

func TestDomainOfClassifiesLinuxOsPlaneButOnlyOnGuests(t *testing.T) {
	// The four OS-plane families on a plain guest, in BOTH dialects, route to linux.
	for _, r := range []string{
		"Service up/down", "Service-up/down",
		"Space on / is >= 90% and < 95% in use", "Space-on-/-is-90-and-95-in-use",
		"Linux High Memory Usage, >= 90% in use", "Linux-High-Memory-Usage-90-in-use",
		"Processor usage over 85%",
	} {
		if got := DomainOf(r, "dc1cloudbeaver01", HostSignals{Guest: true}); got != DomainLinux {
			t.Errorf("DomainOf(%q, guest) = %q, want linux", r, got)
		}
	}
	// The SAME rule on a NON-guest (a bare host, an AP, a switch) is NOT linux — the OS competence is
	// scoped to virtualization guests; network gear firing "Service up/down" is a different plane.
	for _, r := range []string{"Service up/down", "Space on / is >= 90% and < 95% in use"} {
		if got := DomainOf(r, "dc1ap01", HostSignals{}); got != DomainUnknown {
			t.Errorf("DomainOf(%q, non-guest) = %q, want unknown — the linux lane requires a guest", r, got)
		}
	}
	// Precedence 1: the SAME rule on a PVE NODE stays proxmox (never-touch-host wins; the corpus
	// tg78-cluster-01 / tg78-storage-01 escalate rows depend on exactly this ordering).
	if got := DomainOf("Service up/down", "dc1pve03", HostSignals{PveNode: true}); got != DomainProxmox {
		t.Errorf("Service up/down on a pve node = %q, want proxmox — the node branch must precede the linux branch", got)
	}
	// Precedence 2: a guest DEVICE-down stays proxmox (guest-lifecycle, not OS-plane) — the linux branch
	// sits after the guest-down branch by construction.
	if got := DomainOf("Devices up/down", "dc1cloudbeaver01", HostSignals{Guest: true}); got != DomainProxmox {
		t.Errorf("a guest device-down = %q, want proxmox — lifecycle wins over the OS plane", got)
	}
	// The allowlist is POSITIVE: rules outside the four families never match, even on a guest.
	for _, r := range []string{"Device rebooted", "Sensor over limit - Check Device Health Settings", "01 Ping Latency", "NotAServiceThing", ""} {
		if got := DomainOf(r, "dc1cloudbeaver01", HostSignals{Guest: true}); got != DomainUnknown {
			t.Errorf("DomainOf(%q, guest) = %q, want unknown — outside the OS-plane allowlist", r, got)
		}
	}
}

// linux-triage loads ONLY on a Linux incident at a non-fast class, and never on another platform.
func TestLinuxSkillLoadsOnlyOnLinuxDomain(t *testing.T) {
	reg := Default()
	has := func(ctx Context) bool {
		_, loaded := reg.Compose(ctx)
		for _, n := range loaded {
			if n == "linux-triage" {
				return true
			}
		}
		return false
	}
	if !has(Context{Phase: PhaseInvestigate, ExecClass: execclass.StandardAgent, Domain: DomainLinux}) {
		t.Error("linux-triage must load on a STANDARD linux investigation")
	}
	if !has(Context{Phase: PhaseInvestigate, ExecClass: execclass.DeepInvestigation, Domain: DomainLinux}) {
		t.Error("linux-triage must load on a DEEP linux investigation")
	}
	if has(Context{Phase: PhaseInvestigate, ExecClass: execclass.FastAgent, Domain: DomainLinux}) {
		t.Error("a FAST linux triage must stay compact — no domain skill")
	}
	for _, d := range []Domain{DomainUnknown, DomainKubernetes, DomainProxmox, DomainCisco, DomainStorage} {
		if has(Context{Phase: PhaseInvestigate, ExecClass: execclass.StandardAgent, Domain: d}) {
			t.Errorf("linux-triage leaked onto domain %q", d)
		}
	}
	// Additive, never a replacement: the always-on protocols still compose on a linux incident.
	_, loads := reg.Compose(Context{Phase: PhaseInvestigate, ExecClass: execclass.DeepInvestigation, Domain: DomainLinux})
	if !contains(loads, "debugging-protocol") {
		t.Error("the general debugging-protocol dropped out on a linux incident — the domain skill must be additive")
	}
}
