package main

import (
	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/config"
)

// buildModelGateway constructs the reasoning-model gateway and applies the three operator-set OUTPUT bounds,
// carved out of main()'s composition root (TG-501 LOC-debt paydown). Every bound ships INERT — 0 leaves the
// gateway at today's behaviour (unbounded concurrency / omitted max_tokens / unbounded session budget) — and
// each is read through the store-resolving getenv/envInt so a value an operator saves in the console actually
// binds. Behaviour is unchanged by the move; worker_model_budget_test.go pins that all three bounds are wired.
func buildModelGateway() *model.Gateway {
	gw := model.NewGateway(getenv("TG_LITELLM_URL", "http://litellm:4000"), config.SecretRef(getenv("TG_LITELLM_KEY_REF", "env:LITELLM_MASTER_KEY")))
	// TG-384: bound concurrent model calls so a cascade (157 alerts → 157 simultaneous investigations) cannot
	// self-DoS the brain. Ships INERT — 0 leaves the gateway unbounded (today's behaviour), so this deploy
	// changes nothing until an operator sets TG_MODEL_MAX_CONCURRENCY (size it from the 8-slot sidecar, ~8-16).
	gw.SetMaxConcurrency(envInt("TG_MODEL_MAX_CONCURRENCY", 0))
	// TG-48: per-completion output ceiling. The model client set no token budget at all, so a runaway
	// high-severity investigation could spend an unbounded number of output tokens per call and the cost
	// breaker only trips AFTER the daily budget is blown. Ships INERT — 0 omits max_tokens, leaving the
	// model/gateway default (today's behaviour) — until an operator sets TG_MODEL_MAX_TOKENS to a runaway
	// ceiling high enough not to truncate a legitimate long completion.
	gw.MaxTokens = envInt("TG_MODEL_MAX_TOKENS", 0)
	// TG-48: per-SESSION cumulative output-token budget. MaxTokens above bounds one call; this bounds a whole
	// investigation — a looping session can make many in-budget calls and still spend without bound, and the
	// daily cost breaker only trips globally, after the fact. Keyed on the per-session user ("runner:"+ref), a
	// session that reaches the ceiling has its next completion refused (fail-safe: incomplete, not empty). Ships
	// INERT — 0 leaves it unbounded (today's behaviour) — until an operator sets TG_MODEL_SESSION_TOKEN_BUDGET
	// to a runaway ceiling high enough not to truncate a legitimate deep investigation.
	gw.SetSessionTokenBudget(envInt("TG_MODEL_SESSION_TOKEN_BUDGET", 0))
	return gw
}
