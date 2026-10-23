package runner

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/execclass"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/screen"
	"github.com/territory-grounder/grounder/core/skillstore"
)

// REPLAY PROBE (TG-583 slice B, Harness² learnings H2-6d).
//
// Harness² probes a candidate harness by rolling it out on the SAME task the base ran, k times, and reads the
// variance between replicates as the signal ("where they diverged the harness is leaving a decision to chance").
// TG's offline admission gate judged a candidate skill body on a single-shot, tool-less triage — a simulation of
// the agent, not the agent. This probe runs the REAL investigate loop — the production InvestigateActivity with
// the production composition (composeGuidance over the store rows, the pack, the preamble) and the production
// classify stage for the band — against a replay tool-set that serves a past session's RECORDED observations
// (eval.NewReplayToolSet, TG-580), with one thing changed: the skill rows the composer reads carry the body under
// test. Everything the loop would have done to the estate is inert here: a read-only chokepoint, an in-memory
// ledger and prediction store, a manifest sink that keeps nothing, a commit-confirm recorder that arms nothing,
// and no step sink — a probe writes no session_triage, no agent_step, no ledger row.
//
// What a probe deliverable proves: what THIS body would have had the agent conclude and propose for THIS
// incident, given exactly the observations the original session saw. What it does not prove: anything about
// observations the original never made (a novel call is answered by the replay refusal, and the agent reasons
// from that), and nothing about live estate state. k replicates expose the loop's own variance; ProbeDivergence
// counts it. The deliverables are then put to the blind pairwise judge (core/judge.ComparePair) exactly like the
// single-shot ones were.

// ProbeDeps are the production collaborators a probe borrows: the gateway, the loop limits, the estate graph
// (host-type predicates for routing and classification) and the store's production rows (the candidate body is
// swapped in per replicate by the rows override).
type ProbeDeps struct {
	Model     agent.Completer
	Limits    agent.Limits // zero ⇒ agent.DefaultLimits()
	Graph     func() *estate.Graph
	SkillRows func(context.Context) ([]skillstore.ProductionRow, error)
}

// ProbeIncident is the incident framing a probe runs under — the recorded session's own identity.
type ProbeIncident struct {
	ExternalRef, SourceID, AlertRule, Host, Site, Summary string
	Severity                                              ingest.Severity
	Labels                                                map[string]string
	ExecClass                                             string // "" ⇒ STANDARD_AGENT
}

// ProbeDeliverable is one replicate's terminal record — the fields the judge adjudicates, nothing it must not see.
type ProbeDeliverable struct {
	Band, Outcome, Op, OpClass, Target, Prediction, Conclusion string
	Proposed                                                   bool
	Evidence                                                   []string
	Steps                                                      int
	Tools                                                      []string // the ordered tool path
	HarnessFP                                                  string
	SkillLoads                                                 []string
}

// Session renders the deliverable as the judge's Session for this incident.
func (d ProbeDeliverable) Session(inc ProbeIncident) judge.Session {
	return judge.Session{
		Ref: inc.ExternalRef, AlertRule: inc.AlertRule, Host: inc.Host, Severity: inc.Severity.String(),
		Band: d.Band, Proposed: d.Proposed, Op: d.Op, Prediction: d.Prediction, Predicted: d.Prediction != "",
		Evidence: d.Evidence, Conclusion: d.Conclusion, Outcome: d.Outcome,
	}
}

// ProbeDivergence counts how the k replicates disagreed — the Harness² signal that the body leaves a decision to
// chance. Divergent is true when the replicates did not all agree on band, outcome and op.
type ProbeDivergence struct {
	Replicates int            `json:"replicates"`
	Bands      map[string]int `json:"bands"`
	Outcomes   map[string]int `json:"outcomes"`
	Ops        map[string]int `json:"ops"`
	Divergent  bool           `json:"divergent"`
}

// Diverge summarizes the replicates' disagreement.
func Diverge(reps []ProbeDeliverable) ProbeDivergence {
	d := ProbeDivergence{Replicates: len(reps), Bands: map[string]int{}, Outcomes: map[string]int{}, Ops: map[string]int{}}
	for _, r := range reps {
		d.Bands[r.Band]++
		d.Outcomes[r.Outcome]++
		d.Ops[r.Op]++
	}
	d.Divergent = len(d.Bands) > 1 || len(d.Outcomes) > 1 || len(d.Ops) > 1
	return d
}

// String renders the divergence for a detail line, deterministically.
func (d ProbeDivergence) String() string {
	if !d.Divergent {
		return fmt.Sprintf("%d replicate(s) agreed", d.Replicates)
	}
	return fmt.Sprintf("%d replicates DIVERGED — bands %s, outcomes %s, ops %s", d.Replicates, counts(d.Bands), counts(d.Outcomes), counts(d.Ops))
}

func counts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%q×%d", k, m[k]))
	}
	return strings.Join(parts, " ")
}

// WithBody is the rows override that swaps ONE skill's body — the candidate under test — leaving every other
// row, and the row's identity (version label, predicate), exactly as the store serves them. The row's content
// hash is RECOMPUTED over the new body: the registry refuses a row whose hash does not match its body and sends
// the WHOLE session back to the compiled skills ("fallback=row … content hash mismatch"), so a swap that left the
// production hash in place would probe the compiled body under the candidate's name — exactly what the trial-arm
// swap (applyTrialArms) also has to do, and what this probe's first oracle caught.
func WithBody(skillName, body string) func([]skillstore.ProductionRow) []skillstore.ProductionRow {
	return func(rows []skillstore.ProductionRow) []skillstore.ProductionRow {
		out := make([]skillstore.ProductionRow, len(rows))
		copy(out, rows)
		for i := range out {
			if out[i].SkillName == skillName {
				out[i].Body = body
				out[i].ContentHash = skillstore.ContentHash(body, out[i].AppliesWhen)
			}
		}
		return out
	}
}

// ReplayProbe runs the real loop k times for one incident against the given (replay) tool-set, with the store's
// production rows passed through override before composition. Each replicate is a fresh Activities over inert
// collaborators; the band is the production classify stage's over the replicate's proposal (a stop has no band,
// exactly as live). A replicate that errors fails the probe — no deliverable is fabricated from a failed run.
//
// newTools is called ONCE PER REPLICATE: a replay tool-set carries a cursor over the recording (an observation
// served once is then an "exhausted" repeat), so two replicates sharing one tool-set would not see the same world.
func ReplayProbe(ctx context.Context, deps ProbeDeps, newTools func() *agent.ToolSet, inc ProbeIncident, override func([]skillstore.ProductionRow) []skillstore.ProductionRow, k int) ([]ProbeDeliverable, error) {
	if k <= 0 {
		k = 1
	}
	if deps.Model == nil || newTools == nil {
		return nil, fmt.Errorf("replay probe: a model and a tool-set factory are required")
	}
	limits := deps.Limits
	if limits == (agent.Limits{}) {
		limits = agent.DefaultLimits()
	}
	var g *estate.Graph
	if deps.Graph != nil {
		g = deps.Graph()
	}
	if g == nil {
		g = estate.NewGraph()
	}
	rows := deps.SkillRows
	if override != nil && rows != nil {
		inner := rows
		rows = func(ctx context.Context) ([]skillstore.ProductionRow, error) {
			r, err := inner(ctx)
			if err != nil {
				return nil, err
			}
			return override(r), nil
		}
	}
	class := inc.ExecClass
	if class == "" {
		class = string(execclass.StandardAgent)
	}
	env := ingest.IncidentEnvelope{ExternalRef: inc.ExternalRef, SourceID: inc.SourceID, AlertRule: inc.AlertRule,
		Host: inc.Host, Severity: inc.Severity, Site: inc.Site, Summary: inc.Summary, Labels: inc.Labels}
	out := make([]ProbeDeliverable, 0, k)
	for i := 0; i < k; i++ {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		tools := newTools()
		if tools == nil {
			return out, fmt.Errorf("replay probe %s replicate %d: the tool-set factory returned nil", inc.ExternalRef, i+1)
		}
		d := Deps{
			Model: deps.Model, Tools: tools, Limits: limits, SkillRows: rows,
			DurableCheckpointOff: true, // a probe runs inside the GENERATOR's activity ctx — never heartbeat on it
			Gate: &predict.PredictionGate{
				Store: predict.NewMemPredictionStore(),
				Model: &predict.InfragraphModel{Estate: g, DefaultRules: []string{"HostDown", "HighLatency"}, MaxDepth: 3},
				Mode:  predict.ModeEnforce,
			},
			Ledger:        audit.NewLedger(),
			Mutation:      safety.NewReadOnlyChokepoint(), // a probe never actuates
			ManifestSink:  probeManifestSink{},
			CommitConfirm: probeCommitConfirm{},
			HostIsGuest:   g.IsGuest,
			HostIsPveNode: g.IsPveNode,
		}
		acts := NewActivities(d)
		inv, err := acts.InvestigateActivity(ctx, env, class, ClusterMemberContext{})
		if err != nil {
			return out, fmt.Errorf("replay probe %s replicate %d: investigate: %w", inc.ExternalRef, i+1, err)
		}
		del := ProbeDeliverable{
			Outcome: inv.Outcome, Proposed: inv.Proposed, Conclusion: inv.Conclusion, HarnessFP: inv.HarnessFP,
			SkillLoads: inv.SkillLoads, Steps: len(inv.Trajectory),
		}
		for _, st := range inv.Trajectory {
			del.Tools = append(del.Tools, st.Tool)
		}
		if inv.Proposed {
			del.Op, del.OpClass, del.Target = inv.Proposal.Action.Op, inv.Proposal.Action.OpClass, inv.Proposal.Action.Target
			del.Prediction, del.Evidence = inv.Proposal.Prediction, inv.Proposal.EvidenceIDs
			band, err := probeBand(ctx, acts, env, inv)
			if err != nil {
				return out, fmt.Errorf("replay probe %s replicate %d: classify: %w", inc.ExternalRef, i+1, err)
			}
			del.Band = band
		} else {
			del.Evidence = inv.ConclusionEvidence
		}
		out = append(out, del)
	}
	return out, nil
}

// probeBand runs the production classify stage over the replicate's proposal, built exactly as RunnerWorkflow
// builds it (actionID from the content-addressed action, the plan hash, the severity's risk level, the
// safety/jailbreak screens over the proposal) — with no attribution seams, which the probe does not run.
func probeBand(ctx context.Context, acts *Activities, env ingest.IncidentEnvelope, inv InvestigateResult) (string, error) {
	actionID, err := inv.Proposal.Action.ID()
	if err != nil {
		return "", err
	}
	ci := ClassifyInput{
		ExternalRef:       env.ExternalRef,
		ActionID:          actionID,
		PlanHash:          PlanHash(env.ExternalRef, actionID),
		RiskLevel:         riskLevel(env.Severity),
		AlertRule:         env.AlertRule,
		OpClass:           inv.Proposal.Action.OpClass,
		Op:                inv.Proposal.Action.Op,
		Host:              inv.Proposal.Action.Target,
		Rationale:         inv.Proposal.Rationale,
		IncidentHost:      env.Host,
		Reversible:        inv.Proposal.Action.Reversible,
		Jailbreak:         screen.IsJailbreak(inv.Proposal.Rationale + " " + inv.Proposal.ApprovalChoice),
		Stateful:          safety.IsStatefulWorkload(actionSafetyParts(inv.Proposal.Action)...),
		Destructive:       safety.IsDestructiveOp(append(actionSafetyParts(inv.Proposal.Action), inv.Proposal.Rationale)...),
		EvidenceIDs:       inv.Proposal.EvidenceIDs,
		ToolResults:       inv.ToolResults,
		AutoResolveMarked: autoResolveMarked(inv.Proposal.ApprovalChoice),
		HighRiskCategory:  safety.HighRiskCategory(env.Labels["category"]),
	}
	decision, err := acts.ClassifyActivity(ctx, ci)
	if err != nil {
		return "", err
	}
	return decision.Band.String(), nil
}

// probeManifestSink keeps nothing: a probe's sealed manifest is a by-product, never a record.
type probeManifestSink struct{}

func (probeManifestSink) Seal(context.Context, *manifest.ActionManifest) error { return nil }

// probeCommitConfirm arms and resolves nothing: a probe never reaches an effect.
type probeCommitConfirm struct{}

func (probeCommitConfirm) ArmCommitConfirm(context.Context, db.CommitConfirmRow) error { return nil }
func (probeCommitConfirm) Resolve(context.Context, string, string, string, string, string) error {
	return nil
}
