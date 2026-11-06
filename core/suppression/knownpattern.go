package suppression

import (
	"context"
	"regexp"
	"time"
)

// TransientPattern is a host-agnostic known-transient rule keyed on alert_rule and scoped to the estate
// (REQ-401) — the pattern matches by rule across the estate, NOT by one hostname. A match suppresses ONLY
// when it clears three gates: a confidence floor, a transient-nature keyword, and (for learned patterns) a
// recency window — the predecessor's known-transient discipline. A bare rule-equality match is not enough:
// suppression darkens an alert, so it must be a confidently-transient, still-current pattern.
type TransientPattern struct {
	AlertRule  string
	Estate     string
	Confidence float64   // must be >= ConfidenceFloor to suppress
	LastSeen   time.Time // when the pattern last recurred; zero = a static/declared pattern (no recency gate)
}

// KnownPatternStage matches a transient pattern by alert_rule across the estate, gated by confidence, a
// transient keyword, and recency.
type KnownPatternStage struct {
	Patterns []TransientPattern
}

// ConfidenceFloor is the minimum pattern confidence that may suppress (below it, escalate). Deliberately at
// 0.7 — a low-confidence guess must never silence a real alert.
const ConfidenceFloor = 0.7

// RecencyWindow is how recently a LEARNED pattern must have recurred to still suppress; a stale pattern
// (last seen longer ago) fails open.
const RecencyWindow = 7 * 24 * time.Hour

// transientKeywordRE requires the alert rule to name a genuinely transient condition — a flap/blip/recovery,
// not a standing fault. "DiskFull" is not transient and must never be auto-suppressed as a known pattern.
var transientKeywordRE = regexp.MustCompile(`(?i)flap|transient|recover|intermittent|blip|bounce|self.?heal|jitter|churn`)

// Name implements Stage.
func (s *KnownPatternStage) Name() Phase { return PhaseKnownPattern }

// Evaluate suppresses the alert as a known transient only when a pattern matches its alert_rule AND clears
// the confidence floor AND the rule reads as transient AND (if the pattern carries a LastSeen) it is within
// the recency window. Any gate failing — or no matching pattern — fails OPEN to escalation.
func (s *KnownPatternStage) Evaluate(_ context.Context, a Alert, now time.Time) (Decision, error) {
	for _, p := range s.Patterns {
		if p.AlertRule != a.AlertRule {
			continue
		}
		if p.Confidence < ConfidenceFloor {
			continue // gate 1: a low-confidence pattern may not suppress
		}
		if !transientKeywordRE.MatchString(a.AlertRule) {
			continue // gate 2: the rule must name a transient condition
		}
		if !p.LastSeen.IsZero() && now.Sub(p.LastSeen) > RecencyWindow {
			continue // gate 3: a stale learned pattern fails open
		}
		return Decision{Outcome: OutcomeSuppressed, Phase: PhaseKnownPattern, Reason: "confidently-transient, current known pattern", ExternalRef: a.ExternalRef}, nil
	}
	return escalate(a.ExternalRef, PhaseKnownPattern, "no confidently-transient known pattern"), nil
}
