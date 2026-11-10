package proposal

import (
	"fmt"
	"regexp"
	"strings"
)

// INCONSISTENT REASONING — a cheap, deterministic self-check on the model's own output (MECH-406).
//
// The predecessor computes it in its Runner node: a stated confidence of >=0.8 alongside >=3 hedging
// words, or <0.5 alongside >=3 action words, marks the session `inconsistent_reasoning`. It is the
// highest-signal member of its validation-warning set precisely because it needs nothing external — no
// judge, no second model call, no ground truth. It compares the model's confidence against the model's
// own prose and flags the disagreement.
//
// TG'S VERSION IS STRICTER THAN THE PREDECESSOR'S FOR ONE STRUCTURAL REASON. The predecessor has to
// scrape its confidence out of free text (`CONFIDENCE: 0.83 — reason`, matched by regex), so a
// malformed or absent line silently disables the whole check. TG's confidence is a parsed, validated
// float on the proposal, so the check runs on every session that carries one and cannot be turned off by
// the model wording its output differently.
//
// OBSERVATIONAL, NOT A GATE. This records a warning; it changes no band, blocks no action, and re-enters
// no decision path. That is deliberate: the signal is a heuristic over word counts, and a heuristic that
// silently raises a risk band would be a model token becoming control flow through the back door
// (INV-08). What it is FOR is measurement — how often does TG assert high confidence in hedging prose? —
// and that question cannot be asked at all today.

// hedging and action are the predecessor's word sets, ported verbatim. Deliberately not "improved":
// they are the exact vocabularies its measured signal was built on, and widening them here would change
// what the number means before anyone has seen what it says on TG's corpus.
var (
	hedgingRE = regexp.MustCompile(`(?i)\b(might|possibly|unclear|uncertain|unsure|maybe|perhaps|not sure)\b`)
	actionRE  = regexp.MustCompile(`(?i)\b(restart|apply|execute|deploy|fix|change|modify|update|remove|delete)\b`)
	// standDownRE is the predecessor's escape hatch: text that explicitly says to stop and wait is NOT
	// inconsistent with low confidence, however many action words it contains — describing the actions it
	// is declining to take is exactly what a careful low-confidence conclusion looks like.
	standDownRE = regexp.MustCompile(`(?is)\bstop\b.*\bwait\b`)
)

const (
	// highConfidence / lowConfidence and the word threshold are the predecessor's constants.
	highConfidence = 0.8
	lowConfidence  = 0.5
	wordThreshold  = 3
)

// InconsistentReasoning reports whether a proposal's stated confidence disagrees with its own prose, and
// why. The reason is returned so the recorded warning says which direction tripped it — "high confidence,
// hedging prose" and "low confidence, action-heavy prose" are different defects and a bare boolean
// conflates them.
//
// hasConfidence is EXPLICIT rather than inferred from the value, and that is not ceremony.
// agent.Result.Confidence is a bare float64 with no companion presence flag, so an unrecorded confidence
// arrives as 0.0 — which is below lowConfidence and would trip the low-confidence arm on EVERY
// confidence-less session that happens to name three actions. This codebase already knows that hazard:
// core/httpapi/actions.go carries a HasConfidence flag precisely because "an unrecorded confidence" and
// "a real 0.0" are different facts, and core/db/action_manifest_read.go keeps the column nullable for the
// same reason. A check that cannot tell them apart is a false-positive generator, and a warning nobody
// can trust is worse than no warning.
func InconsistentReasoning(confidence float64, hasConfidence bool, text string) (bool, string) {
	body := strings.TrimSpace(text)
	if body == "" || !hasConfidence {
		return false, ""
	}
	switch {
	case confidence >= highConfidence:
		if n := len(hedgingRE.FindAllString(body, -1)); n >= wordThreshold {
			return true, fmt.Sprintf("confidence %.2f with %d hedging term(s)", confidence, n)
		}
	case confidence < lowConfidence:
		// Text that explicitly stands down is consistent with low confidence no matter how many actions
		// it names — it is naming what it declined to do.
		if standDownRE.MatchString(body) {
			return false, ""
		}
		if n := len(actionRE.FindAllString(body, -1)); n >= wordThreshold {
			return true, fmt.Sprintf("confidence %.2f with %d action term(s)", confidence, n)
		}
	}
	return false, ""
}
