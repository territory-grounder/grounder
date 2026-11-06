package skillstore

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/territory-grounder/grounder/core/judge"
)

// The offline admission gate (spec/014 REQ-1307) — the starvation fix's first half: a draft earns its
// online trial by proving itself on the eval harness FIRST (regression must hold, the target dimension
// must improve on discovery), so online traffic is only ever spent on candidates that already cleared
// an offline bar. The sealed holdout is NEVER read here (it exists solely for the post-graduation
// integrity check) — the runner interface cannot even express it.

// OfflineResult is one candidate-vs-production harness run.
type OfflineResult struct {
	RunID          string  `json:"run_id"`
	RegressionPass bool    `json:"regression_pass"` // the regression set did not regress (no loss on a safety criterion)
	DiscoveryDelta float64 `json:"discovery_delta"` // net blind-pairwise preference on the discovery set, (wins − losses) / judged
	Detail         string  `json:"detail,omitempty"`
	// Pairwise is every incident's position-controlled verdict (TG-583) — the audit trail of WHY the gate decided,
	// incident by incident, stored on the version row with the decision.
	Pairwise []judge.PairVerdict `json:"pairwise,omitempty"`
	// Refusal, when set, refuses admission whatever the two numbers say and names why (too few outright wins for
	// the net to count as evidence). Empty means the numbers decide.
	Refusal string `json:"refusal,omitempty"`
	// Divergence is the replay probe's per-incident replicate report (TG-583 slice B): how the k replicates of each
	// body disagreed — Harness²'s "variance between replicates is the signal". Empty when the probe did not run.
	Divergence []DivergenceNote `json:"divergence,omitempty"`
}

// DivergenceNote is one incident's replicate disagreement, per body, rendered.
type DivergenceNote struct {
	Ref        string `json:"ref"`
	Candidate  string `json:"candidate"`
	Production string `json:"production"`
	Probed     bool   `json:"probed"` // false ⇒ this incident had no recording and went single-shot
}

// OfflineRunner executes the candidate-vs-production comparison on the eval sets. The production
// implementation drives the eval harness (task #26's sets); oracles use fakes. By construction it
// receives only the candidate — never a holdout handle.
type OfflineRunner interface {
	RunOffline(ctx context.Context, candidate Version, dimension string) (OfflineResult, error)
}

// OfflineEvalWriter persists the offline scores onto the version row (REQ-1307: stored either way —
// an admission refusal is as visible as a pass).
type OfflineEvalWriter interface {
	SetOfflineEval(ctx context.Context, versionID int64, eval json.RawMessage) error
}

// ErrNotAdmitted refuses a draft whose offline run failed the gate.
var ErrNotAdmitted = fmt.Errorf("skillstore: draft not admitted — offline gate failed")

// AdmitToTrial runs the offline gate for a draft and, on a pass, transitions it draft→trial through
// the audited state machine. On a refusal the draft STAYS a draft with the refusal stored on its row
// (visible in the console's version history), and ErrNotAdmitted is returned.
func AdmitToTrial(ctx context.Context, st Store, lg Ledger, runner OfflineRunner, versionID int64, dimension string) (Version, error) {
	v, err := st.GetVersion(ctx, versionID)
	if err != nil {
		return Version{}, err
	}
	if v.Status != StatusDraft {
		return Version{}, fmt.Errorf("%w: %s is %s, not draft", ErrBadTransition, v.SkillName, v.Status)
	}
	// REQ-1316 (TG-475): the class gate sits BEFORE the offline run — an ineligible class must not spend
	// an eval to be refused. Runbooks draft legitimately (the wiki promote flow) but never trial; rubric
	// cannot even draft (ErrRubricNeverDrafts), so this line is its second, independent wall.
	if sk, err := st.GetSkill(ctx, v.SkillName); err != nil {
		return Version{}, err
	} else if !FlywheelEligible(DefaultClass(sk.Class)) {
		return Version{}, fmt.Errorf("%w: class %s on %s", ErrClassNotTrialEligible, DefaultClass(sk.Class), v.SkillName)
	}
	res, err := runner.RunOffline(ctx, v, dimension)
	if err != nil {
		return Version{}, fmt.Errorf("offline run: %w", err)
	}
	blob, _ := json.Marshal(res)
	if w, ok := st.(OfflineEvalWriter); ok {
		if err := w.SetOfflineEval(ctx, v.ID, blob); err != nil {
			return Version{}, err
		}
	}
	if res.Refusal != "" {
		return v, fmt.Errorf("%w: %s (run %s)", ErrNotAdmitted, res.Refusal, res.RunID)
	}
	if !res.RegressionPass || res.DiscoveryDelta <= 0 {
		return v, fmt.Errorf("%w: regression_pass=%v discovery_delta=%.3f (run %s)",
			ErrNotAdmitted, res.RegressionPass, res.DiscoveryDelta, res.RunID)
	}
	return Transition(ctx, st, lg, v.ID, StatusTrial,
		fmt.Sprintf("offline gate passed: regression held, %s +%.3f on discovery (run %s)", dimension, res.DiscoveryDelta, res.RunID))
}
