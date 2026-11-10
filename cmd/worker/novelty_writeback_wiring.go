package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

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
	maxAge time.Duration,
	cautionsFor func(host, rule string) []knowledge.Incident,
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
			now := time.Now().UTC()
			// THE SAME HORIZON AS THE FEED (TG-673, deep-review F14): the live writeback used to apply no age at all,
			// so a resolution dated outside the retention horizon (a late close-out, a replayed history) was
			// learned and then immediately eligible for pruning by the decay pass — the two paths tugging at the
			// corpus. Stale is refused here on the single predicate both paths share.
			if lessons.Stale(ri, now, maxAge) {
				log.Printf("novelty writeback: %s (host=%s rule=%s) resolved %s ago, past the %s horizon — not learned (TG-673)", ri.ExternalRef, ri.Host, ri.AlertRule, now.Sub(ri.ResolvedAt).Round(time.Hour), maxAge)
				return nil
			}
			// CONTRADICTION (TG-673): a recent CAUTION for the same (host, rule) with the same action says this
			// remedy did NOT resolve cleanly before. One lucky run must not overwrite that memory as "what works
			// here" — the success is logged, not learned, until the caution ages out of the horizon.
			if cautionsFor != nil {
				if c, ok := lessons.Contradicted(ri, cautionsFor(ri.Host, ri.AlertRule), now, maxAge); ok {
					log.Printf("novelty writeback: %s (host=%s rule=%s action=%q) CONTRADICTS caution %s (%s) — not learned as precedent (TG-673)", ri.ExternalRef, ri.Host, ri.AlertRule, ri.Action, c.CautionRef, c.CautionAt.Format(time.RFC3339))
					return nil
				}
			}
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
