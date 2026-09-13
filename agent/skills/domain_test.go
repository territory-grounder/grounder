package skills

import (
	"testing"

	"github.com/territory-grounder/grounder/core/execclass"
	"github.com/territory-grounder/grounder/core/skillstore"
)

// TG-36 — the FIRST domain-scoped skill. The load-bearing property is INV-08 in a new dimension: a
// platform competence must be selected by a TYPED signal derived from the alert, never by the model, and
// must be INVISIBLE on every other platform (including an unclassified incident) — the opposite failure
// from the general protocols, which fail toward MORE guidance.

func TestDomainOfClassifiesTheEstateAlertVocabulary(t *testing.T) {
	// Real production rule names (ingest_alert on dc1tg01).
	k8s := []string{"KubePodNotReady", "KubeNodeNotReady", "KubePersistentVolumeFillingUp", "CiliumAgentNotReady", "kubepodcrashlooping"}
	for _, r := range k8s {
		if got := DomainOf(r, "anyhost", HostSignals{}); got != DomainKubernetes {
			t.Errorf("DomainOf(%q) = %q, want kubernetes", r, got)
		}
	}
	// PROXMOX = a guest-lifecycle fault: the host is a guest AND the alert says the device/host is down. This is
	// intake-independent — the pve-liveness source and a LibreNMS push both stamp a Device-Down-family rule, so
	// the same guest classifies identically whichever sensor saw it (TG-78; the old ^pve-liveness rule matched
	// nothing in production because the source normalizes to Device-Down).
	for _, r := range []string{"Device-Down", "Devices up/down", "Device Down (SNMP unreachable)", "HostDown", "DeviceDown"} {
		if got := DomainOf(r, "dc1bookwyrm01", HostSignals{Guest: true}); got != DomainProxmox {
			t.Errorf("DomainOf(%q, guest) = %q, want proxmox — a guest-down is a proxmox-plane fault", r, got)
		}
	}
	// A device-down on a NON-guest is NOT proxmox (a bare host / switch down is not a guest-lifecycle fault).
	if got := DomainOf("Device-Down", "dc1rtr01", HostSignals{}); got != DomainUnknown {
		t.Errorf("Device-Down on a non-guest classified %q, want unknown", got)
	}
	// A SERVICE/metric alert on a guest is NOT proxmox (a service-plane fault that happens to run on a
	// guest — the guest itself is up). This half of the old discrimination stands unchanged; what the
	// TG-78 linux slice CONSCIOUSLY REVERSED is where those faults now land: the OS-plane families route
	// to DomainLinux (linux-triage — the competence actually scoped to them, owner-commissioned
	// 2026-08-30) instead of the DomainUnknown the pre-slice posture pinned to keep proxmox-triage off
	// them. Both dialects of each rule (provider display + ingest slug) must route identically.
	for _, r := range []string{
		"Service up/down", "Service-up/down",
		"Processor usage over 85%",
		"Space on / is >= 90% and < 95% in use", "Space-on-/-is-90-and-95-in-use",
		"Linux High Memory Usage, >= 90% in use", "Linux-High-Memory-Usage-90-in-use",
	} {
		if got := DomainOf(r, "dc1bookwyrm01", HostSignals{Guest: true}); got != DomainLinux {
			t.Errorf("DomainOf(%q, guest) = %q, want linux — an OS-plane fault on a plain guest is linux-triage's lane (never proxmox: the guest itself is up)", r, got)
		}
	}
	// The allowlist stays POSITIVE: network-plane, device-plane events, and hardware sensors on a guest
	// remain UNKNOWN — handing them systemd competence is the misroute the strict signal exists to prevent.
	for _, r := range []string{"01 Ping Latency", "Device rebooted", "Sensor over limit - Check Device Health Settings", "Port status up/down"} {
		if got := DomainOf(r, "dc1bookwyrm01", HostSignals{Guest: true}); got != DomainUnknown {
			t.Errorf("DomainOf(%q, guest) = %q, want unknown — not an OS-plane family, the linux allowlist must not admit it", r, got)
		}
	}
	// And the SAME OS-plane rule strings on a PVE NODE stay proxmox — the never-touch-host frame wins
	// (the node branch precedes the linux branch by construction; tg78-cluster/storage-01 depend on it).
	for _, r := range []string{"Service up/down", "Space on / is >= 90% and < 95% in use"} {
		if got := DomainOf(r, "dc1pve03", HostSignals{PveNode: true}); got != DomainProxmox {
			t.Errorf("DomainOf(%q, pve node) = %q, want proxmox — the node plane must win over the linux allowlist", r, got)
		}
	}
	// A non-platform alert must be UNKNOWN — never guessed into a platform, which would load a wrong skill.
	for _, r := range []string{"TargetDown", "HostDiskLow", "MeshBGPPeerDown", ""} {
		if got := DomainOf(r, "h", HostSignals{}); got != DomainUnknown {
			t.Errorf("DomainOf(%q) = %q, want unknown — a non-k8s alert must not classify to a platform", r, got)
		}
	}
	// Substring must not misfire: a rule CONTAINING "kube" mid-string is not anchored.
	if got := DomainOf("NotAKubeThing", "h", HostSignals{}); got != DomainUnknown {
		t.Errorf("DomainOf(%q) = %q — the classifier is anchored at the start, a mid-string match is a false positive", "NotAKubeThing", got)
	}
}

// The compiled k8s skill loads ONLY on a k8s incident at a non-fast class, and is absent everywhere else.
func TestK8sSkillLoadsOnlyOnKubernetesDomain(t *testing.T) {
	reg := Default()
	has := func(ctx Context) bool {
		_, loaded := reg.Compose(ctx)
		for _, n := range loaded {
			if n == "k8s-triage" {
				return true
			}
		}
		return false
	}
	if !has(Context{Phase: PhaseInvestigate, ExecClass: execclass.DeepInvestigation, Domain: DomainKubernetes}) {
		t.Error("k8s-triage did NOT load on a k8s deep investigation — the domain skill is unreachable")
	}
	// The ways it must stay silent:
	if has(Context{Phase: PhaseInvestigate, ExecClass: execclass.DeepInvestigation, Domain: DomainProxmox}) {
		t.Error("k8s-triage loaded on a PROXMOX incident — a platform skill leaked across platforms")
	}
	if has(Context{Phase: PhaseInvestigate, ExecClass: execclass.DeepInvestigation, Domain: DomainLinux}) {
		t.Error("k8s-triage loaded on a LINUX incident — a platform skill leaked across platforms")
	}
	if has(Context{Phase: PhaseInvestigate, ExecClass: execclass.DeepInvestigation, Domain: DomainStorage}) {
		t.Error("k8s-triage loaded on a STORAGE incident — a platform skill leaked across platforms")
	}
	if has(Context{Phase: PhaseInvestigate, ExecClass: execclass.DeepInvestigation, Domain: DomainUnknown}) {
		t.Error("k8s-triage loaded on an UNCLASSIFIED incident — domain must be STRICT, unknown selects nothing")
	}
	if has(Context{Phase: PhaseInvestigate, ExecClass: execclass.FastAgent, Domain: DomainKubernetes}) {
		t.Error("k8s-triage loaded on a FAST triage — a fast agent must keep the compact prompt")
	}
	// And the general skills still load on the k8s incident (the domain skill ADDS, never replaces).
	if !has(Context{Phase: PhaseInvestigate, ExecClass: execclass.DeepInvestigation, Domain: DomainKubernetes}) {
		t.Fatal("sanity: k8s incident lost its k8s skill")
	}
	_, k8sLoads := reg.Compose(Context{Phase: PhaseInvestigate, ExecClass: execclass.DeepInvestigation, Domain: DomainKubernetes})
	if !contains(k8sLoads, "debugging-protocol") {
		t.Error("the general debugging-protocol dropped out on a k8s incident — the domain skill must be additive")
	}
}

// The compiled proxmox skill loads ONLY on a Proxmox incident at a non-fast class, and is absent everywhere else
// — the same strict, additive, cross-platform-isolated contract the k8s skill carries (TG-78).
func TestProxmoxSkillLoadsOnlyOnProxmoxDomain(t *testing.T) {
	reg := Default()
	has := func(ctx Context) bool {
		_, loaded := reg.Compose(ctx)
		for _, n := range loaded {
			if n == "proxmox-triage" {
				return true
			}
		}
		return false
	}
	if !has(Context{Phase: PhaseInvestigate, ExecClass: execclass.DeepInvestigation, Domain: DomainProxmox}) {
		t.Error("proxmox-triage did NOT load on a Proxmox deep investigation — the domain skill is unreachable")
	}
	// The ways it must stay silent:
	if has(Context{Phase: PhaseInvestigate, ExecClass: execclass.DeepInvestigation, Domain: DomainKubernetes}) {
		t.Error("proxmox-triage loaded on a KUBERNETES incident — a platform skill leaked across platforms")
	}
	if has(Context{Phase: PhaseInvestigate, ExecClass: execclass.DeepInvestigation, Domain: DomainLinux}) {
		t.Error("proxmox-triage loaded on a LINUX incident — a platform skill leaked across platforms")
	}
	if has(Context{Phase: PhaseInvestigate, ExecClass: execclass.DeepInvestigation, Domain: DomainStorage}) {
		t.Error("proxmox-triage loaded on a STORAGE incident — a platform skill leaked across platforms")
	}
	if has(Context{Phase: PhaseInvestigate, ExecClass: execclass.DeepInvestigation, Domain: DomainUnknown}) {
		t.Error("proxmox-triage loaded on an UNCLASSIFIED incident — domain must be STRICT, unknown selects nothing")
	}
	if has(Context{Phase: PhaseInvestigate, ExecClass: execclass.FastAgent, Domain: DomainProxmox}) {
		t.Error("proxmox-triage loaded on a FAST triage — a fast agent must keep the compact prompt")
	}
	// The domain skill ADDS, never replaces: the general skills still load on the Proxmox incident.
	_, pveLoads := reg.Compose(Context{Phase: PhaseInvestigate, ExecClass: execclass.DeepInvestigation, Domain: DomainProxmox})
	if !contains(pveLoads, "debugging-protocol") {
		t.Error("the general debugging-protocol dropped out on a Proxmox incident — the domain skill must be additive")
	}
}

// exec-safety is the FIRST execute-phase competence (TG-36): it loads ONLY when a PhaseExecute seed is
// composed and is INVISIBLE to every investigate-phase compose. That invisibility is the eval-neutrality
// contract — the binding eval gate exercises PhaseInvestigate scenarios only, so a skill that never composes
// there cannot change any eval prompt or its PromptHash. This test IS that proof: it pins the skill silent
// across every investigate-phase (ExecClass × Domain) combination, so the "dormant build-ahead, eval-neutral"
// claim is enforced by CI, not asserted in prose.
func TestExecSafetyLoadsOnlyOnExecutePhase(t *testing.T) {
	reg := Default()
	has := func(ctx Context) bool {
		_, loaded := reg.Compose(ctx)
		return contains(loaded, "exec-safety")
	}
	// It MUST load when the execute phase is composed — otherwise the execute agent has no safety library.
	if !has(Context{Phase: PhaseExecute, ExecClass: execclass.DeepInvestigation}) {
		t.Error("exec-safety did NOT load on a PhaseExecute compose — the execute-phase competence is unreachable")
	}
	// The eval-neutrality proof: SILENT across every investigate-phase Context (every ExecClass × every
	// Domain). If any of these loaded it, the eval PromptHash would move and the dormant claim would be false.
	for _, ec := range []execclass.Class{execclass.FastAgent, execclass.StandardAgent, execclass.DeepInvestigation, ""} {
		for _, d := range []Domain{DomainUnknown, DomainKubernetes, DomainProxmox, DomainLinux, DomainStorage} {
			if has(Context{Phase: PhaseInvestigate, ExecClass: ec, Domain: d}) {
				t.Errorf("exec-safety LEAKED into a PhaseInvestigate compose (ExecClass=%q Domain=%q) — it must never touch an eval seed", ec, d)
			}
		}
	}
	// Additive, not a replacement: the always-on protocols still compose in the execute phase alongside it.
	_, execLoads := reg.Compose(Context{Phase: PhaseExecute, ExecClass: execclass.DeepInvestigation})
	if !contains(execLoads, "conservative-remediation") {
		t.Error("the always-on conservative-remediation dropped out of the execute-phase compose — exec-safety must be additive")
	}
}

// THE STORE PATH must be strict too. A graduated domain skill carries a STRING predicate, and if the
// store selector ignored Domains, a k8s skill would load on every incident — the exact leak the compiled
// predicate prevents, reintroduced through the flywheel.
func TestStorePredicateDomainIsStrictAndSkipsUnknown(t *testing.T) {
	sel := declarativeApplies(mustAW(t, []string{"kubernetes"}))
	if !sel(Context{Domain: DomainKubernetes}) {
		t.Error("a Domains:[kubernetes] store predicate did NOT match a kubernetes incident")
	}
	if sel(Context{Domain: DomainProxmox}) {
		t.Error("a Domains:[kubernetes] store predicate matched a PROXMOX incident — the store selector ignores Domains")
	}
	if sel(Context{Domain: DomainUnknown}) {
		t.Error("a Domains:[kubernetes] store predicate matched an UNKNOWN-domain incident — domain must be " +
			"STRICT in the store path, or a graduated k8s skill leaks onto every unclassified incident")
	}
	// Empty Domains = not domain-scoped: loads on any platform (the general skills' behaviour).
	if !declarativeApplies(mustAW(t, nil))(Context{Domain: DomainProxmox}) {
		t.Error("a predicate with NO Domains failed to load on a platform incident — empty must mean not-scoped")
	}
}

func mustAW(t *testing.T, domains []string) skillstore.AppliesWhen {
	t.Helper()
	return skillstore.AppliesWhen{Domains: domains}
}

// The node-plane routing (TG-78 node-plane slice): a PVE node routes to Proxmox competence on EVERY alert
// family — the same rule strings that are service-plane or stand-down territory on a guest. The estate
// signal, not the rule text, is what flips the frame.
func TestDomainOfPveNodeRoutesEveryFamily(t *testing.T) {
	for _, rule := range []string{
		"Device Down (SNMP unreachable)",       // the node itself down
		"Service up/down",                      // corosync link check on a node = cluster machinery
		"Space on / is >= 90% and < 95% in use", // node root filling — stand-down on a guest, node-plane here
		"Sensor over limit - Check Device Health Settings",
		"Port status up/down", // even a port alert on a hypervisor gets the never-touch-host frame
	} {
		if got := DomainOf(rule, "dc1pve02", HostSignals{PveNode: true}); got != DomainProxmox {
			t.Errorf("DomainOf(%q, node) = %q, want proxmox — the node identity must dominate", rule, got)
		}
		// The CONTRAST: the same rule on a plain host (neither guest nor node) stays unclassified.
		// KILLING MUTATION: route on the rule text instead of the signal → this arm reddens.
		if got := DomainOf(rule, "dc1plain01", HostSignals{}); got == DomainProxmox {
			t.Errorf("DomainOf(%q, plain host) = proxmox — node routing leaked past the estate signal", rule)
		}
	}
	// Precedence is stated, not accidental: the explicit regex table still wins over node-ness (a kube-prefixed
	// alert is Kubernetes competence wherever it fires), and an inconsistent guest+node input resolves to the
	// node branch (order in DomainOf) — the never-touch-host frame is the safe one under inconsistency.
	if got := DomainOf("KubePodNotReady", "dc1pve02", HostSignals{PveNode: true}); got != DomainKubernetes {
		t.Errorf("regex-table precedence broke: got %q", got)
	}
	if got := DomainOf("Devices up/down", "dc1pve02", HostSignals{Guest: true, PveNode: true}); got != DomainProxmox {
		t.Errorf("inconsistent guest+node input must still land proxmox via the node branch, got %q", got)
	}
}
