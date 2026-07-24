// Package eval is TG's grounding/quality evaluation harness (task #26, first iteration: MEASUREMENT).
//
// It runs a corpus of realistic incidents through the REAL Runner (read-only, mutation OFF) and scores each
// resulting triage session with an LLM-as-judge on five quality dimensions — the same shape the predecessor
// (claude-gateway) uses, plus TG's own grounding signals (a committed, falsifiable prediction). The pure
// logic here (corpus load, judge-response parsing, aggregation) is unit-tested in CI; the actual run against
// the live model gateway is a build-gated integration test (eval_integration_test.go) executed ON the box.
//
// The next iteration turns this measurement into the auto-patching flywheel (3 prompt variants + a control,
// deterministic assignment, Welch t-test, promote the winner) — see README.md.
package eval

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"

	"github.com/territory-grounder/grounder/core/judge"
)

// Incident is one corpus entry — an IncidentEnvelope-shaped realistic alert (real NL host + rule).
type Incident struct {
	ExternalRef string `json:"external_ref"`
	SourceID    string `json:"source_id"`
	AlertRule   string `json:"alert_rule"`
	Host        string `json:"host"`
	Severity    string `json:"severity"`
	Site        string `json:"site"`
	Summary     string `json:"summary"`
}

// Session is the captured outcome of running one incident through the Runner (read-only).
type Session struct {
	Ref        string   `json:"ref"`
	AlertRule  string   `json:"alert_rule"`
	Host       string   `json:"host"`
	Severity   string   `json:"severity"`
	Band       string   `json:"band"`         // AUTO | AUTO_NOTICE | POLL_PAUSE | ""
	Proposed   bool     `json:"proposed"`     // did the agent propose an action?
	ActionID   string   `json:"action_id"`    // the sealed action id (if proposed+gated)
	Prediction string   `json:"prediction"`   // the committed consequence prediction (grounding signal)
	Predicted  bool     `json:"predicted"`    // was a machine prediction committed (falsifiable) ?
	Evidence   []string `json:"evidence"`     // cited evidence ids (INV-11 silent-cognition guard)
	Conclusion string   `json:"conclusion"`   // the agent's grounded no-action rationale on a stop (REQ-1008)
	Decisions  []string `json:"decisions"`    // governance-ledger decision labels for this session
	Outcome    string   `json:"outcome"`      // the RunnerResult outcome string
	Mutated    bool     `json:"mutated"`      // MUST be false (mutation OFF)
	Err        string   `json:"err,omitempty"`
}

// Dimensions are the five quality axes the judge scores 1..5 — the canonical list lives in core/judge
// (the durable judge cron scores the same axes; ONE judge, never two drifting copies).
var Dimensions = judge.Dimensions

// Score is one judged session: a 1..5 per dimension + a one-line rationale.
type Score struct {
	Ref     string         `json:"ref"`
	Scores  map[string]int `json:"scores"`
	Comment string         `json:"comment"`
}

// Scorecard is the aggregate over a run.
type Scorecard struct {
	N              int                `json:"n"`
	Judged         int                `json:"judged"` // sessions the judge actually scored — < N means a DEGRADED run (integrity signal)
	Errors         int                `json:"errors"` // sessions whose triage workflow errored — > 0 means a DEGRADED run (integrity signal)
	Bands          map[string]int     `json:"bands"`
	ProposalRate   float64            `json:"proposal_rate"`
	PredictionRate float64            `json:"prediction_rate"` // fraction with a committed falsifiable prediction
	MutationCount  int                `json:"mutation_count"`  // MUST be 0 (mutation OFF)
	DimMeans       map[string]float64 `json:"dim_means"`
	Overall        float64            `json:"overall"`
}

// LoadCorpus reads the incident corpus.
func LoadCorpus(path string) ([]Incident, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c []Incident
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("corpus %s: %w", path, err)
	}
	return c, nil
}

// judgePrompt builds the strict-JSON judge instruction for one session — the shared core/judge prompt
// over this session's facts (the durable judge cron builds the SAME prompt from the compact record).
func judgePrompt(s Session) string {
	return judge.Prompt(judge.Session{
		Ref:        s.Ref,
		AlertRule:  s.AlertRule,
		Host:       s.Host,
		Severity:   s.Severity,
		Band:       s.Band,
		Proposed:   s.Proposed,
		ActionID:   s.ActionID,
		Prediction: s.Prediction,
		Predicted:  s.Predicted,
		Evidence:   s.Evidence,
		Conclusion: s.Conclusion,
		Decisions:  s.Decisions,
		Outcome:    s.Outcome,
		Mutated:    s.Mutated,
	})
}

// ParseScore extracts the judge's JSON verdict defensively (the model may wrap it in prose / fences).
// It delegates to the shared core/judge parser — ONE parser for the eval harness and the judge cron.
func ParseScore(ref, raw string) (Score, error) {
	js, err := judge.ParseScore(ref, raw)
	return Score{Ref: js.Ref, Scores: js.Scores, Comment: js.Comment}, err
}

// Aggregate builds the scorecard from the sessions + their scores.
func Aggregate(sessions []Session, scores []Score) Scorecard {
	sc := Scorecard{N: len(sessions), Judged: len(scores), Bands: map[string]int{}, DimMeans: map[string]float64{}}
	proposed, predicted := 0, 0
	for _, s := range sessions {
		band := s.Band
		if band == "" {
			band = "none"
		}
		sc.Bands[band]++
		if s.Proposed {
			proposed++
		}
		if s.Predicted {
			predicted++
		}
		if s.Mutated {
			sc.MutationCount++
		}
		if s.Err != "" {
			sc.Errors++ // a triage workflow that errored (e.g. a 429-contended arm) — the gate must not silently pool it
		}
	}
	if sc.N > 0 {
		sc.ProposalRate = float64(proposed) / float64(sc.N)
		sc.PredictionRate = float64(predicted) / float64(sc.N)
	}
	// falsifiable_prediction is N/A for a grounded stand-down (no action ⇒ no prediction to falsify) — a
	// category error to score, and its floor otherwise drags the dimension mean down. Exclude non-applicable
	// sessions from THAT dimension only, so it measures real proposer prediction quality (TG-61 seq C). One
	// rule, judge.PredictionApplicable, shared with the durable judge cron + flywheel DimensionMeans.
	applicable := make(map[string]bool, len(sessions))
	for _, s := range sessions {
		applicable[s.Ref] = judge.PredictionApplicable(judge.Session{Proposed: s.Proposed, Predicted: s.Predicted})
	}
	sums := map[string]int{}
	counts := map[string]int{}
	for _, s := range scores {
		for d, v := range s.Scores {
			if d == judge.DimFalsifiablePrediction && !applicable[s.Ref] {
				continue // N/A for a stand-down — omitted, not floored
			}
			sums[d] += v
			counts[d]++
		}
	}
	var overallSum float64
	var overallN int
	for _, d := range Dimensions {
		if counts[d] > 0 {
			m := float64(sums[d]) / float64(counts[d])
			sc.DimMeans[d] = round2(m)
			overallSum += m
			overallN++
		}
	}
	if overallN > 0 {
		sc.Overall = round2(overallSum / float64(overallN))
	}
	return sc
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

// ScorecardJSON serializes the scorecard deterministically.
func ScorecardJSON(sc Scorecard) []byte {
	b, _ := json.MarshalIndent(sc, "", "  ")
	return b
}

// SortSessions orders sessions by ref for deterministic reporting.
func SortSessions(ss []Session) { sort.Slice(ss, func(i, j int) bool { return ss[i].Ref < ss[j].Ref }) }
