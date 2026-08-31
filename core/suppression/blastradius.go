package suppression

import (
	"context"
	"time"
)

// SuppressionPolicy is an org-global blast-radius record (REQ-402/403). A record folds a child alert
// only WHILE it is currently valid — now between valid_from and valid_until AND last_verified_at within
// its freshness bound — and the alert falls within its declared host/rule scope. Managed via the ticket
// module; org-global (ADR-0010), never per-tenant.
type SuppressionPolicy struct {
	HostScope      string // exact host or "*" (estate-wide)
	RuleScope      string // exact alert_rule or "*"
	Site           string
	ValidFrom      time.Time
	ValidUntil     time.Time
	LastVerifiedAt time.Time
}

// scopeMatch reports whether a scope ("*" or an exact value) matches v.
func scopeMatch(scope, v string) bool { return scope == "*" || scope == v }

// Matches reports whether an alert falls within the policy's declared host/rule scope.
func (p SuppressionPolicy) Matches(a Alert) bool {
	return scopeMatch(p.HostScope, a.Host) && scopeMatch(p.RuleScope, a.AlertRule)
}

// Valid reports whether the policy currently applies: now within [valid_from, valid_until] AND
// last_verified_at fresher than the freshness bound. An expired or stale-verified record is NOT valid
// (REQ-402/405).
func (p SuppressionPolicy) Valid(now time.Time, freshness time.Duration) bool {
	if now.Before(p.ValidFrom) || now.After(p.ValidUntil) {
		return false
	}
	if now.Sub(p.LastVerifiedAt) > freshness {
		return false
	}
	return true
}

// BlastRadiusStage folds a child alert under a currently-valid, live-verified suppression policy.
type BlastRadiusStage struct {
	Policies  []SuppressionPolicy
	Freshness time.Duration
}

// Name implements Stage.
func (s *BlastRadiusStage) Name() Phase { return PhaseBlastRadius }

// Evaluate posts the alert as a NOTICE (no remediation session) under a matching, currently-valid
// policy (REQ-403). A matching-but-expired / stale-verified policy fails OPEN to escalation (REQ-402/405).
func (s *BlastRadiusStage) Evaluate(_ context.Context, a Alert, now time.Time) (Decision, error) {
	for _, p := range s.Policies {
		if !p.Matches(a) {
			continue
		}
		if !p.Valid(now, s.Freshness) {
			// a matching record that is expired / stale-verified must not suppress — fail open.
			return escalate(a.ExternalRef, PhaseBlastRadius, "suppression policy expired or stale-verified — fail open"), nil
		}
		return Decision{Outcome: OutcomeNotice, Phase: PhaseBlastRadius, Reason: "blast-radius fold — posted as notice, no session", ExternalRef: a.ExternalRef}, nil
	}
	return escalate(a.ExternalRef, PhaseBlastRadius, "no blast-radius fold"), nil
}
