package ingest

import (
	"fmt"
	"sort"
	"time"
)

// Normalize validates a RawEvent's candidate fields through the per-field grammar and produces a
// canonical IncidentEnvelope, or a validation error (reject-before-enqueue, INV-04). now is injected
// so timestamp validation is deterministic under test. The unsanitized RawEvent.payload is NOT copied
// into the envelope — it stays behind the unexported field and dies with the RawEvent.
func Normalize(raw RawEvent, now time.Time) (IncidentEnvelope, error) {
	if raw.SourceID == "" {
		return IncidentEnvelope{}, fmt.Errorf("%w: source_id", ErrMissingField)
	}
	if raw.ExternalRef == "" {
		return IncidentEnvelope{}, fmt.Errorf("%w: external_ref", ErrMissingField)
	}
	if raw.AlertRule == "" {
		return IncidentEnvelope{}, fmt.Errorf("%w: alert_rule", ErrMissingField)
	}
	if err := validateSlug("external_ref", raw.ExternalRef, maxExternalRefLen, ErrBadExternalRef); err != nil {
		return IncidentEnvelope{}, err
	}
	if err := validateSlug("alert_rule", raw.AlertRule, maxAlertRuleLen, ErrBadAlertRule); err != nil {
		return IncidentEnvelope{}, err
	}
	sev, err := parseSeverity(raw.Severity)
	if err != nil {
		return IncidentEnvelope{}, err
	}
	if err := validateHost(raw.Host); err != nil {
		return IncidentEnvelope{}, err
	}
	ip, err := validateIP(raw.IP)
	if err != nil {
		return IncidentEnvelope{}, err
	}
	if len(raw.Summary) > maxSummaryLen {
		return IncidentEnvelope{}, fmt.Errorf("%w: summary %d > %d", ErrTooLong, len(raw.Summary), maxSummaryLen)
	}
	if raw.Site != "" {
		if err := validateSlug("site", raw.Site, maxSiteLen, ErrBadAlertRule); err != nil {
			return IncidentEnvelope{}, err
		}
	}
	labels, err := validateLabels(raw.Labels)
	if err != nil {
		return IncidentEnvelope{}, err
	}
	observed := raw.ObservedAt
	if observed.IsZero() {
		observed = now // a provider that omits a timestamp is stamped at receipt, not rejected
	}
	if err := validateTimestamp(observed, now); err != nil {
		return IncidentEnvelope{}, err
	}
	return IncidentEnvelope{
		ExternalRef: raw.ExternalRef,
		SourceID:    raw.SourceID,
		AlertRule:   raw.AlertRule,
		Severity:    sev,
		Host:        raw.Host,
		IP:          ip,
		Site:        raw.Site,
		Summary:     raw.Summary,
		Labels:      labels,
		ObservedAt:  observed,
		ReceivedAt:  now,
	}, nil
}

// Stage names, recorded in execution order so an oracle can prove the deterministic chain ran in code
// before anything is published (REQ-502).
const (
	StageDedup     = "dedup"
	StageFlap      = "flap"
	StageBurst     = "burst"
	StageCorrelate = "correlate"
)

// Pipeline thresholds. Deterministic, single-org.
const (
	dedupWindow    = 24 * time.Hour
	flapWindow     = 15 * time.Minute
	flapThreshold  = 3 // ≥3 fires of the same dedup key WITHIN flapWindow ⇒ flapping
	burstWindow    = 5 * time.Minute
	burstThreshold = 3 // ≥3 distinct correlated incidents ⇒ burst (predecessor BURST_THRESHOLD; ARCHITECTURE.md "3+ hosts")
)

// Decision is the per-event outcome of the pre-model chain.
type Decision struct {
	Envelope       IncidentEnvelope
	Duplicate      bool   // collapsed by dedup (a repeat of an earlier event in-window)
	Flapping       bool   // this key fired ≥ flapThreshold times in flapWindow
	InBurst        bool   // the batch tripped the burst threshold
	CorrelationKey string // the group this incident belongs to
}

// BatchResult is the output of Process: the per-event decisions plus the ordered list of stages that
// ran. Publication happens AFTER Process returns, so Order proves the chain ran in code first.
type BatchResult struct {
	Decisions []Decision
	Order     []string
}

// Pipeline runs the deterministic pre-model chain dedup → flap → burst → correlate over a batch of
// already-normalized envelopes, in code, before any model is spent or any event is published.
// [F] the predecessor pre-model suppression chain, re-expressed single-org.
type Pipeline struct{}

// NewPipeline returns a stateless batch pipeline (windows are computed within the batch relative to
// now; cross-batch dedup state lives in the persistence layer, not here).
func NewPipeline() *Pipeline { return &Pipeline{} }

// Process runs the four deterministic stages over evs and returns the decisions and the stage order.
// It never publishes — the caller publishes the non-duplicate representatives afterward.
func (p *Pipeline) Process(evs []IncidentEnvelope, now time.Time) BatchResult {
	decs := make([]Decision, len(evs))
	for i, e := range evs {
		decs[i] = Decision{Envelope: e, CorrelationKey: e.CorrelationKey()}
	}
	order := make([]string, 0, 4)

	// 1) dedup — collapse repeats of the same DedupKey within the window; first occurrence wins.
	order = append(order, StageDedup)
	seen := map[string]time.Time{}
	for i := range decs {
		k := decs[i].Envelope.DedupKey()
		if first, ok := seen[k]; ok && decs[i].Envelope.ObservedAt.Sub(first) <= dedupWindow {
			decs[i].Duplicate = true
			continue
		}
		seen[k] = decs[i].Envelope.ObservedAt
	}

	// 2) flap — a key that fired ≥ flapThreshold times WITHIN flapWindow is flapping. Flap is a CLUSTERING
	// property: counting raw whole-batch occurrences with no time bound falsely flagged re-deliveries of one
	// alert spread across hours (T, T+40m, T+80m). We collect each key's fire timestamps and flag it only when
	// flapThreshold of them fall inside a single flapWindow span — matching the predecessor's windowed flap.
	order = append(order, StageFlap)
	fireTimes := map[string][]time.Time{}
	for i := range decs {
		k := decs[i].Envelope.DedupKey()
		fireTimes[k] = append(fireTimes[k], decs[i].Envelope.ObservedAt)
	}
	flapping := map[string]bool{}
	for k, ts := range fireTimes {
		if flapsWithinWindow(ts, flapThreshold, flapWindow) {
			flapping[k] = true
		}
	}
	for i := range decs {
		if flapping[decs[i].Envelope.DedupKey()] {
			decs[i].Flapping = true
		}
	}

	// 3) burst — ≥ burstThreshold DISTINCT non-duplicate incidents in the batch ⇒ burst for all.
	order = append(order, StageBurst)
	distinct := map[string]struct{}{}
	for i := range decs {
		if !decs[i].Duplicate {
			distinct[decs[i].CorrelationKey] = struct{}{}
		}
	}
	inBurst := len(distinct) >= burstThreshold
	for i := range decs {
		decs[i].InBurst = inBurst
	}

	// 4) correlate — group non-duplicate incidents by CorrelationKey (already stamped per decision).
	order = append(order, StageCorrelate)

	return BatchResult{Decisions: decs, Order: order}
}

// flapsWithinWindow reports whether at least `threshold` of the given fire times fall inside a single
// `window`-length span. It sorts the times and slides a `threshold`-wide window across them: the key flaps
// only when `threshold` consecutive fires span ≤ `window`. This is the "≥ N fires within W" rule the
// flapThreshold/flapWindow constants describe — re-deliveries of one alert spread wider than the window
// never cluster into a flap, and the window is applied over the raw fire timestamps (a dedup collapse of the
// representative for publication does not erase a fire's contribution to flap clustering).
func flapsWithinWindow(times []time.Time, threshold int, window time.Duration) bool {
	if threshold <= 0 || len(times) < threshold {
		return false
	}
	ts := append([]time.Time(nil), times...)
	sort.Slice(ts, func(i, j int) bool { return ts[i].Before(ts[j]) })
	for i := 0; i+threshold-1 < len(ts); i++ {
		if ts[i+threshold-1].Sub(ts[i]) <= window {
			return true
		}
	}
	return false
}

// Representatives returns the non-duplicate incidents to publish, one per correlation group, in a
// stable order (by correlation key then observed time). These are what become triage.requested events.
func (r BatchResult) Representatives() []IncidentEnvelope {
	byGroup := map[string]IncidentEnvelope{}
	haveGroup := map[string]bool{}
	for _, d := range r.Decisions {
		if d.Duplicate {
			continue
		}
		if !haveGroup[d.CorrelationKey] {
			byGroup[d.CorrelationKey] = d.Envelope
			haveGroup[d.CorrelationKey] = true
		}
	}
	keys := make([]string, 0, len(byGroup))
	for k := range byGroup {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]IncidentEnvelope, 0, len(keys))
	for _, k := range keys {
		out = append(out, byGroup[k])
	}
	return out
}
