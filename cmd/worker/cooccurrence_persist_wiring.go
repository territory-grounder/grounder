package main

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/learn"
)

// wireCoOccurrencePersist restores the co-occurrence learner's last durable snapshot and arms its periodic
// persistence, carved out of main()'s composition root (TG-501 LOC-debt paydown, TG-388 face c). See the
// comments below for the full rationale. Competence-plane only: it never actuates. Behaviour is unchanged
// by the move.
func wireCoOccurrencePersist(pool *db.Pool, learner *learn.CoOccurrenceLearner) {
	// TG-388 face c: the co-occurrence learner lived only in memory, so a redeploy wiped the whole
	// self-learning tier (measured 1,524 learned edges -> 0 on a routine `docker compose up`). Now that a
	// pool exists, RESTORE the last snapshot BEFORE any alert reaches the learner — its Observe feed is
	// wired much later and no incident is processed during this boot setup — then persist it periodically
	// so the tier survives the next restart instead of re-learning from zero. Competence-plane: a load or
	// save failure is non-fatal (the learner just starts/continues in memory), it never actuates.
	coStore := db.NewCoOccurrenceStore(pool)
	loadCtx, loadCancel := context.WithTimeout(context.Background(), 15*time.Second)
	if snap, lerr := coStore.Load(loadCtx); lerr != nil {
		log.Printf("learner: co-occurrence load failed — starting empty (non-fatal): %v", lerr)
	} else if len(snap.Pairs) > 0 || len(snap.Trials) > 0 {
		learner.Restore(snap)
		log.Printf("learner: restored %d co-occurrence pair(s) over %d host(s) from co_occurrence — the self-learning tier survived the restart (TG-388)", len(snap.Pairs), len(snap.Trials))
	}
	loadCancel()
	// "0"/"off" DISABLES persistence explicitly (envDuration alone cannot express this — it treats any
	// non-positive value as "use the default", so the raw string is the only place an off-switch can live).
	if raw := strings.TrimSpace(getenv("TG_LEARN_PERSIST_INTERVAL", "")); raw == "0" || raw == "off" {
		log.Printf("learner: co-occurrence persistence DISABLED (TG_LEARN_PERSIST_INTERVAL=%q) — in-memory-only lifetime, wiped on restart (TG-388)", raw)
	} else {
		persistEvery := envDuration("TG_LEARN_PERSIST_INTERVAL", 15*time.Minute)
		go func() {
			t := time.NewTicker(persistEvery)
			defer t.Stop()
			for range t.C {
				if serr := coStore.Save(context.Background(), learner.Snapshot()); serr != nil {
					log.Printf("learner: co-occurrence save failed (non-fatal, the in-memory tier stands): %v", serr)
				}
			}
		}()
		log.Printf("learner: co-occurrence snapshot persisted every %s (TG_LEARN_PERSIST_INTERVAL) — competence-plane; never actuates (TG-388)", persistEvery)
	}
}
