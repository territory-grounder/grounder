package main

// COST / mutation-breaker governance adapters + the fail-safe escalation oracle, carved out of main()'s
// composition root (TG-501 LOC-debt paydown). The trip recorders hash-chain a breaker halt into the
// governance ledger (INV-19); breakerRearmer is the audit-before-effect recovery counterpart;
// readCostConfig / readCostRates parse the config-not-code spend policy from TG_COST_* env; failSafeActive
// is the escalation-oracle default (fail SAFE toward re-escalation, never toward closure). Behaviour is
// unchanged by the move; the composition-root call sites stay in main().

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/cost"
	"github.com/territory-grounder/grounder/core/safety"
)

// ledgerTripRecorder binds a mutation-breaker auto-trip to the governance ledger (safety.TripRecorder):
// when the breaker trips and disables the gate, the halt is hash-chained like every other governance
// decision (INV-19). A nil ledger is a no-op.
type ledgerTripRecorder struct{ l *audit.Ledger }

func (r ledgerTripRecorder) RecordTrip(reason string) {
	if r.l == nil {
		return
	}
	if _, err := r.l.Append(audit.GovDecision{
		Decision: "safety:breaker-trip",
		Reason:   reason,
		ActionID: "mutation-breaker-trip",
		Withheld: true, // autonomy withheld — the breaker turned mutation off
	}); err != nil {
		log.Printf("mutation breaker: trip applied but ledger append failed: %v", err)
	}
}

// breakerRearmer implements policy.BreakerRearmer: the RECOVERY counterpart to ledgerTripRecorder. When an
// owner-gated mode transition escalates INTO an actuating mode, the ModeController calls Rearm to clear a
// deviation breaker a prior trip left durably open (spec/015 REQ-1523) — without which one (possibly false)
// trip permanently refuses all actuation even after the mode is restored. It appends the audit record
// BEFORE clearing the breaker (audit-before-effect, mirroring the mode transition itself); an append failure
// returns the error and leaves the breaker OPEN (fail-safe — actuation stays halted, never half-enabled). It
// lives in the worker, the single process holding the armed breaker, its shared cross-process store, and the
// ledger writer, so one worker's re-arm closes the shared row for every sibling.
type breakerRearmer struct {
	mb     *safety.MutationBreaker
	ledger *audit.Ledger
}

func (r breakerRearmer) Rearm(ctx context.Context) error {
	if r.mb == nil {
		return nil
	}
	if r.ledger != nil {
		if _, err := r.ledger.Append(audit.GovDecision{
			Decision: "safety:breaker-rearm",
			Reason:   "deviation breaker re-armed on an owner-gated escalation into an actuating mode (spec/015 REQ-1523) — trip cleared so actuation can resume",
			ActionID: "mutation-breaker-rearm",
		}); err != nil {
			return fmt.Errorf("breaker re-arm audit append failed, breaker left open: %w", err)
		}
	}
	return r.mb.Rearm(ctx)
}

// costLedgerTripRecorder binds a COST-breaker auto-trip to the governance ledger (cost.TripRecorder): when
// the daily budget or a session ceiling is exceeded and the breaker force-Shadows, the halt is hash-chained
// like every other governance decision (INV-19). Distinct from the mutation breaker's recorder by its
// 'cost:breaker-trip' decision label so a spend halt is auditable apart from a safety halt. A nil ledger is
// a no-op.
type costLedgerTripRecorder struct{ l *audit.Ledger }

func (r costLedgerTripRecorder) RecordTrip(reason string) {
	if r.l == nil {
		return
	}
	if _, err := r.l.Append(audit.GovDecision{
		Decision: "cost:breaker-trip",
		Reason:   reason,
		ActionID: "cost-breaker-trip",
		Withheld: true, // autonomy withheld — the spend guard forced the mode to Shadow
	}); err != nil {
		log.Printf("cost breaker: trip applied but ledger append failed: %v", err)
	}
}

// readCostConfig reads the operator-declared spend policy (config-not-code) from TG_COST_* env into a
// cost.Config. Money defaults to 0 = DISABLED (a budget/rate that is not set never enforces — the spend
// guard's fail-open posture). Per-model rates come from every TG_COST_RATE_<model>_PER_1K variable (the
// <model> is the gateway tier the agent calls, e.g. "fast" / "primary"); TG_COST_DEFAULT_RATE_PER_1K is
// the fallback for a model with no explicit rate.
func readCostConfig() cost.Config {
	return cost.Config{
		Rates:             readCostRates(),
		DefaultRate:       envFloat("TG_COST_DEFAULT_RATE_PER_1K", 0),
		PerActuationUSD:   envFloat("TG_COST_PER_ACTUATION_USD", 0),
		DailyBudgetUSD:    envFloat("TG_COST_DAILY_BUDGET_USD", 0),
		SessionCeilingUSD: envFloat("TG_COST_SESSION_CEILING_USD", 0),
	}
}

// readCostRates scans the environment for TG_COST_RATE_<model>_PER_1K variables, extracting the per-model
// USD-per-1k-tokens rate keyed by <model>. A non-positive/invalid value is skipped (config-not-code; a
// zero rate contributes no cost, so there is no reason to record it).
func readCostRates() map[string]float64 {
	const (
		prefix = "TG_COST_RATE_"
		suffix = "_PER_1K"
	)
	rates := map[string]float64{}
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		key, val := kv[:eq], kv[eq+1:]
		if !strings.HasPrefix(key, prefix) || !strings.HasSuffix(key, suffix) {
			continue
		}
		model := strings.TrimSuffix(strings.TrimPrefix(key, prefix), suffix)
		if model == "" {
			continue
		}
		if v, err := strconv.ParseFloat(strings.TrimSpace(val), 64); err == nil && v > 0 {
			rates[model] = v
		}
	}
	return rates
}

// failSafeActive is the escalation condition oracle DEFAULT (core/escalation.ConditionChecker): with no
// live post-condition reader wired it treats every incident as STILL ACTIVE, so a due re-check re-escalates
// to a human rather than silently dropping an unresolved incident (fail SAFE toward escalation — never
// toward closure). A live LibreNMS active-alert oracle can later replace it to also DEFER a genuinely
// recovered incident. It reads nothing and mutates nothing.
type failSafeActive struct{}

func (failSafeActive) StillActive(context.Context, string) (bool, error) { return true, nil }
