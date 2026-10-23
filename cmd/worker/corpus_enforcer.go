package main

// TG-519 Slice C — the worker-side tamper-ENFORCEMENT for the maintained precedent corpus (TG_KNOWLEDGE_FILE),
// the owner-armed escalation of TG-510's evidence-only witness. Behind TG_CORPUS_ENFORCE (off by default);
// OFF ⇒ corpusEnforcer is nil and the corpus load path is byte-identical to TG-510 (evidence-only warn).
//
// This is the I/O half: it reads the on-disk maintained corpus and its witness history and hands both to the
// pure knowledge.EnforceCorpusAdmission decision. When that says DROP, the composition root loads the seed
// ∪ maintained union with the maintained path elided, so the union composes from the SEED alone — tampered or
// unverifiable precedent never reaches trusted retrieval.
//
// FAIL DIRECTION (the inverse of TG-510). Evidence fails safe-WARN (a false verify is a false alarm). This
// fails safe-DROP: a false verify DROPS the maintained corpus (degraded retrieval, annoying but safe), because
// the danger being defended is a MISSED tamper reaching the agent's trusted context. Every "cannot verify"
// state (no witness store, unreadable witnesses, no witness yet, an unparseable corpus) drops, not admits.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/knowledge"
)

// anchorReader is the witness-history read surface the enforcer needs — the SELECT half of the append-only
// ledger_anchor store, scoped to the knowledge-corpus domain. *db.ScopedAnchorStore satisfies it; a test
// supplies a fake so the gate is exercised end-to-end without a DB.
type anchorReader interface {
	Anchors(ctx context.Context) ([]audit.Anchor, error)
}

// errNoWitnessStore is the unverifiable state when enforcement is armed but there is no durable store to read
// witnesses from (TG_DB_DSN unset). Fail-CLOSED: with nowhere to verify against, the maintained corpus is
// dropped, never admitted on trust.
var errNoWitnessStore = errors.New("no durable witness store (TG_DB_DSN unset while TG_CORPUS_ENFORCE armed)")

// corpusEnforcer gates whether the maintained corpus may compose into trusted retrieval. Its field is
// immutable after construction, so a single instance is safe to consult from every load/reload.
type corpusEnforcer struct {
	// sink reads the knowledge-corpus witness history. nil ⇒ no witness store ⇒ every load is unverifiable ⇒
	// fail-CLOSED drop (the armed-without-a-DSN posture).
	sink anchorReader
}

// gate decides whether the maintained corpus at corpusPath may compose into trusted retrieval: nil to ADMIT,
// non-nil to DROP (the caller composes seed-only). Fail-CLOSED throughout.
//
//   - maintained file ABSENT (fresh box) or parsed EMPTY  -> ADMIT: there is nothing to drop, and seed-only is
//     the result either way (MergeCorpus(seed, nil)); enforcement never drops the seed on a fresh box.
//   - maintained UNPARSEABLE                              -> DROP as ErrCorpusUnverifiable: an unreadable
//     corpus cannot prove itself. (Atomic temp+rename writes mean a reader never sees a torn write, so an
//     unparseable file is genuine corruption/tamper, not a transient — the same stance the periodic verify
//     takes.)
//   - maintained present with rows                        -> verify against the latest witness (fail-CLOSED on
//     any unverifiable/tampered state, per knowledge.EnforceCorpusAdmission).
func (e *corpusEnforcer) gate(corpusPath string) error {
	rows, found, perr := parseCorpusFile(corpusPath)
	if perr != nil {
		// An unparseable corpus cannot prove itself; wrap as ErrCorpusUnverifiable so callers errors.Is it the
		// same as any other unverifiable state. Fail-CLOSED: drop to seed-only.
		return fmt.Errorf("%w: maintained corpus %s does not parse: %v", knowledge.ErrCorpusUnverifiable, corpusPath, perr)
	}
	if !found || len(rows) == 0 {
		return nil // nothing to gate — seed-only either way; do not drop the seed on a fresh/empty box.
	}
	anchors, aerr := e.readAnchors()
	return knowledge.EnforceCorpusAdmission(rows, anchors, aerr)
}

// readAnchors materialises the witness history for the pure decision, bounded so a stalled substrate cannot
// hang a load. A nil sink is itself an unverifiable state (armed without a DSN).
func (e *corpusEnforcer) readAnchors() ([]audit.Anchor, error) {
	if e.sink == nil {
		return nil, errNoWitnessStore
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return e.sink.Anchors(ctx)
}

// enforcedCorpusPath returns the maintained-corpus path the union load should use, applying TG-519
// enforcement. It is the SINGLE decision point every corpus load routes through:
//
//   - enf == nil (TG_CORPUS_ENFORCE off) ⇒ returns corpusPath UNCHANGED — byte-identical to TG-510
//     evidence-only. The maintained corpus always composes.
//   - armed + gate ADMITS               ⇒ returns corpusPath unchanged (maintained composes).
//   - armed + gate DROPS                ⇒ returns "" and logs a loud refusal, so loadKnowledgeCorpus composes
//     the seed ∪ (elided) union = the SEED ALONE (or empty when no seed). Tampered/unverifiable precedent is
//     kept out of trusted retrieval.
//
// Elision (return "") deliberately reuses loadKnowledgeCorpus's existing "no maintained path" path rather than
// threading a new signature through it: seed-only is EXACTLY the shape a box with no maintained file already
// composes, so the fallback is a well-trodden state, not a new one.
func enforcedCorpusPath(enf *corpusEnforcer, corpusPath string, logf func(string, ...any)) string {
	if enf == nil {
		return corpusPath
	}
	if err := enf.gate(corpusPath); err != nil {
		logf("!!! KNOWLEDGE CORPUS ENFORCEMENT DROP: %v — DROPPING the maintained corpus %s from trusted retrieval and composing from the SEED alone (TG-519 Slice C, fail-CLOSED: tampered or unverifiable precedent must NOT reach the agent's trusted context). Runtime-learned precedent is unavailable until the corpus verifies clean against its witness again.", err, corpusPath)
		return ""
	}
	return corpusPath
}
