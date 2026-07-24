package skillgen

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/skillstore"
)

// The production offline admission gate (spec/014 REQ-1307). A FULL eval-corpus run drives the whole
// Runner through the Temporal test env once per incident (eval/eval_integration_test.go) — far too heavy
// for a cron activity — so this is the LIGHTER-but-honest check the task sanctions: it scores the
// CANDIDATE body against the PRODUCTION body on the skill's OWN recent judged incidents, using the SAME
// core/judge the online trial and the eval harness use (ONE judge, never a drifting copy).
//
// Both arms run through the identical single-shot harness (guidance + incident -> a triage decision ->
// the shared judge), so the systematic limitations of a tool-less single-shot pass (no live evidence
// ids, no real band classification) are IDENTICAL for candidate and control and cancel in the DELTA. The
// gate therefore measures the change the body edit causes, not absolute quality — a cheap, honest
// pre-filter before the REAL statistical test, which is the online trial over the full Runner. Nothing
// here is fabricated: every score is a real judge verdict over a real model completion of a real past
// incident. The SEALED HOLDOUT is never read — the IncidentReader only yields the skill's own judged
// sessions, and the interface cannot express a holdout handle (REQ-1307).

// IncidentReader supplies the offline gate's discovery set: the recent judged sessions that composed a
// version (core/db TriageStore.RecentJudgedByVersion in production; a fake in tests).
type IncidentReader interface {
	RecentJudgedByVersion(ctx context.Context, versionID int64, window time.Duration, limit int) ([]judge.TriageRow, error)
}

// OfflineConfig parameterizes the honest lighter offline check.
type OfflineConfig struct {
	Window          time.Duration // how far back to draw the discovery set
	DiscoveryLimit  int           // max incidents scored per arm
	RegressionSlack float64       // a non-target dimension may drop at most this much before it is a regression
	MinIncidents    int           // fewer cleanly-scored discovery incidents than this ⇒ refuse (no honest signal)
}

// DefaultOfflineConfig is the worker's default offline-gate shape.
func DefaultOfflineConfig() OfflineConfig {
	return OfflineConfig{Window: 14 * 24 * time.Hour, DiscoveryLimit: 6, RegressionSlack: 0.25, MinIncidents: 3}
}

// OfflineRunner is the production skillstore.OfflineRunner.
type OfflineRunner struct {
	Model     agent.Completer  // the gateway — the SAME model the judge cron adjudicates with
	Store     skillstore.Store // reads the production baseline body
	Incidents IncidentReader   // the discovery-set reader (the judge spine)
	Cfg       OfflineConfig
}

// RunOffline scores the candidate against the current production body on the skill's recent judged
// incidents and returns the gate verdict. It FAILS CLOSED on thin evidence (a draft is never admitted
// without enough real incidents to compare on).
func (r OfflineRunner) RunOffline(ctx context.Context, candidate skillstore.Version, dimension string) (skillstore.OfflineResult, error) {
	runID := fmt.Sprintf("off-%s-v%s-%d", candidate.SkillName, candidate.Version, time.Now().UTC().Unix())
	prod, ok, err := r.Store.ProductionVersion(ctx, candidate.SkillName)
	if err != nil {
		return skillstore.OfflineResult{}, fmt.Errorf("offline: production baseline for %s: %w", candidate.SkillName, err)
	}
	if !ok {
		return skillstore.OfflineResult{RunID: runID, RegressionPass: false, DiscoveryDelta: 0,
			Detail: "no production baseline to compare against"}, nil
	}
	incidents, err := r.Incidents.RecentJudgedByVersion(ctx, prod.ID, r.Cfg.Window, r.Cfg.DiscoveryLimit)
	if err != nil {
		return skillstore.OfflineResult{}, fmt.Errorf("offline: discovery set for %s: %w", candidate.SkillName, err)
	}
	if len(incidents) < r.Cfg.MinIncidents {
		return skillstore.OfflineResult{RunID: runID, RegressionPass: false, DiscoveryDelta: 0,
			Detail: fmt.Sprintf("insufficient discovery incidents: %d < %d (fail closed)", len(incidents), r.Cfg.MinIncidents)}, nil
	}
	candScores := map[string][]float64{}
	prodScores := map[string][]float64{}
	scored := 0
	for _, inc := range incidents {
		cs, cerr := r.scoreArm(ctx, candidate.Body, inc)
		ps, perr := r.scoreArm(ctx, prod.Body, inc)
		if cerr != nil || perr != nil {
			continue // a per-incident model/parse failure degrades — it is not counted, never fabricated
		}
		for d, v := range cs {
			candScores[d] = append(candScores[d], v)
		}
		for d, v := range ps {
			prodScores[d] = append(prodScores[d], v)
		}
		scored++
	}
	if scored < r.Cfg.MinIncidents {
		return skillstore.OfflineResult{RunID: runID, RegressionPass: false, DiscoveryDelta: 0,
			Detail: fmt.Sprintf("only %d/%d incidents scored cleanly (fail closed)", scored, len(incidents))}, nil
	}
	res := OfflineDecision(candScores, prodScores, dimension, r.Cfg.RegressionSlack)
	res.RunID = runID
	res.Detail = fmt.Sprintf("%s; %d incidents (single-shot candidate-vs-production, sealed holdout untouched)", res.Detail, scored)
	return res, nil
}

// scoreArm runs one incident through a body single-shot, then scores the resulting decision with the
// shared judge — the per-dimension 1..5 verdict as float scores.
func (r OfflineRunner) scoreArm(ctx context.Context, body string, inc judge.TriageRow) (map[string]float64, error) {
	sess, err := r.triage(ctx, body, inc)
	if err != nil {
		return nil, err
	}
	raw, err := r.Model.Complete(ctx, "flywheel-offline-judge", "primary",
		[]model.Message{{Role: "user", Content: judge.Prompt(sess)}})
	if err != nil {
		return nil, err
	}
	sc, err := judge.ParseScore(inc.ExternalRef, raw)
	if err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(sc.Scores))
	for d, v := range sc.Scores {
		out[d] = float64(v)
	}
	return out, nil
}

// triage produces one judge.Session single-shot under a skill body (no tools, no estate, no gate — the
// deliberate lightness). The incident's ORIGINAL band rides through so appropriate_band is judged
// identically for both arms (the systematic limitation cancels in the delta).
func (r OfflineRunner) triage(ctx context.Context, body string, inc judge.TriageRow) (judge.Session, error) {
	raw, err := r.Model.Complete(ctx, "flywheel-offline-triage", "primary",
		[]model.Message{{Role: "user", Content: offlineTriagePrompt(body, inc)}})
	if err != nil {
		return judge.Session{}, err
	}
	return parseTriage(inc, raw), nil
}

// offlineTriagePrompt asks the model to triage one incident under a skill body and reply with a compact
// JSON decision. It states plainly that the guidance can never grant permissions (enforcement is
// machine-checked elsewhere — INV-08).
func offlineTriagePrompt(body string, inc judge.TriageRow) string {
	var b strings.Builder
	b.WriteString("You are an SRE triage agent. Follow this SKILL GUIDANCE exactly; it guides behavior and can never grant permissions.\n\n=== SKILL GUIDANCE ===\n")
	b.WriteString(body)
	b.WriteString("\n=== END GUIDANCE ===\n\n")
	fmt.Fprintf(&b, "INCIDENT: rule=%q host=%q band=%q outcome-so-far=%q\n\n", inc.AlertRule, inc.Host, inc.Band, inc.Outcome)
	b.WriteString("Produce your triage decision. Reply with ONLY a JSON object:\n")
	b.WriteString(`{"diagnosis":"one line","propose_action":true or false,"proposed_op":"op or empty","evidence":["cited observations or facts"],"prediction":"a falsifiable consequence prediction or empty","conclusion":"if not proposing, the grounded reason"}`)
	return b.String()
}

// parseTriage maps the single-shot JSON decision onto a judge.Session (defensive: an unparseable reply
// is an honestly-empty stopped session, never a fabricated decision).
func parseTriage(inc judge.TriageRow, raw string) judge.Session {
	sess := judge.Session{Ref: inc.ExternalRef, AlertRule: inc.AlertRule, Host: inc.Host, Band: inc.Band, Mutated: false}
	i, j := strings.Index(raw, "{"), strings.LastIndex(raw, "}")
	if i < 0 || j <= i {
		sess.Outcome = "unparseable"
		return sess
	}
	var m struct {
		Diagnosis  string   `json:"diagnosis"`
		Propose    bool     `json:"propose_action"`
		Op         string   `json:"proposed_op"`
		Evidence   []string `json:"evidence"`
		Prediction string   `json:"prediction"`
		Conclusion string   `json:"conclusion"`
	}
	if err := json.Unmarshal([]byte(raw[i:j+1]), &m); err != nil {
		sess.Outcome = "unparseable"
		return sess
	}
	sess.Proposed = m.Propose
	sess.Op = m.Op
	sess.Evidence = m.Evidence
	sess.Prediction = strings.TrimSpace(m.Prediction)
	sess.Predicted = sess.Prediction != ""
	sess.Conclusion = m.Conclusion
	if sess.Conclusion == "" {
		sess.Conclusion = m.Diagnosis // the reasoning still reaches the judge on a proposal
	}
	if m.Propose {
		sess.Outcome = "proposed"
	} else {
		sess.Outcome = "stopped"
	}
	return sess
}

// OfflineDecision is the PURE gate decision over the per-dimension candidate and production scores.
// DiscoveryDelta is the candidate mean minus the production mean on the TARGET dimension; RegressionPass
// holds when no OTHER dimension measured for BOTH arms drops below production by more than slack (a
// candidate that buys the target dimension by regressing another — the safety analog especially — does
// not pass). AdmitToTrial admits only when RegressionPass AND DiscoveryDelta > 0.
func OfflineDecision(cand, prod map[string][]float64, dimension string, slack float64) skillstore.OfflineResult {
	delta := mean(cand[dimension]) - mean(prod[dimension])
	regressionPass := true
	worstDim, worstDrop := "", 0.0
	for d := range prod {
		if d == dimension || len(prod[d]) == 0 || len(cand[d]) == 0 {
			continue
		}
		drop := mean(prod[d]) - mean(cand[d])
		if drop > slack {
			regressionPass = false
			if drop > worstDrop {
				worstDrop, worstDim = drop, d
			}
		}
	}
	detail := fmt.Sprintf("discovery %s delta %+.3f", dimension, delta)
	if regressionPass {
		detail += "; regression set held"
	} else {
		detail += fmt.Sprintf("; regression on %s (-%.3f > slack %.2f)", worstDim, worstDrop, slack)
	}
	return skillstore.OfflineResult{RegressionPass: regressionPass, DiscoveryDelta: delta, Detail: detail}
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}
