package main

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/core/db"
	coreingest "github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/verify"
	"github.com/territory-grounder/grounder/temporal/runner"
)

// postStateLedgerFreshWithin is the front-door liveness bound the ledger reader requires before it will call
// the estate "observed" (see db.LedgerObserver). 14 days of live deliveries: max gap 52m, none over 60m.
const postStateLedgerFreshWithin = time.Hour

// postStateSource decides which reader backs the post-state verdict and the clear/necessity check (TG-625).
//
//	TG_POSTSTATE_SOURCE=auto (default) ⇒ the ledger on an ACTUATION-only plane, LibreNMS everywhere else
//	TG_POSTSTATE_SOURCE=ledger         ⇒ the ledger on every plane
//	TG_POSTSTATE_SOURCE=librenms       ⇒ LibreNMS on every plane (the pre-TG-625 wiring; the rollback knob)
//
// WHY auto PICKS BY PLANE. The actuation plane's LibreNMS token is topology-scoped by design (TG-337), so its
// /alerts read returns 403 on every call and every execution there was verdict-NULL. The triage and `both`
// planes hold the alert-reading token and keep the independent monitor exactly as before (byte-identical).
func postStateSource(get func(k, def string) string, plane credential.ProcessPlane) string {
	switch v := strings.ToLower(strings.TrimSpace(get("TG_POSTSTATE_SOURCE", "auto"))); v {
	case "ledger", "librenms":
		return v
	default:
		if plane == credential.ProcessPlaneActuation {
			return "ledger"
		}
		return "librenms"
	}
}

// applyLedgerReaders installs the two ledger readers: the PAIR reader backs the verdict's post-state (a cascade
// is a new (host, rule)), the HOST reader backs the clear/necessity check (host-quiet, the TG-454 fallback's
// own granularity). Split out so the assignment itself is testable without a database.
func applyLedgerReaders(deps *runner.Deps, pairs, hosts func(context.Context, string, string) ([]verify.ObservedAlert, bool)) {
	deps.PostStateObserve = pairs
	deps.ClearObserve = hosts
}

// wirePostStateSource points deps.PostStateObserve and deps.ClearObserve at TG's own ledger when
// postStateSource says so. It must run AFTER the falsifiability scorer captured its observer (that scorer
// stays bound to the LibreNMS reader on every plane, so both planes keep scoring with one instrument) and
// BEFORE runner.NewActivities copies deps. A nil pool leaves the existing wiring untouched.
func wirePostStateSource(deps *runner.Deps, pool *db.Pool, plane credential.ProcessPlane) {
	src := postStateSource(getenv, plane)
	if src != "ledger" {
		log.Printf("post-state: verdict + clear reads use LibreNMS active alerts (TG_POSTSTATE_SOURCE→%s, plane=%s)", src, plane)
		return
	}
	if pool == nil {
		log.Printf("post-state: TG_POSTSTATE_SOURCE→ledger but no durable pool — keeping the existing reader (verdicts stay unverifiable where it cannot read)")
		return
	}
	obs := db.NewLedgerObserver(pool, coreingest.MaxOpenIncident, postStateLedgerFreshWithin)
	// TG-681: the per-SITE front-door arm. The estate-wide bound above is satisfied by the chatty site while the
	// acting site's own intake may have been silent for days; this records the acting site's newest-delivery age
	// on every observation and, only when the operator sets a bound, reads an older site as UNOBSERVABLE.
	bound := siteLivenessBound(getenv)
	policy := db.SiteLivenessPolicy{LastDelivery: db.NewSiteFrontDoor(pool).LastDelivery, Bound: bound, Now: time.Now, Log: log.Printf}
	applyLedgerReaders(deps, policy.Wrap(obs.Observe), policy.Wrap(obs.ObserveOpenHosts))
	log.Printf("post-state: verdict + clear/necessity reads use TG's OWN ingest ledger (TG-625; plane=%s) — open (host, rule) pairs incl. re-fires and Alertmanager, host+rule only (TG-337 holds), ok only while the front door delivered within %s; per-site front-door arm: %s (TG-681)", plane, postStateLedgerFreshWithin, siteLivenessPosture(bound))
}

// siteLivenessKey is the operator knob for the per-site arm (TG-681): a Go duration. Unset, empty, unparsable or
// non-positive ⇒ RECORD-ONLY (the site age is logged on every observation and decides nothing). A duration
// makes a site whose newest accepted delivery is older than it read unobservable for reads that name that site.
// It is a bound on the acting SITE's intake, not on the target host: the ledger holds no per-host heartbeat.
const siteLivenessKey = "TG_POSTSTATE_SITE_FRESH_WITHIN"

// siteLivenessBound parses the per-site bound. The fail direction is deliberate: a bad value is record-only,
// never a surprise refusal — the knob tightens only when it parses.
func siteLivenessBound(get func(k, def string) string) time.Duration {
	v, err := time.ParseDuration(strings.TrimSpace(get(siteLivenessKey, "")))
	if err != nil || v <= 0 {
		return 0
	}
	return v
}

// siteLivenessPosture renders the arm's posture for the boot line.
func siteLivenessPosture(bound time.Duration) string {
	if bound <= 0 {
		return "RECORD-ONLY (" + siteLivenessKey + " unset — the acting site's newest-delivery age is logged per observation and decides nothing)"
	}
	return "ENFORCING (a read naming a declared site whose newest delivery is older than " + bound.String() + " is unobservable — verdict withheld, no credit)"
}
