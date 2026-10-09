// Package rationale cross-checks the model's STATED prose against the MECHANICALLY-CHOSEN action.
//
// THE SEAM THIS CLOSES (TG-317, TG-154 §2/T7). Nothing compared the rationale to the argv. A proposal whose
// rationale reads "restart nginx on web01 because the unit is failed" could carry target db01 and every
// gate passed: the grammar is valid, the op-class is allowlisted, the argv builds deterministically from
// the structured params, the evidence binds by target equality (TG-166), the prediction is committed. The
// rationale is prose the pipeline never read.
//
// That is the cheapest seam for a prompt-injected or subverted agent, because the rationale is the ONE
// field a human reviewer on a POLL_PAUSE actually reads. A poll notice can say one thing while the sealed
// action does another, and the vote authorizes the action, not the prose.
//
// THIS IS A HEURISTIC AND IS BUILT AS ONE. It is deliberately NOT an LLM-judged consistency score: that
// would put the model back inside the loop that is under suspicion. Deterministic token comparison only,
// recorded as an observation the classifier reads.
//
// THE THREE OUTCOMES, and why the third is not a refusal:
//
//	names a DIFFERENT host and no matching one  -> Disagrees. Escalate the band.
//	names the SAME host                          -> agrees. No escalation.
//	names NO host at all                         -> ABSTAIN. Not evidence of anything.
//
// Abstention is the common case and must stay cheap: plenty of honest rationales never name a host ("the
// unit is failed and the restart is idempotent"). Treating silence as disagreement would poll everything
// and the check would be turned off within a week.
//
// AND IT ESCALATES, IT NEVER REFUSES. A refusal on a text heuristic takes the estate offline on a wording
// change. A band escalation costs a human glance and is honest about the check's strength.
package rationale

import (
	"regexp"
	"strings"
)

// hostToken matches a host-like word: starts with a letter, contains at least one digit, made of letters,
// digits and hyphens, at least three characters. That admits web01, db01, dc1tg01, dc2fw01 — the
// estate's actual naming — and rejects the false-positive families that would otherwise poll everything:
//
//	"5min", "10%", "3x"      -> start with a digit
//	"nginx", "postgresql"    -> no digit (they are UNIT-like, not host-like; see below)
//	"h2", "v3"               -> shorter than three characters
//
// A dotted FQDN is handled by comparing first labels, not by matching the whole name here.
var hostToken = regexp.MustCompile(`\b[a-z][a-z0-9-]*[0-9][a-z0-9-]*\b`)

// Finding is the observation the classifier reads. The zero value is the abstain case: no disagreement,
// nothing to say — which is what an unwired or silent check must look like.
type Finding struct {
	// Disagrees is true ONLY when the rationale named at least one host-like token and none of them
	// matched the sealed target. It is the single bit the band decision keys on.
	Disagrees bool
	// Named are the host-like tokens found in the rationale, in order of appearance, deduplicated. Carried
	// so the poll notice can show the human WHAT the prose claimed — a poll that says "the rationale
	// disagrees" without saying how is a poll nobody can adjudicate.
	Named []string
	// Target is the sealed target the comparison was made against, normalised the same way Named is.
	Target string
}

// normalise lowercases and reduces a hostname to its first label, so a rationale naming `web01` agrees
// with a sealed target of `web01.prod.example.net`. Comparing full strings would report a disagreement on
// every deployment that seals FQDNs, which is a false positive that would discredit the check immediately.
func normalise(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	return h
}

// Check compares a free-text rationale against the sealed action target.
//
// An empty target ABSTAINS rather than disagreeing: with nothing to compare against, every rationale would
// "disagree", which would poll the entire estate the moment a caller passed a blank. The heuristic must
// fail toward silence, because its failure mode is noise and noise is what gets it disabled.
func Check(rationaleText, sealedTarget string) Finding {
	target := normalise(sealedTarget)
	if target == "" {
		return Finding{}
	}

	seen := map[string]bool{}
	var named []string
	for _, tok := range hostToken.FindAllString(strings.ToLower(rationaleText), -1) {
		t := normalise(tok)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		named = append(named, t)
	}
	if len(named) == 0 {
		return Finding{Target: target} // ABSTAIN — named no host, which is not evidence of anything
	}
	return Finding{Disagrees: !seen[target], Named: named, Target: target}
}

// Reason renders the disagreement for a poll notice. Empty when there is nothing to report, so a caller
// can append it unconditionally.
func (f Finding) Reason() string {
	if !f.Disagrees {
		return ""
	}
	return "rationale names " + strings.Join(f.Named, ", ") + " but the sealed action targets " + f.Target
}
