package estate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// LearnedMinObservations is the co-occurrence count a learned dependency needs before it becomes an edge: a
// single coincidence is not a dependency. Repeated observation is what separates a causal edge from noise.
const LearnedMinObservations = 3

// CoOccurrence is one observed incident co-occurrence pattern: across Count incidents, when Primary (the root
// that alerted first) had an incident, Dependent also alerted within the cascade window — evidence that
// Dependent depends-on Primary. The direction comes from ordering (root first); co-occurrence alone is
// symmetric, so the observer must record which host was the root.
type CoOccurrence struct {
	Primary   string `json:"primary"`
	Dependent string `json:"dependent"`
	Count     int    `json:"count"`
	// PrimaryTrials is how many incidents the primary had in total. When > 0 the learned edge confidence is
	// BASE-RATE-AWARE (LaplaceConfidence: Count/PrimaryTrials smoothed) instead of the count-only ramp — a
	// dependent that follows the primary 3/3 times outranks one that follows 3/50. Zero uses the count ramp.
	PrimaryTrials  int      `json:"primary_trials,omitempty"`
	ExpectedAlerts []string `json:"expected_alerts,omitempty"`
	// MeanDelaySeconds is the mean time gap between the primary's alert and the dependent's, averaged over
	// every co-occurring pair observed (TG-188). It is the propagation-DELAY estimate the co-occurrence
	// learner already had in hand — both timestamps are present at the moment a pair is counted — and used
	// to discard. Zero means unlearned (a count-only or pre-TG-188 source), never "instantaneous": a
	// consumer must treat 0 as "no estimate", the same discipline the confidence path already uses.
	MeanDelaySeconds float64 `json:"mean_delay_seconds,omitempty"`
	// MeanRecoverySeconds is the DEPENDENT's mean observed time-to-recover across its episodes (TG-188
	// organic recovery: onset→clear pairing in the learner). HOST-level attribution — the learner cannot
	// know which root caused an episode, so every pair the host depends in carries the same estimate. Zero
	// means unlearned, never an instant recovery; the chaos tier's per-edge ground truth outranks this via
	// the Upsert winning-provenance ratchet (learned edges are capped 0.75 < chaos 0.90).
	MeanRecoverySeconds float64 `json:"mean_recovery_seconds,omitempty"`
}

// LearnedSource is an estate.EdgeSource over incident co-occurrence observations — the graph's SELF-LEARNING
// tier. A pair observed at least LearnedMinObservations times becomes a learned `depends_on` edge
// (Dependent → Primary) at LearnedConfidence(count), HARD-CAPPED at 0.75 — deliberately below both any live
// source (PVE 0.95 / CMDB 0.90 / declared 0.85) AND the 0.80 suppression cutoff, so a heuristic edge can
// never outrank ground truth or suppress on its own; it only ENRICHES the blast-radius prediction.
type LearnedSource struct {
	observations []CoOccurrence
	minObs       int
}

// LearnedOption configures a LearnedSource.
type LearnedOption func(*LearnedSource)

// WithMinObservations overrides the co-occurrence threshold (for tests / operator tuning).
func WithMinObservations(n int) LearnedOption {
	return func(s *LearnedSource) {
		if n > 0 {
			s.minObs = n
		}
	}
}

// NewLearnedSource wraps a set of incident co-occurrence observations as an edge source.
func NewLearnedSource(obs []CoOccurrence, opts ...LearnedOption) *LearnedSource {
	s := &LearnedSource{observations: obs, minObs: LearnedMinObservations}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Source implements EdgeSource.
func (s *LearnedSource) Source() Source { return SourceIncident }

// Edges implements EdgeSource: each co-occurrence at/above the threshold becomes a learned depends_on edge at
// LearnedConfidence(count). Confidence is set EXPLICITLY (learned edges are not in the SourceConfidence
// policy map — they scale with observation count), so Build does not overwrite it. A pair below the
// threshold, a self-loop, or a nameless endpoint is skipped.
func (s *LearnedSource) Edges(context.Context) ([]Edge, error) {
	var edges []Edge
	for _, o := range s.observations {
		primary, dependent := strings.TrimSpace(o.Primary), strings.TrimSpace(o.Dependent)
		if primary == "" || dependent == "" || primary == dependent || o.Count < s.minObs {
			continue
		}
		edges = append(edges, Edge{
			From:            Entity{Type: TypeHost, Name: dependent},
			To:              Entity{Type: TypeHost, Name: primary},
			Rel:             RelDependsOn,
			Confidence:      LaplaceConfidence(o.Count, o.PrimaryTrials), // base-rate-aware when trials known; else count ramp
			Source:          SourceIncident,
			ExpectedAlerts:  o.ExpectedAlerts,
			DelaySeconds:    o.MeanDelaySeconds,    // the propagation delay the learner already measured (TG-188 slice 2); 0 = unlearned
			RecoverySeconds: o.MeanRecoverySeconds, // the dependent's organic MTTR (TG-188); 0 = unlearned; chaos outranks via the ratchet
		})
	}
	return edges, nil
}

// ParseCoOccurrences reads a JSON array of CoOccurrence observations (an operator-exported incident-history
// summary, until the outcome-labelled memory loop feeds it automatically). A malformed entry — an empty
// endpoint or a negative count — is rejected loudly, never silently dropped.
func ParseCoOccurrences(r io.Reader) ([]CoOccurrence, error) {
	var obs []CoOccurrence
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&obs); err != nil {
		return nil, fmt.Errorf("estate: malformed co-occurrence JSON: %w", err)
	}
	for i, o := range obs {
		if strings.TrimSpace(o.Primary) == "" || strings.TrimSpace(o.Dependent) == "" {
			return nil, fmt.Errorf("estate: co-occurrence %d: primary and dependent are required", i)
		}
		if o.Count < 0 {
			return nil, fmt.Errorf("estate: co-occurrence %d: negative count", i)
		}
	}
	return obs, nil
}

// compile-time proof the learned source satisfies the edge-source seam.
var _ EdgeSource = (*LearnedSource)(nil)
