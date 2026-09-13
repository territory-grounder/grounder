package main

// Mutation-breaker CONFIG helpers, carved out of main()'s composition root (TG-501 LOC-debt paydown):
// the operator-declared trip threshold (fail toward the tightest setting) and the honest boot-log label for
// the shared breaker store's durability. Behaviour is unchanged by the move.

import (
	"strconv"
	"strings"

	"github.com/territory-grounder/grounder/core/db"
)

// mutationBreakerThreshold reads the operator-declared breaker trip threshold (config-not-code). The first
// canary uses 1 (a single deviation halts) per the readiness review; a non-positive/invalid value falls
// back to 1 — fail toward the tightest setting, never a looser one.
func mutationBreakerThreshold() int {
	n, err := strconv.Atoi(strings.TrimSpace(getenv("TG_MUTATION_BREAKER_THRESHOLD", "1")))
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// breakerStoreKind names the durability of the shared breaker store for the boot log — DURABLE and
// cross-process when a DB pool exists, in-process otherwise. Honest labelling: a trip that does not cross to
// siblings must not be logged as if it does.
func breakerStoreKind(pool *db.Pool) string {
	if pool != nil {
		return "DURABLE cross-process store (mutation_breaker_state)"
	}
	return "in-process store (single-worker; a trip does NOT cross to siblings)"
}
