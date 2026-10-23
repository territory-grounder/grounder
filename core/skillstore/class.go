package skillstore

import "errors"

// The prose-artifact CLASS MODEL (spec/014 REQ-1315/1316, ADR-0017, TG-470): every LLM-facing prose
// artifact — agent skills, the base prompt's trialable half, runbooks, the judge-rubric mirror — is a
// row on the ONE skill store, discriminated by a closed artifact_class vocabulary. Not a parallel
// table: the store's two structural invariants (one production version per name, one active trial per
// name) are exactly what every prose class needs, and re-implementing them is how the predecessor's
// supersede logic drifted (the 0009 migration header's founding lesson).
//
// The vocabulary is mirrored by the database CHECK constraint (migration 0089), so an out-of-band
// writer cannot mint a class the code does not know — the same design as Status. Per-class treatment
// (body caps here; flywheel eligibility filtering, compose destination, pinned wire-format rows in
// later tasks) keys on this closed set and NOTHING else: no model token ever names a class (INV-08).

// ArtifactClass discriminates what kind of prose artifact a skill row carries (REQ-1315).
type ArtifactClass string

const (
	// ClassSkill: an agent skill — first-class flywheel artifact (the store's founding class).
	ClassSkill ArtifactClass = "skill"
	// ClassPrompt: the base prompt's trialable half — flywheel-eligible; its wire-format half is a
	// class-forced-PINNED row the flywheel can never draft against (lands with the base-prompt split).
	ClassPrompt ArtifactClass = "prompt"
	// ClassRunbook: knowledge-library content — never composes into the agent seed, no trial verb
	// (the compose/wiki mechanics land with the wiki task; the class is law now).
	ClassRunbook ArtifactClass = "runbook"
	// ClassRubric: the pinned mirror of the embedded judge rubric — never graduating (the write-refusal
	// mechanics land with the rubric-mirror task; the class is law now).
	ClassRubric ArtifactClass = "rubric"
)

// ErrUnknownClass refuses a write whose skill row carries a class outside the closed vocabulary
// (REQ-1315). The schema CHECK is the other half of the refusal; this is the domain half, so a class
// minted by a NEWER schema than this binary still fails closed here instead of drawing a guessed cap.
var ErrUnknownClass = errors.New("skillstore: unknown artifact class")

// ErrRubricNeverDrafts refuses ANY draft against a rubric-class artifact (TG-474, ADR-0017): the rubric
// mirror is a pinned projection of the embedded judge rubric — the embed is the sole authority, and a
// store-side edit would be a second rubric the judge never reads. Distinct from ErrPinnedSkill so the
// refusal names the CLASS rule, not just this row's pin flag.
var ErrRubricNeverDrafts = errors.New("skillstore: rubric-class artifacts never take drafts — the embedded judge rubric is the sole authority")

// ErrClassNotTrialEligible refuses trial admission for a class outside FlywheelEligible (REQ-1316):
// runbooks are library content promoted by an operator, never A/B-trialed; rubric is doubly walled (it
// cannot even draft). TWO independent walls raise it — the flywheel's AdmitToTrial gate (TG-475, before
// any offline eval is spent) and the Transition state machine's trial seam (TG-476, so the operator
// verb path is refused server-side too: hiding a console button is not a control).
var ErrClassNotTrialEligible = errors.New("skillstore: artifact class is not trial-eligible")

// DefaultClass maps the ABSENT class to ClassSkill — every row that predates the class model is a
// skill, exactly as the schema column default says (back-compat, REQ-1315). A STATED class passes
// through untouched, even an invalid one: normalization never rewrites a value validation must see.
func DefaultClass(c ArtifactClass) ArtifactClass {
	if c == "" {
		return ClassSkill
	}
	return c
}

// ValidArtifactClass reports whether c is one of the four known classes — a CLOSED enumeration
// (REQ-1315). The zero value is NOT valid: callers normalize with DefaultClass first, so an
// un-normalized absent class is caught rather than silently treated as anything.
func ValidArtifactClass(c ArtifactClass) bool {
	switch c {
	case ClassSkill, ClassPrompt, ClassRunbook, ClassRubric:
		return true
	}
	return false
}

// MaxBodyBytes is the DOMAIN-layer body cap per class (REQ-1316): skill 8 KiB, prompt 16 KiB,
// runbook/rubric 32 KiB. The SCHEMA ceiling (32 KiB, migration 0089) admits only the largest class;
// this function is the law that keeps a skill body at 8 KiB even though the schema now admits more —
// enforced at the write gate (ValidateDraft) and RE-CHECKED at composition (agent/skills.NewFromStore).
// An UNKNOWN class — the un-normalized zero value included — gets 0: every body is refused, never
// admitted under a guessed cap (fail closed).
func MaxBodyBytes(c ArtifactClass) int {
	switch c {
	case ClassSkill:
		return 8192
	case ClassPrompt:
		return 16384
	case ClassRunbook, ClassRubric:
		return 32768
	}
	return 0
}

// FlywheelEligible reports whether the flywheel may draft/trial against this class (REQ-1316):
// skill and prompt ONLY. runbook and rubric are NEVER trial-eligible — a knowledge page and the
// judge's own measuring stick must not be A/B-rewritten by the thing they inform/measure. This
// predicate lands NOW as the vocabulary's law; the flywheel filter that consumes it is the
// flywheel-generalization task (TG-475) — until then the flywheel reaches only skill-class rows
// anyway (production versions of compiled-registry skills).
func FlywheelEligible(c ArtifactClass) bool {
	return c == ClassSkill || c == ClassPrompt
}
