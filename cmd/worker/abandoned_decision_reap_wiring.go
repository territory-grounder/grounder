package main

import (
	"context"
	"log"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/persist"
	"github.com/territory-grounder/grounder/temporal/runner"
)

// wireAbandonedDecisionReap arms the abandoned pending-decision sweep, carved out of main()'s composition
// root (TG-501 LOC-debt paydown). See the comments below for the full rationale. A no-op unless
// pendingWriter is backed by the durable *db.PendingStore. Behaviour is unchanged by the move.
func wireAbandonedDecisionReap(pendingWriter persist.PendingWriter) {
	// REAP ABANDONED DECISIONS — a periodic sweep, because the row that strands is created BY the event
	// that strands it. pending_decision is written when a poll opens and cleared when the workflow records
	// an outcome; a workflow killed before resolving (a worker restart mid-deploy is the ordinary way)
	// leaves its row open forever. Nothing reconciled the table against workflow liveness, so the console
	// listed decisions with caller_can_act=true whose vote could only ever 409.
	//
	// Measured live 2026-07-29: 13 of 136 open decisions past the 24h VoteWait bound, oldest 84.5h; voting
	// the three eldest returned "no waiting decision for that ref".
	//
	// The deadline is VoteWait plus an hour of margin: inside that window a human can still answer and the
	// workflow's own timer may still fire, so reaping there would close a live decision out from under
	// both. The sweep is idempotent (status='open' only), so a human vote is never overwritten.
	if ps, ok := pendingWriter.(*db.PendingStore); ok && ps != nil {
		const reapEvery = 30 * time.Minute
		reapOlderThan := runner.VoteWait + time.Hour
		go func() {
			t := time.NewTicker(reapEvery)
			defer t.Stop()
			for {
				// A bounded context per sweep, like the sibling pollers: a hung DB must not wedge the
				// goroutine, and the sweep is cheap enough that a short deadline is generous.
				sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				now := time.Now().UTC()
				n, err := ps.ReapAbandoned(sctx, now.Add(-reapOlderThan), now)
				cancel()
				if err != nil {
					log.Printf("decisions: abandoned-decision sweep failed (non-blocking): %v", err)
				} else if n > 0 {
					log.Printf("decisions: reaped %d abandoned decision(s) open past %s — their workflow no longer exists, so no vote could reach them", n, reapOlderThan)
				}
				<-t.C
			}
		}()
	}
}
