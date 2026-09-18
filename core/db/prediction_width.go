package db

import (
	"context"
	"fmt"
)

// prediction_width.go — HOW WIDE IS THE BLAST-RADIUS PREDICTION, AND HOW OFTEN IS IT EMPTY? (TG-352)
//
// core/risk/classifier.go forces extra review when BlastRadiusWide is set, and that flag is
// "the predicted blast-radius exceeds the configured threshold". So an EMPTY predicted set is not neutral:
// it is one fewer reason to demand a human.
//
// Measured over all 2,047 rows of infragraph_prediction on 2026-08-07:
//
//	empty predictions            1386 of 2047   (67.7%)
//	avg width, UNSCORED (1693)    6.2 hosts, 10.8% over the wide threshold
//	avg width, SCORED    (354)   43.9 hosts, 45.5% over it
//
// TG-352 reports "~44 hosts per incident" and infers a loss of discrimination. That average is taken over
// the SCORED rows only, and scoring is heavily biased toward the wide ones — a prediction can only be
// scored if an outcome was observed. Across the full population the average is 12.7 and 83% of
// predictions sit at or under the threshold, so the flag does discriminate.
//
// The finding the scoped average hid is the opposite one: the predictor returns NOTHING two-thirds of the
// time, and for blast radius that is the LESS cautious direction.
//
// Counts only; no payload is read.
type PredictionWidth struct {
	// Rows is the denominator, published even at zero so an absent series is the register being gone.
	Rows int64
	// Empty counts predictions naming NO host — the reading that removes a poll-forcing reason.
	Empty int64
	// Wide counts predictions over the threshold the risk classifier uses.
	Wide int64
	// Scored counts rows with an outcome (tp IS NOT NULL) — the sub-population every precision figure
	// about this table is computed over, published so nobody quotes a precision without its base.
	Scored int64
	// EmptyOnConnected counts the only empty prediction that is WRONG: one whose target has dependents in
	// the estate graph.
	//
	// Bare emptiness is not a defect and counting it is a trap I fell into. Measured 2026-08-07: all 1386
	// empty predictions were on targets with in-degree ZERO — genuine leaves — and TG's actuation allowlist
	// is specifically leaf app guests, so restarting one affects nothing else and an empty blast radius is
	// the CORRECT answer. A rule on tg_prediction_empty would page on health.
	//
	// This is the discriminating count: the predictor returning "nothing is affected" about a host that
	// 52 other things depend on is the state that means it has gone blind. It is currently 0, and that 0
	// is meaningful precisely because the denominator beside it is 1386.
	EmptyOnConnected int64
}

// CountPredictionWidth measures the prediction population against the caller's wide threshold. The
// threshold is passed in rather than read here so it comes from the SAME env the risk classifier uses —
// a register with its own copy would report on a boundary the classifier does not enforce.
func (s *Pool) CountPredictionWidth(ctx context.Context, wideThreshold int) (PredictionWidth, error) {
	var w PredictionWidth
	// The in-degree is taken from the LARGEST recent snapshot rather than the newest: the estate refresh
	// writes a small 17-edge actuation-plane snapshot interleaved with the full triage one (TG-346), and
	// keying on the newest row would compute every host's in-degree as 0 — which would report every empty
	// prediction as correct, the exact reassuring direction this counter exists to resist.
	const q = `
		WITH latest AS (
		    SELECT graph_json FROM estate_snapshot
		    WHERE jsonb_array_length(graph_json->'nodes') > 100
		    ORDER BY captured_at DESC LIMIT 1
		),
		indeg AS (
		    SELECT e->>'to_name' AS host, count(*) AS n
		    FROM latest, LATERAL jsonb_array_elements(graph_json->'edges') e
		    GROUP BY 1
		)
		SELECT count(*),
		       count(*) FILTER (WHERE jsonb_array_length(p.predicted_hosts) = 0),
		       count(*) FILTER (WHERE jsonb_array_length(p.predicted_hosts) > $1),
		       count(*) FILTER (WHERE p.tp IS NOT NULL),
		       count(*) FILTER (WHERE jsonb_array_length(p.predicted_hosts) = 0
		                          AND COALESCE(i.n, 0) > 0)
		FROM infragraph_prediction p
		LEFT JOIN indeg i ON i.host = p.target_host`
	if err := s.QueryRow(ctx, q, wideThreshold).
		Scan(&w.Rows, &w.Empty, &w.Wide, &w.Scored, &w.EmptyOnConnected); err != nil {
		return PredictionWidth{}, fmt.Errorf("db: prediction width: %w", err)
	}
	return w, nil
}
