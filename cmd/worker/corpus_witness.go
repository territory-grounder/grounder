package main

// TG-510 Slice A — the shared record-on-write tamper-evidence for the maintained precedent corpus
// (TG_KNOWLEDGE_FILE). Every lane that writes that corpus routes through a *corpusWitness so the file and its
// external witness (an append-only ledger_anchor row the writer cannot rewrite) advance together, and every
// write-time re-read of the file is first checked against the latest witness so an out-of-band edit cannot be
// silently LAUNDERED into a fresh witness. It is nil when disarmed (flag off / no DSN / no path), in which
// case the write path is byte-identical to before. EVIDENCE-ONLY and fail-safe: it can only WARN, never block.

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/knowledge"
)

// corpusWitness records + verifies HEAD anchors of the maintained precedent corpus under the knowledge-corpus
// domain. Its fields are immutable after construction, so a single instance is safe to share across the
// corpus write lanes (the four in main() + the AWX ingest lane).
type corpusWitness struct {
	sink   *db.ScopedAnchorStore
	window int
}

// newCorpusWitness builds the witness bound to the knowledge-corpus domain. Returns nil when there is no
// durable store (disarmed), so callers can treat nil as "tamper-evidence off — behave exactly as before".
func newCorpusWitness(pool *db.Pool, window int) *corpusWitness {
	if pool == nil {
		return nil
	}
	return &corpusWitness{sink: db.NewAnchorStore(pool).Scoped(knowledge.CorpusAnchorDomain), window: window}
}

// detectOnWrite is the WRITE-TIME limb that closes read-merge-write LAUNDERING (see core/knowledge
// DetectCorpusTamperOnWrite): the freshly-read on-disk `existing` a legitimate write is about to extend MUST
// still reproduce the latest witness. If it diverges, the file was edited outside the write path since the
// last legitimate write — ALARM now, BEFORE the union re-baselines it. Evidence-only: it warns and returns;
// the caller proceeds with the write (Slice A never blocks). A witness-read failure is surfaced, not an
// alarm (an unreadable witness store is not proof of tamper); the periodic verify remains the standing net.
func (w *corpusWitness) detectOnWrite(existing []knowledge.Incident) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	anchors, err := w.sink.Anchors(ctx)
	if err != nil {
		log.Printf("corpus anchor: write-time verify could not read witnesses: %v (proceeding; the periodic verify still checks)", err)
		return
	}
	if terr := knowledge.DetectCorpusTamperOnWrite(existing, anchors); terr != nil {
		log.Printf("!!! KNOWLEDGE CORPUS TAMPER DETECTED (write-time): %v — the on-disk corpus a legitimate write is about to extend did NOT come through the corpus write path (an out-of-band edit); the new witness will re-baseline over it, so THIS is the one detection of this tamper — investigate (TG-510 Slice A, evidence-only — the write is NOT blocked, enforcement is a later owner-armed slice)", terr)
	}
}

// record appends a HEAD witness of the just-written corpus. Best-effort: a failure only logs (the corpus is
// already durable; the next write re-witnesses). Bounded so a stalled substrate cannot hold a write lock
// without limit.
func (w *corpusWitness) record(corpus []knowledge.Incident) {
	a := audit.ComputeAnchor(knowledge.CorpusHeadState(corpus, w.window))
	a.At = time.Now().UTC()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := w.sink.Record(ctx, a); err != nil {
		log.Printf("corpus anchor: witness record failed: %v (evidence only — the corpus write already succeeded; the next write retries)", err)
		return
	}
	log.Printf("corpus anchor: witnessed knowledge-corpus HEAD seq=%d hash=%.12s digest=%.12s (window %d) — external tamper-evidence for the maintained precedent corpus (TG-510 Slice A)", a.Seq, a.Hash, a.Digest, a.WindowSize)
}

// anchors exposes the witness history for the periodic verify loop.
func (w *corpusWitness) anchors(ctx context.Context) ([]audit.Anchor, error) {
	return w.sink.Anchors(ctx)
}

// sameCorpusFile reports whether two configured paths resolve to the SAME file — used to decide whether the
// AWX playbooks lane writes the maintained precedent corpus (TG_KNOWLEDGE_FILE) and so must route through the
// witness. Prefers os.SameFile (follows symlinks / distinct spellings) when both exist; falls back to an
// absolute-cleaned path comparison when one does not exist yet.
func sameCorpusFile(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if fa, ea := os.Stat(a); ea == nil {
		if fb, eb := os.Stat(b); eb == nil {
			return os.SameFile(fa, fb)
		}
	}
	ca, ea := filepath.Abs(filepath.Clean(a))
	cb, eb := filepath.Abs(filepath.Clean(b))
	return ea == nil && eb == nil && ca == cb
}
