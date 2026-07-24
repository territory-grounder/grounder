package skillstore

import (
	"context"
	"encoding/json"
	"fmt"
)

// The offline admission gate (spec/014 REQ-1307) — the starvation fix's first half: a draft earns its
// online trial by proving itself on the eval harness FIRST (regression must hold, the target dimension
// must improve on discovery), so online traffic is only ever spent on candidates that already cleared
// an offline bar. The sealed holdout is NEVER read here (it exists solely for the post-graduation
// integrity check) — the runner interface cannot even express it.

// OfflineResult is one candidate-vs-production harness run.
type OfflineResult struct {
	RunID          string  `json:"run_id"`
	RegressionPass bool    `json:"regression_pass"` // the regression set did not regress
	DiscoveryDelta float64 `json:"discovery_delta"` // target-dimension delta on the discovery set
	Detail         string  `json:"detail,omitempty"`
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
	if !res.RegressionPass || res.DiscoveryDelta <= 0 {
		return v, fmt.Errorf("%w: regression_pass=%v discovery_delta=%.3f (run %s)",
			ErrNotAdmitted, res.RegressionPass, res.DiscoveryDelta, res.RunID)
	}
	return Transition(ctx, st, lg, v.ID, StatusTrial,
		fmt.Sprintf("offline gate passed: regression held, %s +%.3f on discovery (run %s)", dimension, res.DiscoveryDelta, res.RunID))
}
