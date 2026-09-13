package skillstore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
)

// --- The distillate-corpus tamper chain (TG-489; owner ruling TG-488 B24 / TG-146 S6) ---
//
// skill_version is the de-novo distillate corpus: the learned content the flywheel writes and seed
// composition loads into the agent's trusted <behavioral_guidance> block. Its existing protections are
// write-path protections — the Transition state machine, CHECK constraints, append-only rationale, the
// compose class filter (TG-476). None of them can SEE an out-of-band write: a raw UPDATE to a
// production row's body reaches the next session's prompt with no control in between.
//
// The chain closes that: every appended version row carries a link binding its CREATION-IMMUTABLE
// facts to the entire prior history, and a singleton head row records the latest link and row count.
// Verification recomputes the whole chain from genesis at load — a tampered body, a forged or deleted
// row, or a stale head all surface as a named refusal, and composition falls back to the compiled
// registry IN FULL (the same visible fallback any store failure takes; nothing store-backed loads from
// a corpus that cannot prove itself).
//
// Deliberately OUTSIDE the chain: status, rationale, ledger_seq, eval blobs and status_changed_at —
// they mutate legitimately (UpdateVersion's contract), and their integrity story is the state machine
// plus the governance ledger, not this chain. The chain binds what CreateVersion promises never
// changes: id, skill_name, version, the content hash over body+predicate, author, source, parent.
//
// v1 boundaries, stated (all recorded as follow-ups on TG-489):
//   - a tamperer who rewrites EVERY subsequent link AND the head row consistently is not caught by
//     this chain alone — that requires anchoring the head outside the database (the
//     governance-ledger anchor). What v1 buys: every cheap tamper (edit a row, delete a row, forge
//     a row, restore an old dump over a newer corpus) breaks loudly.
//   - verification is a full-corpus walk on EVERY composer read: one SHA-256 + a body read per row.
//     Free at today's 58 rows; a corpus in the thousands makes this a per-session latency/DB line
//     item and wants incremental verification or a head-keyed cache — sized deliberately, later,
//     not silently.

// ChainGenesis is the fixed link value the first appended row chains from, and the head value of a
// verified EMPTY corpus. An empty corpus is a healthy state with its own denominator ("0 of 0"), never
// an error and never conflated with a refusal (TG-365).
const ChainGenesis = "tg-distillate-chain/genesis/1"

// ChainFacts are the creation-immutable facts of one skill_version row — the exact fields
// CreateVersion fixes forever (body and predicate are bound through the recomputed ContentHash, so a
// body edit breaks the chain even if the stored content_hash column is edited to match).
type ChainFacts struct {
	ID              int64
	SkillName       string
	Version         string
	ContentHash     string
	Author          string
	Source          string
	ParentVersionID int64
}

// ChainLink computes the link for one appended row: sha256 over the previous link and the row's
// creation-immutable facts, every variable-length field length-prefixed so no field boundary is
// forgeable by content.
func ChainLink(prev string, f ChainFacts) string {
	h := sha256.New()
	wf := func(s string) {
		fmt.Fprintf(h, "%d:", len(s))
		io.WriteString(h, s)
	}
	wf(prev)
	fmt.Fprintf(h, "#%d#%d#", f.ID, f.ParentVersionID)
	wf(f.SkillName)
	wf(f.Version)
	wf(f.ContentHash)
	wf(f.Author)
	wf(f.Source)
	return hex.EncodeToString(h.Sum(nil))
}

// ChainRow pairs a row's facts with the link stored on it ("" = the column is NULL — a row written
// around the chained writer).
type ChainRow struct {
	Facts      ChainFacts
	StoredLink string
}

// Chain-verdict reasons. The vocabulary is closed; "" means intact.
const (
	ChainReasonUninitialized = "uninitialized" // no head row — EnsureChain has not run
	ChainReasonMissingLink   = "missing-link"  // a row carries NULL — written around the chained writer
	ChainReasonLinkMismatch  = "link-mismatch" // recomputed link differs — content or history tampered
	ChainReasonCountMismatch = "count-mismatch"
	ChainReasonHeadMismatch  = "head-mismatch"
)

// ChainReport is the verification verdict, always carrying its denominator.
type ChainReport struct {
	OK          bool
	Total       int    // rows examined
	Verified    int    // rows whose link recomputed clean (prefix of Total)
	Head        string // recomputed head (== ChainGenesis for an empty corpus)
	StoredHead  string
	StoredCount int64
	BrokenID    int64  // first offending row id (0 when the defect is not row-local)
	Reason      string // "" when OK; else one of the ChainReason* constants
}

// String renders the verdict WITH its denominator — "0 of 0" and a refusal are different sentences by
// construction (TG-365: a verdict without a denominator is what a broken query and a healthy system
// produce identically).
func (r ChainReport) String() string {
	switch {
	case r.OK && r.Total == 0:
		return "distillate chain OK — 0 of 0 rows (genesis; an empty corpus is a state, not an error)"
	case r.OK:
		return fmt.Sprintf("distillate chain OK — %d of %d rows verified, head=%.12s…", r.Verified, r.Total, r.Head)
	case r.Reason == ChainReasonUninitialized:
		return "distillate chain UNINITIALIZED — no head row; refuse store-backed compose until EnsureChain runs (worker boot does this)"
	case r.BrokenID != 0:
		return fmt.Sprintf("distillate chain BROKEN at row id=%d (%s) — %d of %d rows verified; store-backed compose refused",
			r.BrokenID, r.Reason, r.Verified, r.Total)
	default:
		return fmt.Sprintf("distillate chain BROKEN (%s) — %d of %d rows verified, head=%.12s… stored=%.12s…/%d; store-backed compose refused",
			r.Reason, r.Verified, r.Total, r.Head, r.StoredHead, r.StoredCount)
	}
}

// VerifyChain recomputes the chain from genesis over rows (which MUST be in ascending id order — the
// append order bigserial fixes) and checks the stored head row against the recomputation. Pure; the
// caller supplies facts whose ContentHash was RECOMPUTED from the row's body and predicate, so a body
// edit is a link mismatch even when the stored content_hash column was edited to match.
func VerifyChain(rows []ChainRow, storedHead string, storedCount int64) ChainReport {
	rep := ChainReport{Total: len(rows), StoredHead: storedHead, StoredCount: storedCount}
	prev := ChainGenesis
	for _, r := range rows {
		if r.StoredLink == "" {
			rep.BrokenID, rep.Reason, rep.Head = r.Facts.ID, ChainReasonMissingLink, prev
			return rep
		}
		want := ChainLink(prev, r.Facts)
		if r.StoredLink != want {
			rep.BrokenID, rep.Reason, rep.Head = r.Facts.ID, ChainReasonLinkMismatch, prev
			return rep
		}
		prev = want
		rep.Verified++
	}
	rep.Head = prev
	if storedCount != int64(len(rows)) {
		rep.Reason = ChainReasonCountMismatch
		return rep
	}
	if storedHead != prev {
		rep.Reason = ChainReasonHeadMismatch
		return rep
	}
	rep.OK = true
	return rep
}

// UninitializedChainReport is the fixed verdict for a store whose head row does not exist yet. It is
// NOT ok: composition must refuse store-backed content until the chain is initialized, and the report
// names the repair.
func UninitializedChainReport(total int) ChainReport {
	return ChainReport{Total: total, Reason: ChainReasonUninitialized}
}
