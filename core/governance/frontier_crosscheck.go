package governance

import "context"

// The frontier cross-check is the no-human eval anchor (PORT-FIDELITY-AUDIT P1-15, the predecessor's
// judge-frontier-crosscheck.py). The judge-liveness monitor catches a judge that STOPS writing real scores
// (the judged fraction drops). It cannot catch two failure modes that the frontier cross-check exists for:
//
//   - DRIFT — the local judge keeps scoring, so liveness reads healthy, but its verdicts have silently gone
//     wrong. Only an INDEPENDENT re-judgment over the SAME rubric can surface the disagreement.
//   - DEATH (confirmed) — the local judge returns "unscored" (a -1 row) for sessions the frontier scores as
//     genuinely judgeable. A dead judge that still WRITES -1 rows never looks "dark" to any purely-local
//     metric; the frontier proves the sessions WERE judgeable, so the local -1s are a fault, not a
//     legitimately-unscorable tail. This is the exact 3-week dead-judge class no purely-LLM metric caught.
//
// The comparison is a pure function (Evaluate); the frontier-model I/O is confined to the injected PairSource
// so the anchor's decision logic is deterministic and oracle-testable.

const (
	// CrossCheckMinSample is the smallest sample worth a DRIFT warning — a thinner sample is too noisy to
	// page on. Mirrors the predecessor's `judge_frontier_pairs > 5` DRIFT gate (so the total sample must
	// EXCEED 5, i.e. be >= 6). DEATH is deliberately NOT gated on this: a dead judge must page even on a thin
	// window (the predecessor's standalone `local_unscored_rate > 0.5` term has no pairs gate).
	CrossCheckMinSample = 5
	// CrossCheckAgreementFloor is the local↔frontier verdict agreement rate below which DRIFT is raised.
	CrossCheckAgreementFloor = 0.6
	// CrossCheckDeathFraction is the fraction of frontier-scored-but-locally-unscored sessions above which
	// DEATH is raised — the local judge is returning -1 for sessions the frontier proves were judgeable.
	CrossCheckDeathFraction = 0.5
)

// CrossCheckPair is one session's local judgment paired with an INDEPENDENT frontier re-judgment over the
// same rubric. LocalScored/FrontierScored are false when that judge returned "unscored" (a -1 row). The
// verdict strings are compared only when both scored.
type CrossCheckPair struct {
	SessionID       string
	LocalScored     bool
	LocalVerdict    string
	FrontierScored  bool
	FrontierVerdict string
}

// PairSource yields recent local↔frontier judgment pairs. The frontier re-judgment (an LLM call over the
// same rubric) lives behind this seam; the monitor's decision never depends on how the pairs were produced.
type PairSource interface {
	RecentCrossCheckPairs(ctx context.Context) ([]CrossCheckPair, error)
}

// CrossCheckResult is the anchor's reading. Comparable is the DRIFT denominator (both judges scored); Agree
// is the count that matched. DeathHits is the count the frontier scored while the local judge did not.
type CrossCheckResult struct {
	Sample        int
	Comparable    int
	Agree         int
	AgreementRate float64
	DeathHits     int
	DeathFraction float64
	Drift         bool
	Death         bool
	Warned        bool
}

// FrontierCrossCheckMonitor re-judges a sample of recently local-judged sessions with a frontier model and
// raises DRIFT / DEATH warnings with ZERO operator involvement. Thresholds default to the package constants
// when left zero.
type FrontierCrossCheckMonitor struct {
	Pairs          PairSource
	Escalation     Escalator
	MinSample      int
	AgreementFloor float64
	DeathFraction  float64
}

func (m *FrontierCrossCheckMonitor) minSample() int {
	if m.MinSample > 0 {
		return m.MinSample
	}
	return CrossCheckMinSample
}

func (m *FrontierCrossCheckMonitor) agreementFloor() float64 {
	if m.AgreementFloor > 0 {
		return m.AgreementFloor
	}
	return CrossCheckAgreementFloor
}

func (m *FrontierCrossCheckMonitor) deathFraction() float64 {
	if m.DeathFraction > 0 {
		return m.DeathFraction
	}
	return CrossCheckDeathFraction
}

// Evaluate is the pure cross-check decision over a set of pairs. DEATH is raised whenever the fraction of
// frontier-scored-but-locally-unscored sessions STRICTLY EXCEEDS the death fraction — with NO sample gate, so
// a dead judge pages even on a thin window (the predecessor's standalone `local_unscored_rate > 0.5` term).
// DRIFT is raised when the TOTAL sample exceeds the min-sample floor, at least one pair is comparable (both
// scored — the predecessor's `agreement_rate >= 0` guard), and that agreement rate is below the floor; a thin
// sample is too noisy to page DRIFT on.
func (m *FrontierCrossCheckMonitor) Evaluate(pairs []CrossCheckPair) CrossCheckResult {
	res := CrossCheckResult{Sample: len(pairs)}
	for _, p := range pairs {
		if !p.LocalScored && p.FrontierScored {
			res.DeathHits++
		}
		if p.LocalScored && p.FrontierScored {
			res.Comparable++
			if p.LocalVerdict == p.FrontierVerdict {
				res.Agree++
			}
		}
	}
	if res.Comparable > 0 {
		res.AgreementRate = float64(res.Agree) / float64(res.Comparable)
	}
	if res.Sample > 0 {
		res.DeathFraction = float64(res.DeathHits) / float64(res.Sample)
	}
	min := m.minSample()
	// DEATH: no sample gate, STRICT threshold (`local_unscored_rate > 0.5`). DeathFraction is 0 on an empty
	// sample, so it never fires spuriously; on a thin all-dead window it pages, as the predecessor does.
	if res.DeathFraction > m.deathFraction() {
		res.Death = true
	}
	// DRIFT: gated on the TOTAL pair count (`pairs > 5`), needs a meaningful agreement rate (Comparable > 0 —
	// the `agreement_rate >= 0` guard; a rate over zero comparable pairs is meaningless), below the floor.
	if res.Comparable > 0 && res.Sample > min && res.AgreementRate < m.agreementFloor() {
		res.Drift = true
	}
	return res
}

// Run fetches recent pairs and evaluates them, routing a DEATH and/or DRIFT warning through the escalation
// module. A warning fails the run open (it returns the escalation error) rather than swallowing it — a
// broken alerting path must not silently mask a dead judge.
func (m *FrontierCrossCheckMonitor) Run(ctx context.Context) (CrossCheckResult, error) {
	pairs, err := m.Pairs.RecentCrossCheckPairs(ctx)
	if err != nil {
		return CrossCheckResult{}, err
	}
	res := m.Evaluate(pairs)
	if m.Escalation != nil {
		if res.Death {
			if err := m.Escalation.Warn(ctx, "judge-death", "frontier scored sessions the local judge left unscored"); err != nil {
				return res, err
			}
			res.Warned = true
		}
		if res.Drift {
			if err := m.Escalation.Warn(ctx, "judge-drift", "local↔frontier verdict agreement below floor"); err != nil {
				return res, err
			}
			res.Warned = true
		}
	}
	return res, nil
}
