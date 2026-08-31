// Actor-evidence banding policy — the evidence-class table and band-floor mapping (spec/026 REQ-2611,
// owner-ratified 2026-07-31).
//
// Provenance: live fault 1406. The agent read the PVE task trail (`root@pam` vzstop), named the fault
// correctly, and required human confirmation before reversing an operator-authored state — operationally
// right, yet indistinguishable in the old rubric from failing to name the fix. The policy this file
// implements separates the two questions permanently:
//
//   - The PROPOSE DUTY is absolute and band-independent (REQ-2609): actor-evidence NEVER suppresses a
//     proposal. Nothing in this file can suppress one — every function returns a Band, not a veto.
//   - Actor-evidence RAISES THE BAR for executing it (REQ-2611): an operator-declared mapping takes an
//     evidence class to a band FLOOR, composed with the computed band in the SAFE DIRECTION ONLY.
//
// This file supplies ONLY the closed evidence-class table and the evidence→floor mapping. The composition
// seam that applies it inside the one typed deterministic gate — the `core/risk` GatedInput floor field and
// the classifier's safe-direction clamp — is DEFERRED to and delivered by spec/028 T-028-4 (the NoticeFloor
// pattern, TRAILER + lockstep restamp there). Composing the band anywhere else would create a second
// banding authority, which is the exact defect class INV-09 exists to prevent.
package proposal

import (
	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/core/safety"
)

// EvidenceClass is one operator-policy class of actor evidence. The table is CLOSED (v1): a class not
// declared here maps to no floor — fail toward the classifier's own computed band, never toward a looser
// one (the composed result can only ever be as strict or stricter than the computed band).
type EvidenceClass string

const (
	// EvidenceAuthoredAction: a NAMED principal deliberately performed a state-changing action on the
	// target (e.g. a `vzstop` by `root@pam` in the PVE task log). Reversing an operator-authored state
	// without asking silently overrides a human's change — the v1 policy floors it at POLL_PAUSE.
	EvidenceAuthoredAction EvidenceClass = "authored-action"
	// EvidenceMaintenanceWindow: a declared maintenance sentinel covered the observation window.
	// Declared for the closed table; carries NO floor in v1 (declared maintenance is tier-1
	// suppression's business upstream — a session that reaches proposing was not suppressed).
	EvidenceMaintenanceWindow EvidenceClass = "maintenance-window"
	// EvidenceDeclaredChaos: a declared chaos/benchmark window covered the observation window. Same v1
	// posture as maintenance-window: in the table, no floor.
	EvidenceDeclaredChaos EvidenceClass = "declared-chaos"
)

// authoredActionKinds is the CLOSED set of domain verbs that mark an evidence record as a deliberate,
// state-changing act by a principal (per attribution.Evidence.ActionKind vocabulary, spec/023). A verb not
// listed here derives NO class — conservative: an unknown verb never manufactures a floor, and adding a
// verb is a reviewed policy change to this table, not a runtime discovery.
var authoredActionKinds = map[string]bool{
	"vzstop":     true,
	"vzstart":    true,
	"vzshutdown": true,
	"qmstop":     true,
	"qmstart":    true,
	"qmshutdown": true,
	"sudo":       true,
	"MR-merged":  true,
}

// ClassifyEvidence derives the evidence classes present in a set of reader-captured actor-evidence
// records (attribution.Evidence, the migration-0035 element shape — REQ-2610: this is the additive
// element-shape EXTENSION, a derivation, not a schema change). Deterministic and conservative: a record
// classifies as authored-action only when it names BOTH a principal and a closed-table state-changing
// verb. Order-independent; duplicates collapse.
func ClassifyEvidence(evs []attribution.Evidence) map[EvidenceClass]bool {
	out := map[EvidenceClass]bool{}
	for _, e := range evs {
		if e.Actor != "" && authoredActionKinds[e.ActionKind] {
			out[EvidenceAuthoredAction] = true
		}
	}
	return out
}

// FloorFor returns the strictest band floor the operator policy declares for the given evidence classes,
// and whether any floor applies. v1 policy (owner-ratified 2026-07-31): authored-action ⇒ POLL_PAUSE.
// Classes with no declared floor contribute nothing — absence of policy is absence of floor, never a
// default floor (the computed band already fails closed to POLL_PAUSE on its own error paths, INV-09).
func FloorFor(classes map[EvidenceClass]bool) (safety.Band, bool) {
	if classes[EvidenceAuthoredAction] {
		return safety.BandPollPause, true
	}
	return safety.BandAuto, false // no floor: the loosest possible floor composes as a no-op
}

// ComposeFloor composes a computed band with an evidence floor in the SAFE DIRECTION ONLY: the result is
// the STRICTER of the two (Band's enum order is strictness-descending: BandPollPause < BandAutoNotice <
// BandAuto, zero value = most restrictive). A floor can therefore only RAISE the approval bar — computed
// AUTO or AUTO_NOTICE + a POLL_PAUSE floor ⇒ POLL_PAUSE; a computed POLL_PAUSE passes through unchanged —
// and can NEVER lower a computed band, by construction rather than by case analysis (REQ-2611).
//
// NOTE: this is the mapping-table half only. Production composition happens inside the risk classifier's
// typed gate once spec/028 T-028-4 lands the GatedInput floor seam; nothing calls this from the decision
// path until then (a second banding authority is the defect, not the feature).
func ComposeFloor(computed, floor safety.Band) safety.Band {
	if floor < computed {
		return floor
	}
	return computed
}
