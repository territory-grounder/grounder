package main

// History / prior-verdict READER ADAPTERS, carved out of main()'s composition root (TG-501 LOC-debt
// paydown). Each is a 1:1 projection bridging a DB store or a tracker capability to a module's narrow
// read-only Reader seam, so the module imports no DB/vendor code and unit-tests against a fake. Behaviour
// is unchanged by the move; the sole consumers are the incidenthistory.New / trackerhistory.New call sites
// still in main().

import (
	"context"
	"time"

	tracker "github.com/territory-grounder/grounder/adapters/tracker"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/modules/observability/incidenthistory"
	"github.com/territory-grounder/grounder/modules/tracker/trackerhistory"
	"github.com/territory-grounder/grounder/temporal/runner"
)

// priorVerdictReader adapts the pgx durable actuation-verdict read (db.PriorVerdictStore) to the Runner's
// narrow PriorVerdicts seam, applying the operator-declared RECENCY BOUND (spec/001 REQ-015, TG-223): only
// verdicts recorded inside `window` are handed to the classifier, so an ancient deviation can never pin a
// host to POLL_PAUSE forever. The window is evaluated per call (time.Now at read time), never captured at
// boot, so a long-running worker's window does not silently slide into an absolute cutoff.
//
// Rule-FAMILY scoping is deliberately NOT applied here: the fold belongs with the one family authority in
// the Runner (core/knowledge.CanonicalRule), exactly as the incident-history tool folds its own rows — a
// SQL alias list would re-create the two-vocabulary drift the recovery belt already paid for. This is a
// 1:1 projection plus the time bound and the verdict-validity screen; a ledger row outside
// {match, partial, deviation} is passed through UNCHANGED so the classifier can treat it as a deviation
// (fail closed), never dropped as if it were clean.
func priorVerdictReader(store *db.PriorVerdictStore, window time.Duration) func(context.Context, string) ([]runner.PriorVerdict, error) {
	return func(ctx context.Context, host string) ([]runner.PriorVerdict, error) {
		rows, err := store.RecentForHost(ctx, host, time.Now().UTC().Add(-window), 50)
		if err != nil {
			return nil, err
		}
		out := make([]runner.PriorVerdict, 0, len(rows))
		for _, r := range rows {
			out = append(out, runner.PriorVerdict{
				Verdict:   safety.Verdict(r.Verdict),
				AlertRule: r.AlertRule,
				At:        r.At,
			})
		}
		return out, nil
	}
}

// incidentHistoryReader adapts the pgx prior-session read (db.IncidentHistoryStore) to the
// incident-history tool's narrow Reader seam. The module restates its own row type so it imports no DB
// code and its formatting logic unit-tests against a fake; this 1:1 projection is the only bridge.
func incidentHistoryReader(store *db.IncidentHistoryStore) incidenthistory.Reader {
	return func(ctx context.Context, host string, limit int) ([]incidenthistory.PriorIncident, error) {
		rows, err := store.PriorSessions(ctx, host, limit)
		if err != nil {
			return nil, err
		}
		out := make([]incidenthistory.PriorIncident, 0, len(rows))
		for _, r := range rows {
			out = append(out, incidenthistory.PriorIncident{
				ExternalRef:    r.ExternalRef,
				Rule:           r.AlertRule,
				Outcome:        r.Outcome,
				OpClass:        r.OpClass,
				Proposed:       r.Proposed,
				Mutated:        r.Mutated,
				ConfirmedClear: r.ConfirmedClear,
				Conclusion:     r.Conclusion,
				At:             r.CreatedAt,
			})
		}
		return out, nil
	}
}

// trackerHistoryReader adapts the tracker HISTORY CAPABILITY to the trackerhistory tool's reader seam.
//
// It takes adapters/tracker.History rather than a concrete vendor module, which is the whole point: the
// previous signature took *youtrack.Module, and that one parameter type is what excluded every other
// tracker backend from tracker history by construction. Query-building and comment extraction now live
// in each backend, where the vendor's query language actually is.
//
// The capability is READ-ONLY by construction — it has one method and it searches — so this path cannot
// write to the shared corpus even by mistake.
func trackerHistoryReader(h tracker.History) trackerhistory.Reader {
	return func(ctx context.Context, host, rule string, limit int) ([]trackerhistory.TrackedIncident, error) {
		incidents, err := h.SearchIncidents(ctx, host, rule, limit)
		if err != nil {
			return nil, err // an unreadable tracker is an outage, never "this estate has no history"
		}
		out := make([]trackerhistory.TrackedIncident, 0, len(incidents))
		for _, i := range incidents {
			ti := trackerhistory.TrackedIncident{
				ID: i.ID, Summary: i.Summary, State: i.State, Filed: i.Filed, Comments: i.Comments,
			}
			// With several trackers merged, an unqualified id names nothing a reader can go look up.
			if i.Source != "" {
				ti.ID = i.Source + ":" + i.ID
			}
			out = append(out, ti)
		}
		return out, nil
	}
}
