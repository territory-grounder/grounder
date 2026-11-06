package suppression

import (
	"context"
	"time"
)

// SchedStatus is the observe-before-live lifecycle of a discovered schedule. The zero value is
// SchObserving — an observing row NEVER suppresses. Only a promoted (live) row may suppress (REQ-404).
type SchedStatus int

const (
	SchObserving SchedStatus = iota // zero value — learning; never suppresses
	SchLive                         // promoted; may suppress, subject to kill switch / expiry / window
	SchDisabled                     // drifted / expired / killed
)

func (s SchedStatus) String() string {
	switch s {
	case SchLive:
		return "live"
	case SchDisabled:
		return "disabled"
	default:
		return "observing"
	}
}

// ScheduleSource is the PROVENANCE of a schedule row: an operator DECLARATION (a human stated this reboot
// is intentional) or a pattern TG LEARNED from recurring, boot-verified reboots. The two lanes sit side by
// side and are gated differently — a declared schedule is authorized by the declaration itself, a learned
// one must earn LIVE through observe→verify→promote and stays subject to the governance-demotion escape.
type ScheduleSource int

const (
	// SourceLearned is the ZERO VALUE deliberately: a row whose provenance was never stated is treated as
	// LEARNED, which is the STRICTER lane (observe-before-live plus the demotion consult), so a dropped or
	// forgotten field can never silently promote a row into the operator-authorized lane. [O] the core/safety
	// zero-value discipline — the zero value is the most restrictive.
	SourceLearned ScheduleSource = iota
	SourceDeclared
)

func (s ScheduleSource) String() string {
	if s == SourceDeclared {
		return "declared"
	}
	return "learned"
}

// Schedule is a discovered_scheduled_reboots row viewed by the suppression domain: a learned reboot
// schedule with a validity window, an observe-before-live status, an observed-boot count, a kill switch,
// and a freshness stamp.
type Schedule struct {
	Host           string
	Kind           string
	Cron           string
	Timezone       string
	Source         ScheduleSource
	Status         SchedStatus
	ObservedCount  int
	ObservedBoots  []time.Time // the DISTINCT in-window boot timestamps observed across runs (deduped, capped)
	KillSwitch     bool
	ValidFrom      time.Time
	ValidUntil     time.Time
	LastVerifiedAt time.Time
}

// Suppresses reports whether this schedule may suppress an alert observed at t (evaluated at now). It
// must be LIVE, un-killed, and un-expired, and its DST-correct window must contain t. An observing,
// disabled, killed, or expired row can never suppress (REQ-404/405) — the fail direction is safe.
func (sc Schedule) Suppresses(w WindowEvaluator, t, now time.Time) bool {
	if sc.Status != SchLive || sc.KillSwitch {
		return false
	}
	// Temporally bounded: a row applies only WHILE now is inside [valid_from, valid_until]. A row that
	// is not yet valid (valid_from in the future) or expired (past valid_until) must never suppress.
	if !sc.ValidFrom.IsZero() && now.Before(sc.ValidFrom) {
		return false
	}
	if !sc.ValidUntil.IsZero() && now.After(sc.ValidUntil) {
		return false
	}
	return w.Contains(sc, t)
}

// DemotionLookup answers whether a (host, alert_rule) tuple currently carries a LIVE governance demotion —
// the analysis-only policy row written when a LEARNED suppression is proven to have silenced an incident
// that later needed action (spec/004 REQ-301, spec/005 REQ-411). core/governance's DemotionLookupOf
// satisfies it structurally, so the suppression domain never imports the governance package.
//
// Learning without unlearning is a one-way ratchet: the first bad lesson would keep suppressing real
// incidents forever. This is the escape, and it is consulted on the LIVE decision path.
type DemotionLookup interface {
	Demoted(ctx context.Context, host, alertRule string, now time.Time) (bool, error)
}

// Renewer renews the validity of a schedule that actually matched, so an ACTIVELY-firing learned schedule
// does not silently expire out from under the matcher (the predecessor's renew_on_match). Renewal touches
// only the validity window and the freshness stamp — never the promotion state.
type Renewer interface {
	RenewOnMatch(k ScheduleKey, until, now time.Time)
}

// ScheduledStage is phase SR: it suppresses a reboot-class alert on a host carrying a live, un-killed,
// un-expired schedule whose window contains the alert time. It serves BOTH lanes — operator-declared rows
// and learned rows — and applies the learned lane's extra gate (the governance-demotion consult).
type ScheduledStage struct {
	Schedules []Schedule
	Window    WindowEvaluator
	// Demotions is consulted BEFORE a LEARNED schedule is honored. Nil ⇒ no demotion state is available and
	// learned rows are honored on their own promotion (the declared lane is never gated by it).
	Demotions DemotionLookup
	// Renew + RenewFor implement renew-on-match for the row that actually suppressed. Zero RenewFor ⇒ off.
	Renew    Renewer
	RenewFor time.Duration
}

// Name implements Stage.
func (s *ScheduledStage) Name() Phase { return PhaseScheduledReboot }

// Evaluate suppresses an on-schedule reboot; anything else (not a reboot, no live schedule, wrong
// window, observing/disabled/killed/expired row, or a governance-demoted learned row) fails OPEN to
// escalation.
func (s *ScheduledStage) Evaluate(ctx context.Context, a Alert, now time.Time) (Decision, error) {
	if !a.IsReboot {
		return escalate(a.ExternalRef, PhaseScheduledReboot, "not a reboot-class alert"), nil
	}
	for _, sc := range s.Schedules {
		if sc.Host != a.Host {
			continue
		}
		if !sc.Suppresses(s.Window, a.ObservedAt, now) {
			continue
		}
		// The LEARNED lane's extra gate: a tuple carrying a live governance demotion has already been PROVEN
		// to hide a real incident behind this lesson, so the lesson stops suppressing until the demotion
		// expires (REQ-411). An UNREADABLE demotion state is treated as a refusal to suppress, not as a
		// clean bill of health — every failure direction in this stage ends at INVESTIGATING.
		if sc.Source == SourceLearned && s.Demotions != nil {
			demoted, err := s.Demotions.Demoted(ctx, a.Host, a.AlertRule, now)
			if err != nil {
				return escalate(a.ExternalRef, PhaseScheduledReboot, "governance demotion state unreadable — a learned schedule may not suppress unverified"), nil
			}
			if demoted {
				return escalate(a.ExternalRef, PhaseScheduledReboot, "learned schedule is governance-demoted to analysis-only after it silenced a real incident — investigate"), nil
			}
		}
		if s.Renew != nil && s.RenewFor > 0 {
			s.Renew.RenewOnMatch(KeyOf(sc), now.Add(s.RenewFor), now)
		}
		return Decision{
			Outcome: OutcomeSuppressed, Phase: PhaseScheduledReboot,
			Reason:      "on-schedule reboot on a live " + sc.Source.String() + " schedule",
			ExternalRef: a.ExternalRef,
			// The signals carry the matched row's identity so the caller can run the two-phase verify against
			// the row that actually suppressed (and demote exactly that row when the verify reverses it).
			Signals: map[string]string{"schedule_source": sc.Source.String(), "cron": sc.Cron, "kind": sc.Kind, "timezone": sc.Timezone},
		}, nil
	}
	return escalate(a.ExternalRef, PhaseScheduledReboot, "no live schedule window match"), nil
}
