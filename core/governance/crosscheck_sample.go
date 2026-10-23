package governance

// The SAMPLE port of the frontier cross-check (spec/004 REQ-307, TG-222): the recently-ended sessions to
// re-judge, each carrying the LOCAL judge's reading of it.
//
// It lives in core/ so the pgx reader (core/db) and the frontier re-judger (temporal/governance) both depend
// on this one type instead of on each other. The monitor's decision logic never sees it — the monitor
// consumes CrossCheckPair — so the pure Evaluate stays independent of how a pair was produced.

import "context"

// CrossCheckRow is one sampled session: the facts needed to rebuild the shared judge prompt, plus the local
// judge's reading. LocalScored is false when the local judge produced no positively-scored dimension, which
// in TG is an ABSENT session_judgment row rather than the predecessor's `-1` sentinel — the judge cron omits
// a dimension it did not score instead of fabricating one.
type CrossCheckRow struct {
	ExternalRef string
	Host        string
	AlertRule   string
	Band        string
	Outcome     string
	Proposed    bool
	Op          string
	Conclusion  string
	Prediction  string
	LocalScored bool
	LocalMean   float64
}

// CrossCheckSampleStore yields the sessions to re-judge. core/db.GovernanceReadStore satisfies it in
// production; a fake drives the oracles.
type CrossCheckSampleStore interface {
	RecentForCrossCheck(ctx context.Context, limit int) ([]CrossCheckRow, error)
}
