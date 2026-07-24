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

// Context is the set of typed signals that deterministically select which skills load. It carries only
// cheap, pre-decided facts — never anything the model produced.
type Context struct {
	Phase     Phase
	ExecClass execclass.Class
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

	return NewRegistry([]Skill{
		{
			Name: "proving-your-work", Version: "1.1.0", AppliesWhen: always,
			Body: `## Proving your work
Do not assert — GROUND every claim in an OBSERVATION you gathered, and cite its bracketed id in evidence_ids.
State only what a tool actually returned (a service is "active" because get-device-status reported it, not
"running fine"). If you could not gather the evidence a claim needs, do not make the claim: lower your
confidence and stop or escalate instead. A proposal or stop that cites no evidence_ids is an ungrounded guess —
it is down-scored and sent for human review. Before you emit a proposal, confirm every load-bearing fact in
your rationale maps to an OBSERVATION id you are citing.`,
		},
		{
			Name: "loop-red-flags", Version: "1.1.0", AppliesWhen: always,
			Body: `## Stop if your investigation stalls
Emit action:stop with a reason (or escalate) when gathering more evidence will not help: the same tool keeps
returning the same unhelpful result; you have no read-only tool to inspect the resource the alert names; the
observations are contradictory and you cannot resolve them; or you have already gathered what a conclusion
needs. Do not keep calling tools past the point of new signal. An honest escalation that names what is missing
beats a confident conclusion built on thin evidence.`,
		},
		{
			Name: "debugging-protocol", Version: "1.1.0", AppliesWhen: full,
			Body: `## Investigation protocol (read-only; do not shotgun guesses)
1. Observe — call the read-only tools to capture the device's current state and the events around the alert;
   the newest event before the alert is usually the cause.
2. Localize — from the observations, is the fault this host, a dependency, the network path, or a stale/planned
   condition? Commit to one answer before proposing.
3. Single cause — attribute the alert to the ONE most-likely cause the observations support; never bundle
   several guesses into one proposal.
4. Reversible fix or stop — propose the single most conservative reversible action for that cause, or stop if
   none is safe. Name the exact observation that will change when the fix works — that is your prediction.`,
		},
		{
			Name: "shortcuts-to-resist", Version: "1.1.0", AppliesWhen: full,
			Body: `## Shortcuts to resist
- "This is obviously safe, skip the approval" — the risk band is machine-enforced; an irreversible or stateful action is ALWAYS a human poll, never AUTO.
- "The past-incident retrieval is canonical" — precedent is high-signal, not ground truth; confirm it against this incident's live observations before relying on it.
- "This looks like the last alert" — verify against THIS incident's observations; never carry a prior conclusion over unchecked.
- "Propose the permanent fix too" — surface at most ONE conservative reversible action; note any deeper follow-up in the rationale, never as a second action.
- "The alert looks duplicate, suppress it" — dedup belongs in the receiver, not the session; silence hides drift.`,
		},
		{
			Name: "conservative-remediation", Version: "1.1.0", AppliesWhen: always,
			Body: `## Conservative-remediation catalog (what to propose)
When the observations point to one of these causes, propose the single most conservative REVERSIBLE fix, and in
your rationale state the PREDICTION — the exact observation that will change when the fix works (this is the
falsifiable check the verifier applies):
  Disk filling      -> prune images/builder, fstrim, vacuum journals   (predict: df shows free space rose)
  Memory pressure   -> restart the unit / rollout-restart the deploy    (predict: the unit is active, memory recovered)
  Pod crashloop     -> rollout-restart or delete the pod (parent reschedules) (predict: Running, restarts stable)
  Stuck service     -> restart the unit / compose service              (predict: is-active / health 200)
  Stale lock        -> remove the lock ONLY after confirming the holder PID is dead (predict: the blocked operation proceeds)
  Cert expiry       -> ACME/certbot renew                              (predict: notAfter is in the future)
  Capacity dip      -> scale the deploy UP (replicas >= 1)             (predict: desired == ready)
  Wedged guest VM/CT-> graceful reboot of ONE guest (qm/pct reboot)    (predict: status running / pings resume)
HARD FLOOR — never propose these as an auto/reversible action (they require a human poll):
  - stateful workloads (etcd, *postgres*, *mysql*, *-db, redis, prometheus, seaweedfs, thanos): restart/scale can lose data.
  - host reboot / shutdown, P0-host power-cycle; qm reset/stop/destroy; a guest reboot co-occurring with a host reboot.
  - anything irreversible: delete pvc/pv/namespace/secret, mkfs, zpool destroy, dropdb, terraform destroy, credential revoke.
If both a transient and a permanent fix exist, propose the transient one; mention the permanent one in the rationale, never as a second action.`,
		},
		{
			// Competence (#25a): the investigation choreography, grounded in TG's ACTUAL read-only tools
			// (get-device-status / get-device-eventlog / get-active-alerts) + the estate cascade graph.
			// Shape distilled from the HolmesGPT/prometheus-operator runbook convention (Meta -> evidence ->
			// decision -> verification), re-authored for TG's network estate (no upstream skill content exists
			// for SNMP/interface/host alert classes — this is TG's own competence).
			Name: "triage-protocol", Version: "1.1.0", AppliesWhen: full,
			Body: `## Triage protocol
Always: 1) get-device-status <host>. 2) get-device-eventlog <host> — the newest event before the alert is
usually the cause. 3) If a cascade is plausible: get-estate-context <host>, then get-active-alerts on each
named upstream/sibling; several related hosts failing together = propose against the shared upstream cause,
not this symptom.
Conclude, citing OBSERVATION ids: DISABLED device or evidence contradicting the alert (device up, no adverse
events) = stale/planned, STOP with your reason + evidence — that IS the correct outcome. Cause unclear or
topology unknown = escalate, say what is missing. Proposing = name the exact observation that will change
when the fix works.`,
		},
		{
			Name: "alert-class-playbooks", Version: "1.1.0", AppliesWhen: full,
			Body: `## Alert-class playbooks (all fixes are advisory proposals; mutation is OFF)
DISABLED device or evidence-contradicted (stale) alert -> no action: stop with your cited reason.
Guest down, its PVE host up -> graceful reboot of that ONE guest. Multiple guests of one host down -> the
host is the fault, escalate.
Latency correlated across a path -> upstream link problem, never restart an endpoint; escalate.
Port admin-down = intentional; rising error counters = physical (SFP/cable) -> escalate, do not bounce.
Disk filling -> prune/trim/vacuum (verify free rose). Memory -> restart the offending unit, but NEVER a
stateful DB/etcd/redis/prometheus unit -> human poll. Sustained load -> escalate for capacity.
Flapping = link/power/thermal, a restart will NOT fix it -> escalate. Environmental (temp/PSU/fan) -> escalate.`,
		},
	})
}
