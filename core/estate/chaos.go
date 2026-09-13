package estate

import (
	"context"
	"strings"
	"time"
)

// ChaosEdgeTTL is how long a chaos-injected cascade edge stays fresh after the injection that produced it.
// Chaos edges are GROUND TRUTH about a fault we deliberately caused, but topology drifts, so the knowledge
// ages. Crucially, the decay pass (core/estate/decay.go) is SourceIncident-scoped and never touches the chaos
// tier — so a chaos edge that did NOT carry an expiry would live forever. The TTL is therefore the edge's whole
// lifetime: ValidUntil is the latest injection's timestamp plus this window, and the graph's freshness check
// drops it once past. The worker's ledger lookback matches this TTL, so an injection older than the TTL is
// never even read (its edge would already be expired), keeping the read bounded as the ledger grows.
const ChaosEdgeTTL = 30 * 24 * time.Hour

// DefaultChaosMinInjections is how many DISTINCT injections of a root a downstream host must have followed
// before the pair becomes a chaos edge. Default 1: unlike co-occurrence (LearnedMinObservations=3) the root
// here is not INFERRED from alert ordering — we injected it, so a single observed cascade is real evidence,
// not a coincidence of who-alarmed-first. The tight cascade window (core/db.DefaultChaosCascadeWindow) plus the
// self-expiry above are what bound a spurious co-alerter, not a high repetition threshold that would starve the
// feed on an estate where the same root is rarely injected twice.
const DefaultChaosMinInjections = 1

// ChaosCascade is one (root, downstream) pair observed from the injection ledger: across Injections distinct
// injected faults on Root, Downstream (a DIFFERENT host) alerted inside the cascade window. LatestInjectedAt is
// the most recent such injection — the freshness anchor for the edge's expiry. It is produced by the DB reader
// (core/db.AxisReadStore.ChaosCascades) and consumed by ChaosSource; it lives here, beside CoOccurrence,
// because core/estate owns the edge vocabulary and core/db already imports it (estate never imports db).
type ChaosCascade struct {
	Root             string
	Downstream       string
	Injections       int
	LatestInjectedAt time.Time
	// MeanDelaySeconds is the mean GROUND-TRUTH propagation delay: how long after the injection each downstream
	// alert arrived, averaged over the matching (injection, alert) pairs (TG-188 slice 2b). Unlike the
	// co-occurrence learner's INFERRED delay, the root failure time is KNOWN (we injected it), so this is a
	// measured cascade timing, not an estimate from alert ordering. Zero means unmeasured.
	MeanDelaySeconds float64
	// MeanRecoverySeconds is the mean GROUND-TRUTH recovery time (MTTR): how long after each downstream's
	// cascade alert it recovered (observed via ingest_transition), in a fault injected on the root. Zero means
	// no observed recovery (unmeasured) — the same absent-is-not-zero discipline as MeanDelaySeconds. TG-188
	// slice 2c.
	MeanRecoverySeconds float64
	// ObservedRules is the DISTINCT set of alert rules the downstream actually fired inside the cascade window,
	// across the matching injections — the MEASURED expected-alert set for this edge (TG-188: ExpectedAlerts
	// was static/operator-declared until this). Empty means no rule was recorded (ingest rows predating the
	// alert_rule column, or blank rules), which the source treats as "no measurement", never as "expect nothing".
	ObservedRules []string
}

// ChaosSource is an estate.EdgeSource over the injection engine's ground-truth cascades — the graph's
// CHAOS-CALIBRATED tier (TG-188). When a fault is injected on Root and Downstream alerts inside the cascade
// window, Downstream depends-on Root: we CAUSED the root failure, so unlike the co-occurrence learner the
// direction of causation is not fabricated from ordering. Chaos edges stamp SourceChaos at 0.90
// (SourceConfidence) — above the learned cap (0.75) because the root is observed, not guessed — and Upsert lets
// chaos re-label an equal-confidence seed edge (estate.go). The source is LOADER-BASED (like SnapshotRelaySource)
// rather than slice-based, because its evidence lives in the database and is re-read on each estate refresh so
// an injection between boots teaches the graph without a restart; the loader is late-bound at pool connect.
type ChaosSource struct {
	load          func(ctx context.Context) ([]ChaosCascade, error)
	ttl           time.Duration
	minInjections int
}

// ChaosOption configures a ChaosSource.
type ChaosOption func(*ChaosSource)

// WithChaosEdgeTTL overrides the edge lifetime (for tests / operator tuning). A non-positive value is ignored.
func WithChaosEdgeTTL(d time.Duration) ChaosOption {
	return func(s *ChaosSource) {
		if d > 0 {
			s.ttl = d
		}
	}
}

// WithChaosMinInjections overrides the distinct-injection threshold. A non-positive value is ignored.
func WithChaosMinInjections(n int) ChaosOption {
	return func(s *ChaosSource) {
		if n > 0 {
			s.minInjections = n
		}
	}
}

// NewChaosSource wraps a late-bound cascade loader as an edge source. load is called on every Edges() (i.e.
// every estate refresh); a nil load is treated as "no cascades yet" so a source wired before the pool connects
// degrades to empty rather than panicking.
func NewChaosSource(load func(ctx context.Context) ([]ChaosCascade, error), opts ...ChaosOption) *ChaosSource {
	s := &ChaosSource{load: load, ttl: ChaosEdgeTTL, minInjections: DefaultChaosMinInjections}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Source implements EdgeSource.
func (s *ChaosSource) Source() Source { return SourceChaos }

// Edges implements EdgeSource: each cascade at/above the injection threshold becomes a chaos depends_on edge
// (Downstream → Root) carrying a ValidUntil of the latest injection plus the TTL, so the edge SELF-EXPIRES —
// the decay pass never touches the chaos tier, so an edge without an expiry would be immortal. A cascade with
// no timestamp is skipped rather than emitted open-ended, so a chaos edge is NEVER immortal. Confidence is left
// 0 so Build stamps the SourceChaos policy default (0.90). A self-loop, a nameless endpoint, or a sub-threshold
// pair is skipped. A load error is returned so Build isolates it per-source (the prior graph survives), never
// silently swallowed.
func (s *ChaosSource) Edges(ctx context.Context) ([]Edge, error) {
	if s.load == nil {
		return nil, nil
	}
	cascades, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	var edges []Edge
	for _, c := range cascades {
		root, downstream := strings.TrimSpace(c.Root), strings.TrimSpace(c.Downstream)
		if root == "" || downstream == "" || root == downstream || c.Injections < s.minInjections || c.LatestInjectedAt.IsZero() {
			continue
		}
		edges = append(edges, Edge{
			From:            Entity{Type: TypeHost, Name: downstream},
			To:              Entity{Type: TypeHost, Name: root},
			Rel:             RelDependsOn,
			Source:          SourceChaos, // Confidence 0 → Build stamps the 0.90 policy default
			ValidUntil:      c.LatestInjectedAt.Add(s.ttl),
			DelaySeconds:    c.MeanDelaySeconds,    // ground-truth cascade propagation delay (TG-188 slice 2b); reuses the slice-2 Edge field
			RecoverySeconds: c.MeanRecoverySeconds, // ground-truth cascade recovery/MTTR (TG-188 slice 2c)
			ExpectedAlerts:  c.ObservedRules,       // MEASURED expected-alert set: what the downstream actually fired in the drill (TG-188)
		})
	}
	return edges, nil
}

// compile-time proof the chaos source satisfies the edge-source seam.
var _ EdgeSource = (*ChaosSource)(nil)
