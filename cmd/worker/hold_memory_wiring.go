package main

import (
	"context"
	"log"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/holds"
	"github.com/territory-grounder/grounder/temporal/runner"
)

// wireHoldMemory arms the durable HOLD memory (TG-667, hold_decision, migration 0127): the store the runner
// writes beside every hold/deny ledger row and re-reads at classify time, so a person-caused or
// stateful/destructive hold is remembered when the same (host, rule) re-fires after the 30-minute attribution
// lookback has expired. It is cross-process by construction — the worker that held and the worker that
// triages the re-detection need not be the same process — so it is always the shared pgx store, never an
// in-memory one. Kept out of main() under the TG-501 LOC ratchet. The returned seams are typed as the runner
// expects (an interface and a func), so a caller without a pool simply leaves them nil: the runner then
// remembers nothing, which is the pre-TG-667 posture, and never sees a typed-nil interface.
func wireHoldMemory(pool *db.Pool) (runner.HoldRecorder, func(context.Context, string, string, time.Time) (holds.HoldDecision, bool, error)) {
	if pool == nil {
		log.Print("hold memory: no DB pool — holds are NOT remembered across re-detections (TG-667); the fallback approver judges every re-fire on fresh input only")
		return nil, nil
	}
	window := envDuration("TG_HOLD_REDETECT_WINDOW", holds.DefaultRedetectWindow)
	if window <= 0 {
		log.Print("hold memory: TG_HOLD_REDETECT_WINDOW=0 — OFF by operator choice; holds are recorded nowhere and never consulted (TG-667)")
		return nil, nil
	}
	store := db.NewHoldStore(pool)
	log.Printf("hold memory: armed on hold_decision — sticky holds keep their (host, rule) held for %s on re-detection (TG_HOLD_REDETECT_WINDOW; TG-667)", window)
	return store, store.OpenFor
}
