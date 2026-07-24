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

// Schedule is a discovered_scheduled_reboots row viewed by the suppression domain: a learned reboot
// schedule with a validity window, an observe-before-live status, an observed-boot count, a kill switch,
// and a freshness stamp.
type Schedule struct {
	Host           string
	Kind           string
	Cron           string
	Timezone       string
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

// ScheduledStage is phase SR: it suppresses a reboot-class alert on a host carrying a live, un-killed,
// un-expired schedule whose window contains the alert time.
type ScheduledStage struct {
	Schedules []Schedule
	Window    WindowEvaluator
}

// Name implements Stage.
func (s *ScheduledStage) Name() Phase { return PhaseScheduledReboot }

// Evaluate suppresses an on-schedule reboot; anything else (not a reboot, no live schedule, wrong
// window, observing/disabled/killed/expired row) fails OPEN to escalation.
func (s *ScheduledStage) Evaluate(_ context.Context, a Alert, now time.Time) (Decision, error) {
	if !a.IsReboot {
		return escalate(a.ExternalRef, PhaseScheduledReboot, "not a reboot-class alert"), nil
	}
	for _, sc := range s.Schedules {
		if sc.Host != a.Host {
			continue
		}
		if sc.Suppresses(s.Window, a.ObservedAt, now) {
			return Decision{Outcome: OutcomeSuppressed, Phase: PhaseScheduledReboot, Reason: "on-schedule reboot on a live registered schedule", ExternalRef: a.ExternalRef}, nil
		}
	}
	return escalate(a.ExternalRef, PhaseScheduledReboot, "no live schedule window match"), nil
}
