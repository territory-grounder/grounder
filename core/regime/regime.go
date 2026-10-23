// Package regime is the Actuation Regime Engine — the "through which effect channel?" layer of TG's
// autonomy loop (spec/017, TG-110). It sits ABOVE the spec/013 actuation interceptor and BELOW the
// operator's regime configuration, and it answers exactly one question: given an authorized, authenticated,
// mode-permitted change, which regime-aware effect LANE does the target's management regime sanction?
//
// A heterogeneous estate is not one regime. A host managed by hand takes raw SSH (native-ssh, spec/013); a
// host owned by AWX takes an idempotent, reviewed job template (awx-job); a cluster under GitOps takes a
// merge request, never a direct write (gitops-mr); and so on. Mutating an AWX- or GitOps-managed target
// DIRECTLY causes drift, revert, and split-brain — the regime engine routes each change down the lane its
// target's regime sanctions so mutation-ON is both useful AND safe across the estate.
//
// This engine COMPOSES over the already-built controls and REPLACES none of them. The policy engine
// (spec/015) answers "may TG act?" (authZ); the credential engine (spec/016) answers "with what identity?"
// (authN); the actuation interceptor (spec/013, core/actuate) is the wired chain
// (admission → never-auto floor → policy authorize → credential authenticate → mode chokepoint → execute →
// verify); the mode chokepoint (core/safety) is the sole actuation keystone. EVERY lane of this engine is an
// effect leaf that plugs INTO that interceptor beneath the same gates — it authorizes nothing, authenticates
// nothing, and lifts no floor of its own. "A lane is a channel, not a permission." Lane selection changes
// HOW an authorized change is applied, never WHETHER.
//
// Foundation slice (T-017-1 + T-017-2): the closed Regime enum, the config-not-code RegimeResolver
// (Target → Regime → Lane, one-regime-or-refuse, fail-closed on the unknown/ambiguous target), the Lane
// abstraction, and the LaneEffect composition seam that hands a selected lane's UNEXPORTED effect leaf to the
// spec/013 interceptor — the one path to any effect. The AWX-job actuator (T-017-3), the GLOBAL async-verify
// channel (T-017-4), the knowledge lane (T-017-5), persistence (T-017-6), and the console (T-017-7) land on
// this same seam in later slices; the awx-job lane is DEFINED here carrying a not-yet-wired effect leaf that
// refuses until T-017-3.
//
// Provenance: [R] paradigm-rule 8 · [O] INV-02/INV-09/INV-17/INV-21, spec/017 (TG-110). Phase 2: the
// mutating lanes stay OFF (mode Shadow) until the owner-present flip; regime resolution and the seam are
// Phase-1-safe (they select and route, they never actuate).
package regime

// Regime is the CLOSED set of management regimes a target can be governed by (REQ-1700). It is config-not-
// code: which regime a target belongs to is operator-declared DATA on the estate (resolver.go), never a
// hardcoded branch. The set is closed by construction — an unknown string is not a Regime and Valid() rejects
// it — so a typo or a poisoned rule can never invent a sixth, weaker channel.
type Regime string

const (
	// RegimeNativeSSH is the already-built spec/013 raw-SSH effect channel: a host managed by hand. It is the
	// operator-declared DEFAULT lane an unknown target may fall back to (REQ-1701).
	RegimeNativeSSH Regime = "native-ssh"
	// RegimeAWXJob is the AWX / AWX-Tower job-template channel (the first non-SSH lane, T-017-3): a host owned
	// by AWX takes an idempotent, reviewed, self-auditing job template, not a raw command.
	RegimeAWXJob Regime = "awx-job"
	// RegimeGitOpsMR is the GitOps merge-request channel: a target under GitOps takes an MR, never a direct
	// write (a later lane on this same seam).
	RegimeGitOpsMR Regime = "gitops-mr"
	// RegimeK8sDeclarative is the declarative-Kubernetes channel (a later lane on this same seam).
	RegimeK8sDeclarative Regime = "k8s-declarative"
	// RegimeAPI is the direct-API channel for targets whose regime is a first-class API (a later lane on this
	// same seam).
	RegimeAPI Regime = "api"
	// RegimeProxmox is the Proxmox VE hypervisor channel: a guest LIFECYCLE op (start / stop) runs through the
	// PVE REST API against the hypervisor, NOT through the guest's own management channel — the same guest that
	// is native-ssh for a service restart is proxmox-mediated for start/stop, so this lane is selected by the
	// op-class's effect KIND (proxmox-lifecycle), not the target host's management regime.
	RegimeProxmox Regime = "proxmox"
	// RegimeCiscoInteractive is the Cisco interactive-CLI channel (TG-85): an ASA/IOS device whose only
	// management surface is its own CLI over SSH — no API, no declarative source. Its effect leaf is the
	// modules/actuation/cisco WriteModule (typed config lines behind an operator prefix allowlist + the
	// named reversible-op path), constructed and injected by the worker ONLY when the operator declares
	// devices AND arms the lane; until then the lane carries the fail-closed pending leaf.
	RegimeCiscoInteractive Regime = "cisco-interactive"
)

// returnsHandleNotOutcome reports whether a regime's leaf answers with a JOB HANDLE rather than a completed
// effect — the job is queued, the estate is untouched at return, and success or failure lands minutes later.
//
// It is deliberately NARROWER than the neighbouring AsyncObservable (asyncverify.go), and the two must not be
// confused. AsyncObservable is `Valid() && != native-ssh`, a REQ-1709 classification that counts PROXMOX as
// async — but the Proxmox leaf polls its task to completion (pollTask) before returning, so its effect HAS
// landed and its post-state IS immediately observable. Gating the synchronous path on AsyncObservable would
// therefore refuse every start-guest heal: the one op-class at LevelAuto and the whole actuation evidence
// base. This predicate answers the only question the dispatch actually needs — did the effect happen before
// the call returned?
//
// This exists to keep an async lane OFF the synchronous verify path. Driven synchronously, a launch handle is
// adjudicated as a finished mutation: the leaf returns exit 0, the verifier observes an estate the job has not
// touched yet, scores `match`, and `match` is the only promoting outcome on the graduation ladder — so a
// mutating op-class would climb toward AUTO on launches that later FAILED, and nothing would ever read the
// terminal job status. The deferred-verify channel (asyncverify.go) exists precisely to prevent this and
// currently has no producer, so the honest posture is to refuse rather than to adjudicate blind.
//
// GITOPS-MR joins here (TG-122): opening a merge request returns an MR handle, not a completed effect — the
// estate is untouched until a human merges and Atlantis/Argo reconciles, minutes-to-days later. So it is
// refused on the synchronous path exactly like awx-job until the deferred-verify producer is wired; injecting a
// gitops-mr actuator does not arm it.
func returnsHandleNotOutcome(r Regime) bool { return r == RegimeAWXJob || r == RegimeGitOpsMR }

// LaunchReturnsHandle is the EXPORTED read of returnsHandleNotOutcome for the runner's deferred-verify
// PRODUCER (TG-122 slice 0): the dispatch that must Reserve a pending-verification BEFORE launching such a
// lane keys on this exact predicate — NOT on the broader AsyncObservable, which counts proxmox (whose leaf
// polls its task to completion, so its effect is synchronously adjudicable and must never be deferred).
func LaunchReturnsHandle(r Regime) bool { return returnsHandleNotOutcome(r) }

// allRegimes is the closed, ordered enumeration. It is the single source of truth for Valid and AllRegimes;
// there is no other place a regime string is minted.
var allRegimes = []Regime{
	RegimeNativeSSH,
	RegimeAWXJob,
	RegimeGitOpsMR,
	RegimeK8sDeclarative,
	RegimeAPI,
	RegimeProxmox,
	RegimeCiscoInteractive,
}

// AllRegimes returns a copy of the closed regime set (stable order). Callers (the resolver's config
// validation, the console lane-coverage view) enumerate the sanctioned regimes without being able to mutate
// the canonical set.
func AllRegimes() []Regime {
	out := make([]Regime, len(allRegimes))
	copy(out, allRegimes)
	return out
}

// Valid reports whether r is one of the sanctioned regimes. An empty or unknown string is NOT valid —
// fail closed by construction: a rule that names an unknown regime is rejected at config time rather than
// silently routing a target down an undefined channel (REQ-1700).
func (r Regime) Valid() bool {
	switch r {
	case RegimeNativeSSH, RegimeAWXJob, RegimeGitOpsMR, RegimeK8sDeclarative, RegimeAPI, RegimeProxmox, RegimeCiscoInteractive:
		return true
	default:
		return false
	}
}

// String renders the regime as its stable slug (the value the operator declares in config and the ledger
// records). It never guesses: an invalid regime renders its raw (possibly empty) string, and Valid gates use.
func (r Regime) String() string { return string(r) }
