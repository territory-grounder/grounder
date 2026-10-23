package knowledge

// TG-510 Slice A — tamper-EVIDENCE for the maintained precedent corpus.
//
// The maintained knowledge corpus (TG_KNOWLEDGE_FILE) is an operator-editable JSON array of prior
// Incident rows that the retriever surfaces as PRECEDENT into the next agent session's trusted context.
// Until this, it was UNCHAINED: a raw edit to the file — a rewritten Resolution, a Source upgraded to
// "verified-resolution", an injected or deleted row — reaches that trusted retrieval with no integrity
// control between the edit and the model. This closes that with the SAME witness-over-time primitive the
// governance ledger uses (core/audit.ComputeAnchor + the append-only ledger_anchor store, TG-515), reused
// by DOMAIN: the corpus records under its own domain string so its witnesses are never checked against the
// governance chain's rows and vice-versa.
//
// EVIDENCE ONLY (Slice A). This layer DETECTS and lets the worker WARN. It never blocks, refuses, or
// enforces — enforcement (refusing to compose from a corpus that cannot prove itself, as skillstore's
// chain does) is a later owner-armed slice. Fail-safe by construction: a verify mismatch is a false ALARM
// at worst, NEVER a false PASS.
//
// WHY THIS IS NOT THE GOVERNANCE ledger's check. core/audit.VerifyAgainstAnchors is built for an
// APPEND-ONLY, positionally-stable chain: seq k witnesses a row that never changes, and GROWTH beyond a
// witnessed HEAD is legitimate (a new append), so it deliberately does not flag rows past the anchored
// seq. The corpus is the opposite: a MUTABLE, re-sorted SET keyed by ExternalRef (MergeCorpus does
// last-write-wins updates; decay removes rows). Two consequences drive the design here:
//
//   1. An un-witnessed APPEND is TAMPER for the corpus (an injected precedent row), but VerifyAgainstAnchors
//      would PASS it — a false pass, disqualifying for a fail-safe control. So the corpus is verified as a
//      WHOLE-SET commitment: the current corpus must REPRODUCE the anchor recorded by the most recent
//      legitimate write (VerifyCorpusAgainstAnchor). The HEAD hash folds every row, so any add / remove /
//      edit — including an append that sorts last — changes it and is caught.
//   2. Checking against the WHOLE anchor history (as the governance verifier does) would false-ALARM on
//      every legitimate mid-insert or LWW update, because those shift the positional chain relative to older
//      witnesses. The corpus checks the LATEST witness only — "does today's corpus match the last thing the
//      trusted writer wrote?" — which is the meaningful question for a snapshot and never fires on ordinary
//      learning.
//
// TWO LIMBS, because record-on-write alone LAUNDERS a tamper. Every corpus writer re-reads the file off disk
// as `existing`, merges new rows, and writes+witnesses the union — so a naive record-on-write would re-read
// an out-of-band edit, fold it into `existing`, and record a FRESH witness OVER the tamper, silently
// re-baselining it clean before the coarse periodic verify ever runs (and writes fire far more often than
// that verify, so laundering would usually win). The fix is a WRITE-TIME limb:
//
//   - DetectCorpusTamperOnWrite runs at each chokepoint, on the freshly-read `existing`, BEFORE the new
//     witness is recorded. If `existing` no longer reproduces the latest witness, the file was edited outside
//     the write path since the last legitimate write — it ALARMS there (evidence-only: it warns, then the
//     write proceeds and re-baselines, so this is the ONE detection of that tamper). This is what closes the
//     laundering: the tamper is caught at the moment a write would otherwise absorb it.
//   - VerifyCorpusAgainstAnchor runs on the periodic loop as the standing net for a tamper that no write has
//     yet touched.
//
// The witness records only what the TRUSTED WRITE PATH wrote (record-on-write); it is NOT taken at load or
// boot over whatever is already on disk, so a pre-existing tamper is never blessed as the baseline. The
// guarantee is therefore honest tamper-EVIDENCE: it detects a change made AFTER the first witness, at the
// next write (write-time) or the next periodic pass, whichever comes first. Authenticating content that never
// came through the writer is a different control (signing), out of scope.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/territory-grounder/grounder/core/audit"
)

// CorpusAnchorDomain is the ledger_anchor store DOMAIN (the store key that scopes one chain's witnesses
// from another's, migration 0103) under which the maintained precedent corpus records its HEAD anchors.
// The governance ledger uses audit.DomainGovernanceLedger; this is TG-510's "second consumer records under
// its own domain string" (see core/audit/anchor.go). It is a short, non-secret chain NAME — never argv,
// credential, or payload (INV-13).
const CorpusAnchorDomain = "knowledge-corpus"

// corpusChainGenesis is the fixed link the corpus chain folds from, and the HEAD of an EMPTY corpus. An
// empty corpus is a healthy state (all rows decayed, or nothing learned yet), not an error — it witnesses
// "0 of 0", the same way skillstore's ChainGenesis treats an empty distillate corpus (TG-489).
const corpusChainGenesis = "tg-knowledge-corpus-chain/genesis/1"

// ErrCorpusTamper reports that the current maintained corpus does not reproduce its latest recorded
// witness — a precedent row was edited, added, or removed OUT OF BAND (not through the corpus write path).
// It is the corpus's answer to a raw file edit that the write-path protections cannot see.
var ErrCorpusTamper = errors.New("knowledge: maintained corpus does not reproduce its recorded HEAD witness (tamper-evidence)")

// incidentContentHash is the per-row content commitment: sha256 over the row's canonical JSON. Hashing the
// marshalled form (the SAME encoder WriteCorpus round-trips through) covers EVERY field an edit could touch
// — ExternalRef, Host/AlertRule/Site, the Summary and Resolution the agent leans on, ResolvedAt, Tags, and
// the Source trust label — and stays automatically in sync if Incident gains a field later. A change to any
// field changes these bytes, so a body edit is a link mismatch even if some other column were edited to
// compensate.
func incidentContentHash(inc Incident) string {
	b, err := json.Marshal(inc)
	if err != nil {
		// Incident has no unmarshalable field (no channels/funcs/cycles), so this is unreachable in
		// practice; hashing the error text keeps the function total and deterministic rather than panicking.
		b = []byte("knowledge/corpus-chain/marshal-error:" + err.Error())
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// corpusChainLink folds one row into the chain: sha256 over the previous link, the row's ExternalRef
// (its identity), and its content hash — every variable-length field length-prefixed (the TG-489 / entryHash
// discipline) so no field boundary is forgeable by content. Pure and deterministic.
func corpusChainLink(prev, externalRef, contentHash string) string {
	h := sha256.New()
	writeField := func(s string) {
		fmt.Fprintf(h, "%d:", len(s))
		h.Write([]byte(s))
	}
	writeField(prev)
	writeField(externalRef)
	writeField(contentHash)
	return hex.EncodeToString(h.Sum(nil))
}

// CorpusHeadState folds the maintained corpus into an audit.HeadState the anchor math witnesses: Seq is the
// row count, Hash is the chain HEAD (a rolling commitment over EVERY row, in a canonical order), and Recent
// is the trailing `window` rows as RowRefs (the localiser the digest folds, mirroring the governance HEAD).
//
// ORDER-STABLE by construction: the rows are sorted by (ExternalRef, content hash) before folding, so the
// HeadState depends only on the SET of rows and their content, never on the array order the file happens to
// carry — a benign reordering of the JSON is not flagged, and record and verify agree regardless of how
// each read the file. ExternalRef is unique per row in a well-formed corpus (ParseCorpus rejects empty
// refs, MergeCorpus dedupes), and the content-hash tie-break makes the order total even if a tamper
// introduces duplicates. Pure — no I/O, no clock — so it is oracle-testable.
func CorpusHeadState(corpus []Incident, window int) audit.HeadState {
	if window <= 0 {
		window = audit.DefaultAnchorWindow
	}
	type row struct {
		ref, contentHash string
	}
	rows := make([]row, len(corpus))
	for i, inc := range corpus {
		rows[i] = row{ref: inc.ExternalRef, contentHash: incidentContentHash(inc)}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].ref != rows[j].ref {
			return rows[i].ref < rows[j].ref
		}
		return rows[i].contentHash < rows[j].contentHash
	})
	refs := make([]audit.RowRef, 0, len(rows))
	prev := corpusChainGenesis
	for i, r := range rows {
		prev = corpusChainLink(prev, r.ref, r.contentHash)
		refs = append(refs, audit.RowRef{Seq: int64(i + 1), Hash: prev})
	}
	hs := audit.HeadState{Seq: int64(len(rows)), Hash: prev}
	if len(refs) == 0 {
		return hs // empty corpus: Seq 0, Hash = genesis, no trailing window
	}
	start := len(refs) - window
	if start < 0 {
		start = 0
	}
	hs.Recent = append([]audit.RowRef(nil), refs[start:]...)
	return hs
}

// VerifyCorpusAgainstAnchor checks the CURRENT maintained corpus against the LATEST recorded witness. A
// mutable snapshot is tamper-evident by whole-set commitment: the current corpus must reproduce the anchor
// the most recent legitimate write recorded. The HEAD hash commits to every row and the count, so any row
// edited, added (including an append that sorts last — the case core/audit.VerifyAgainstAnchors would let
// pass), or removed changes the recomputed anchor and is reported. The digest is recomputed over the
// anchor's OWN window so a clean corpus reproduces it byte-for-byte.
//
// Fail-safe: it compares the FULL recomputed anchor (Seq, Hash, Digest) and reports on any divergence, so it
// can only ALARM, never bless a changed corpus as clean. Pure — the killing-mutation oracle lives here.
func VerifyCorpusAgainstAnchor(corpus []Incident, latest audit.Anchor) error {
	got := audit.ComputeAnchor(CorpusHeadState(corpus, latest.WindowSize))
	if got.Seq != latest.Seq || got.Hash != latest.Hash || got.Digest != latest.Digest {
		return fmt.Errorf("%w: current corpus HEAD (seq=%d hash=%.12s… digest=%.12s…) does not reproduce the "+
			"latest witness (seq=%d hash=%.12s… digest=%.12s…) — a maintained precedent row was edited, added, "+
			"or removed out of band, not through the corpus write path",
			ErrCorpusTamper, got.Seq, got.Hash, got.Digest, latest.Seq, latest.Hash, latest.Digest)
	}
	return nil
}

// DetectCorpusTamperOnWrite is the WRITE-TIME limb of tamper-evidence — the one that closes read-merge-write
// LAUNDERING. Every corpus writer re-reads the file off disk as `existing`, merges, and records a fresh
// witness over the union; without this check, an out-of-band edit present in `existing` would be re-witnessed
// (laundered) clean. Run BEFORE recording the new witness, on the freshly-read `existing`: it must still
// reproduce the LATEST recorded witness (what the last legitimate write left on disk). If it diverges, the
// file was edited outside the write path since then — return ErrCorpusTamper so the caller ALARMS (then, in
// Slice A, proceeds: evidence-only, the write is never blocked, and this is the ONE detection before the
// union re-baselines).
//
// `anchors` is the witness history in record order (AnchorStore.Anchors); the last element is the latest.
// An EMPTY history is NOT tamper: the first armed write establishes the baseline, and there is nothing yet to
// contradict. Pure — no I/O — so the laundering oracle is testable without a DB.
func DetectCorpusTamperOnWrite(existing []Incident, anchors []audit.Anchor) error {
	if len(anchors) == 0 {
		return nil // no witness yet — the first armed write establishes the baseline, which is not tamper
	}
	return VerifyCorpusAgainstAnchor(existing, anchors[len(anchors)-1])
}
