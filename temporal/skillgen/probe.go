package skillgen

import (
	"context"
	"fmt"
	"strings"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/skillstore"
	"github.com/territory-grounder/grounder/core/trace"
	"github.com/territory-grounder/grounder/eval"
	"github.com/territory-grounder/grounder/temporal/runner"
)

// THE REPLAY PROBE IN THE OFFLINE GATE (TG-583 slice B, Harness² learnings H2-6d).
//
// When a discovery incident has a recorded trajectory (agent_step + agent_step_evidence, TG-580), the gate no
// longer simulates the agent with a single-shot triage: it re-runs the REAL loop (runner.ReplayProbe) against the
// recording — once per body, k replicates each — and puts THOSE deliverables to the blind pairwise judge. Incidents
// without a recording keep the single-shot path, and the result says which incidents were probed. The k replicates
// of each body are compared pairwise in alignment (candidate replicate i against production replicate i): a
// preference that every aligned pair shares is the incident's verdict; one that depends on the replicate is a tie
// that says so, and each body's own replicate disagreement is kept as the divergence report.

// Recordings loads a session's recorded trajectory; a session with none yields an empty recording (not an error).
type Recordings interface {
	Load(ctx context.Context, externalRef string) (eval.RecordedSession, error)
}

// StepRecordings is the production Recordings: the agent_step and agent_step_evidence stores.
type StepRecordings struct {
	Steps interface {
		Steps(ctx context.Context, externalRef string) ([]trace.AgentStep, error)
	}
	Evidence trace.AgentStepEvidenceLister
}

// Load joins steps and evidence into the replay recording.
func (s StepRecordings) Load(ctx context.Context, externalRef string) (eval.RecordedSession, error) {
	return eval.LoadRecordedSession(ctx, s.Steps, s.Evidence, externalRef)
}

// ProbeRun is the probe entrypoint — runner.ReplayProbe in production, a fake in the oracles.
type ProbeRun func(ctx context.Context, deps runner.ProbeDeps, newTools func() *agent.ToolSet, inc runner.ProbeIncident, override func([]skillstore.ProductionRow) []skillstore.ProductionRow, k int) ([]runner.ProbeDeliverable, error)

// ProbeConfig arms the replay probe in the offline gate. Replicates <= 0 or a nil config ⇒ the probe is off and
// every incident goes single-shot.
type ProbeConfig struct {
	Replicates int
	Deps       runner.ProbeDeps
	Recordings Recordings
	Run        ProbeRun // nil ⇒ runner.ReplayProbe
}

func (p *ProbeConfig) enabled() bool { return p != nil && p.Replicates > 0 && p.Recordings != nil }

func (p *ProbeConfig) run() ProbeRun {
	if p.Run != nil {
		return p.Run
	}
	return runner.ReplayProbe
}

// probeOutcome is one incident's probe: the aggregated pairwise verdict and both bodies' divergence.
type probeOutcome struct {
	verdict judge.PairVerdict
	note    skillstore.DivergenceNote
	cand    string // the candidate's first deliverable, rendered for the composer
	prod    string // the production body's first deliverable, rendered
}

func renderDeliverable(d runner.ProbeDeliverable) string {
	s := fmt.Sprintf("band=%q outcome=%q proposed=%v", d.Band, d.Outcome, d.Proposed)
	if d.Op != "" {
		s += fmt.Sprintf(" op=%q", d.Op)
	}
	if d.Steps > 0 {
		s += fmt.Sprintf(" steps=%d", d.Steps)
	}
	return s
}

func renderVerdict(v judge.PairVerdict) string {
	switch {
	case v.Winner == "left":
		return fmt.Sprintf("candidate preferred (criterion %d)", v.DecidedBy)
	case v.Winner == "right":
		return fmt.Sprintf("production preferred (criterion %d)", v.DecidedBy)
	case v.Identical:
		return "identical deliverables"
	case v.PositionDependent:
		return "tie (the judge followed the position)"
	case v.ReplicateDependent:
		return "tie (replicate-dependent)"
	}
	return "tie"
}

// probePair re-runs the real loop for both bodies against the incident's recording and judges the aligned
// replicate pairs. The replay tool-set is built fresh for every replicate (its cursor is per set).
func (r OfflineRunner) probePair(ctx context.Context, rec eval.RecordedSession, skillName, candBody, prodBody string, inc judge.TriageRow) (probeOutcome, error) {
	p := r.Probe
	var g *estate.Graph
	if p.Deps.Graph != nil {
		g = p.Deps.Graph()
	}
	if g == nil {
		g = estate.NewGraph()
	}
	// The incident framing the loop sees: the recorded session's own identity. The compact record carries no
	// severity or summary, so the probe runs at WARNING with no summary — the same for both bodies, so it cancels.
	einc := eval.Incident{ExternalRef: inc.ExternalRef, SourceID: "offline-probe", AlertRule: inc.AlertRule, Host: inc.Host, Severity: "warning"}
	pinc := runner.ProbeIncident{ExternalRef: inc.ExternalRef, SourceID: "offline-probe", AlertRule: inc.AlertRule, Host: inc.Host, Severity: ingest.SeverityWarning}
	newTools := func() *agent.ToolSet { return eval.NewReplayToolSet(rec, einc, g) }
	cands, err := p.run()(ctx, p.Deps, newTools, pinc, runner.WithBody(skillName, candBody), p.Replicates)
	if err != nil {
		return probeOutcome{}, fmt.Errorf("probe candidate: %w", err)
	}
	prods, err := p.run()(ctx, p.Deps, newTools, pinc, runner.WithBody(skillName, prodBody), p.Replicates)
	if err != nil {
		return probeOutcome{}, fmt.Errorf("probe production: %w", err)
	}
	if len(cands) == 0 || len(prods) == 0 {
		return probeOutcome{}, fmt.Errorf("probe returned no deliverable")
	}
	n := len(cands)
	if len(prods) < n {
		n = len(prods)
	}
	var verdicts []judge.PairVerdict
	for i := 0; i < n; i++ {
		v, err := judge.ComparePair(ctx, pairCompleter{r.Model}, judge.Pair{Ref: inc.ExternalRef, Left: cands[i].Session(pinc), Right: prods[i].Session(pinc)})
		if err != nil {
			return probeOutcome{}, err
		}
		verdicts = append(verdicts, v)
	}
	return probeOutcome{
		verdict: aggregateAligned(verdicts),
		note: skillstore.DivergenceNote{Ref: inc.ExternalRef, Probed: true,
			Candidate: runner.Diverge(cands).String(), Production: runner.Diverge(prods).String()},
		cand: renderDeliverable(cands[0]),
		prod: renderDeliverable(prods[0]),
	}, nil
}

// The offline runner IS the composer's prober (skillstore derives it from CreationDeps.Runner) — pinned at compile time.
var _ skillstore.Prober = OfflineRunner{}

// composeProbeIncidents bounds how many recorded incidents the COMPOSER's per-proposal probe runs (slice C): the
// composer reads a report, not a verdict, and each incident costs 2 × k real loops.
const composeProbeIncidents = 3

// ProbeDraft is skillstore.Prober for the composer (TG-583 slice C): the draft body and the production body re-run
// through the real loop against up to composeProbeIncidents recorded discovery incidents (the sessions the proposer
// read, excludeRefs, skipped), rendered one line per incident — deliverables, divergence, the pairwise verdict. ""
// when the probe is off, the skill has no production version, or no incident with a recording remains.
func (r OfflineRunner) ProbeDraft(ctx context.Context, skillName, body string, excludeRefs map[string]bool) (string, error) {
	if !r.Probe.enabled() {
		return "", nil
	}
	prod, ok, err := r.Store.ProductionVersion(ctx, skillName)
	if err != nil || !ok {
		return "", err
	}
	incidents, err := r.Incidents.RecentJudgedByVersion(ctx, prod.ID, r.Cfg.Window, r.Cfg.DiscoveryLimit)
	if err != nil {
		return "", err
	}
	var lines []string
	for _, inc := range incidents {
		if len(lines) >= composeProbeIncidents {
			break
		}
		if excludeRefs[inc.ExternalRef] {
			continue
		}
		rec, err := r.Probe.Recordings.Load(ctx, inc.ExternalRef)
		if err != nil || len(rec.Calls) == 0 {
			continue
		}
		out, err := r.probePair(ctx, rec, skillName, body, prod.Body, inc)
		if err != nil {
			lines = append(lines, fmt.Sprintf("%s: probe failed — %v", inc.ExternalRef, err))
			continue
		}
		lines = append(lines, fmt.Sprintf("%s: candidate → %s (%s); production → %s (%s); pairwise: %s",
			inc.ExternalRef, out.cand, out.note.Candidate, out.prod, out.note.Production, renderVerdict(out.verdict)))
	}
	return strings.Join(lines, "\n"), nil
}

// aggregateAligned folds the aligned replicate pairs' verdicts into one: every pair naming the same winner is
// that winner (on the earliest criterion any pair decided on); otherwise a tie whose rationale says the preference
// depended on the replicate. Identical-or-positional ties pass through when every pair tied.
func aggregateAligned(vs []judge.PairVerdict) judge.PairVerdict {
	if len(vs) == 1 {
		return vs[0]
	}
	out := judge.PairVerdict{Ref: vs[0].Ref}
	winner, decided, allIdentical, allPositional := vs[0].Winner, vs[0].DecidedBy, true, true
	safetyLoss := 0 // the earliest safety criterion any aligned pair LOST on (0 = none)
	for _, v := range vs {
		out.Calls += v.Calls
		out.Why = append(out.Why, v.Why...)
		if !v.Identical {
			allIdentical = false
		}
		if !v.PositionDependent {
			allPositional = false
		}
		if v.Winner == "right" && v.DecidedBy >= 1 && v.DecidedBy <= judge.SafetyCriteria && (safetyLoss == 0 || v.DecidedBy < safetyLoss) {
			safetyLoss = v.DecidedBy
		}
		if v.Winner != winner {
			winner = "" // the replicates disagreed on the direction
		}
		if v.DecidedBy > 0 && (decided == 0 || v.DecidedBy < decided) {
			decided = v.DecidedBy
		}
	}
	// A safety-criterion LOSS in any replicate is the incident's verdict: a body that loses on DELIVERY or BAND in
	// one run out of k is a body that sometimes acts unsafely — the regression set must see it, whatever the other
	// replicates did (review of slice B: folding it into a tie let the safety direction fail open for k>1).
	if safetyLoss > 0 {
		out.Winner, out.DecidedBy = "right", safetyLoss
		out.Why = append(out.Why, fmt.Sprintf("safety: the candidate LOST on criterion %d in at least one of %d aligned replicate pairs — recorded as the loss", safetyLoss, len(vs)))
		return out
	}
	out.Identical = allIdentical
	out.PositionDependent = allPositional && !allIdentical
	if winner == "" {
		if !allIdentical && !allPositional {
			out.ReplicateDependent = true
			out.Why = append(out.Why, fmt.Sprintf("replicate-dependent: %d aligned pairs did not agree on a winner — the body leaves this decision to chance", len(vs)))
		}
		return out
	}
	out.Winner, out.DecidedBy = winner, decided
	return out
}

// excludeCited drops the discovery incidents the candidate's rationale cites (the sessions the proposer READ,
// TG-582 rows `… -> <ref>:[STEP n] …`) and names them, so the gate judges transfer, not fit.
func excludeCited(incidents []judge.TriageRow, rationale string) ([]judge.TriageRow, []string) {
	cited := skillstore.CitedRefs(rationale)
	if len(cited) == 0 {
		return incidents, nil
	}
	var kept []judge.TriageRow
	var dropped []string
	for _, inc := range incidents {
		if cited[inc.ExternalRef] {
			dropped = append(dropped, inc.ExternalRef)
			continue
		}
		kept = append(kept, inc)
	}
	return kept, dropped
}

func joinRefs(refs []string) string { return strings.Join(refs, ", ") }
