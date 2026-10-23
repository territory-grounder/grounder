package main

import (
	"context"
	"log"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/falsify"
	"github.com/territory-grounder/grounder/core/learn"
)

// wireDecayReconciliation arms the shared recency/decay reconciliation pass, carved out of main()'s
// composition root (TG-501 LOC-debt paydown, design-wisdom #11): ages the three learned stores — lessons
// provenance-prune, core/learn co-occurrence half-life, and estate decay-on-disproof — so recent evidence
// dominates and reality-contradicted state fades. Competence-plane only; see the comments below. OFF by
// default (TG_DECAY_INTERVAL unset). Behaviour is unchanged by the move.
func wireDecayReconciliation(
	dbPool *db.Pool,
	reconcileLessons func(),
	learner *learn.CoOccurrenceLearner,
	discoveryCorpus *falsify.MemDiscoveryCorpus,
	estateHolder *estate.Holder,
	publishEstate func(*estate.Graph),
) {
	// The shared RECENCY/DECAY reconciliation (design-wisdom #11, Gulli ch14 — periodic reconciliation): ONE
	// periodic pass ages the THREE learned stores so recent evidence dominates and reality-contradicted state
	// fades — (1) lessons prune by provenance age, (2) core/learn co-occurrence counts decay on a half-life,
	// (3) estate learned edges a fresh verify DISPROOF contradicts lose confidence / age out. It is
	// COMPETENCE-plane only: it ages LEARNED state and NEVER touches the estate itself, actuates, or gates —
	// mutation stays OFF. OFF by default (TG_DECAY_INTERVAL unset); every step is fail-safe — a panic is
	// recovered and logged, so a decay error can never crash the worker. All knobs are config-not-code.
	if iv := getenv("TG_DECAY_INTERVAL", ""); iv != "" {
		if d, derr := time.ParseDuration(iv); derr == nil && d > 0 {
			learnHalfLife := envDuration("TG_LEARN_HALFLIFE", 30*24*time.Hour)
			edgeDecayFactor := envFloat("TG_ESTATE_DECAY_FACTOR", estate.DefaultDecayFactor)
			// TG-206a: durably record each decayed edge's disproof (attributed to the misprediction that
			// disproved it) so a contradiction survives a restart and the learned-tier lifecycle (TG-388) has a
			// disproof history to consult. pgx when a pool exists; nil (skip) otherwise — the decay itself is
			// unaffected either way (best-effort, competence-plane; the graph swap is authoritative).
			var edgeDisproofStore estate.EdgeDisproofStore
			if dbPool != nil {
				edgeDisproofStore = db.NewEdgeDisproofs(dbPool)
			}
			runDecay := func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("decay: reconciliation pass panicked: %v (recovered — worker unaffected; never actuates)", r)
					}
				}()
				now := time.Now()
				// 1) lessons: prune precedents older than the retention horizon (no-op unless configured).
				reconcileLessons()
				// 2) core/learn: half-life the co-occurrence counts so old evidence fades toward zero.
				if st := learner.Decay(now, learnHalfLife); st.Pairs > 0 || st.Pruned > 0 {
					log.Printf("decay: learn half-life (%s) decayed %d co-occurrence pair(s), pruned %d faded", learnHalfLife, st.Pairs, st.Pruned)
				}
				// 3) estate: decay-on-disproof over the LEARNED tier. The disproof hosts are the surprise-hosts +
				// rule-mismatch hosts off the typed core/verify.VerdictDetail the falsify Scorer captured
				// (discoveryCorpus). Applied to a CLONE then atomically swapped, so a concurrent prediction read
				// never sees a half-mutated graph; only Source==incident edges decay (ground truth is untouched).
				// PATHS, not a flat host set (TG-206): only edges connecting a target to ITS OWN surprise hosts
				// decay. disproofHosts is still computed for the log line's denominator, so the operator can
				// see how many hosts were implicated beside how many edges actually aged.
				snap := discoveryCorpus.Snapshot()
				if paths := disproofPaths(snap); len(paths) > 0 {
					// TG-388: decay the SOURCE first — the learner's own co-occurrence counts, which the 5-minute
					// estate refresh rebuilds its learned edges from. The graph decay below is TRANSIENT: the next
					// refresh recomputes confidence from these very counts and overwrites the decayed clone (measured
					// net-zero over 11 passes). Decaying the counts here is what actually PERSISTS a disproof, and the
					// learner's own countDecayFloor ages a faded pair OUT — the age-out the graph's Floor=0 never
					// reached. The graph decay is kept for its durable per-edge audit record (TG-206a).
					if st := learner.DecayOnDisproof(paths, edgeDecayFactor); st.Pairs > 0 || st.Pruned > 0 {
						log.Printf("decay: disproof decayed %d learned pair(s) at source, aged out %d — persists through the estate refresh (TG-388)", st.Pairs, st.Pruned)
					}
					hosts := disproofHosts(snap)
					if newG, rep := estateHolder.Graph().DecayOnDisproof(estate.Disproof{Paths: paths, At: now}, estate.DecayOptions{Factor: edgeDecayFactor}); rep.Decayed > 0 {
						estateHolder.Set(newG)
						publishEstate(newG)
						log.Printf("decay: estate decay-on-disproof decayed %d learned edge(s), aged out %d (from %d mispredicted path(s) over %d implicated host(s)) — competence-plane; never actuates", rep.Decayed, rep.AgedOut, len(paths), len(hosts))
						// TG-206a: attach the contradiction to the edge durably — the disproof record survives the
						// restart the in-memory DecayReport did not. Best-effort: a persist failure never undoes the decay.
						if edgeDisproofStore != nil && len(rep.Disproofs) > 0 {
							if n, err := edgeDisproofStore.Record(context.Background(), now, rep.Disproofs); err != nil {
								log.Printf("decay: edge-disproof persist failed (non-fatal, the in-memory decay stands): %v", err)
							} else {
								log.Printf("decay: persisted %d per-edge disproof record(s) — attributed to the mispredictions that disproved them (TG-206a)", n)
							}
						}
					}
				}
			}
			runDecay() // one reconciliation pass at boot
			go func() {
				t := time.NewTicker(d)
				defer t.Stop()
				for range t.C {
					runDecay()
				}
			}()
			log.Printf("decay: shared recency/decay reconciliation every %s (lessons provenance-prune, learn half-life %s, estate decay-on-disproof factor %.2f) — competence-plane; never actuates", d, learnHalfLife, edgeDecayFactor)
		} else {
			log.Printf("decay: invalid TG_DECAY_INTERVAL %q — reconciliation disabled", iv)
		}
	}
}
