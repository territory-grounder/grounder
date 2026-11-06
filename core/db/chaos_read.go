package db

import (
	"context"
	"fmt"
	"time"

	"github.com/territory-grounder/grounder/core/estate"
)

// DefaultChaosCascadeWindow is how long after an injection a DIFFERENT host's alert counts as CAUSED by that
// injection — the propagation window for a chaos-calibrated dependency (TG-188). It mirrors the co-occurrence
// learner's cascade window: a real cascade propagates in minutes, and a tight window is the coincidence control
// that lets chaos emit on a SINGLE ground-truth injection without admitting unrelated co-alerters. It is a
// Postgres interval literal passed as a BOUND PARAMETER (cast ::interval), never concatenated into SQL.
const DefaultChaosCascadeWindow = "10 minutes"

// DefaultChaosRecoveryWindow bounds how long after a downstream's cascade alert its recovery transition may
// arrive and still be attributed to that alert (TG-188 slice 2c). Recovery is a much slower process than
// propagation — dominated by the monitoring poll and the operator/self-heal — so it is HOURS where the cascade
// window is minutes. It reuses the value the A6b time-to-recovery axis already trusts (axis_read.go
// healCorrelationWindow = "6 hours"): the same reviewed, live-verified bound on the same (host + time-ordering)
// correlation, since a recovery never shares an external_ref with the raise it clears. Passed as a BOUND
// PARAMETER (cast ::interval), never concatenated into SQL.
const DefaultChaosRecoveryWindow = "6 hours"

// ChaosCascades reads the injection ledger (injected_fault) and returns, for every (injected root, downstream
// co-alerter) pair, how many DISTINCT injections of the root that downstream host followed within cascadeWindow,
// the most recent such injection (the freshness anchor for the edge's expiry), the mean propagation delay, and
// the mean RECOVERY time / MTTR of the downstream. Root is GROUND TRUTH — we injected it — which is why the
// resulting SourceChaos edges rate 0.90, above the learned cap (0.75). since bounds the lookback (the worker
// passes now−estate.ChaosEdgeTTL, so an injection whose edge would already be expired is never read). SAME-host
// alerts are excluded (a.host <> f.host): same-host detection is the axis-A1 recall signal; this asks the
// orthogonal question of what ELSE failed when the root did.
//
// RECOVERY (slice 2c): a LEFT JOIN LATERAL takes, per (injection, downstream-alert) pair, the FIRST recovery
// transition (ingest_transition) for that DOWNSTREAM host at/after its alert and within DefaultChaosRecoveryWindow
// — the same (host + time-ordering + bounded-window) correlation A6b uses, because a recovery arrives as its own
// alert with its own external_ref and never joins the raise on a key. mean_recovery_seconds is the mean, over the
// pairs that HAD a correlated recovery, of (recovery − downstream alert): the downstream's observed time-to-recover
// in a fault we injected on the root. A pair with no observed recovery is EXCLUDED from that mean (avg skips the
// NULL), never counted as an instant zero — the same absent-is-not-zero discipline as the delay mean. And a
// GROUP with NO observed recovery at all yields avg=NULL → outer coalesce 0: the "0 = unmeasured" sentinel
// Edge.RecoverySeconds documents, NOT an instant recovery (the at/after-alert guard forces recovery−alert > 0,
// so a real measured MTTR is never 0). The LATERAL
// yields exactly ONE recovered_at per (f,a) row (a scalar min), so it never multiplies rows: Injections and
// mean_delay_seconds are byte-for-byte what they were before recovery was added. This deliberately does NOT use
// injected_fault.restored_at — that column is the INJECTOR's own undo time (a scheduled harness restore), not an
// OBSERVED estate recovery, so it measures how long we chose to hold the fault, not how fast the estate recovered.
//
// OBSERVED RULES (TG-188, chaos-measured ExpectedAlerts): observed_rules is the DISTINCT, sorted set of
// alert rules the downstream fired inside the cascade window across the group's injections — the MEASURED
// expected-alert set the chaos source carries onto Edge.ExpectedAlerts. Blank rules are excluded by the
// FILTER; a group with only blank rules yields '{}' (the outer coalesce), which the source treats as
// "unmeasured", never as "expect nothing". ORDER BY inside the aggregate makes the set deterministic.
//
// Bound parameters only: since ($1), the interval literals cascadeWindow ($2) and the recovery window ($3), each
// cast ::interval — the same discipline the axis-A1 detection query uses. No value is concatenated into the
// statement.
func (s *AxisReadStore) ChaosCascades(ctx context.Context, since time.Time, cascadeWindow string) ([]estate.ChaosCascade, error) {
	rows, err := s.p.Pool.Query(ctx, `
		SELECT f.host AS root,
		       a.host AS downstream,
		       count(DISTINCT f.id) AS injections,
		       max(f.injected_at) AS latest_injected_at,
		       coalesce(avg(extract(epoch from (a.received_at - f.injected_at))), 0)::float8 AS mean_delay_seconds,
		       coalesce(avg(extract(epoch from (rec.recovered_at - a.received_at))), 0)::float8 AS mean_recovery_seconds,
		       coalesce(array_agg(DISTINCT a.alert_rule ORDER BY a.alert_rule)
		                FILTER (WHERE a.alert_rule <> ''), '{}') AS observed_rules
		FROM injected_fault f
		JOIN ingest_alert a
		  ON a.host <> f.host
		 AND a.received_at >= f.injected_at
		 AND a.received_at <= f.injected_at + ($2)::interval
		LEFT JOIN LATERAL (
		  SELECT min(r.received_at) AS recovered_at
		    FROM ingest_transition r
		   WHERE r.kind = 'recovery'
		     AND r.host = a.host
		     AND r.received_at >= a.received_at
		     AND r.received_at <= a.received_at + ($3)::interval
		) rec ON true
		WHERE f.injected_at >= $1
		GROUP BY f.host, a.host
		ORDER BY injections DESC, root, downstream`, since, cascadeWindow, DefaultChaosRecoveryWindow)
	if err != nil {
		return nil, fmt.Errorf("db: chaos cascades: %w", err)
	}
	defer rows.Close()

	var out []estate.ChaosCascade
	for rows.Next() {
		var c estate.ChaosCascade
		if err := rows.Scan(&c.Root, &c.Downstream, &c.Injections, &c.LatestInjectedAt, &c.MeanDelaySeconds, &c.MeanRecoverySeconds, &c.ObservedRules); err != nil {
			return nil, fmt.Errorf("db: chaos cascade scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: chaos cascades rows: %w", err)
	}
	return out, nil
}
