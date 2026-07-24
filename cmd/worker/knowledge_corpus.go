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
		logf("knowledge: corpus file %s unreadable: %v (kept prior)", corpusPath, merr)
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
	return knowledge.NewLexicalRetriever(corpus)
}
