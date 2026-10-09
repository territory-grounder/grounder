package suppression

import (
	"context"
	"path"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/ingest"
)

// SuppressRule is one operator-declared active-memory suppression rule — the ONLY suppression path a human
// explicitly authorizes. HostPattern and RulePattern are globs (path.Match syntax: `*`, `?`, `[set]`; either
// may be `*` for any); the rule silences an alert whose Host AND AlertRule both match, with the operator's
// stated Reason recorded on the decision. It ports the predecessor's phase-3 `openclaw_memory`
// category='triage-rule' rows (key `<hostpat>:<rulepat>`, value `suppress:<reason>`), re-expressed as typed,
// operator-curated config injected into the stage — the escape hatch by which a standing, known-safe
// condition is silenced by an accountable operator decision rather than by a learned heuristic.
type SuppressRule struct {
	HostPattern string
	RulePattern string
	Reason      string
}

// ActiveMemoryStage is phase 3: it suppresses an alert that matches an explicit operator rule. It is the
// LAST, most-permissive phase — it runs only after dedup, blast-radius, scheduled-reboot, and known-pattern
// have all failed to match, and it suppresses only on an accountable, human-declared rule.
type ActiveMemoryStage struct {
	Rules []SuppressRule
}

// Name implements Stage.
func (s *ActiveMemoryStage) Name() Phase { return PhaseActiveMemory }

// Evaluate suppresses the alert when an operator rule's host+rule globs both match. Anything else fails
// OPEN to escalation. Two fail-safe properties hold by construction: a critical or unknown severity is NEVER
// auto-resolved by an operator rule even if one matches (defense-in-depth — the chain's severity floor
// already short-circuits these before any stage, but the stage stays correct when used standalone); and a
// malformed glob matches nothing, so a broken operator rule fails open rather than silencing everything.
func (s *ActiveMemoryStage) Evaluate(_ context.Context, a Alert, _ time.Time) (Decision, error) {
	if a.Severity == ingest.SeverityCritical || a.Severity == ingest.SeverityUnknown {
		return escalate(a.ExternalRef, PhaseActiveMemory, "critical/unknown severity is never suppressed by an operator rule"), nil
	}
	for _, r := range s.Rules {
		if !matchGlob(r.HostPattern, a.Host) || !matchGlob(r.RulePattern, a.AlertRule) {
			continue
		}
		reason := strings.TrimSpace(r.Reason)
		if reason == "" {
			reason = "unspecified"
		}
		return Decision{
			Outcome:     OutcomeSuppressed,
			Phase:       PhaseActiveMemory,
			Reason:      "operator active-memory rule: " + reason,
			ExternalRef: a.ExternalRef,
		}, nil
	}
	return escalate(a.ExternalRef, PhaseActiveMemory, "no matching operator active-memory rule"), nil
}

// matchGlob reports whether name matches a path.Match glob. A malformed pattern (e.g. an unterminated `[`
// range) matches nothing — a broken operator rule must fail OPEN, never silence every alert. An empty
// pattern matches only an empty name, so an operator wanting "any" writes `*`.
func matchGlob(pattern, name string) bool {
	ok, err := path.Match(pattern, name)
	return err == nil && ok
}
