package governance

// The production PairSource for the frontier cross-check (spec/004 REQ-307, TG-222).
//
// Before this file the ONLY implementation of core/governance.PairSource in the tree was a test fake, so the
// anchor that catches a three-week dead judge had nowhere to get its pairs. This is the model I/O the
// monitor's package comment says lives "behind the injected seam": it re-judges a sample of recently-ended
// sessions with an INDEPENDENT (frontier) model over the SAME rubric — core/judge.Prompt and
// core/judge.ParseScore, never a second drifting copy — and reports each session's local↔frontier pair.
//
// Independence is the whole value: a purely-local metric cannot distinguish "the judge scored nothing
// because nothing was judgeable" from "the judge is dead". A second opinion can, because it scores the same
// sessions.

import (
	"context"
	"fmt"
	"sort"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/agent"
	coregov "github.com/territory-grounder/grounder/core/governance"
	"github.com/territory-grounder/grounder/core/judge"
)

// ModelPairSource re-judges a sample with a frontier model and pairs the verdicts.
type ModelPairSource struct {
	// Sample is the judge-independent session sample (its LocalScored/LocalMean come from session_judgment).
	Sample coregov.CrossCheckSampleStore
	// Model is the SAME gateway every other component calls — so the frontier call is metered, observed and
	// guarded by the production model breaker (TG-221) like every other model call. A trip therefore
	// short-circuits the cross-check LOUDLY (Run returns the error) instead of quietly producing zero pairs,
	// which would read as "no drift, no death" — a false all-clear from a broken instrument.
	Model agent.Completer
	// Tier is the frontier model name. It SHOULD differ from the local judge's tier; when it does not, the
	// cross-check degenerates into the judge grading itself, so IndependentOf refuses that configuration.
	Tier string
	// Limit bounds one sample (each row costs one frontier completion).
	Limit int
}

// IndependentOf reports whether this source is genuinely independent of the local judge's tier. A
// cross-check run on the SAME tier is not an anchor — it is the judge grading itself, and a self-agreeing
// dead judge is exactly the blind spot this control exists to close (docs/PORT-FIDELITY-AUDIT §3-8).
func (s *ModelPairSource) IndependentOf(localTier string) bool {
	return s != nil && s.Tier != "" && s.Tier != localTier
}

// verdictBucket maps a 1..5 mean score to a coarse VERDICT the two judges are compared on.
//
// Comparing raw means would make the anchor fire on rounding noise; comparing a coarse band asks the
// question that matters — "did the two judges reach the same conclusion about this session?" — and matches
// the predecessor's verdict-string comparison. An unscored reading has no bucket.
func verdictBucket(mean float64) string {
	switch {
	case mean >= 4:
		return "strong"
	case mean >= 3:
		return "adequate"
	default:
		return "weak"
	}
}

// RecentCrossCheckPairs re-judges the sample with the frontier model and returns one pair per session.
//
// A per-row frontier failure (model error, unparseable reply) yields FrontierScored=false for that row. That
// is the conservative direction by construction: DEATH counts rows the FRONTIER scored and the local judge
// did not, so a frontier that fails can only ever UNDER-report death, never invent it. A failure to read the
// sample at all is returned as an error — an empty pair set would evaluate to a clean bill of health.
func (s *ModelPairSource) RecentCrossCheckPairs(ctx context.Context) ([]coregov.CrossCheckPair, error) {
	if s == nil || s.Sample == nil || s.Model == nil || s.Tier == "" {
		return nil, fmt.Errorf("governance: frontier pair source is not configured (sample/model/tier)")
	}
	rows, err := s.Sample.RecentForCrossCheck(ctx, s.Limit)
	if err != nil {
		return nil, fmt.Errorf("governance: cross-check sample: %w", err)
	}
	// Deterministic order so a run is reproducible and an oracle can assert on it.
	sort.Slice(rows, func(i, j int) bool { return rows[i].ExternalRef < rows[j].ExternalRef })

	pairs := make([]coregov.CrossCheckPair, 0, len(rows))
	for _, r := range rows {
		p := coregov.CrossCheckPair{SessionID: r.ExternalRef, LocalScored: r.LocalScored}
		if r.LocalScored {
			p.LocalVerdict = verdictBucket(r.LocalMean)
		}
		facts := judge.Session{
			Ref: r.ExternalRef, Host: r.Host, AlertRule: r.AlertRule, Band: r.Band,
			Outcome: r.Outcome, Proposed: r.Proposed, Op: r.Op,
			Conclusion: r.Conclusion, Prediction: r.Prediction,
		}
		raw, cerr := s.Model.Complete(ctx, "frontier-crosscheck", s.Tier,
			[]model.Message{{Role: "user", Content: judge.Prompt(facts)}})
		if cerr != nil {
			pairs = append(pairs, p) // FrontierScored stays false — under-reports death, never invents it
			continue
		}
		sc, perr := judge.ParseScore(r.ExternalRef, raw)
		if perr != nil {
			pairs = append(pairs, p)
			continue
		}
		var sum float64
		var n int
		for _, v := range sc.Scores {
			if v > 0 {
				sum += float64(v)
				n++
			}
		}
		if n > 0 {
			p.FrontierScored = true
			p.FrontierVerdict = verdictBucket(sum / float64(n))
		}
		pairs = append(pairs, p)
	}
	return pairs, nil
}

// compile-time proof this is the port the monitor consumes.
var _ coregov.PairSource = (*ModelPairSource)(nil)
