package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"

	"github.com/territory-grounder/grounder/core/knowledge"
	"github.com/territory-grounder/grounder/core/lessons"
	"github.com/territory-grounder/grounder/temporal/runner"
)

// wireNoveltyWriteback arms the novelty WRITEBACK feeder (TG-124), carved out of main()'s composition root
// (TG-501 LOC-debt paydown): the LIVE close-out counterpart to the operator-export lessons feed. See the
// comment below for the full rationale. Requires a durable corpus (TG_KNOWLEDGE_FILE); a nil knowledgeHolder
// or blank corpusPath leaves the seam nil (fail-safe — the writeback is simply skipped). Behaviour is
// unchanged by the move.
func wireNoveltyWriteback(
	deps *runner.Deps,
	knowledgeHolder *knowledge.Holder,
	corpusPath string,
	lessonsMu *sync.Mutex,
	persistCorpus func(existing, merged []knowledge.Incident) error,
	loadCorpus func() *knowledge.LexicalRetriever,
	syncEmbed func(),
) {
	// The novelty WRITEBACK feeder (TG-124): the LIVE close-out counterpart to the operator-export lessons
	// feed (appendLessons, above). When the terminal reconcile confirms a CLEAN resolution, ReconcileActivity
	// emits the resolved incident through this seam; it is distilled (lessons.Merge → the SAME confirmed-clean
	// gate the export/decay paths use) and merged into the durable corpus the retriever reloads — so a
	// graduated op-class's next same-shape incident is no longer flagged NOVEL (it now has a precedent row
	// knowledge.Count sees, keyed on the EXACT (host, rule) the classifier read). The read-merge-write-reload is
	// serialized with the export/decay paths via lessonsMu so the three never race the corpus file. Requires a
	// durable corpus (TG_KNOWLEDGE_FILE) to persist into; without one there is nowhere to record a precedent, so
	// the seam stays nil (fail-safe — the writeback is simply skipped, novelty behaves exactly as before). It
	// writes ONLY the knowledge corpus — never the estate, never gated by the mutation chokepoint.
	if knowledgeHolder != nil && corpusPath != "" {
		deps.LearnResolved = func(_ context.Context, ri lessons.ResolvedIncident) error {
			lessonsMu.Lock()
			defer lessonsMu.Unlock()
			var existing []knowledge.Incident
			if cf, err := os.Open(corpusPath); err == nil {
				existing, _ = knowledge.ParseCorpus(cf)
				cf.Close()
			}
			// Distill through the SAME confirmed-clean gate the operator export uses; a non-clean or already-known
			// record contributes 0 and leaves the corpus (and its file) untouched — an idempotent no-op.
			merged, added := lessons.Merge(existing, []lessons.ResolvedIncident{ri})
			if added == 0 {
				// Writeback DIAGNOSTICS (TG-124): the reconcile gate already passed (Verdict=match, ConfirmedClear),
				// yet Merge added nothing — either lessons.Lesson rejected the record (blank external_ref/action) or
				// the (host, rule) is ALREADY a precedent. Both are otherwise-silent no-ops that read identically to a
				// gate failure from outside, so name the drop reason to make the observed writeback miss diagnosable.
				already := false
				for _, e := range existing {
					if e.Host == ri.Host && e.AlertRule == ri.AlertRule {
						already = true
						break
					}
				}
				log.Printf("novelty writeback: distilled-but-DROPPED %s (host=%s rule=%s action=%q): already_known=%v (0 added — no precedent recorded)", ri.ExternalRef, ri.Host, ri.AlertRule, ri.Action, already)
				return nil
			}
			// WRITE-SCREEN VISIBILITY (TG-296). The row about to be persisted has been content-screened on the
			// way in: lessons.Lesson neutralizes prompt-injection spans and redacts credentials in the alert
			// narrative, then FLAGS the row rather than rejecting it (rejecting would let whoever wrote the alert
			// body choose which (host, rule) TG is never allowed to de-novel). The flag rides the corpus row
			// durably, but a flag nobody reads is not a control — so name it here, at the moment of the write,
			// where an operator watching the worker sees that a hostile or credential-bearing alert body reached
			// the learning loop and what was stripped from it.
			for _, inc := range merged {
				if inc.ExternalRef != ri.ExternalRef {
					continue
				}
				if flags := lessons.ScreenedTags(inc); len(flags) > 0 {
					log.Printf("novelty writeback: precedent %s (host=%s rule=%s) was CONTENT-SCREENED on write %v — the hostile/credential span is neutralized in the STORED row; the lesson itself is kept, so the (host,rule) still de-novels", ri.ExternalRef, ri.Host, ri.AlertRule, flags)
				}
				break
			}
			// Atomic write + tamper-evidence witness, through the single maintained-corpus chokepoint.
			if werr := persistCorpus(existing, merged); werr != nil {
				return fmt.Errorf("novelty writeback: %w", werr)
			}
			knowledgeHolder.Set(loadCorpus()) // reload the seed∪maintained union after the write — never the maintained-only set (the seed must stay visible to the novelty gate)
			syncEmbed()                       // the new precedent becomes semantically retrievable too (best-effort, never blocking)
			log.Printf("novelty writeback: distilled resolved incident %s into %s (host=%s rule=%s)", ri.ExternalRef, corpusPath, ri.Host, ri.AlertRule)
			return nil
		}
		log.Printf("novelty writeback: live close-out feeder armed (corpus %s) — a confirmed-clean resolution de-novels its (host, rule)", corpusPath)
	}
}
