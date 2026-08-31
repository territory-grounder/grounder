package knowledge

// TG-519 Slice C — tamper-ENFORCEMENT for the maintained precedent corpus (the owner-armed escalation of
// TG-510's tamper-EVIDENCE).
//
// TG-510 (corpus_chain.go) DETECTS an out-of-band edit and lets the worker WARN; it never blocks, because its
// fail-safe direction is fail-safe-WARN — a false verify is a false ALARM at worst, and letting one more
// warning through is cheaper than silencing a real one. Enforcement asks a DIFFERENT question with the
// OPPOSITE fail direction:
//
//	TG-510 evidence:     "did this corpus change out of band?"          -> WARN (never blocks). fail-safe-WARN.
//	TG-519 enforcement:  "may this corpus compose into TRUSTED retrieval?" -> DROP on any doubt. fail-safe-DROP.
//
// WHY THE DIRECTION INVERTS. The maintained corpus is precedent the retriever surfaces into the next agent
// session's trusted context. The dangerous failure for a security control here is a MISSED tamper reaching
// trusted retrieval — a rewritten Resolution, a Source laundered up to "verified-resolution", an injected
// precedent row — because that steers a real actuation. A false DROP is the cheap failure: the maintained
// corpus is excluded and retrieval degrades to the SEED (bootstrap) corpus alone (or empty) — annoying
// (runtime-learned precedent is temporarily unavailable) but SAFE. So enforcement fails CLOSED: on tamper OR
// on ANY state where the corpus cannot prove itself, DROP it. This mirrors skillstore's chain (core/skillstore
// /chain.go): a corpus that cannot prove itself does not compose; composition falls back to the trusted
// baseline IN FULL.
//
// This file is the PURE decision. The worker's cmd/worker/corpus_enforcer.go reads the on-disk corpus and its
// witness history (I/O) and calls EnforceCorpusAdmission; the composition root (cmd/worker/main.go) drops the
// maintained corpus from the union load when this says so. Behind TG_CORPUS_ENFORCE (off by default); OFF ⇒
// exactly TG-510's evidence-only behavior, byte-identical.

import (
	"errors"
	"fmt"

	"github.com/territory-grounder/grounder/core/audit"
)

// ErrCorpusUnverifiable reports that the maintained corpus could not be checked against a witness AT ALL —
// there is no witness store, the witness history is unreadable, or no witness has been recorded yet. For
// ENFORCEMENT (TG-519) this is fail-CLOSED and treated exactly like a detected tamper: an un-checkable corpus
// is dropped from trusted retrieval, not admitted on the benefit of the doubt.
//
// THIS IS THE DELIBERATE INVERSION of the evidence layer's DetectCorpusTamperOnWrite (corpus_chain.go), which
// treats an EMPTY witness history as NOT-tamper (the first armed write legitimately establishes the baseline —
// there is nothing yet to contradict, and evidence fails safe-WARN so admitting is correct there). Enforcement
// fails safe-DROP, so the SAME "no witness yet" input yields the OPPOSITE verdict here: unverifiable ⇒ drop.
// Two controls, two questions, two fail directions — do not converge them.
var ErrCorpusUnverifiable = errors.New("knowledge: maintained corpus cannot be verified against a witness (enforcement fails closed — dropping it from trusted retrieval)")

// EnforceCorpusAdmission is the ENFORCEMENT decision: may `maintained` compose into trusted retrieval? It
// returns nil to ADMIT the maintained corpus, or a non-nil error to DROP it (the caller then composes from the
// SEED corpus alone, or empty). Fail-CLOSED — any doubt drops:
//
//   - anchorsErr != nil        (the witness history could not be read)          -> DROP (ErrCorpusUnverifiable)
//   - len(anchors) == 0        (nothing has ever been witnessed)                -> DROP (ErrCorpusUnverifiable)
//   - the corpus does not reproduce the LATEST witness (a tamper)               -> DROP (ErrCorpusTamper)
//   - the corpus reproduces the latest witness (clean)                          -> ADMIT (nil)
//
// Pure — no I/O, no clock — so the killing oracle lives here and needs no DB. The caller supplies the already-
// materialised (anchors, anchorsErr) it read from the witness store, and the freshly-parsed maintained rows.
//
// The LATEST witness is the one the most recent legitimate write recorded; a mutable snapshot is verified
// against it (VerifyCorpusAgainstAnchor's whole-set commitment), not against the whole history — which would
// false-alarm on ordinary learning and false-PASS an injected row that sorts last (see corpus_chain.go).
func EnforceCorpusAdmission(maintained []Incident, anchors []audit.Anchor, anchorsErr error) error {
	if anchorsErr != nil {
		return fmt.Errorf("%w: the witness history could not be read: %v", ErrCorpusUnverifiable, anchorsErr)
	}
	if len(anchors) == 0 {
		// Enforcement's inversion: evidence treats this as "first write establishes the baseline" and admits;
		// enforcement cannot verify an unwitnessed corpus, so it DROPS. Arm TG_CORPUS_APPEND_ONLY so writes
		// record witnesses, or the maintained corpus stays dropped.
		return fmt.Errorf("%w: no witness has ever been recorded (arm TG_CORPUS_APPEND_ONLY so writes witness the corpus)", ErrCorpusUnverifiable)
	}
	return VerifyCorpusAgainstAnchor(maintained, anchors[len(anchors)-1])
}
