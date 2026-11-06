package runner

// TG-483 (TG-146 C1 carve): the mechanical deviation verdict is computed at the interceptor's ~1s
// post-execution read and then FROZEN, and ConfirmedClear re-observes only the incident's own host — so a
// COLLATERAL cascade our heal causes on a SIBLING host, surfacing after the verdict read but within the
// settle window, was invisible to both, and the run graded clean. This activity is the terminus-time
// re-check: it asks TG's OWN durable alert capture whether any (host, rule) FIRST surfaced inside the
// action target's blast radius during the settle window. The workflow runs it at the session terminus
// (after the ConfirmedClear loop, i.e. after the settle window has actually elapsed) and a positive
// answer blocks both the auto-close and the graduation credit — the frozen execute-time MATCH no longer
// outranks damage that surfaced later.
//
// The answer is POSITIVELY typed (Observed *bool), the same discipline as the commit-confirm consult's
// ObservedAlerting: true and false are earned readings; nil is "could not observe" (no graph members, no
// reader) and the caller must treat it as unknown — never as an all-clear. Fail direction: unknown/error
// changes NOTHING (today's behavior) — the detection residual this ticket closes is the false CLEAN, and
// an observability outage must not convert every terminus into a deviation hold (the credit side already
// has its own belts, REQ-1223 terminus promote + TG-124).

import (
	"context"
	"strings"
	"time"

	"go.temporal.io/sdk/workflow"
)

// CollateralHit is one (host, rule) that first surfaced inside the collateral window — the runner-local
// mirror of core/db.CollateralAlert (the runner never imports core/db; the composition root maps).
type CollateralHit struct {
	Host      string
	AlertRule string
}

// ObserveCollateralInput anchors the scan. Anchor is the ACTION TARGET resolved guest-first (the entity
// whose blast radius the heal perturbed — params["guest"] over Action.Target, the REQ-2908 resolution),
// while ExcludeHost/ExcludeRule are the INCIDENT's own identity (env.Host / env.AlertRule — whose re-fire
// is flap machinery's business, not collateral). Since is actionAt: the heal instant.
type ObserveCollateralInput struct {
	Anchor      string
	ExcludeHost string
	ExcludeRule string
	Since       time.Time
}

// ObserveCollateralResult carries the positively-typed answer plus the evidence rows (bounded) for the
// reconciler's reason string and the operator's log line.
type ObserveCollateralResult struct {
	Observed *bool
	Hits     []CollateralHit
}

// ObserveCollateralActivity performs the terminus collateral scan. Decision table:
//   - no anchor / nil seams / no enumerable members → Observed=nil (UNKNOWN — nothing was observed, and
//     the activity says so rather than fabricating a clean estate from a blind read);
//   - members enumerated, reader errors → error (the workflow's .Get error path treats it as unknown);
//   - members enumerated, reader answers → Observed=&true with the hits, or &false on a genuinely
//     surveyed-and-quiet radius.
//
// The anchor itself is excluded from the member scan only implicitly: if the graph includes the anchor in
// its own radius, a NEW rule first-surfacing on the anchor (not the incident's own family) IS collateral —
// the heal was supposed to fix the incident, not light up its target with a different alert.
func (a *Activities) ObserveCollateralActivity(ctx context.Context, in ObserveCollateralInput) (ObserveCollateralResult, error) {
	anchor := strings.TrimSpace(in.Anchor)
	if anchor == "" || a.D.BlastMembers == nil || a.D.CollateralOpenedSince == nil {
		return ObserveCollateralResult{}, nil // unknown — unobservable, stated as such
	}
	members := a.D.BlastMembers(anchor)
	if len(members) == 0 {
		return ObserveCollateralResult{}, nil // graph cannot enumerate (unseeded / unresolvable) — unknown
	}
	hits, err := a.D.CollateralOpenedSince(ctx, members, in.ExcludeHost, in.ExcludeRule, in.Since)
	if err != nil {
		return ObserveCollateralResult{}, err
	}
	observed := len(hits) > 0
	return ObserveCollateralResult{Observed: &observed, Hits: hits}, nil
}

// logCollateralHits names the observed collateral on the workflow logger (replay-safe — the SDK suppresses
// logs during replay) so the operator's first read of a demoted terminus says WHICH neighbors lit up, not
// just that something did. Bounded upstream (the store LIMITs its scan).
func logCollateralHits(ctx workflow.Context, externalRef string, hits []CollateralHit) {
	l := workflow.GetLogger(ctx)
	for _, h := range hits {
		l.Warn("terminus collateral observed", "external_ref", externalRef, "host", h.Host, "alert_rule", h.AlertRule)
	}
}
