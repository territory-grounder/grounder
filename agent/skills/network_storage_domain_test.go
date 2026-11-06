package skills

import (
	"testing"

	"github.com/territory-grounder/grounder/core/execclass"
)

// TG-78 network + storage slices — the two ESTATE-IDENTITY domains. Neither class has an honest rule
// prefix (the measured vocabulary is generic SNMP rules fired by guests and PVE nodes too), so both route
// on the typed inventory signal alone, every alert family — the IsPveNode discipline extended. The
// load-bearing properties pinned here: identity dominates symptom; the rule table still wins over
// identity (an unambiguous rule prefix is stronger evidence than a host class); no signal ⇒ byte-identical
// pre-slice routing.
func TestDomainOfRoutesDeviceIdentities(t *testing.T) {
	// A NETWORK DEVICE routes to cisco on EVERY family — the generic rules that would otherwise be
	// unknown (or would steal into linux were the host a guest).
	for _, r := range []string{"Port status up/down", "Port utilisation over threshold", "01 Ping Latency", "Devices up/down", "Service up/down", "Sensor over limit - Check Device Health Settings"} {
		if got := DomainOf(r, "dc1sw01", HostSignals{NetworkDevice: true}); got != DomainCisco {
			t.Errorf("DomainOf(%q, network device) = %q, want cisco — the device identity dominates the symptom", r, got)
		}
	}
	// A STORAGE APPLIANCE routes to storage on every family — including the two measured DSM families
	// (the sensor-health rule the corpus once taught propose on, and the disk-space rule that must NOT
	// take the linux lane's disk framing).
	for _, r := range []string{"Sensor over limit - Check Device Health Settings", "Space on / is >= 90% and < 95% in use", "Space-on-/-is-90-and-95-in-use", "Port utilisation over threshold", "Devices up/down"} {
		if got := DomainOf(r, "dc1syno01", HostSignals{StorageAppliance: true}); got != DomainStorage {
			t.Errorf("DomainOf(%q, storage appliance) = %q, want storage — the appliance identity dominates the symptom", r, got)
		}
	}
	// The RULE TABLE still wins over identity: an unambiguous prefix names its platform whatever fires it.
	if got := DomainOf("ASABindingDrift", "dc1sw01", HostSignals{NetworkDevice: true}); got != DomainCisco {
		t.Errorf("ASA rule on a network device = %q, want cisco", got)
	}
	if got := DomainOf("KubePodNotReady", "dc1syno01", HostSignals{StorageAppliance: true}); got != DomainKubernetes {
		t.Errorf("a ^kube rule wins over the appliance identity, got %q — rule prefixes are stronger evidence", got)
	}
	// PVE-node precedence is stated above both device branches: a node that somehow also carried a device
	// signal stays proxmox (the never-touch-host frame cannot be displaced by a mis-typed second signal).
	if got := DomainOf("Service up/down", "dc1pve03", HostSignals{PveNode: true, NetworkDevice: true, StorageAppliance: true}); got != DomainProxmox {
		t.Errorf("pve-node identity must dominate every other signal, got %q", got)
	}
	// No signal ⇒ the pre-slice posture, byte-identical: the same rules stay unknown on a bare host.
	for _, r := range []string{"Port status up/down", "Sensor over limit - Check Device Health Settings"} {
		if got := DomainOf(r, "dc1ap01", HostSignals{}); got != DomainUnknown {
			t.Errorf("DomainOf(%q, no signals) = %q, want unknown — an unseeded estate routes rule-only", r, got)
		}
	}
}

// storage-triage loads ONLY on a Storage incident at a non-fast class, and never on another platform —
// the same strict, additive, cross-platform-isolated contract every domain skill carries.
func TestStorageSkillLoadsOnlyOnStorageDomain(t *testing.T) {
	reg := Default()
	has := func(ctx Context) bool {
		_, loaded := reg.Compose(ctx)
		for _, n := range loaded {
			if n == "storage-triage" {
				return true
			}
		}
		return false
	}
	if !has(Context{Phase: PhaseInvestigate, ExecClass: execclass.StandardAgent, Domain: DomainStorage}) {
		t.Error("storage-triage must load on a STANDARD storage investigation")
	}
	if !has(Context{Phase: PhaseInvestigate, ExecClass: execclass.DeepInvestigation, Domain: DomainStorage}) {
		t.Error("storage-triage must load on a DEEP storage investigation")
	}
	if has(Context{Phase: PhaseInvestigate, ExecClass: execclass.FastAgent, Domain: DomainStorage}) {
		t.Error("a FAST storage triage must stay compact — no domain skill")
	}
	for _, d := range []Domain{DomainUnknown, DomainKubernetes, DomainProxmox, DomainCisco, DomainLinux} {
		if has(Context{Phase: PhaseInvestigate, ExecClass: execclass.StandardAgent, Domain: d}) {
			t.Errorf("storage-triage leaked onto domain %q", d)
		}
	}
	// Additive, never a replacement: the always-on protocols still compose on a storage incident.
	_, loads := reg.Compose(Context{Phase: PhaseInvestigate, ExecClass: execclass.DeepInvestigation, Domain: DomainStorage})
	if !contains(loads, "debugging-protocol") {
		t.Error("the general debugging-protocol dropped out on a storage incident — the domain skill must be additive")
	}
}
