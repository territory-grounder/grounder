package main

// wireConversationMemory arms the TG-80 P2-8 cross-session conversation memory: the runner deps' read
// (seed assembly) and write (terminal recorder) over the 0109 store, plus the TTL reap loop — OUT of
// main() per the TG-501 ratchet. Behaviour keys purely on the store: no env knob, compiled TTL/limits
// (db.DefaultConversationTTL), because the memory is derived, purgeable content whose only tuning that
// matters is "bounded" (INV-14).

import (
	"context"
	"log"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/temporal/runner"
)

// wirePriorSessionMemory wires the runner's two durable PRIOR-SESSION reads — the pair that let a
// session see what earlier sessions on the same subject already established:
//
//   - TG-80 P2-6: PriorJailbreaks, the repeat-offender count behind the hostile disposition (how many
//     times THIS host's output already tripped the screen).
//   - TG-80 P2-8: the conversation memory — the lineage's terminal digests in (seed) and this
//     session's digest out (terminal recorder), plus the TTL reap.
//
// Both are pgx reads over records the triage plane already owns; neither reaches an effect leaf.
func wirePriorSessionMemory(deps *runner.Deps, pool *db.Pool) {
	deps.PriorJailbreaks = db.NewSessionReadStore(pool).PriorJailbreaks
	store := db.NewConversationStore(pool)
	deps.ConversationTurns = func(ctx context.Context, key, excludeRef string, limit int) ([]runner.ConversationTurn, error) {
		rows, err := store.Recent(ctx, key, excludeRef, limit)
		if err != nil {
			return nil, err
		}
		out := make([]runner.ConversationTurn, 0, len(rows))
		for _, r := range rows {
			out = append(out, runner.ConversationTurn{ExternalRef: r.ExternalRef, Content: r.Content, CreatedAt: r.CreatedAt})
		}
		return out, nil
	}
	deps.ConversationAppend = func(ctx context.Context, key, ref, content string) error {
		return store.Append(ctx, key, ref, content, db.DefaultConversationTTL)
	}
	// The TTL reap loop — the evidence-reap shape (bounded context per tick, non-blocking error, work
	// then wait). Deletion runs only through the 0109 SECURITY DEFINER function: bounded batches of
	// EXPIRED rows, no parameter that can name one.
	go func() {
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for {
			sctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			n, err := store.Reap(sctx, db.DefaultConversationReapBatch)
			cancel()
			if err != nil {
				log.Printf("conversation memory: reap failed (non-blocking) — the hot tier is unbounded until this succeeds: %v", err)
			} else if n > 0 {
				log.Printf("conversation memory: reaped %d expired turn(s)", n)
			}
			<-t.C
		}
	}()
	log.Printf("conversation memory: armed — lineage digests fold into deep seeds as <conversation_memory> (TTL %s, hot tier %d turns)", db.DefaultConversationTTL, 5)
}
