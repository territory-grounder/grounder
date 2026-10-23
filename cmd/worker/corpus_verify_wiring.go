package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/territory-grounder/grounder/core/knowledge"
)

// wireCorpusVerify arms the consuming half of TG-510 Slice A corpus tamper-evidence, carved out of main()'s
// composition root (TG-501 LOC-debt paydown): periodically re-derives the maintained precedent corpus HEAD
// and WARNs if it no longer reproduces the latest recorded witness. Evidence-only and fail-safe — see the
// comments below. A no-op without both an armed witness sink and a corpus path. Behaviour is unchanged by
// the move.
func wireCorpusVerify(corpusEvidence *corpusWitness, corpusPath string) {
	// TG-510 Slice A: the CONSUMING half of corpus tamper-evidence — periodically re-derive the maintained
	// precedent corpus HEAD and WARN if it no longer reproduces the LATEST recorded witness (a raw edit that
	// reached the file outside the write path). The corpus is a MUTABLE, re-sorted set, not an append-only
	// ledger, so its check is a whole-set commitment against the most recent legitimate write's witness
	// (knowledge.VerifyCorpusAgainstAnchor), NOT the governance ledger's grow-only VerifyAgainstAnchors — which
	// would false-PASS an injected row that sorts last and false-ALARM on ordinary learning. EVIDENCE-ONLY and
	// fail-safe: a mismatch is a WARNING, retrieval is never blocked (enforcement is a later owner-armed slice).
	// Coarse cadence (a full re-derive per pass); an immediate first pass witnesses the corpus at boot — when a
	// raw edit made while the worker was down is worth catching most. Armed only with the flag + a DSN + a path.
	//
	// It reads the corpus file and the witness history WITHOUT the corpus write lock, so in the sub-second
	// window between a legitimate write's atomic rename and its witness INSERT a concurrent pass can observe the
	// new corpus against the not-yet-recorded witness and WARN spuriously. That is fail-safe by the control's
	// own bar (a false ALARM at worst, never a false pass) and astronomically unlikely at this cadence; the next
	// pass, and every pass after the write completes, reads clean. TG-516 killed the ANALOGOUS boot read-race in
	// the governance verifier by reading the anchors BEFORE the chain — valid there because that chain is
	// append-only + monotonic and the check is an INEQUALITY (anchor.seq <= chain.maxSeq), so a later chain read
	// can only have caught up. That trick does NOT transfer here: the corpus is a MUTABLE snapshot verified by
	// EXACT whole-set match against the LATEST witness (needed to catch a rollback to an older witnessed state,
	// which "match any witness" would false-PASS), and no read ordering makes an exact match across two
	// independently-mutated stores race-free — only reading both under the write lock would, which (per TG-516's
	// own reasoning) is not worth coupling a coarse observe-only job to the hot write path for a fail-safe alarm.
	if corpusEvidence != nil && corpusPath != "" {
		if iv := getenv("TG_CORPUS_VERIFY_INTERVAL", "24h"); iv != "" {
			if d, derr := time.ParseDuration(iv); derr == nil && d > 0 {
				verifyCorpusOnce := func() {
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
					defer cancel()
					anchors, aerr := corpusEvidence.anchors(ctx)
					if aerr != nil {
						log.Printf("corpus verify: could not read witnesses: %v (retry next tick) — an unverified corpus is not a clean one", aerr)
						return
					}
					if len(anchors) == 0 {
						return // nothing witnessed yet (no write since arming) — nothing to contradict
					}
					var current []knowledge.Incident
					if cf, cerr := os.Open(corpusPath); cerr == nil {
						parsed, perr := knowledge.ParseCorpus(cf)
						cf.Close()
						if perr != nil {
							log.Printf("!!! KNOWLEDGE CORPUS TAMPER/CORRUPTION: the maintained corpus %s no longer parses (%v) — it cannot be verified against its witness; treat as tampered (TG-510 Slice A, evidence-only — retrieval is NOT blocked)", corpusPath, perr)
							return
						}
						current = parsed
					} else if !os.IsNotExist(cerr) {
						log.Printf("corpus verify: could not read corpus %s: %v (retry next tick)", corpusPath, cerr)
						return
					}
					// The LATEST witness is the one the last legitimate write recorded; a mutable snapshot is
					// verified against it, not the whole history (which would false-alarm on ordinary learning).
					if terr := knowledge.VerifyCorpusAgainstAnchor(current, anchors[len(anchors)-1]); terr != nil {
						log.Printf("!!! KNOWLEDGE CORPUS TAMPER DETECTED: %v — a maintained precedent row that reaches the next agent session's trusted retrieval did NOT come through the corpus write path; investigate (TG-510 Slice A, evidence-only — retrieval is NOT blocked, enforcement is a later owner-armed slice)", terr)
					}
				}
				verifyCorpusOnce() // immediate boot pass
				go func() {
					t := time.NewTicker(d)
					defer t.Stop()
					for range t.C {
						verifyCorpusOnce()
					}
				}()
				log.Printf("corpus verify: checking the maintained knowledge corpus against its recorded witnesses every %s (knowledge.VerifyCorpusAgainstAnchor) — the consuming half of TG-510 Slice A (evidence-only, fail-safe)", d)
			} else if derr != nil {
				log.Printf("corpus verify: invalid TG_CORPUS_VERIFY_INTERVAL %q — corpus tamper-verification disabled", iv)
			}
		}
	}
}
