// plane_split.go — the read/actuation credential-plane split (spec/022 REQ-2203, TG-157/TG-153 High#3).
//
// REQ-2203: the read-only triage plane SHALL NOT co-hold the actuation SSH identity or any mutation
// write-token; a compromise of the triage plane SHALL NOT yield an actuation-capable credential, so the read
// and mutate blast-radii are disjoint. The primary enforcement is at the substrate: the triage worker and the
// actuation worker run under DISTINCT OpenBao AppRoles whose policies do not overlap (design.md §3). This file
// is the in-process defense-in-depth beneath that: it classifies the credential REFERENCES the process holds
// into the two planes and asserts, at boot, that no reference appears in both — so a configuration mistake that
// pointed a triage source at the actuation key (or handed the triage token the actuation write scope) fails
// closed at startup rather than silently collapsing the split. References are safe to name (INV-13: they are
// not the secret), so a violation is reported by ref, never by value.
//
// ---------------------------------------------------------------------------------------------------------
// TG-153 (2026-08-04) — THE DISJOINTNESS CHECK ABOVE WAS TRUE AND INSUFFICIENT.
//
// Validate answers "do the two planes share a reference?". It cannot answer "is this process supposed to hold
// an actuation reference AT ALL?", because until TG-153 there was only ever ONE process: the worker that ran
// the LLM triage agent over untrusted alert/syslog/host content ALSO built the actuation SSH runner
// (cmd/worker/main.go — agent.NewReadOnlyToolSet at :816, sshactuation.NewNativeRunner at :1987). Two disjoint
// references in one address space are one compromise away from being the same reference: the July-2026
// HuggingFace intrusion was exactly that chain — untrusted data reached a processing worker, and from that
// foothold the actor harvested every credential the worker could reach.
//
// The substrate half landed first and is verified live: OpenBao policies `tg-triage-ro` (read secret/data/tg/*,
// DENY tg/actuator + tg/proxmox) and `tg-actuate-ro` (read ONLY tg/actuator + tg/proxmox), bound to the
// AppRoles `tg-triage` and `tg-actuate`. Proven 2026-08-04: the triage token gets 403 on tg/actuator and 200
// on hostdiag; the actuate token gets 200 on tg/actuator and 403 on hostdiag. So OpenBao already refuses each
// plane the other's credentials — what was missing is that TG ran ONE process under ONE identity, so nothing
// consumed that split.
//
// ProcessPlane + ValidateFor below are the boot-time half of the process split. A process DECLARES which plane it
// runs (TG_CREDENTIAL_PLANE); ValidateFor then refuses a triage process that was handed ANY actuation
// reference, and an actuation process that was handed ANY read-triage reference. That is the check that makes
// a MISCONFIGURED split fail closed instead of quietly collapsing back into one co-holding process — the
// operator who sets TG_CREDENTIAL_PLANE=triage but leaves TG_ACTUATION_SSH_KEY in the same .env has NOT split
// anything, and the boot log must say so rather than print "plane split OK".
//
// ProcessPlaneBoth is the DEFAULT and is byte-identical to the pre-TG-153 behaviour (ValidateFor(ProcessPlaneBoth) runs
// exactly Validate and nothing else): this is a security fix that must not break every existing single-worker
// installation on upgrade.
// ---------------------------------------------------------------------------------------------------------
package credential

import (
	"fmt"
	"strings"

	"github.com/territory-grounder/grounder/core/config"
)

// ProcessPlane is the credential plane a PROCESS runs (TG-153). It is declared by the operator through
// TG_CREDENTIAL_PLANE and decides, at the composition root, which credentials the process may ACQUIRE at all
// — not which it may use. The distinction is the whole point: a process that constructs the actuation runner
// and then guards every call with an `if` still holds the key in its address space, and an address space is
// what an intrusion reads.
type ProcessPlane string

const (
	// ProcessPlaneTriage is the process that reads UNTRUSTED content — alert bodies, syslog, host command output —
	// and drives the LLM triage agent over it. It holds estate READ credentials and MUST hold no actuation
	// credential: its compromise yields what it could already read, and nothing that mutates the estate.
	ProcessPlaneTriage ProcessPlane = "triage"
	// ProcessPlaneActuation is the process that MUTATES the estate. It holds the actuation SSH identity and the
	// proxmox/AWX write tokens, polls only the actuation task queue, and never registers an untrusted-content
	// reader — so there is no path by which attacker-authored text reaches the process holding the key.
	ProcessPlaneActuation ProcessPlane = "actuation"
	// ProcessPlaneBoth is the DEFAULT and the pre-TG-153 posture: one process holds both planes. It is retained
	// because a security fix that breaks every existing single-worker deployment on upgrade does not get
	// deployed, and an undeployed control protects nobody. `both` is honest about what it is — it is the
	// co-holding posture, not a split — and the boot log says so.
	ProcessPlaneBoth ProcessPlane = "both"
)

// ParseProcessPlane reads the operator's TG_CREDENTIAL_PLANE declaration. Blank/unset ⇒ ProcessPlaneBoth (the
// behaviour-preserving default for every deployment that has not opted in). An UNRECOGNISED value is an
// ERROR, never a silent default: "TG_CREDENTIAL_PLANE=triage-only" that quietly fell back to `both` would
// leave an operator believing they had split the planes while the actuation key sat beside the agent.
func ParseProcessPlane(s string) (ProcessPlane, error) {
	switch p := ProcessPlane(strings.ToLower(strings.TrimSpace(s))); p {
	case "":
		return ProcessPlaneBoth, nil
	case ProcessPlaneTriage, ProcessPlaneActuation, ProcessPlaneBoth:
		return p, nil
	default:
		return ProcessPlaneBoth, fmt.Errorf("credential plane split (TG-153): TG_CREDENTIAL_PLANE=%q is not a plane — use %q (untrusted-content triage, holds NO actuation credential), %q (estate mutation, reads no untrusted content) or %q (the default: one process holds both, exactly as before)", s, ProcessPlaneTriage, ProcessPlaneActuation, ProcessPlaneBoth)
	}
}

// HoldsTriage reports whether this process may acquire read-triage credentials and register
// untrusted-content readers.
func (p ProcessPlane) HoldsTriage() bool { return p == ProcessPlaneTriage || p == ProcessPlaneBoth }

// HoldsActuation reports whether this process may acquire actuation credentials and run the actuation
// activities.
func (p ProcessPlane) HoldsActuation() bool {
	return p == ProcessPlaneActuation || p == ProcessPlaneBoth
}

// CredentialClass is the plane a credential belongs to.
type CredentialClass int

const (
	// ClassReadTriage credentials answer read-only triage/investigation (estate reads, host-log reads, the
	// read-scoped substrate token). A compromise here can read what it triages and nothing more.
	ClassReadTriage CredentialClass = iota
	// ClassActuation credentials can MUTATE the estate (the actuation SSH identity, the proxmox/AWX/k8s
	// write-tokens). These must never be reachable from the triage plane.
	ClassActuation
)

func (c CredentialClass) String() string {
	if c == ClassActuation {
		return "actuation"
	}
	return "read-triage"
}

// PlaneSet declares the credential REFERENCES (never values) the process is configured to hold, partitioned by
// plane. Empty references are ignored (an unconfigured source contributes nothing).
type PlaneSet struct {
	ReadTriage []config.SecretRef // syslog/host-diag read keys, estate read tokens, the read-scoped substrate token
	Actuation  []config.SecretRef // the SSH mutate identity, plus the proxmox, AWX and k8s write tokens
}

// Validate enforces REQ-2203: no reference may serve BOTH planes. It returns the first crossing reference (safe
// to name) so the caller fails closed at boot. A reference of "" (unset) never crosses.
func (p PlaneSet) Validate() error {
	read := make(map[config.SecretRef]bool, len(p.ReadTriage))
	for _, r := range p.ReadTriage {
		if r != "" {
			read[r] = true
		}
	}
	for _, r := range p.Actuation {
		if r != "" && read[r] {
			return fmt.Errorf("credential plane split (REQ-2203): reference %q is configured for BOTH the read-triage and actuation planes — the triage plane must never co-hold an actuation credential; give each plane a distinct reference (and a distinct OpenBao role)", config.LoggableRef(string(r)))
		}
	}
	return nil
}

// ValidateFor enforces REQ-2203 FOR THE PLANE THIS PROCESS DECLARED (TG-153). It is the boot gate of the
// process split:
//
//   - ProcessPlaneBoth  — exactly Validate() and nothing more. Byte-identical to the pre-TG-153 behaviour, because
//     a co-holding process is what every existing deployment runs and this must not break on upgrade.
//   - ProcessPlaneTriage — Validate(), PLUS: the process must declare NO actuation reference at all. A triage
//     process that was handed the SSH mutate key or a write token has not split anything; it is the old
//     co-holding worker wearing a label, and the label is worse than nothing because it is believed.
//   - ProcessPlaneActuation — the mirror: no read-triage reference. An actuation process that also holds the estate
//     read tokens is a process an attacker can pivot INTO the triage plane from, which is the same
//     blast-radius merge measured from the other end.
//
// The caller MUST build the PlaneSet from the process's raw configuration — every reference the operator
// actually set — and not from a list already filtered by plane. A PlaneSet assembled after plane filtering
// would make this assertion vacuous: it would find no actuation reference on the triage plane because the
// filter removed them, and would report a split that the environment does not have.
//
// The refusal names the reference (safe: INV-13 — a ref is not a secret) and the variable-level remedy, so an
// operator who mis-split their .env is told which line to move rather than that "something crossed".
func (p PlaneSet) ValidateFor(plane ProcessPlane) error {
	if err := p.Validate(); err != nil {
		return err
	}
	nonEmpty := func(rs []config.SecretRef) (config.SecretRef, bool) {
		for _, r := range rs {
			if r != "" {
				return r, true
			}
		}
		return "", false
	}
	switch plane {
	case ProcessPlaneTriage:
		if r, ok := nonEmpty(p.Actuation); ok {
			return fmt.Errorf("credential plane split (REQ-2203, TG-153): this process declared TG_CREDENTIAL_PLANE=%s but was handed the ACTUATION reference %q — a triage process reads untrusted alert/syslog/host content, so any actuation credential in its environment is one intrusion away from mutating the estate; move that reference to the actuation worker (TG_CREDENTIAL_PLANE=%s, OpenBao AppRole tg-actuate) or run TG_CREDENTIAL_PLANE=%s if you did not mean to split", ProcessPlaneTriage, config.LoggableRef(string(r)), ProcessPlaneActuation, ProcessPlaneBoth)
		}
	case ProcessPlaneActuation:
		if r, ok := nonEmpty(p.ReadTriage); ok {
			return fmt.Errorf("credential plane split (REQ-2203, TG-153): this process declared TG_CREDENTIAL_PLANE=%s but was handed the READ-TRIAGE reference %q — the actuation process must hold nothing beyond what mutates the estate, or a compromise of it also yields the read plane; move that reference to the triage worker (TG_CREDENTIAL_PLANE=%s, OpenBao AppRole tg-triage) or run TG_CREDENTIAL_PLANE=%s if you did not mean to split", ProcessPlaneActuation, config.LoggableRef(string(r)), ProcessPlaneTriage, ProcessPlaneBoth)
		}
	}
	return nil
}

// SummaryFor is the boot-log line for a declared plane: what the process holds AND what it therefore cannot
// reach. `both` prints the historic wording unchanged (it is the historic posture), and says out loud that it
// is NOT a split — a co-holding worker that logged "plane split OK" is how this gap survived to TG-153.
func (p PlaneSet) SummaryFor(plane ProcessPlane) string {
	switch plane {
	case ProcessPlaneTriage:
		return fmt.Sprintf("plane=triage — %d read-triage ref(s) held, 0 actuation ref(s): a compromise of this process CANNOT reach an estate-mutating credential", p.count(p.ReadTriage))
	case ProcessPlaneActuation:
		return fmt.Sprintf("plane=actuation — %d actuation ref(s) held, 0 read-triage ref(s); this process registers no untrusted-content reader and polls only the actuation queue", p.count(p.Actuation))
	default:
		return p.Summary() + " — plane=both: ONE process holds BOTH planes (the pre-TG-153 posture, unchanged). This is not a split: set TG_CREDENTIAL_PLANE=triage/actuation on two workers to bound the blast radius"
	}
}

func (PlaneSet) count(rs []config.SecretRef) int {
	n := 0
	for _, r := range rs {
		if r != "" {
			n++
		}
	}
	return n
}

// Summary is a value-less description for the boot log: how many references each plane holds.
func (p PlaneSet) Summary() string {
	nz := func(rs []config.SecretRef) int {
		n := 0
		for _, r := range rs {
			if r != "" {
				n++
			}
		}
		return n
	}
	return fmt.Sprintf("plane split OK: %d read-triage ref(s) disjoint from %d actuation ref(s)", nz(p.ReadTriage), nz(p.Actuation))
}
