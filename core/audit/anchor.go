package audit

// TG-80 P1#1 — the ledger-HEAD ANCHOR: external tamper-EVIDENCE for the append-only governance spine,
// adopted from the h-apache-stack peer's "anchor the action-log HEAD in a domain the writer cannot rewrite".
//
// WHY VerifyChain IS NOT ENOUGH. VerifyChain (ledger.go) re-walks the rows it is GIVEN and proves they are an
// internally-consistent hash chain from seq 1. Two tampers survive that check, because both leave a
// self-consistent chain behind:
//
//   1. TAIL TRUNCATION / ROLLBACK. Delete the rows after some seq k. The surviving prefix 1..k still hash-
//      verifies perfectly — VerifyChain reports CLEAN over a ledger that has silently lost its most recent
//      decisions. migration 0015 REVOKEs UPDATE/DELETE from tg_runtime, but that boundary is reversible by a
//      privileged role (0015.down, a mis-scoped retention job, a compromised superuser) — the very actor a
//      post-hoc audit trail exists to catch.
//   2. WHOLESALE RE-LINK. Rewrite a row and recompute every hash from it to the HEAD. The result is again a
//      valid chain; VerifyChain accepts it.
//
// WHAT AN ANCHOR ADDS: a WITNESS OVER TIME. Periodically (temporal/ledgeranchor) the HEAD is folded into a
// deterministic digest and recorded to an append-only store the recording principal itself cannot UPDATE or
// DELETE (ledger_anchor, migration 0092, same REVOKE as the spine — and, once wired through a Temporal
// activity, its event history, a separate credential domain the DB role cannot reach at all). An anchor at T1
// FIXES the HEAD at T1: seq k_1 and its chain hash. A later truncation back below k_1, or a re-link that
// changes the hash AT k_1, is then detectable as a HEAD that has REGRESSED below a witness or a witnessed hash
// that no longer matches — the two blind spots above, made loud. The anchor turns tamper-evidence that an
// attacker holding the ledger-write credential can erase into tamper-evidence recorded by a principal that
// holds no such credential.
//
// The HEAD chain hash already commits to the WHOLE chain (each row's hash includes its predecessor's, so the
// HEAD hash is a rolling commitment to rows 1..Seq). The trailing WINDOW folded into the digest is therefore a
// LOCALISER, not the source of the commitment: it lets the verifier report which recent rows an anchor still
// covers, and makes a within-window edit fail the digest as well as the HEAD-hash check.

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// anchorDomain domain-separates the anchor digest from a ledger row hash: the two are computed over
// different shapes and must never be confused for, or substituted for, one another.
const anchorDomain = "tg/ledger-anchor/v1"

// DefaultAnchorWindow is the number of trailing rows folded into an anchor when a caller does not choose one.
const DefaultAnchorWindow = 64

// DomainGovernanceLedger is the anchor-store domain for the governance_ledger chain — the first anchored chain
// and the value every anchor recorded before the TG-515 generalization is backfilled to. A domain is the
// store KEY that scopes one chain's witnesses from another's; it is deliberately NOT folded into the Digest,
// because the digest already commits to the chain's own (seq, hash, window) — content that naturally separates
// chains — so keeping domain out of the digest preserves byte-compatibility with every pre-generalization
// anchor. A second consumer (TG-510's knowledge corpus) records under its own domain string.
const DomainGovernanceLedger = "governance-ledger"

// RowRefsOf projects governance-ledger entries down to the (seq, hash) RowRefs VerifyAgainstAnchors needs — the
// governance chain's adapter to the domain-agnostic verifier. Any other anchored chain builds its own RowRefs
// directly from its own row type.
func RowRefsOf(entries []LedgerEntry) []RowRef {
	refs := make([]RowRef, len(entries))
	for i, e := range entries {
		refs[i] = RowRef{Seq: e.Seq, Hash: e.Hash}
	}
	return refs
}

// ErrAnchorMismatch reports that the current chain contradicts a recorded HEAD anchor — a truncated tail, a
// removed covered row, or a rewritten prefix. It is the anchor's answer to a tamper VerifyChain cannot see.
var ErrAnchorMismatch = errors.New("audit: ledger HEAD anchor does not match the current chain (tamper)")

// RowRef is the minimal (seq, chain-hash) projection of a governance_ledger row an anchor needs.
type RowRef struct {
	Seq  int64
	Hash string
}

// HeadState is the ledger HEAD plus a trailing window — the input to ComputeAnchor. Seq is max(seq) (0 for an
// empty ledger); Hash is the HEAD row's chain hash, which commits to every row 1..Seq; Recent is the trailing
// rows in ASCENDING seq order, with Recent[len-1].Seq == Seq.
type HeadState struct {
	Seq    int64
	Hash   string
	Recent []RowRef
}

// Anchor is one recorded external witness of the ledger HEAD at a point in time (one ledger_anchor row).
// WindowSize is the number of trailing rows the Digest was taken over; At is metadata (when it was recorded)
// and is deliberately NOT folded into the Digest, so two witnesses of the same immutable HEAD are equal.
type Anchor struct {
	Seq        int64
	Hash       string
	WindowSize int
	Digest     string
	At         time.Time
}

// ComputeAnchor derives the deterministic HEAD digest for a HeadState. Pure and total: an empty ledger
// (Seq 0, no rows) yields a well-defined digest over "nothing witnessed" rather than a panic. Fields are
// length-prefixed (the same discipline as entryHash) so no concatenation of two fields can collide with a
// different pair.
func ComputeAnchor(h HeadState) Anchor {
	sum := sha256.New()
	var num [8]byte
	writeUint := func(v int64) {
		binary.BigEndian.PutUint64(num[:], uint64(v))
		sum.Write(num[:])
	}
	writeField := func(s string) {
		writeUint(int64(len(s)))
		sum.Write([]byte(s))
	}
	writeField(anchorDomain)
	writeUint(h.Seq)
	writeField(h.Hash)
	writeUint(int64(len(h.Recent)))
	for _, r := range h.Recent {
		writeUint(r.Seq)
		writeField(r.Hash)
	}
	return Anchor{
		Seq:        h.Seq,
		Hash:       h.Hash,
		WindowSize: len(h.Recent),
		Digest:     hex.EncodeToString(sum.Sum(nil)),
	}
}

// VerifyAgainstAnchors re-derives every recorded anchor from the CURRENT chain and returns the first
// contradiction. current is the full chain in ascending seq as (seq, hash) RowRefs — the governance ledger
// maps LedgerStore.All to these, and ANY other anchored chain (e.g. the knowledge corpus, TG-510) supplies its
// own RowRefs, so this one verifier serves every chain the anchor store witnesses by DOMAIN (TG-515). anchors
// is that chain's witness history (AnchorStore.Anchors for its domain). It is the check VerifyChain cannot
// perform, because it compares the chain against a record made BEFORE the tamper rather than against itself.
// Pure — no DB, no chain-specific row type, oracle-testable.
func VerifyAgainstAnchors(current []RowRef, anchors []Anchor) error {
	bySeq := make(map[int64]RowRef, len(current))
	var maxSeq int64
	for _, e := range current {
		bySeq[e.Seq] = e
		if e.Seq > maxSeq {
			maxSeq = e.Seq
		}
	}
	for _, a := range anchors {
		if a.Seq == 0 {
			continue // an anchor of the empty ledger witnessed nothing — not a fixed point to regress from
		}
		if a.Seq > maxSeq {
			return fmt.Errorf("%w: anchor witnessed HEAD seq %d but the current chain stops at seq %d — the rows "+
				"it witnessed (through seq %d) were truncated or rolled back (VerifyChain cannot see this: the "+
				"shortened prefix still hash-verifies)", ErrAnchorMismatch, a.Seq, maxSeq, a.Seq)
		}
		head, ok := bySeq[a.Seq]
		if !ok {
			return fmt.Errorf("%w: the anchored HEAD seq %d is absent from the current chain — a covered row "+
				"was removed", ErrAnchorMismatch, a.Seq)
		}
		if head.Hash != a.Hash {
			return fmt.Errorf("%w: the anchored HEAD seq %d hash changed — the row and every row before it "+
				"were rewritten (an internally-consistent forgery VerifyChain would accept)", ErrAnchorMismatch, a.Seq)
		}
		hs, err := headStateFromChain(bySeq, a.Seq, a.WindowSize)
		if err != nil {
			return fmt.Errorf("%w: cannot rebuild the window the anchor at seq %d covered: %v", ErrAnchorMismatch, a.Seq, err)
		}
		if got := ComputeAnchor(hs).Digest; got != a.Digest {
			return fmt.Errorf("%w: the digest over the rows the anchor at seq %d covered differs — those rows "+
				"were altered", ErrAnchorMismatch, a.Seq)
		}
	}
	return nil
}

// headStateFromChain rebuilds the HeadState an anchor was taken over from the current rows: the HEAD at
// headSeq and the trailing `window` rows [headSeq-window+1 .. headSeq] (clamped at seq 1). Every row in that
// span must be present — a hole is itself tamper — else it returns an error the caller surfaces as a mismatch.
func headStateFromChain(bySeq map[int64]RowRef, headSeq int64, window int) (HeadState, error) {
	if window <= 0 {
		window = 1
	}
	start := headSeq - int64(window) + 1
	if start < 1 {
		start = 1
	}
	recent := make([]RowRef, 0, headSeq-start+1)
	for s := start; s <= headSeq; s++ {
		e, ok := bySeq[s]
		if !ok {
			return HeadState{}, fmt.Errorf("row seq %d missing from the window [%d..%d]", s, start, headSeq)
		}
		recent = append(recent, RowRef{Seq: e.Seq, Hash: e.Hash})
	}
	return HeadState{Seq: headSeq, Hash: bySeq[headSeq].Hash, Recent: recent}, nil
}
