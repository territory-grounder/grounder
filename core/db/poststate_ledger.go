package db

import (
	"context"
	"time"

	"github.com/territory-grounder/grounder/core/verify"
)

// LedgerObserver reads the estate's currently-open alerts from TG's OWN ingest ledger: the (host, rule) pairs
// whose latest event is a raise or a re-fire, with no recovery after it, inside the staleness bound. It has
// the shape of the post-state and clear readers the actuation chain consumes (verify.ObservedAlert, ok).
//
// WHY IT EXISTS (TG-625). On the ACTUATION plane those readers were the LibreNMS /api/v0/alerts read, and that
// plane's LibreNMS token is topology-scoped BY DESIGN (TG-337: the plane that holds the mutation keys must
// never read untrusted alert bodies), so the read returned 403 on every call. Every execution's verdict was
// therefore NULL ("unverifiable"), and since 2026-07 no action TG took on that plane carried evidence of what
// it did. This reader keeps TG-337 whole: it selects host and alert_rule only — never summary or labels — from
// tables the actuation role already reads. It also sees Alertmanager incidents that carry a host label (e.g.
// am-PVEMemoryPressureHigh-<node>), which LibreNMS never did; host-less Alertmanager rules (cluster-scoped k8s
// alerts store host=”) are invisible to it, exactly as they are to every host-keyed reader.
//
// WHAT IT IS TRUSTED FOR. The immediate verdict this feeds can DEMOTE but never PROMOTE (interceptor
// recordGraduation, REQ-1223); credit is decided at the session terminus after the settled clear check and the
// collateral re-check (temporal/runner/reconcile.go). So a seconds-old ledger reading can only ever cost an
// op-class a deviation, never earn it one.
//
// HONEST ok. ok=true asserts "the estate was observable just now", so it requires TWO things:
//   - the read itself succeeded; and
//   - the front door is ALIVE: some delivery, from any source, arrived within freshWithin. A silent intake makes
//     an empty open-set indistinguishable from a blind one — and an empty set read as "quiet" would license a
//     `match` verdict and a "no longer necessary" refusal on zero evidence. Silence is ok=false, never empty.
//
// Sized on the live estate: across 14 days of deliveries the largest gap between any two was 52 minutes
// (16 gaps over 30m, none over 60m), so the default bound of 60 minutes does not read a normal lull as blind.
type LedgerObserver struct {
	hist        *AlertHistoryStore
	p           *Pool
	staleAfter  time.Duration
	freshWithin time.Duration
	now         func() time.Time
}

// NewLedgerObserver builds the reader. staleAfter bounds an open incident whose recovery never arrived (the
// caller passes ingest.MaxOpenIncident, the same bound the suppression and baseline readers use); freshWithin
// is the front-door liveness bound.
func NewLedgerObserver(p *Pool, staleAfter, freshWithin time.Duration) *LedgerObserver {
	return &LedgerObserver{hist: NewAlertHistoryStore(p), p: p, staleAfter: staleAfter, freshWithin: freshWithin, now: time.Now}
}

// Observe returns every currently-open (host, rule) pair. The host and site arguments are accepted for the
// reader's shape and deliberately unused: the verdict needs the WHOLE estate's post-state (a cascade lands on
// hosts the action never named), exactly as the LibreNMS reader it replaces returned every active alert.
func (o *LedgerObserver) Observe(ctx context.Context, _, _ string) ([]verify.ObservedAlert, bool) {
	if o == nil || o.p == nil {
		return nil, false
	}
	now := o.now().UTC()
	live, err := o.frontDoorLive(ctx, now)
	if err != nil || !live {
		return nil, false // unreadable or silent intake: not a quiet estate (see the type comment)
	}
	pairs, err := o.hist.OpenIncidentPairs(ctx, now, o.staleAfter)
	if err != nil {
		return nil, false
	}
	if pairs == nil {
		pairs = []verify.ObservedAlert{} // a READ that found nothing open is an empty set, not an absent one
	}
	return pairs, true
}

// ObserveOpenHosts is the CLEAR/NECESSITY reader: every host holding an open incident, at HOST granularity —
// the same question the necessity gate asks ("is the target still alerting at all?") and the same set its
// durable fallback already reads (OpenIncidentHosts, TG-454), so the primary and the fallback cannot disagree.
// Unlike the pair walk it keeps an incident whose alert_rule is empty. Same honest ok as Observe.
func (o *LedgerObserver) ObserveOpenHosts(ctx context.Context, _, _ string) ([]verify.ObservedAlert, bool) {
	if o == nil || o.p == nil {
		return nil, false
	}
	now := o.now().UTC()
	live, err := o.frontDoorLive(ctx, now)
	if err != nil || !live {
		return nil, false
	}
	hosts, err := o.hist.OpenIncidentHosts(ctx, now, o.staleAfter)
	if err != nil {
		return nil, false
	}
	out := make([]verify.ObservedAlert, 0, len(hosts))
	for h := range hosts {
		out = append(out, verify.ObservedAlert{Host: h})
	}
	return out, true
}

// frontDoorLive reports whether any accepted delivery (every source writes one ingest_alert_occurrence row per
// delivery, TG-399) arrived within freshWithin of now. It reads one indexed timestamp, never a row's content.
func (o *LedgerObserver) frontDoorLive(ctx context.Context, now time.Time) (bool, error) {
	var newest *time.Time
	if err := o.p.QueryRow(ctx,
		`SELECT max(received_at) FROM ingest_alert_occurrence WHERE received_at <= $1`, now).Scan(&newest); err != nil {
		return false, err
	}
	return newest != nil && now.Sub(*newest) <= o.freshWithin, nil
}
