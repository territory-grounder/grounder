package main

// The read-only AWX PLAYBOOKS-as-knowledge ingest lane (spec/017 T-017-5 follow-on), carved out of main()'s
// composition root (TG-501 LOC-debt paydown). armAWXPlaybooksIngest is OFF unless TG_AWXPLAYBOOKS_* is fully
// configured; when armed it ingests AWX runbooks (re-read by id) into a FileCorpus and, when a semantic index is
// configured, folds them into the vector index over the UNION of the live corpus + the runbooks so a partial sync
// never prunes a lesson. It launches NOTHING and never crashes the worker. Behaviour is unchanged by the move;
// its probeReg.offer("knowledge", ...) seam is counted by the package-wide wiring-inventory net.

import (
	_ "time/tzdata" // embed the IANA zoneinfo DB so time.LoadLocation resolves on distroless (no OS tzdata)

	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/knowledge"
	"github.com/territory-grounder/grounder/modules/knowledge/awxplaybooks"
)

// armAWXPlaybooksIngest arms the read-only playbooks-as-knowledge cron (spec/017 T-017-5 follow-on). Disabled
// unless TG_AWXPLAYBOOKS_* is fully configured. It ingests AWX runbooks (re-read by id) into a FileCorpus and,
// when a semantic index is configured, folds them into the vector index over the UNION of the live corpus +
// the runbooks so a partial sync never prunes a lesson. It launches NOTHING and never crashes the worker.
func armAWXPlaybooksIngest(dbPool *db.Pool, holder *knowledge.Holder, probeReg *probeRegistry, corpusLock sync.Locker) {
	base := strings.TrimSpace(getenv("TG_AWXPLAYBOOKS_BASE_URL", ""))
	tokenRef := strings.TrimSpace(getenv("TG_AWXPLAYBOOKS_SENSOR_TOKEN_REF", ""))
	corpusPath := strings.TrimSpace(getenv("TG_AWXPLAYBOOKS_CORPUS", ""))
	interval := envDuration("TG_AWXPLAYBOOKS_INTERVAL", 0) // OFF by default — opt-in
	if base == "" || tokenRef == "" || corpusPath == "" || interval <= 0 {
		log.Printf("awxplaybooks knowledge lane: disabled (needs TG_AWXPLAYBOOKS_BASE_URL + TG_AWXPLAYBOOKS_SENSOR_TOKEN_REF + TG_AWXPLAYBOOKS_CORPUS + TG_AWXPLAYBOOKS_INTERVAL>0) — read-only, launches nothing")
		return
	}
	client, err := awxplaybooks.NewClient(awxplaybooks.ClientConfig{
		BaseURL:    base,
		TokenRef:   config.SecretRef(tokenRef),
		CACertPath: getenv("TG_AWXPLAYBOOKS_CA", ""),
	})
	if err != nil {
		log.Printf("awxplaybooks knowledge lane: disabled — client build failed: %v (read-only; never fatal)", err)
		return
	}
	// The client is offered, not the Ingest: Ingest.Run WRITES the corpus file, so it can never be a probe.
	probeReg.offer("knowledge", awxplaybooks.SourceType, client)
	corpus := awxplaybooks.FileCorpus{Path: corpusPath}
	// Whether this lane writes the SAME file as the maintained precedent corpus (TG_KNOWLEDGE_FILE — the
	// configuration this lane's own reachability note below recommends). Two consequences when it does: TG-510
	// (route through the tamper-evidence witness when armed) and TG-520 (serialize with the lessons lanes).
	// When the files differ, this lane writes its own separate corpus and needs neither.
	sameFile := sameCorpusFile(corpusPath, strings.TrimSpace(getenv("TG_KNOWLEDGE_FILE", "")))
	// TG-510: route the read-merge-write through the SAME record-on-write witness as the other corpus lanes
	// when tamper-evidence is armed — otherwise its writes would BYPASS the chokepoint (an unwitnessed write
	// the verify false-alarms on, and an unrouted laundering path).
	var store awxplaybooks.CorpusStore = corpus
	if sameFile && truthyEnv("TG_CORPUS_APPEND_ONLY") && dbPool != nil {
		if ev := newCorpusWitness(dbPool, envInt("TG_CORPUS_ANCHOR_WINDOW", audit.DefaultAnchorWindow)); ev != nil {
			store = &witnessedCorpusStore{inner: corpus, ev: ev}
			log.Printf("awxplaybooks knowledge lane: writes to %s route through TG-510 corpus tamper-evidence (same file as TG_KNOWLEDGE_FILE) — record-on-write + write-time verify (not a chokepoint bypass)", corpusPath)
		}
	}
	ingest, err := awxplaybooks.NewIngest(client, store)
	if err != nil {
		log.Printf("awxplaybooks knowledge lane: disabled — ingest build failed: %v", err)
		return
	}
	ingest.Logf = log.Printf
	// TG-520: when this lane writes the SAME file as the lessons lanes, serialize its read-merge-write with
	// them under the shared lock — FLAG-INDEPENDENT (the last-writer-wins data-loss race exists whenever both
	// lanes write one file, regardless of tamper-evidence). Ingest.Run locks only the fast post-fetch mutation,
	// never the AWX API fetch, so the lessons lanes are never blocked for an AWX round-trip.
	if sameFile && corpusLock != nil {
		ingest.CorpusLock = corpusLock
		log.Printf("awxplaybooks knowledge lane: writes to %s serialize with the lessons lanes under the shared corpus lock (TG-520 — no concurrent last-writer-wins on the shared file)", corpusPath)
	}
	var index knowledge.IndexSync
	if dbPool != nil && strings.TrimSpace(getenv("TG_EMBED_MODEL", "")) != "" {
		index = db.NewKnowledgeEmbeddingStore(dbPool)
	}
	runOnce := func() {
		rctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		res, rerr := ingest.Run(rctx)
		if rerr != nil {
			log.Printf("awxplaybooks knowledge lane: ingest failed: %v (prior corpus intact; retried next tick)", rerr)
			return
		}
		// Make the ingested runbooks semantically retrievable WITHOUT pruning the lessons index: sync over the
		// UNION of the live retriever corpus + the freshly-ingested runbooks. SyncIndex prunes refs absent from
		// the corpus it is handed, so a subset would drop lessons — the union never does.
		if index != nil && (res.Added > 0 || res.Updated > 0) {
			runbooks, lerr := corpus.Load(rctx)
			if lerr != nil {
				log.Printf("awxplaybooks knowledge lane: reload corpus for index sync failed: %v", lerr)
				return
			}
			combined := runbooks
			if holder != nil {
				combined = knowledge.MergeCorpus(holder.Snapshot(), runbooks)
			}
			if up, pruned, serr := knowledge.SyncIndex(rctx, index, combined); serr != nil {
				log.Printf("awxplaybooks knowledge lane: index sync failed: %v (runbooks still lexically discoverable once corpus reloads)", serr)
			} else {
				// TELL THE TRUTH ABOUT REACHABILITY. This line used to claim "runbooks now RAG-retrievable",
				// which is FALSE unless the runbooks are also in the retriever's own corpus:
				// FusedRetriever drops every semantic match whose ref is absent from Base (semantic.go — the
				// join that stops a stale index row resurrecting removed precedent). Indexing a ref the
				// retriever will always reject makes it findable by nothing.
				//
				// So the claim is now CHECKED rather than asserted, against the same Base the retriever
				// consults — and a lane that is ingesting into the void says so on every tick instead of
				// reporting success.
				reachable := 0
				if holder != nil {
					for _, rb := range runbooks {
						if _, ok := holder.ByRef(rb.ExternalRef); ok {
							reachable++
						}
					}
				}
				switch {
				case len(runbooks) == 0:
					log.Printf("awxplaybooks knowledge lane: index synced (%d upserted, %d pruned) — no runbooks ingested", up, pruned)
				case reachable == 0:
					log.Printf("awxplaybooks knowledge lane: index synced (%d upserted, %d pruned) BUT NONE OF THE %d RUNBOOKS ARE RETRIEVABLE — "+
						"they are absent from the retriever corpus (TG_KNOWLEDGE_FILE), and FusedRetriever drops any semantic match it cannot resolve there. "+
						"Point TG_AWXPLAYBOOKS_CORPUS at the retriever corpus, or treat this lane as index-only.", up, pruned, len(runbooks))
				case reachable < len(runbooks):
					log.Printf("awxplaybooks knowledge lane: index synced (%d upserted, %d pruned) — only %d of %d runbooks are retrievable (the rest are absent from the retriever corpus)", up, pruned, reachable, len(runbooks))
				default:
					log.Printf("awxplaybooks knowledge lane: index synced (%d upserted, %d pruned) — %d runbooks RAG-retrievable", up, pruned, reachable)
				}
			}
		}
	}
	runOnce() // ingest immediately at boot
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			runOnce()
		}
	}()
	log.Printf("awxplaybooks knowledge lane: ARMED — read-only AWX runbook ingest every %s into %s (re-read by id, launches nothing; discovery grants no authority)", interval, corpusPath)
}

// witnessedCorpusStore decorates a CorpusStore with TG-510 record-on-write tamper-evidence, so the AWX lane
// witnesses the maintained precedent corpus exactly like the four lanes in main() when it writes the same
// file. It remembers what Load returned and, on the next Save, (1) runs the WRITE-TIME verify over that
// remembered `existing` — the read-merge-write laundering check — then (2) records a witness of what was
// written. Ingest.Run calls Load then Save in sequence on ONE store, single-threaded, so the remembered value
// is the `existing` that was merged. Evidence-only: detectOnWrite/record only ever WARN; the Save proceeds.
type witnessedCorpusStore struct {
	inner awxplaybooks.CorpusStore
	ev    *corpusWitness
	last  []knowledge.Incident
}

func (w *witnessedCorpusStore) Load(ctx context.Context) ([]knowledge.Incident, error) {
	got, err := w.inner.Load(ctx)
	if err == nil {
		w.last = got
	}
	return got, err
}

func (w *witnessedCorpusStore) Save(ctx context.Context, merged []knowledge.Incident) error {
	w.ev.detectOnWrite(w.last) // write-time laundering check over the `existing` this Save is replacing
	if err := w.inner.Save(ctx, merged); err != nil {
		return err
	}
	w.ev.record(merged)
	return nil
}
