package agent

import (
	"encoding/json"
	"strings"

	"github.com/territory-grounder/grounder/core/proposal"
)

// TG-46 — self-consistency on the ONE forced-decision cycle: N independent samples, MECHANICAL majority.
//
// CONSTITUTION.md:116 promises self-consistency flagging that no code delivered. The 07-18 disposition on
// TG-46 ruled: narrow fix only — N-sample majority vote gated by execclass/severity on the forced-decision
// cycle, mechanical selection, INV-08-safe; NO wholesale Tree-of-Thoughts. The hedging-mismatch half already
// ships (core/proposal.InconsistentReasoning, consumed in temporal/runner). This file is the multi-sample
// half: the vote that turns N drawn decision samples into ONE selected decision.
//
// INV-08 IS THE DESIGN CONSTRAINT. Selection is counting and field comparison over the STRUCTURED decision
// fields each sample already parses to — never a judge, never a model reading the samples, never a token of
// any sample choosing among them. The winning sample's FULL TEXT then re-enters the loop's existing parse
// path byte-identically, so every downstream gate (confidence thresholds, citation gate, ParseProposal,
// band classifier) sees exactly what it would have seen had that text been the only draw.

// Decide-sample kinds — the CLOSED vocabulary of what one forced-decision sample resolved to. The two
// decided kinds carry a vote; the two undecided kinds hold none (a tool call or unparseable output is a
// FAILURE to obey "decide now", not a decision to weigh).
const (
	decideKindStop    = "stop"
	decideKindPropose = "propose"
	decideKindTool    = "tool"
	decideKindInvalid = "invalid"
)

// DecideSample is the structured decision record of ONE forced-decision sample (TG-46): what the sample
// resolved to (Kind), and — for a proposal — the typed action-identity fields the vote and the provenance
// record read. It deliberately does NOT retain the sample's full text: the winner's text becomes the
// session's decision through the normal path, and a losing sample's prose was never acted on — its vote key
// is what the record needs, and keeping N full model outputs per decide cycle would multiply the stored
// untrusted text for no consumer. DATA ONLY (INV-08): the slice is recorded on Result for provenance and
// read by no gate.
type DecideSample struct {
	Kind       string  // decideKindStop | decideKindPropose | decideKindTool | decideKindInvalid
	OpClass    string  // proposals only — the typed op_class the sample proposed
	Target     string  // proposals only — the typed target
	Reversible bool    // proposals only — the sample's own reversibility claim (tie-rank input)
	Confidence float64 // the directive confidence, with the same prose-scalar fallback the loop applies
}

// decided reports whether this sample actually DECIDED — the forced-decision cycle asked for a proposal or
// a grounded stop, and only those two kinds hold a vote.
func (s DecideSample) decided() bool { return s.Kind == decideKindStop || s.Kind == decideKindPropose }

// voteKey is the majority key: the (kind, op_class) pair. At the decide boundary the model emits no band —
// the band is computed downstream by core/risk over the whole gated input — so the band-shaped half of the
// ticket's "(op_class, band)" pair is the DISPOSITION: stand-down (stop) versus actuate (propose), with
// op_class as the action identity. Target, reversibility and confidence are deliberately NOT key material:
// the downstream classifier — not this vote — is the band authority over a proposal's fine structure, and
// fragmenting a real action-majority over a flag would manufacture splits.
func (s DecideSample) voteKey() string {
	if s.Kind == decideKindStop {
		return decideKindStop
	}
	return decideKindPropose + "/" + s.OpClass
}

// parseDecideSample resolves ONE sample's structured decision fields through the SAME grammar the loop
// itself applies — stripFences + strict directive decode, ParseProposal for a proposal, ParseConfidence as
// the prose fallback — so a sample votes exactly as the loop would have read it. Anything that is not a
// clean stop or a schema-valid proposal is an UNDECIDED kind: a tool call is recorded as such, and every
// other shape (unparseable JSON, unknown action, a proposal failing the single grammar) is "invalid".
func parseDecideSample(raw string) DecideSample {
	var d directive
	if json.NewDecoder(strings.NewReader(stripFences(raw))).Decode(&d) != nil {
		return DecideSample{Kind: decideKindInvalid}
	}
	conf := d.Confidence
	if conf == 0 {
		if v, ok := ParseConfidence(raw); ok {
			conf = v
		}
	}
	switch d.Action {
	case "stop":
		return DecideSample{Kind: decideKindStop, Confidence: conf}
	case "tool":
		return DecideSample{Kind: decideKindTool, Confidence: conf}
	case "propose":
		p, err := proposal.ParseProposal(d.Proposal)
		if err != nil {
			return DecideSample{Kind: decideKindInvalid, Confidence: conf}
		}
		return DecideSample{
			Kind:       decideKindPropose,
			OpClass:    p.Action.OpClass,
			Target:     p.Action.Target,
			Reversible: p.Action.Reversible,
			Confidence: conf,
		}
	default:
		return DecideSample{Kind: decideKindInvalid, Confidence: conf}
	}
}

// conservativeRank orders a decided sample by how much AUTONOMOUS ACTUATION selecting it can lead to —
// the "lowest band" ladder of the tie rule, smaller = more conservative:
//
//	0 — stop: a stand-down actuates nothing, ever. Stand-down beats actuate.
//	1 — propose, irreversible: the band classifier's inviolable step-1 floor clamps any not-proven-
//	    reversible action to POLL_PAUSE (core/risk/classifier.go), so selecting it routes the
//	    disagreement to a HUMAN — nothing runs without an approval.
//	2 — propose, reversible: the only shape that can reach an AUTO band downstream.
//
// The rank reads ONLY the sample's own typed fields. It deliberately does not consult core/safety's
// never-auto op-class floor: the agent loop does not import the safety core (the same posture the
// ReconLimiter seam documents), and the rank is a tie ORDER for selection among samples — the real band
// authority remains the downstream classifier, which applies every floor to whatever wins.
func conservativeRank(s DecideSample) int {
	switch {
	case s.Kind == decideKindStop:
		return 0
	case !s.Reversible:
		return 1
	default:
		return 2
	}
}

// moreConservative is the STRICT TOTAL order the tie rule selects by: band-ceiling rank first
// (conservativeRank), then LOWER confidence (below EscalateThreshold the loop itself hands the decision to
// an operator — lower confidence is the more-human-review direction), then a stated-arbitrary lexicographic
// backstop (op_class, target, raw text) so the order is total and the winner cannot depend on draw order.
func moreConservative(a, b DecideSample, rawA, rawB string) bool {
	if ra, rb := conservativeRank(a), conservativeRank(b); ra != rb {
		return ra < rb
	}
	if a.Confidence != b.Confidence {
		return a.Confidence < b.Confidence
	}
	if a.OpClass != b.OpClass {
		return a.OpClass < b.OpClass
	}
	if a.Target != b.Target {
		return a.Target < b.Target
	}
	return rawA < rawB
}

// decideByMajority selects ONE decision from N drawn forced-decision samples, mechanically (TG-46, INV-08).
//
//   - MAJORITY: each decided sample votes its (kind, op_class) key; the key with the most votes wins.
//   - TIE → THE CONSERVATIVE RESOLUTION: among all samples of the tied top keys, the single most
//     conservative sample under moreConservative wins the key for its side — stand-down beats actuate,
//     the lowest band-ceiling beats the auto-eligible, lower confidence beats higher. tieBroken records
//     that this rule fired (the loud-marker signal).
//   - REPRESENTATIVE: within the winning key the recorded decision is the HIGHEST-confidence sample
//     (ties → lexicographically smallest text). The key already fixed WHAT was decided; among texts
//     stating the same decision the sharpest statement carries it, and its confidence still passes the
//     loop's unchanged stop/escalate thresholds downstream.
//   - NO SAMPLE DECIDED (every draw was a tool call or invalid): the vote cannot select a decision that
//     does not exist. The lexicographically smallest raw is returned — a deterministic, order-independent
//     stand-in for "one of the drawn texts" — and re-enters the normal path, where a tool call dispatches
//     and unparseable output fails closed exactly as a single draw would have. disagreement then counts
//     EVERY sample: the whole draw failed the forced decision, and the record says so.
//
// disagreement = samples whose vote key differs from the winner's (undecided samples always count — they
// disagreed by failing to decide). Every rule above is order-independent: same samples, any arrival order,
// same winner text.
func decideByMajority(raws []string) (winner string, samples []DecideSample, disagreement int, tieBroken bool) {
	samples = make([]DecideSample, len(raws))
	votes := map[string][]int{} // vote key → indexes of the decided samples that cast it
	for i, r := range raws {
		samples[i] = parseDecideSample(r)
		if samples[i].decided() {
			k := samples[i].voteKey()
			votes[k] = append(votes[k], i)
		}
	}
	if len(votes) == 0 {
		winner = raws[0]
		for _, r := range raws[1:] {
			if r < winner {
				winner = r
			}
		}
		return winner, samples, len(raws), false
	}
	top := 0
	for _, idxs := range votes {
		if len(idxs) > top {
			top = len(idxs)
		}
	}
	tied := 0
	winnerKey := ""
	best := -1 // the most conservative sample across the tied top keys
	for k, idxs := range votes {
		if len(idxs) != top {
			continue
		}
		tied++
		winnerKey = k
		for _, i := range idxs {
			if best == -1 || moreConservative(samples[i], samples[best], raws[i], raws[best]) {
				best = i
			}
		}
	}
	if tied > 1 {
		tieBroken = true
		winnerKey = samples[best].voteKey()
	}
	rep := -1
	for _, i := range votes[winnerKey] {
		if rep == -1 || samples[i].Confidence > samples[rep].Confidence ||
			(samples[i].Confidence == samples[rep].Confidence && raws[i] < raws[rep]) {
			rep = i
		}
	}
	return raws[rep], samples, len(raws) - len(votes[winnerKey]), tieBroken
}
