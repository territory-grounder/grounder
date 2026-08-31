package main

import (
	"context"
	"log"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/temporal/opclasscluster"
)

// wireOpclassClustering arms the earned-catalog clustering pass (spec/028 REQ-2811/REQ-2812, epic TG-227
// Stage 2), carved out of main()'s composition root (TG-501 LOC-debt paydown): recurring free-form
// proposals accrue into op-class CANDIDATES an operator later ratifies from an evidence dossier —
// OBSERVE-ONLY, the pass can advance a candidate to ratify_ready and no further. A no-op without a
// database pool. Behaviour is unchanged by the move.
func wireOpclassClustering(dbPool *db.Pool, ledger *audit.Ledger, estateHolder *estate.Holder) {
	if dbPool != nil {
		if d, derr := time.ParseDuration(getenv("TG_OPCLASS_CLUSTER_INTERVAL", "1h")); derr == nil && d > 0 {
			cands := db.NewOpClassCandidateStore(dbPool)
			clusterJob := opclasscluster.Job{
				Store:  cands,
				Ledger: ledger,
				// The DEAD-MAN's inputs come from tables this cron does NOT write (the occurrence journal
				// and session_triage, both written by the runner). A liveness check derived from a
				// component's own output can only ever report "I am fine" — which is exactly how the LLM
				// judge stayed dead for days behind a green process.
				Liveness: func(ctx context.Context, window time.Duration) (opclasscluster.Liveness, error) {
					newest, sessions, err := cands.Liveness(ctx, window)
					if err != nil {
						return opclasscluster.Liveness{}, err
					}
					return opclasscluster.Liveness{NewestOccurrence: newest, SessionsSince: sessions}, nil
				},
				Ready: opclasscluster.NewReadyResolver(estateHolder.Graph, nil),
			}
			go opclasscluster.RunPeriodically(context.Background(), clusterJob, d, func(cerr error) {
				log.Printf("opclass clustering: pass refused: %v (retry next tick)", cerr)
			})
			log.Printf("opclass clustering: candidacy pass every %s — observe-only, ratification stays an operator act", d)
		} else if derr != nil {
			log.Printf("opclass clustering: invalid TG_OPCLASS_CLUSTER_INTERVAL — clustering disabled")
		}
	}
}
