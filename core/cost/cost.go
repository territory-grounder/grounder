// Package cost is Territory Grounder's spend guard: a running cost accumulator plus a $-ceiling circuit
// breaker, modeled CLOSELY on the safety mutation breaker (core/safety.MutationBreaker over core/breaker)
// but guarding a different thing. The mutation breaker guards SAFETY — a deviation/chain-gap forces the
// mode to Shadow and it FAILS CLOSED (an unreadable safety breaker reads OPEN, so a sibling can never
// actuate on an unobservable safety floor). This breaker guards SPEND — the daily/session LLM+actuation
// budget — and it deliberately FAILS OPEN: an unreadable cost store must NOT halt legitimate operations,
// because a cost-store outage is not a safety event. A blown budget is money, not a broken invariant, so a
// cost-store we cannot read is treated as "not tripped" and LOGGED loudly, never as a halt.
//
// The two breakers are INDEPENDENT: this package never imports core/safety.MutationBreaker, never touches
// the never-auto floor / chokepoint semantics, and adds ONLY a spend guard — it never enables actuation.
// Its single reaction to a trip is the SAME kill wire the mutation breaker uses: force the mode chokepoint
// to Shadow (ForceShadow). Under Shadow that force is a no-op (nothing to halt), so — like the mutation
// breaker — it is INERT as a halt while the mode is already Shadow; it becomes load-bearing only once a
// canary escalates the mode. Unlike the mutation breaker it still ACCRUES under Shadow (a read-only
// investigation still spends model tokens), so it can trip and record even today; only its halt effect is
// inert until the flip.
//
// DURABLE + CROSS-PROCESS (like mutation_breaker_state, migration 0021): the day/session accumulators and
// the breaker's open/closed state live in a pgx Store (migration 0023, cost_accrual + cost_breaker_state),
// so every sibling worker accrues into ONE shared daily total and reads ONE shared trip state. A budget
// trip in one worker force-Shadows every sibling: each worker's NEXT model completion re-reads the shared
// daily total (already over budget) and the shared OPEN state, and force-Shadows its own mode. The
// in-memory MemStore is the pure oracle used in tests and the single-process (no-DB) fast path.
//
// Single-organization (paradigm-rule 1): one logical cost breaker named "cost"; no tenant dimension.
//
// Provenance: [O] INV-09/INV-15/INV-21 · CONSTITUTION.md:130 ("named, observable circuit breakers with
// persisted state") · spec/013 REQ-1211..1215 (the spend-guard sibling of the mutation breaker REQ-1210).
package cost

import (
	"context"
	"time"
)

// BreakerName is the stable slug of the single logical cost breaker (single-org). It keys the shared
// cost_breaker_state row and labels the tg_cost_breaker_state gauge.
const BreakerName = "cost"

// Bucket kinds for the durable accumulators. BucketDay is the UTC-date-keyed daily-budget accumulator;
// BucketSession is the per-triage-session accumulator (keyed by external_ref). Both accrue additively
// (each worker adds its increment to the shared running total).
const (
	BucketDay     = "day"
	BucketSession = "session"
)

// Store is the durable, cross-process persistence for the spend guard (pgx in production, MemStore in
// tests / the no-DB fast path). It holds two things: the additive day/session accumulators (cost_accrual)
// and the latest-wins breaker state (cost_breaker_state). Every method is safe for concurrent use across
// goroutines AND processes — the pgx implementation coordinates through shared rows exactly as the
// in-memory twin coordinates through a shared map, which is what lets one worker's trip be read by another.
type Store interface {
	// Accrue ADDS usd to the (kind,key) bucket and returns the new running total. It is an additive upsert
	// (not latest-wins): concurrent workers each add their increment to the same shared total.
	Accrue(ctx context.Context, kind, key string, usd float64) (total float64, err error)
	// Total reads the current running total for a (kind,key) bucket; an unseen bucket is 0 (not an error).
	Total(ctx context.Context, kind, key string) (total float64, err error)
	// BreakerOpen reports whether the shared cost breaker state row is OPEN (a trip), with the recorded
	// reason. An unseen breaker is CLOSED (open=false) — the correct never-yet-tripped default.
	BreakerOpen(ctx context.Context) (open bool, reason string, err error)
	// TripBreaker sets the shared cost breaker state OPEN (latest-wins upsert by the single "cost" name),
	// stamping the trip reason and the day total at the moment of the trip. Idempotent (re-tripping an
	// already-open breaker just refreshes the reason/usd/timestamp).
	TripBreaker(ctx context.Context, reason string, usdAtTrip float64) error
}

// ShadowForcer is the one-method kill seam the breaker holds — the SAME seam the mutation breaker holds
// (core/safety.ShadowForcer). The mode chokepoint (*safety.Chokepoint) satisfies it structurally, so the
// spend guard's kill wire runs through the single source of truth without this package importing
// core/safety. ForceShadow drops the active mode to Shadow (read-only); it is always safe and idempotent.
type ShadowForcer interface {
	ForceShadow(reason string)
}

// TripRecorder records a cost-breaker trip to a durable audit surface (the worker binds it to the
// governance ledger so an auto-halt is hash-chained like every other decision, INV-19). OPTIONAL: a nil
// recorder still trips the breaker; it just adds no audit note. Defined here so this package need not
// import core/audit.
type TripRecorder interface {
	// RecordTrip is called AFTER the breaker has tripped and ForceShadow has run.
	RecordTrip(reason string)
}

// Config is the operator-declared spend policy (config-not-code; read from TG_COST_* env in the worker
// composition root, never hard-coded here). All money is US dollars.
//
// DISABLED semantics (fail-open for spend, documented deliberately): a budget of 0 (or absent) means that
// budget is NOT enforced — the breaker never trips on it. With BOTH DailyBudgetUSD and SessionCeilingUSD
// at 0 the breaker is a pure meter: it may still accrue for the tg_cost_usd_today gauge, but nothing ever
// halts. This is the spend-guard posture — a budget guard that is not configured must never block work.
type Config struct {
	// Rates maps a gateway model name (the tier the agent calls, e.g. "fast" / "primary") to its USD cost
	// per 1,000 tokens. A model absent here falls back to DefaultRate.
	Rates map[string]float64
	// DefaultRate is the USD-per-1k-tokens rate for a model with no explicit Rates entry (0 ⇒ that model
	// contributes no cost).
	DefaultRate float64
	// PerActuationUSD is a flat USD increment added per actuation (the effect path's per-op cost). Inert
	// while mutation is OFF (nothing actuates), it is part of the cost model armed for the flip.
	PerActuationUSD float64
	// DailyBudgetUSD is the UTC-day spend ceiling; 0 ⇒ the daily budget is DISABLED (never trips).
	DailyBudgetUSD float64
	// SessionCeilingUSD is the per-session spend ceiling; 0 ⇒ the session ceiling is DISABLED (never trips).
	SessionCeilingUSD float64
}

// Enabled reports whether ANY cost tracking is configured (a rate or a budget). When false the worker
// leaves the model gateway un-wrapped — zero overhead, zero behavior change, honestly disabled.
func (c Config) Enabled() bool {
	if c.DefaultRate > 0 || c.PerActuationUSD > 0 || c.DailyBudgetUSD > 0 || c.SessionCeilingUSD > 0 {
		return true
	}
	for _, r := range c.Rates {
		if r > 0 {
			return true
		}
	}
	return false
}

// enforces reports whether at least one budget is armed (a positive ceiling exists). With no budget armed
// the breaker never trips — it is a meter only.
func (c Config) enforces() bool {
	return c.DailyBudgetUSD > 0 || c.SessionCeilingUSD > 0
}

// rateFor returns the USD-per-1k-tokens rate for a model: its explicit Rates entry, else DefaultRate.
func (c Config) rateFor(model string) float64 {
	if r, ok := c.Rates[model]; ok {
		return r
	}
	return c.DefaultRate
}

// usdForTokens converts an approximate token count for a model into USD: (tokens / 1000) * rate. A
// non-positive token count contributes 0 (a defensive guard — an approximation is never negative).
func (c Config) usdForTokens(model string, tokens int) float64 {
	if tokens <= 0 {
		return 0
	}
	return (float64(tokens) / 1000.0) * c.rateFor(model)
}

// utcDayKey formats an instant as its UTC calendar date (YYYY-MM-DD) — the daily-budget bucket key. Keying
// on the UTC day means the daily total naturally resets at 00:00 UTC (a new key, a fresh $0 total); the
// breaker's force-Shadow, like the mutation breaker's, does NOT auto-re-enable at rollover — re-enabling
// actuation is a separate, owner-gated mode transition, never automatic.
func utcDayKey(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

// stateValue is the tg_cost_breaker_state gauge value: 0 closed / 2 open. It mirrors the mutation
// breaker's circuit_breaker_state scale (0 closed / 1 half-open / 2 open) — the cost breaker has no
// half-open probe (a spend guard is either within budget or over it), so it emits 0 or 2 only.
func stateValue(open bool) float64 {
	if open {
		return 2
	}
	return 0
}
