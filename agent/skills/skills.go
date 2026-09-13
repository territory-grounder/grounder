// Package skills is the agent's behavioral-competence library: the step-by-step procedures the model runs
// WITHIN the guardrails, composed deterministically into the session seed. TG faithfully ported the
// predecessor's ENFORCEMENT (risk bands, the never-auto floor, the read-only tool topology, the wired
// interceptor) but not its COMPETENCE — the predecessor's `.claude/skills` choreography (the Debugging
// Protocol, Proving-Your-Work, Shortcuts-to-Resist, the conservative situation→action catalog). Before this,
// TG's agent seed was one line ("investigate read-only and propose"); this makes the behavior explicit,
// typed, and testable.
//
// Provenance: [F] the predecessor `.claude/skills/{chatops-workflow,conservative-remediation}` prose and its
// pure-rule composition (lib/team_formation.py — a deterministic roster engine, NO LLM in the selector),
// re-expressed as a typed registry. Improvement (external-audit #4, the phase-aware prompt compiler): skills
// are selected by execution class + phase, so a FAST_AGENT gets a compact set and a DEEP_INVESTIGATION the
// full protocols — keeping irrelevant instruction out of the prompt. INV-08: the selector is a pure function
// of typed signals; no model token chooses which skills load.
package skills

import (
	"regexp"
	"sort"
	"strings"

	"github.com/territory-grounder/grounder/core/execclass"
)

// Phase is the coarse lifecycle phase a seed is being composed for.
type Phase string

const (
	// PhaseInvestigate is the read-only fact-gathering + propose phase (the only phase wired in Phase 0/1).
	PhaseInvestigate Phase = "investigate"
	// PhaseExecute is the autonomous remediation phase (Phase 2; mutation OFF until then).
	PhaseExecute Phase = "execute"
)

// Domain is the estate platform an incident belongs to — a TYPED signal derived from the alert, so a
// platform-specific competence (k8s, proxmox, …) can be selected deterministically (INV-08) rather than
// loaded on every incident. It is DERIVED from the alert rule and host by DomainOf, never chosen by the
// model, and DomainUnknown is the honest zero: a platform skill must NOT load on an unclassified incident.
type Domain string

const (
	// DomainUnknown is the zero value: the alert did not classify to a known platform. A domain-scoped
	// skill selects on a SPECIFIC domain, so it never loads here — the safe default, never a catch-all.
	DomainUnknown Domain = ""
	// DomainKubernetes covers Kube*/Cilium*/k8s node+pod+PV alerts.
	DomainKubernetes Domain = "kubernetes"
	// DomainProxmox covers the pve-liveness / hypervisor lane.
	DomainProxmox Domain = "proxmox"
	// DomainCisco covers the Cisco ASA/IOS estate (TG-85). ONLY unambiguous Cisco alerts map here — the
	// ASA* rule family names the firewall in its own rule prefix. The Mesh(BGP|BFD|IPsec) routing alerts
	// are DELIBERATELY unclassified: they may be Cisco IOS or a non-Cisco fabric, and a cisco skill on a
	// non-cisco incident is exactly the domain noise the strict signal exists to prevent.
	DomainCisco Domain = "cisco"
	// DomainStorage covers a dedicated STORAGE APPLIANCE (a Synology DSM NAS — TypeStorageAppliance in
	// the estate, stamped from os=dsm): RAID/volume/disk-state triage where the never-reboot-to-clear
	// floor binds and no unit verb applies (TG-78 storage slice). Routed by estate identity, never by
	// rule string — the appliance's alerts are the same generic SNMP families guests fire.
	DomainStorage Domain = "storage"
	// DomainLinux covers the OS/SERVICE plane of a plain Linux guest (TG-78 linux slice — this estate's
	// Debian LXC + Ubuntu VM population): the service-up/down, disk-space, memory, and CPU rule families
	// on a host that is a virtualization guest and not a hypervisor node. The guest's DEVICE-down stays
	// DomainProxmox (a lifecycle fault), and the same rule strings on a PVE node stay DomainProxmox
	// (node-plane); this domain is the service-plane remainder those branches deliberately left.
	DomainLinux Domain = "linux"
)

// domainRuleRE maps an alert-rule prefix to a domain. First match wins; anchored at the start so a
// substring cannot misfire. Deliberately keyed on the ALERT RULE, not on labels["category"] — the estate
// uses that label for its own subsystem taxonomy and it collides with TG's reading (TG-405), so a
// label-based classifier would misroute; the rule name is the provider's own stable identity.
//
// The Proxmox lane is NOT here: a `^pve-liveness` rule was tried, but the only pve-liveness source
// deliberately stamps alert_rule="Device-Down" so a liveness incident classifies IDENTICALLY to a push-sourced
// one (modules/ingest/pveliveness — "no skill/prompt drift between the two intakes"), so that rule matched
// nothing in production and the skill was dead-on-arrival. Proxmox competence is a GUEST-LIFECYCLE plane and is
// routed by (host-is-a-guest AND the alert says the device/host is down) below — intake-independent, so both a
// push Device-Down and a liveness Device-Down of the same guest classify the same (TG-78).
var domainRuleRE = []struct {
	re     *regexp.Regexp
	domain Domain
}{
	{regexp.MustCompile(`(?i)^(kube|cilium)`), DomainKubernetes},
	{regexp.MustCompile(`(?i)^asa`), DomainCisco},
}

// hostDownRE matches the DEVICE/HOST-DOWN alert-rule family — the guest itself unreachable — as opposed to a
// service/metric alert WITHIN a host. It is the second half of the Proxmox trigger: a service-down that
// happens to run on a guest is a service-plane fault, not a guest-lifecycle one, so only a device/host-down on
// a guest routes to Proxmox competence. Covers the LibreNMS push spellings ("Devices up/down", "Device Down
// (SNMP unreachable)"), the cascade names (HostDown/DeviceDown), and the pve-liveness source's "Device-Down".
var hostDownRE = regexp.MustCompile(`(?i)(device|host)s?[ _-]?(up/?down|down)|devices? up/down`)

// linuxRuleRE matches the OS/SERVICE-plane rule families of a plain Linux guest — the DomainLinux
// allowlist (TG-78 linux slice). Anchored at the start like the domain table, and spelled with
// flexible separators because the SAME rule crosses TG in two dialects: the provider's display form
// ("Service up/down", "Space on / is >= 90% and < 95% in use") and the ingest-slugified form
// ("Service-up/down", "Space-on-/-is-90-and-95-in-use") — both must route identically or the eval
// corpus and the live estate would grade different competence. The four families are the measured
// vocabulary (490 service + 396 disk sessions in 90 days; memory/CPU are the thin tail): a POSITIVE
// list, so device-plane events, hardware sensors, and network-plane rules never match.
var linuxRuleRE = regexp.MustCompile(`(?i)^(service[ _-]?up/?down|space[ _-]?on[ _-]?/|linux[ _-]?high[ _-]?memory|processor[ _-]?usage)`)

// HostSignals is the estate-derived identity of an incident's host — the typed, authoritative inventory
// facts DomainOf routes on (each from its Graph accessor; all false when the estate is unseeded, which
// fail-closes to rule-only routing). Named fields, not positional bools: every call site reads as what
// it asserts, and a new identity class extends the struct without re-rippling every caller.
type HostSignals struct {
	Guest            bool // estate.Graph.IsGuest — a virtualization guest (runs_on a hypervisor)
	PveNode          bool // estate.Graph.IsPveNode — a Proxmox hypervisor node
	NetworkDevice    bool // estate.Graph.IsNetworkDevice — switch/router/AP/firewall (os: ios/iosxe/ciscosb/asa)
	StorageAppliance bool // estate.Graph.IsStorageAppliance — a Synology DSM NAS (os: dsm)
}

// DomainOf classifies an incident to its platform from typed alert fields. Pure, no I/O, no model input —
// the same discipline every other skill selector uses (INV-08). HostSignals are typed inputs the caller
// derives from the estate graph (estate.Graph.IsGuest / IsPveNode / IsNetworkDevice / IsStorageAppliance —
// authoritative inventory facts), NOT estate queries inside this function, so the classifier stays pure.
// An unmatched incident is DomainUnknown, which no domain-scoped skill selects on.
//
// HostSignals replaced the growing positional bool list when the device-identity slots landed (TG-78
// network+storage slices): four same-typed positional flags invite a silent swap that still compiles and
// still routes — just to the wrong competence. Named fields make every call site read as what it asserts.
func DomainOf(alertRule, host string, sig HostSignals) Domain {
	r := strings.TrimSpace(alertRule)
	for _, m := range domainRuleRE {
		if m.re.MatchString(r) {
			return m.domain
		}
	}
	// A PVE NODE routes to Proxmox competence on EVERY alert family, not just host-down (TG-78 node-plane
	// slice): the host's identity as a hypervisor dominates — a "Space on /" or a corosync service-down on
	// a node is a node/storage/cluster-plane concern whose fix shape and never-touch-host floor the skill
	// carries, where the same rule string on a guest is ordinary service-plane triage. This is checked
	// BEFORE the guest branch: after the estate-side hardening a node is never also a guest, but the order
	// states the precedence rather than leaning on that invariant.
	if sig.PveNode {
		return DomainProxmox
	}
	// A NETWORK DEVICE (switch/router/AP/firewall — TypeNetworkDevice, stamped from the LibreNMS os) routes
	// to the cisco pack's network competence on EVERY alert family (TG-78 network slice): there is no honest
	// rule prefix — the measured vocabulary is generic SNMP rules fired by guests and PVE nodes too (a
	// ^Port regex would steal dc2pve01 from the node plane and ten Linux guests) — so the device's
	// estate identity is the only signal that steals nothing. Same identity-dominates-symptom shape as the
	// node branch above; the shipped cisco competence is already ASA/IOS-scoped.
	if sig.NetworkDevice {
		return DomainCisco
	}
	// A STORAGE APPLIANCE (a Synology DSM NAS — TypeStorageAppliance, os=dsm) routes to storage competence
	// on every family (TG-78 storage slice): a sensor/space/port alert on the NAS is an appliance-plane
	// concern (RAID/volume/disk state, never-reboot-to-clear) whatever the rule string says. Checked before
	// the guest branches — an appliance is never a guest, but the order states the precedence.
	if sig.StorageAppliance {
		return DomainStorage
	}
	// Proxmox = a GUEST-LIFECYCLE fault: the host is a virtualization guest AND the alert says the device/host
	// itself is down (the guest unreachable). Intake-independent — both a push Device-Down and a pve-liveness
	// Device-Down of the same guest land here — so no skill/prompt drift between the two intakes (TG-78).
	if sig.Guest && hostDownRE.MatchString(r) {
		return DomainProxmox
	}
	// Linux = an OS/SERVICE-plane fault on a plain guest (TG-78 linux slice): the host is a guest (a PVE
	// node took the branch above — the never-touch-host frame must win for the same rule strings) and the
	// rule is one of the OS-plane families. A POSITIVE allowlist, deliberately NOT a fall-through:
	// device-plane events (Device rebooted), hardware/environmental sensors, and network-plane rules stay
	// DomainUnknown — handing those systemd competence is exactly the misroute the strict domain signal
	// exists to prevent (the storage-pack lesson). This CONSCIOUSLY REVERSES the pre-slice posture that
	// pinned these families to DomainUnknown to keep proxmox-triage off them: they now load linux-triage,
	// the competence actually scoped to them.
	if sig.Guest && linuxRuleRE.MatchString(r) {
		return DomainLinux
	}
	return DomainUnknown
}

// Context is the set of typed signals that deterministically select which skills load. It carries only
// cheap, pre-decided facts — never anything the model produced.
type Context struct {
	Phase     Phase
	ExecClass execclass.Class
	// Domain is the incident's platform (DomainOf), so a platform-specific competence loads only on that
	// platform. Zero value DomainUnknown selects no domain-scoped skill — a Cisco/k8s procedure on an
	// unrelated incident is noise, and INV-08 forbids the model choosing to suppress it.
	Domain Domain
}

// Skill is one composable behavioral procedure. AppliesWhen is a PURE predicate over the Context (no LLM,
// no I/O); Body is the guidance injected verbatim into the seed.
type Skill struct {
	Name        string
	Version     string
	AppliesWhen func(Context) bool
	Body        string
}

// Registry is an ordered set of skills composed deterministically per Context.
type Registry struct {
	skills []Skill
}

// NewRegistry builds a registry from an ordered skill list.
func NewRegistry(skills []Skill) *Registry { return &Registry{skills: skills} }

// Compose returns the concatenated bodies of every applicable skill (in registry order) plus the ordered
// list of loaded skill names — a stable, deterministic function of the Context. The names support a
// `skill_load` audit record so the composed behavior is reconstructable.
func (r *Registry) Compose(ctx Context) (body string, loaded []string) {
	var parts []string
	for _, s := range r.skills {
		if s.AppliesWhen != nil && s.AppliesWhen(ctx) {
			parts = append(parts, s.Body)
			loaded = append(loaded, s.Name)
		}
	}
	return strings.Join(parts, "\n\n"), loaded
}

// All returns a copy of the ordered skill list (the store composer + boot importer read it; the copy
// keeps the registry immutable from outside).
func (r *Registry) All() []Skill {
	out := make([]Skill, len(r.skills))
	copy(out, r.skills)
	return out
}

// Names returns every registered skill name, sorted (for tests / introspection).
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.skills))
	for _, s := range r.skills {
		out = append(out, s.Name)
	}
	sort.Strings(out)
	return out
}

// deepOrStandard is true for the classes that warrant the full behavioral protocols. An EMPTY class
// (no classification happened) deliberately gets the FULL set: composing a seed for an unclassified
// incident must fail toward more behavioral guidance, never toward less — only an explicit FAST_AGENT
// classification earns the compact prompt.
func deepOrStandard(c execclass.Class) bool {
	return c == execclass.StandardAgent || c == execclass.DeepInvestigation || c == ""
}

// Default is the built-in skill library, faithfully ported from the predecessor's chatops-workflow +
// conservative-remediation skills, selected by execution class + phase.
func Default() *Registry {
	always := func(Context) bool { return true }
	full := func(ctx Context) bool { return deepOrStandard(ctx.ExecClass) }
	// k8sDeep selects the Kubernetes competence ONLY on a k8s incident AND a non-fast investigation — the
	// same phase gate the full protocols use, so a FAST_AGENT triage stays compact. DomainUnknown never
	// matches: the skill is invisible on every non-k8s incident, which is the whole point of the domain
	// signal (INV-08 — a pure predicate over typed facts).
	k8sDeep := func(ctx Context) bool { return ctx.Domain == DomainKubernetes && deepOrStandard(ctx.ExecClass) }
	// proxmoxDeep selects the Proxmox competence ONLY on a Proxmox incident AND a non-fast investigation — the
	// same strict domain + phase gate as k8sDeep, so a FAST_AGENT triage stays compact and the skill is invisible
	// on every non-Proxmox incident (DomainUnknown never matches).
	proxmoxDeep := func(ctx Context) bool { return ctx.Domain == DomainProxmox && deepOrStandard(ctx.ExecClass) }
	// ciscoDeep selects the Cisco competence ONLY on a Cisco-domain incident AND a non-fast investigation —
	// the same strict domain + phase gate as k8sDeep/proxmoxDeep, so a FAST_AGENT triage stays compact and
	// the skill is invisible on every non-Cisco incident (DomainUnknown never matches).
	ciscoDeep := func(ctx Context) bool { return ctx.Domain == DomainCisco && deepOrStandard(ctx.ExecClass) }
	// linuxDeep selects the Linux guest OS competence ONLY on a Linux-domain incident AND a non-fast
	// investigation — the same strict domain + phase gate as the other three, so a FAST_AGENT triage stays
	// compact and the skill is invisible on every non-Linux incident (DomainUnknown never matches).
	linuxDeep := func(ctx Context) bool { return ctx.Domain == DomainLinux && deepOrStandard(ctx.ExecClass) }
	// storageDeep selects the storage-appliance competence ONLY on a Storage-domain incident AND a non-fast
	// investigation — the same strict domain + phase gate as the other four (DomainUnknown never matches).
	storageDeep := func(ctx Context) bool { return ctx.Domain == DomainStorage && deepOrStandard(ctx.ExecClass) }
	// executePhase selects the execute-phase safety competence ONLY when composing a PhaseExecute seed — the
	// autonomous remediation phase (Phase 2). It is the FIRST and only execute-phase skill; every other skill
	// selects on PhaseInvestigate or `always` (which, being unconditional, also composes in execute). Because
	// only PhaseInvestigate is wired today (mutation OFF until the Phase-2 flip), this predicate is FALSE for
	// every current seed compose AND for every eval scenario, so the skill is dormant build-ahead: it changes
	// no investigate-phase prompt and no eval PromptHash. It arms itself when PhaseExecute is first composed —
	// no separate flag, exactly the INV-08 discipline (a pure predicate over a typed signal, not a model choice).
	executePhase := func(ctx Context) bool { return ctx.Phase == PhaseExecute }
	// deepInvestigation selects the correlated-incident competence ONLY on an INVESTIGATE-phase compose whose
	// incident the correlation stage routed to the DEEP class (execclass.DeepInvestigation — Correlated OR
	// Novel: the shortcut was refused). It gates on PhaseInvestigate because this is TRIAGE guidance — a cascade
	// is reasoned about while investigating, never while executing an already-approved fix — so it never joins
	// an execute-phase seed. A FAST_AGENT or a plain StandardAgent incident never matches either, so the cascade
	// choreography is invisible on the single-system happy path and composes only where a cluster's shared-cause
	// reasoning is warranted.
	deepInvestigation := func(ctx Context) bool {
		return ctx.Phase == PhaseInvestigate && ctx.ExecClass == execclass.DeepInvestigation
	}

	return NewRegistry([]Skill{
		{
			Name: "proving-your-work", Version: "1.2.0", AppliesWhen: always,
			Body: seedBody("proving-your-work"),
		},
		{
			Name: "loop-red-flags", Version: "1.1.0", AppliesWhen: always,
			Body: seedBody("loop-red-flags"),
		},
		{
			Name: "debugging-protocol", Version: "1.2.0", AppliesWhen: full,
			Body: seedBody("debugging-protocol"),
		},
		{
			Name: "shortcuts-to-resist", Version: "1.1.0", AppliesWhen: full,
			Body: seedBody("shortcuts-to-resist"),
		},
		{
			// k8s-triage (TG-36): the FIRST domain-scoped skill, selected on Context.Domain == Kubernetes.
			// GROUNDED, not invented: every rule below is from docs/K8S-MANAGEMENT-REGIME-DESIGN.md and the
			// estate's strict-GitOps posture (feedback_k8s_strict_gitops.md, k8s-diagnostician.md), not from
			// the model's priors. It only sharpens diagnosis + proposal SHAPE for k8s incidents; it grants no
			// capability and the never-auto floor + interceptor chokepoint are unchanged.
			Name: "k8s-triage", Version: "1.1.0", AppliesWhen: k8sDeep,
			Body: seedBody("k8s-triage"),
		},
		{
			// proxmox-triage (TG-78): the PVE platform competence, selected on Context.Domain == Proxmox.
			// GROUNDED in universal PVE architecture (guest / node / storage / cluster planes) and this estate's
			// posture — the never-auto floor (no host/node ops from triage) and the reversible allowlist (reboot
			// ONE guest whose node + storage are healthy) — not the model's priors. Like k8s-triage it only
			// sharpens WHICH fault and the proposal SHAPE; it grants no capability and the never-auto floor +
			// interceptor chokepoint are unchanged.
			Name: "proxmox-triage", Version: "1.0.0", AppliesWhen: proxmoxDeep,
			Body: seedBody("proxmox-triage"),
		},
		{
			// cisco-triage (TG-85): the ASA/IOS platform competence, selected on Context.Domain == Cisco.
			// GROUNDED in the predecessor's battle-tested ASA doctrine (the show-ladder ordering, the
			// ACE-below-a-deny trap, shun's config-diff invisibility, the crypto never-auto floor and the
			// infra never-shun set) — not the model's priors. Ported from the pre-TG-471 branch body onto
			// the seeds/ embed; the one content change: the proposal-shape preamble now states the write
			// lane ships DARK until armed (the transport exists since the TG-85 slices merged) instead of
			// the stale "no actuation transport exists yet". Like the other domain skills it only sharpens
			// WHICH fault and the proposal SHAPE; it grants no capability and the never-auto floor +
			// interceptor chokepoint are unchanged.
			Name: "cisco-triage", Version: "1.1.0", AppliesWhen: ciscoDeep,
			Body: seedBody("cisco-triage"),
		},
		{
			// linux-triage (TG-78 linux slice, owner-commissioned 2026-08-30): the Debian/Ubuntu guest OS
			// competence, selected on Context.Domain == Linux — the service/disk/memory/CPU families on
			// plain guests that the proxmox (lifecycle/node) and k8s lanes deliberately left unclassified.
			// GROUNDED in the estate's measured history, not priors: the loopback-rootfs disk verdict is
			// core/diagcorpus expectations + the opcover disk-grow exemption; the self-healing service
			// discrimination is the nc01/nc02 61-stand-down record; the unit-state/OOM/deleted-file
			// doctrine is standard systemd semantics the always-on playbooks do NOT carry (this seed adds
			// what they cannot: state-word discrimination, log-plane attribution, the restart-loop trap —
			// it deliberately does not restate their disk-grow refusal or tool lists). Like every domain
			// skill it sharpens WHICH fault and the proposal SHAPE; it grants no capability and the
			// never-auto floor + interceptor chokepoint are unchanged.
			Name: "linux-triage", Version: "1.0.0", AppliesWhen: linuxDeep,
			Body: seedBody("linux-triage"),
		},
		{
			// storage-triage (TG-78 storage slice, owner-commissioned 2026-08-30): the storage-appliance
			// competence, selected on Context.Domain == Storage — the DSM NAS lane the estate-identity
			// routing carved (17 measured appliance incidents; the corpus once taught propose on the
			// sensor-health family — proposed:shadow 2026-08-04 — and this skill grades the correction).
			// GROUNDED in the storage-alert-triage runbook doctrine (trajectory-not-threshold,
			// never-reboot-to-clear, name-the-failed-member) + the get-device-storage-health read that
			// un-gated it (per-volume capacity vs its own warn threshold; state sensors baseline 1 on
			// these MIBs). Escalate-heavy BY DESIGN: the estate declares no storage op-class, so the
			// deliverable is the named member + trajectory + blast radius, never a verb. Grants no
			// capability; the never-auto floor + interceptor chokepoint are unchanged.
			Name: "storage-triage", Version: "1.0.0", AppliesWhen: storageDeep,
			Body: seedBody("storage-triage"),
		},
		{
			// v1.3.0 (2026-07-31, spec/026 REQ-2601/REQ-2609 — the open proposal plane): v1.2 said STOP for
			// every cause with no registry slug, which made this catalog the measured "stand-down generator"
			// (100% stand-down on action-warranted incidents with no registered class). v1.3 converts that
			// branch into the FREE-FORM PROPOSE DUTY: when observations confirm an action-warranted cause and
			// no registered op-class addresses it, the agent still NAMES the addressing fix with a free-form
			// op_class slug — recorded as a shadow proposal, structurally unexecutable (nil argv, every effect
			// leaf refuses, never-auto floor; spec/026 REQ-2608). Substitution stays banned; the hard floor is
			// untouched; actor-evidence never suppresses the proposal (owner ruling 2026-07-31, fault 1406).
			Name: "conservative-remediation", Version: "1.3.0", AppliesWhen: always,
			Body: seedBody("conservative-remediation"),
		},
		{
			// Competence (#25a): the investigation choreography, grounded in TG's ACTUAL read-only tools
			// (get-device-status / get-device-eventlog / get-active-alerts) + the estate cascade graph.
			// Shape distilled from the HolmesGPT/prometheus-operator runbook convention (Meta -> evidence ->
			// decision -> verification), re-authored for TG's network estate (no upstream skill content exists
			// for SNMP/interface/host alert classes — this is TG's own competence).
			Name: "triage-protocol", Version: "1.3.0", AppliesWhen: full,
			Body: seedBody("triage-protocol"),
		},
		{
			Name: "alert-class-playbooks", Version: "1.3.0", AppliesWhen: full,
			Body: seedBody("alert-class-playbooks"),
		},
		{
			// correlated-triage (TG-36): the DEEP-investigation competence — how to reduce a correlated cluster
			// (multi-host, or cross-source) to its ONE causal subject instead of answering each symptom, and when a
			// cross-source pair is a COMPROMISE hypothesis to escalate rather than remediate. GROUNDED in TG's OWN
			// correlator (core/correlate: the multi-host + cross-source rules, the TG-376 causal collapse) and
			// estate graph (blast-radius in-degree / runs_on election), not the model's priors. Selected on
			// execclass.DeepInvestigation only, so it composes into no FAST/StandardAgent seed; it grants no
			// capability and the never-auto floor + interceptor chokepoint are unchanged — read-only guidance that
			// sharpens WHICH node and the proposal SHAPE for a cascade.
			Name: "correlated-triage", Version: "1.0.0", AppliesWhen: deepInvestigation,
			Body: seedBody("correlated-triage"),
		},
		{
			// exec-safety (TG-36): the FIRST execute-phase competence — the safety choreography for the
			// autonomous remediation phase (Phase 2). GROUNDED in TG's OWN model, not invented: the mode
			// chokepoint (fail-closed to Shadow), the conservative-remediation HARD FLOOR (stateful /
			// host-power / irreversible stay human-poll even after approval), the recorded-inverse-before-seal
			// rule (TG-82), the pre-state re-verify (TG-58), and the falsifiable-prediction verify the scorer
			// already applies. It grants NO capability and changes NO enforcement — the interceptor chokepoint,
			// never-auto floor, and effect-leaf refusals are the controls; this is read-only guidance the model
			// consults. Selected on PhaseExecute only, so it composes into no investigate-phase seed and no eval
			// scenario (dormant until the flip) — a behavioral library that must be READY when execute arms,
			// not authored under time pressure at the flip.
			Name: "exec-safety", Version: "1.0.0", AppliesWhen: executePhase,
			Body: seedBody("exec-safety"),
		},
	})
}
