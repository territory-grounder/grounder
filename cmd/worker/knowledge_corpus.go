package main

import (
	"os"

	"github.com/territory-grounder/grounder/core/knowledge"
)

// The knowledge corpus is a UNION of two files (deploy-persistence fix, TG-124 follow-on):
//
//   - the SEED (TG_KNOWLEDGE_SEED_FILE) — the tracked, deploy-synced bootstrap precedents
//     (deploy/knowledge/corpus.seed.json). The AWX deploy's `copy` from a fresh clone OVERWRITES it
//     every deploy; it is effectively read-only at runtime.
//   - the MAINTAINED corpus (TG_KNOWLEDGE_FILE) — the untracked, deploy-persistent file the worker
//     WRITES (novelty writeback, lessons merge, decay prune). The deploy's copy never touches
//     untracked destination files, so runtime learning SURVIVES a deploy.
//
// Before the split both roles shared the seed path, so every deploy silently wiped every runtime
// de-novel (observed live 2026-07-23: 9 writebacks from a morning fault sweep vanished at the !538
// deploy; the corpus regressed to the tracked seed). The retrieval plane reads the UNION so a fresh
// box (maintained not yet written) is still armed from the seed — the novelty gate never fails open
// on first boot.

// parseCorpusFile reads one corpus file: (nil, false, nil) when the path is empty or the file does
// not exist yet — a fresh maintained corpus before the first writeback is the normal state, not an
// error. found=true when the file was opened (even if it parses to zero rows).
func parseCorpusFile(path string) (rows []knowledge.Incident, found bool, err error) {
	if path == "" {
		return nil, false, nil
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	rows, perr := knowledge.ParseCorpus(f)
	if perr != nil {
		return nil, true, perr
	}
	return rows, true, nil
}

// loadKnowledgeCorpus parses the seed ∪ maintained union into a retriever. Returns nil (the caller
// keeps its prior corpus / the no-corpus semantics) when:
//   - the MAINTAINED file exists but is unreadable/unparseable — it is the write target, so a torn
//     write must never downgrade the retriever (today's keep-prior behavior), or
//   - NOTHING was found and no seed is configured — the wholly-absent corpus (the novelty-gate
//     DISABLED warning fires upstream), never masked behind an empty-but-armed retriever.
//
// A SEED failure degrades to maintained-only with a loud log: the seed is read-only bootstrap, so
// its loss must not nuke accumulated runtime precedent. The maintained file simply not existing yet
// is NOT an error (fresh box ⇒ seed-only union ⇒ the gate stays armed).
func loadKnowledgeCorpus(seedPath, corpusPath string, logf func(string, ...any)) *knowledge.LexicalRetriever {
	maintained, foundM, merr := parseCorpusFile(corpusPath)
	if merr != nil {
		// "kept prior" is true for a RELOAD and false at boot, where there is no prior — the same shape
		// as the estate refresh line that printed reassurance during the outage it was describing. Say
		// what is actually known: the load failed and the caller decides what standing that leaves.
		logf("knowledge: corpus file %s unreadable: %v (no retriever from this load — a reload keeps the prior corpus, a BOOT has none)", corpusPath, merr)
		return nil
	}
	seed, _, serr := parseCorpusFile(seedPath)
	if serr != nil {
		logf("knowledge: seed corpus %s unreadable: %v (degrading to maintained-only this load — runtime precedent intact, bootstrap missing)", seedPath, serr)
	}
	if !foundM && seedPath == "" {
		logf("knowledge: corpus file %s absent and no seed configured (TG_KNOWLEDGE_SEED_FILE unset) — no retriever", corpusPath)
		return nil
	}
	corpus := knowledge.MergeCorpus(seed, maintained)
	logf("knowledge: corpus loaded — %d prior incidents (seed %d + maintained %d)", len(corpus), len(seed), len(maintained))
	// THE RECENCY CHANNEL IS DECLARED, NOT ASSUMED (TG-240).
	//
	// Incident.ResolvedAt, recencyScore (linear decay over 90 days, weight 0.25) and stalenessNote all
	// exist and all run. None of them can act on a row whose ResolvedAt is the zero value — and every one
	// of the shipped seed rows is such a row, because corpus.seed.json has no resolved_at field at all.
	// The effect is precedent that cannot be RETIRED: a fix that stopped holding six months ago ranks
	// identically to one confirmed last week.
	//
	// The dates are genuinely unknown, so this does not invent them — fabricating a resolved_at would make
	// the ranking worse in a way nobody could see. It states the size of the blind spot instead, which is
	// the difference between a channel that is inert and a channel that is inert AND silent. Same reason
	// the wiring yield register exists: a lane producing nothing must say so rather than look healthy.
	if undated := countUndated(corpus); undated > 0 {
		logf("knowledge: recency channel INERT for %d of %d precedent(s) — they carry no resolved_at, so "+
			"decay cannot retire them and a six-month-old fix ranks with one confirmed last week "+
			"(the shipped seed has no resolved_at field; runtime-written precedent does)", undated, len(corpus))
	}
	// TG-50: arm the lexical min-relevance floor. Unset/≤0 ⇒ 0 ⇒ the shipped score>0 behaviour (byte-identical).
	return knowledge.NewLexicalRetriever(corpus).SetMinScore(envFloat("TG_RETRIEVE_MIN_SCORE", 0)).SetIDFTags(truthyEnv("TG_RETRIEVAL_IDF_TAGS"))
}

// newKnowledgeHolder wraps the loaded corpus in a Holder, or returns NIL when there is no corpus.
//
// WHY THIS FUNCTION EXISTS, and it is not a refactor. The composition root called
// knowledge.NewHolder(loadCorpus()) directly, and NewHolder replaces a nil retriever with an EMPTY one
// so Retrieve never dereferences nil. That is correct for the Holder — and catastrophic here, because it
// launders "the corpus could not be read" into "the corpus is empty", the exact distinction
// loadKnowledgeCorpus's own doc says must "never [be] masked behind an empty-but-armed retriever".
//
// What the laundering produced. With TG_KNOWLEDGE_FILE set and the corpus file torn or unparseable,
// loadKnowledgeCorpus returns nil, NewHolder turns that into an armed empty retriever, and the composition
// root's `if knowledgeHolder != nil` then WIRES deps.PriorIncidents. It answers (0, true) — count zero,
// positively KNOWN — for every (host, alert_rule) in the estate. runner.novelIncident reads known=true and
// n==0 as "this key has no precedent", so EVERY incident is novel and every incident forces the
// first-sight-human poll, fleet-wide.
//
// Both intended behaviours were unreachable at once: the documented fail-safe ("no corpus ⇒ PriorIncidents
// stays nil ⇒ novelty is UNKNOWN and the gate does NOT fire — no false positives") and the WARNING written
// to announce it ("a forgotten TG_KNOWLEDGE_FILE silently removes the one control meant to force a human
// onto a never-seen (host,rule)"). The `case knowledgeHolder == nil` arm of the lessons switch was
// likewise dead.
//
// Returning nil restores both: the caller's else arm fires, the operator is warned, and novelty reports
// UNKNOWN instead of confidently reporting zero.
func newKnowledgeHolder(seedPath, corpusPath string, logf func(string, ...any)) *knowledge.Holder {
	r := loadKnowledgeCorpus(seedPath, corpusPath, logf)
	if r == nil {
		// NOT NewHolder(nil). "Unreadable" and "empty" are different facts and only one of them is safe to
		// act on.
		return nil
	}
	return knowledge.NewHolder(r)
}

// countUndated reports how many precedents carry no resolution date, i.e. how much of the corpus the
// recency channel cannot act on. Separate from the log line so the number is testable without a logger.
func countUndated(corpus []knowledge.Incident) int {
	var n int
	for _, inc := range corpus {
		if inc.ResolvedAt.IsZero() {
			n++
		}
	}
	return n
}

// newCautionHolder wraps the TG-52 caution lane — the failed/deviated/unconfirmed trajectories
// lessons.CautionMerge distills — in its OWN Holder, a store SEPARATE from the precedent corpus, or nil when
// TG_CAUTION_FILE is unset. Unlike the precedent corpus there is NO shipped seed and NO seed∪maintained
// union: a caution is only ever runtime-written, so an absent file is the normal first-boot state (an
// empty-but-armed holder), not an error. A present-but-unreadable file degrades to nil — the caution lane is
// optional enrichment, so a torn read disables the lane rather than blocking triage, the same fail-quiet
// posture precedent() takes on a poisoned snippet.
func newCautionHolder(cautionPath string, logf func(string, ...any)) *knowledge.Holder {
	if cautionPath == "" {
		return nil
	}
	rows, found, err := parseCorpusFile(cautionPath)
	if err != nil {
		logf("caution: corpus %s unreadable: %v — caution lane retrieval disabled this load", cautionPath, err)
		return nil
	}
	if !found {
		logf("caution: no caution corpus at %s yet (fresh) — lane armed, empty until the first failed trajectory is distilled", cautionPath)
	}
	// TG-50: arm the lexical min-relevance floor on the initial holder too (loadKnowledgeCorpus arms reloads).
	return knowledge.NewHolder(knowledge.NewLexicalRetriever(rows).SetMinScore(envFloat("TG_RETRIEVE_MIN_SCORE", 0)).SetIDFTags(truthyEnv("TG_RETRIEVAL_IDF_TAGS")))
}
