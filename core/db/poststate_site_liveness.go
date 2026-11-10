package db

// The per-SITE front-door arm of the ledger post-state reader (TG-681).
//
// WHY IT EXISTS. LedgerObserver (TG-625) answers "is the estate observable right now?" with ONE estate-wide
// question: did ANY accepted delivery arrive within freshWithin. On this estate that question is answered by
// the chatty site: over 14 days of production deliveries the busy site's 99th-percentile inter-delivery gap was
// 16 minutes while the quiet site went more than three hours between deliveries seven times and once went
// 90 hours. So a verdict for an action AT the quiet site can read "estate observable, no open pair on the
// target" while that site's intake has been silent for days — and silence is exactly what a dead poller, a
// severed inter-site path or a stopped alert bridge look like. The alert stream is a SHARED input: the same
// silence that would make the triggering alert stale makes the post-state read vacuous and the clear check
// pass, for the predictor, the verifier and the necessity re-check alike.
//
// WHAT THIS DOES. For a read that names a declared site, it measures that site's own newest-delivery age and
// records it on every observation. With a bound set, an older site reads UNOBSERVABLE (nil, false) — the
// verdict is withheld and no credit is granted, the same honest answer LedgerObserver already gives for a
// silent estate. With no bound (the default) it is record-only: the age is logged beside the read, nothing
// changes in the verdict, and the operator has the number to set a bound from.
//
// WHAT IT CANNOT DO, stated so nobody over-reads it. "No delivery from the site" is ambiguous between "the
// site is quiet" and "the site is blind" — the ledger holds no heartbeat, only state changes. A bound therefore
// trades missed-silence for withheld-verdicts on a genuinely quiet site, and the right value is an operator's
// decision over measured gaps, not a constant this file can know. The authoritative per-device freshness signal
// (the monitor's own poll age) is not readable from the actuation plane by design (TG-337).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/verify"
)

// PostStateReader is the shape of the two readers the actuation chain consumes (runner.Deps.PostStateObserve
// and ClearObserve): the observed alerts for a (targetHost, site) and whether the estate was observable.
type PostStateReader func(ctx context.Context, targetHost, site string) ([]verify.ObservedAlert, bool)

// SiteFrontDoor reads the newest accepted delivery stamped with one site. It reads one indexed timestamp and no
// alert content (TG-337 holds on the actuation plane).
type SiteFrontDoor struct{ p *Pool }

// NewSiteFrontDoor builds the per-site front-door reader over the ingest occurrence ledger.
func NewSiteFrontDoor(p *Pool) *SiteFrontDoor { return &SiteFrontDoor{p: p} }

// LastDelivery reports the newest received_at of any accepted delivery stamped with site, at or before now.
// known=false means the site has no delivery on record at all (a site that never delivered is not a fresh
// site). An error is a read failure, which a caller must not read as either.
func (s *SiteFrontDoor) LastDelivery(ctx context.Context, site string, now time.Time) (time.Time, bool, error) {
	if s == nil || s.p == nil {
		return time.Time{}, false, errors.New("db: site front door: no pool")
	}
	var newest *time.Time
	if err := s.p.QueryRow(ctx,
		`SELECT max(received_at) FROM ingest_alert_occurrence WHERE site = $1 AND received_at <= $2`, site, now).Scan(&newest); err != nil {
		return time.Time{}, false, err
	}
	if newest == nil {
		return time.Time{}, false, nil
	}
	return newest.UTC(), true, nil
}

// SiteLivenessPolicy wraps a PostStateReader with the per-site front-door arm. All collaborators are injected so
// the oracle drives it without a database; production wires SiteFrontDoor.LastDelivery, time.Now and log.Printf.
type SiteLivenessPolicy struct {
	// LastDelivery answers the site's newest accepted delivery (SiteFrontDoor.LastDelivery in production). nil
	// disables the arm entirely — the wrapped reader is returned unchanged.
	LastDelivery func(ctx context.Context, site string, now time.Time) (time.Time, bool, error)
	// Bound is the oldest newest-delivery age at which the site still counts as observable. 0 (the default) is
	// RECORD-ONLY: the age is logged and the inner reader's answer stands. A positive bound makes an older,
	// never-delivered or unreadable site answer (nil, false) — unobservable, verdict withheld.
	Bound time.Duration
	Now   func() time.Time
	Log   func(format string, args ...any)
}

// Wrap returns a reader that applies the policy before delegating to inner. A read with no site, an undeclared
// site label, or a site outside the canonical vocabulary carries no site claim and is passed through: the
// estate-wide liveness in LedgerObserver remains the only arm, exactly as before this file existed.
func (p SiteLivenessPolicy) Wrap(inner PostStateReader) PostStateReader {
	if inner == nil || p.LastDelivery == nil {
		return inner
	}
	return func(ctx context.Context, targetHost, site string) ([]verify.ObservedAlert, bool) {
		canon := ingest.CanonicalizeSite(site)
		if canon == "" || !ingest.IsDeclaredSite(canon) {
			return inner(ctx, targetHost, site)
		}
		now := p.clock()
		newest, known, err := p.LastDelivery(ctx, canon, now)
		switch {
		case err != nil:
			p.logf("post-state: site %s front-door liveness UNREADABLE (%v) — %s", canon, err, p.disposition(true))
			if p.Bound > 0 {
				return nil, false
			}
		case !known:
			p.logf("post-state: site %s has NO accepted delivery on record — %s", canon, p.disposition(true))
			if p.Bound > 0 {
				return nil, false
			}
		default:
			age := now.Sub(newest)
			if age < 0 {
				age = 0
			}
			stale := p.Bound > 0 && age > p.Bound
			p.logf("post-state: site %s front door age %s (newest delivery %s) — %s", canon, age.Round(time.Second),
				newest.UTC().Format(time.RFC3339), p.disposition(stale))
			if stale {
				return nil, false
			}
		}
		return inner(ctx, targetHost, site)
	}
}

// disposition renders what the policy did with this observation, so the log line states the consequence and
// not just the number.
func (p SiteLivenessPolicy) disposition(stale bool) string {
	if p.Bound <= 0 {
		return "record-only (no TG_POSTSTATE_SITE_FRESH_WITHIN bound; the estate-wide liveness arm decides)"
	}
	if stale {
		return fmt.Sprintf("OLDER than the %s site bound: reading as UNOBSERVABLE (verdict withheld, no credit)", p.Bound)
	}
	return fmt.Sprintf("within the %s site bound", p.Bound)
}

func (p SiteLivenessPolicy) clock() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p SiteLivenessPolicy) logf(format string, args ...any) {
	if p.Log != nil {
		p.Log(format, args...)
	}
}
