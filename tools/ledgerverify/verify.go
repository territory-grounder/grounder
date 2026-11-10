package main

import (
	"time"

	"github.com/territory-grounder/grounder/core/audit"
)

const provesNote = "Recomputes every row's hash and link, and re-derives each recorded head anchor from the current chain. It detects an edited, removed, reordered or duplicated row and a rollback behind a recorded anchor. It cannot detect a rewrite by an actor who also rewrote every anchor in the same database, nor anything after the last anchor. The separate Temporal witness is not read here."

type Result struct {
	Rows           int       `json:"rows"`
	MaxSeq         int64     `json:"max_seq"`
	HeadHash       string    `json:"head_hash"`
	ChainOK        bool      `json:"chain_ok"`
	ChainError     string    `json:"chain_error"`
	AnchorsChecked int       `json:"anchors_checked"`
	AnchorsOK      bool      `json:"anchors_ok"`
	AnchorError    string    `json:"anchor_error"`
	VerifiedAt     time.Time `json:"verified_at"`
	Proves         string    `json:"proves"`
}

func Verify(entries []audit.LedgerEntry, anchors []audit.Anchor, now time.Time) Result {
	r := Result{Rows: len(entries), AnchorsChecked: len(anchors), VerifiedAt: now.UTC(), Proves: provesNote}
	if len(entries) == 0 {
		r.ChainError = "the ledger is empty: nothing was verified"
		r.AnchorError = r.ChainError
		return r
	}
	last := entries[len(entries)-1]
	r.MaxSeq, r.HeadHash = last.Seq, last.Hash
	if err := audit.VerifyChain(entries); err != nil {
		r.ChainError = err.Error()
	} else {
		r.ChainOK = true
	}
	if len(anchors) == 0 {
		r.AnchorError = "no anchors recorded: a rollback cannot be detected"
		return r
	}
	if err := audit.VerifyAgainstAnchors(audit.RowRefsOf(entries), anchors); err != nil {
		r.AnchorError = err.Error()
	} else {
		r.AnchorsOK = true
	}
	return r
}
