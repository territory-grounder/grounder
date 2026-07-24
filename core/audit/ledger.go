// Package audit owns Territory Grounder's tamper-evident governance ledger and the required-field
// classification audit record.
//
// Provenance: [O] INV-19 (append-only SHA-256 prev-row hash-chained decision log; every governance
// decision is a required output {decision, reason, action_id, withheld_flag}; the chain is enforced by the
// runtime role's privilege boundary — no UPDATE/DELETE grant (migration 0015 REVOKEs them from tg_runtime,
// making the spine tamper-RESISTANT, not merely tamper-evident), never by a trigger — and re-walked by a
// LedgerVerifier), spec/006 REQ-503.
//
// This in-memory Ledger is the oracle-testable core of the chain; the pgx-backed store (no-UPDATE/DELETE
// privilege boundary per migration 0015, LedgerVerifier schedule) wraps it under compose. VerifyChain is a pure
// function over the persisted rows, so tamper detection is testable without a database.
package audit

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
)

// GovDecision is the required output of any governance decision function — the four fields the ledger
// persists for every decision. Producing a complete GovDecision requires all four; the writer rejects
// a decision missing the load-bearing ones (fail closed). [O] INV-19.
type GovDecision struct {
	Decision string // the decision taken, e.g. "classify:AUTO", "gate:deny", "verdict:deviation"
	Reason   string // the machine reason/signal that produced it
	ActionID string // the content-hashed action this decision is bound to (INV-07)
	Withheld bool   // true when autonomy was withheld (poll/deny) — the "one channel allowed to say no"
}

// LedgerEntry is one appended, hash-chained governance decision (a governance_ledger row).
type LedgerEntry struct {
	Seq      int64  `json:"seq"`
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
	ActionID string `json:"action_id"`
	Withheld bool   `json:"withheld"`
	PrevHash string `json:"prev_hash"`
	Hash     string `json:"hash"`
}

var (
	// ErrIncompleteDecision fails closed when a governance decision omits a required field.
	ErrIncompleteDecision = errors.New("audit: governance decision missing a required field (decision, action_id)")
	// ErrChainBroken is returned by VerifyChain when the hash chain does not verify (tampering).
	ErrChainBroken = errors.New("audit: governance ledger hash chain broken")
)

// entryHash computes the row hash over length-prefixed canonical fields INCLUDING prevHash, so any
// change to any field, to a row's sequence, or to chain order is detectable on re-walk.
func entryHash(seq int64, decision, reason, actionID string, withheld bool, prevHash string) string {
	h := sha256.New()
	var num [8]byte
	binary.BigEndian.PutUint64(num[:], uint64(seq))
	h.Write(num[:])
	writeField := func(s string) {
		binary.BigEndian.PutUint64(num[:], uint64(len(s)))
		h.Write(num[:])
		h.Write([]byte(s))
	}
	writeField(decision)
	writeField(reason)
	writeField(actionID)
	if withheld {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	writeField(prevHash)
	return hex.EncodeToString(h.Sum(nil))
}

// Ledger is the append-only SHA-256 prev-row hash-chained governance ledger. It is org-global: one
// continuous chain over the whole deployment's decisions (ADR-0010). [O] INV-19.
type Ledger struct {
	entries  []LedgerEntry
	lastSeq  int64         // the chain position, tracked explicitly so a ledger seeded from a persisted TAIL
	lastHash string        // continues the chain (seq+1, prev=lastHash) rather than restarting at 1
	sink     LedgerSink    // optional durable mirror — each appended entry is also persisted (INV-19 across restarts)
	riskSink RiskAuditSink // optional durable writer for the full session_risk_audit row
	// mu serializes the chain. The hash chain is INHERENTLY sequential (each row's seq + prev_hash depend on
	// its predecessor), and the Ledger is SHARED across the worker's concurrent Temporal activities, so a lock
	// is required for correctness — without it concurrent Append races and produces a non-monotonic, gap-broken
	// chain with lost audit records. AppendRiskAudit also holds it (it calls Append).
	mu sync.Mutex
}

// LedgerSink durably persists each appended ledger entry (a pgx-backed governance_ledger writer in
// production). The in-memory chain remains authoritative for the seq/hash computation; the sink is a
// write-through mirror, so a sink failure surfaces as the Append error rather than being silently dropped.
type LedgerSink interface {
	Persist(LedgerEntry) error
}

// RiskAuditSink durably persists each full session_risk_audit row (the classification detail behind the
// ledger's decision summary). Attached to the Ledger so AppendRiskAudit writes both through one carrier.
type RiskAuditSink interface {
	PersistRiskAudit(RiskAudit) error
}

// NewLedger returns an empty in-memory ledger (chain starts at seq 1).
func NewLedger() *Ledger { return &Ledger{} }

// NewLedgerFromTail returns a ledger that CONTINUES a persisted chain: the next Append is (lastSeq+1) linked
// to lastHash, so a restarted worker extends the durable chain instead of forking a new one from seq 1. The
// in-memory entries slice starts empty — full-chain verification of a durable ledger reads from the store
// (VerifyChain over the persisted rows), not this process's local window.
func NewLedgerFromTail(lastSeq int64, lastHash string) *Ledger {
	return &Ledger{lastSeq: lastSeq, lastHash: lastHash}
}

// WithSink attaches a durable mirror to the ledger and returns it (chainable at construction).
func (l *Ledger) WithSink(sink LedgerSink) *Ledger {
	l.sink = sink
	return l
}

// WithRiskSink attaches a durable session_risk_audit writer and returns the ledger (chainable).
func (l *Ledger) WithRiskSink(sink RiskAuditSink) *Ledger {
	l.riskSink = sink
	return l
}

// Append validates and appends a governance decision, chaining it to the previous row's hash. Decision
// and ActionID are required — an incomplete decision is rejected (fail closed). When a durable sink is
// attached the entry is mirrored to it; a sink error fails the Append (the decision is not silently
// unpersisted). Returns the entry.
func (l *Ledger) Append(d GovDecision) (LedgerEntry, error) {
	if d.Decision == "" || d.ActionID == "" {
		return LedgerEntry{}, ErrIncompleteDecision
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	seq := l.lastSeq + 1
	prev := l.lastHash
	e := LedgerEntry{
		Seq:      seq,
		Decision: d.Decision,
		Reason:   d.Reason,
		ActionID: d.ActionID,
		Withheld: d.Withheld,
		PrevHash: prev,
		Hash:     entryHash(seq, d.Decision, d.Reason, d.ActionID, d.Withheld, prev),
	}
	if l.sink != nil {
		if err := l.sink.Persist(e); err != nil {
			return LedgerEntry{}, err // fail closed — do not advance the chain if the durable write failed
		}
	} else {
		// Retain the entry in memory ONLY when there is no durable sink — an in-memory ledger IS its own
		// record (Entries()/Verify() read it). When a sink is attached the DB is the record and the chain
		// continues from lastSeq/lastHash, so retaining every entry would be an unbounded leak in a
		// long-running worker; full-chain verification of a durable ledger reads from the store instead.
		l.entries = append(l.entries, e)
	}
	l.lastSeq = seq
	l.lastHash = e.Hash
	return e, nil
}

// Len returns the number of appended entries.
func (l *Ledger) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// Entries returns a copy of the persisted rows (safe to hand to a verifier or a read model).
func (l *Ledger) Entries() []LedgerEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]LedgerEntry, len(l.entries))
	copy(out, l.entries)
	return out
}

// Verify re-walks this ledger's chain and rejects tampering.
func (l *Ledger) Verify() error { return VerifyChain(l.Entries()) }

// VerifyChain is the LedgerVerifier: a pure function that re-walks a slice of persisted rows,
// recomputes each hash, checks the prev-hash linkage and monotonic sequence, and returns ErrChainBroken
// if any row was altered, reordered, or removed. Running it over rows read back from storage is how
// GovernanceChainBroken is detected. [O] INV-19.
func VerifyChain(entries []LedgerEntry) error {
	prev := ""
	for i, e := range entries {
		if e.Seq != int64(i)+1 {
			return fmt.Errorf("%w: row %d has non-monotonic seq %d", ErrChainBroken, i, e.Seq)
		}
		if e.PrevHash != prev {
			return fmt.Errorf("%w: seq %d prev-hash linkage broken", ErrChainBroken, e.Seq)
		}
		want := entryHash(e.Seq, e.Decision, e.Reason, e.ActionID, e.Withheld, e.PrevHash)
		if e.Hash != want {
			return fmt.Errorf("%w: seq %d content was tampered", ErrChainBroken, e.Seq)
		}
		prev = e.Hash
	}
	return nil
}
