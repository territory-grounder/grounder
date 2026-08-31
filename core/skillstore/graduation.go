package skillstore

import (
	"context"
	"fmt"
	"time"
)

// The post-graduation regression watch (spec/014 REQ-1310) — the rollback the predecessor never had
// (its runbook documented a manual jq edit against a file that contained no trial entries; the
// procedure was production-unexercised). A graduate is watched for a bounded window: each judged
// session that composed the new version scores against the trial's control mean; enough consecutive
// failures auto-retire the graduate, restore the prior production version, ledger the demotion, and
// escalate — a skill that passed its trial cannot silently persist degradation.

// WatchState is one graduate's watch bookkeeping (persisted by the WatchStore).
type WatchState struct {
	VersionID    int64
	SkillName    string
	ControlMean  float64 // the trial's concurrent-control mean at graduation
	MinLift      float64
	Failures     int    // consecutive below-threshold judged sessions
	Threshold    int    // failures that trip the demotion (default 5)
	Dimension    string // the trial's target dimension — the judged score the watch compares
	ExpiresAt    time.Time
	PriorVersion int64 // the version retired at graduation — restored on a trip
}

// WatchStore persists watches (pgx in core/db as skill_watch rows; MemWatchStore in oracles).
type WatchStore interface {
	PutWatch(ctx context.Context, w WatchState) error
	OpenWatches(ctx context.Context) ([]WatchState, error)
	UpdateWatch(ctx context.Context, w WatchState) error
	CloseWatch(ctx context.Context, versionID int64, reason string) error
}

// Escalator surfaces a demotion to the human queue (the escalation-queue enqueue in production).
type Escalator func(ctx context.Context, externalRef, reason string) error

// DefaultWatchWindow bounds the regression watch (REQ-1310).
const DefaultWatchWindow = 7 * 24 * time.Hour

// DefaultWatchThreshold trips the demotion after this many consecutive regressing sessions.
const DefaultWatchThreshold = 5

// OpenWatch arms the watch at graduation (called by the finalizer after a successful Transition).
// dimension is the trial's target dimension: the judged score the watch compares against the control
// mean — a watch never scores a session on an axis its trial did not measure.
func OpenWatch(ctx context.Context, ws WatchStore, versionID, priorVersion int64, skillName, dimension string, controlMean, minLift float64, now time.Time) error {
	return ws.PutWatch(ctx, WatchState{
		VersionID: versionID, SkillName: skillName, ControlMean: controlMean, MinLift: minLift,
		Threshold: DefaultWatchThreshold, Dimension: dimension,
		ExpiresAt: now.Add(DefaultWatchWindow), PriorVersion: priorVersion,
	})
}

// ObserveJudgedSession scores one judged session against every open watch whose version it composed
// (REQ-1310). scores is the session's per-dimension judged verdict; each watch reads ONLY its own
// dimension (the one its trial measured) and a session not judged on that dimension is no evidence
// either way. A score below control−lift counts a failure; at/above resets the streak (CONSECUTIVE
// failures trip, matching the breaker's semantics). A tripped watch demotes: the graduate retires via
// the audited Transition, the prior production version is restored, and the demotion escalates. An
// expired watch closes as survived.
func ObserveJudgedSession(ctx context.Context, ws WatchStore, st Store, lg Ledger, esc Escalator,
	composedVersionIDs []int64, scores map[string]float64, now time.Time) error {
	watches, err := ws.OpenWatches(ctx)
	if err != nil {
		return err
	}
	composed := make(map[int64]bool, len(composedVersionIDs))
	for _, id := range composedVersionIDs {
		composed[id] = true
	}
	for _, w := range watches {
		if now.After(w.ExpiresAt) {
			if err := ws.CloseWatch(ctx, w.VersionID, "watch expired — graduate survived its regression window"); err != nil {
				return err
			}
			continue
		}
		if !composed[w.VersionID] {
			continue
		}
		score, judged := scores[w.Dimension]
		if !judged {
			continue // the session was not judged on this watch's dimension — no evidence either way
		}
		if score >= w.ControlMean-w.MinLift {
			if w.Failures != 0 {
				w.Failures = 0
				if err := ws.UpdateWatch(ctx, w); err != nil {
					return err
				}
			}
			continue
		}
		w.Failures++
		if w.Failures < w.Threshold {
			if err := ws.UpdateWatch(ctx, w); err != nil {
				return err
			}
			continue
		}
		// Tripped — demote, restore, escalate (each step audited; a partial failure surfaces).
		reason := fmt.Sprintf("regression watch tripped: %d consecutive judged sessions below control %.2f−%.2f",
			w.Failures, w.ControlMean, w.MinLift)
		if _, err := Transition(ctx, st, lg, w.VersionID, StatusRetired, reason); err != nil {
			return fmt.Errorf("watch demotion of version %d: %w", w.VersionID, err)
		}
		if w.PriorVersion > 0 {
			// The prior version is retired (superseded at graduation) — restoration is a NEW draft row
			// carrying its body, admitted straight to production through the audited path. Terminal
			// states stay terminal (append-only history); the restore is its own ledgered decision.
			if err := restorePrior(ctx, st, lg, w); err != nil {
				return fmt.Errorf("watch restore of prior version %d: %w", w.PriorVersion, err)
			}
		}
		if err := ws.CloseWatch(ctx, w.VersionID, reason); err != nil {
			return err
		}
		if esc != nil {
			_ = esc(ctx, fmt.Sprintf("skill:%s", w.SkillName), reason+" — graduate retired, prior version restored")
		}
	}
	return nil
}

// restorePrior re-instates the pre-graduation body as a fresh production version (append-only history:
// a retired row never resurrects; its BODY returns under a new version id, rationale-linked).
func restorePrior(ctx context.Context, st Store, lg Ledger, w WatchState) error {
	prior, err := st.GetVersion(ctx, w.PriorVersion)
	if err != nil {
		return err
	}
	creator, ok := st.(interface {
		CreateVersion(context.Context, Version) (Version, error)
	})
	if !ok {
		return fmt.Errorf("store cannot create the restore row")
	}
	restored, err := creator.CreateVersion(ctx, Version{
		SkillName: prior.SkillName, Version: prior.Version + "+restored",
		Body: prior.Body, AppliesWhen: prior.AppliesWhen,
		ContentHash: ContentHash(prior.Body, prior.AppliesWhen),
		Author:      "flywheel", Source: "watch-restore:" + fmt.Sprint(w.VersionID),
		Rationale:       "restore of v" + prior.Version + " after the graduate's regression watch tripped",
		ParentVersionID: prior.ID,
	})
	if err != nil {
		return err
	}
	if _, err := Transition(ctx, st, lg, restored.ID, StatusTrial, "watch-restore admission (mechanical)"); err != nil {
		return err
	}
	_, err = Transition(ctx, st, lg, restored.ID, StatusProduction, "watch-restore promotion — the pre-graduation body returns")
	return err
}
