package groundnet

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"

	"github.com/territory-grounder/grounder/core/audit"
)

// TranslogDomain is the anchor domain for the groundnet transparency log — a third append-only
// hash-chain beside the governance ledger (migration 0015) and the knowledge corpus, on the same
// tamper-evident discipline (REQ-2105/2106). It is NOT a blockchain: provenance is a signed,
// append-only, witnessed log (the canonical spec §5), and this is its per-estate local root.
const TranslogDomain = "groundnet-translog"

// translogGenesis is the fixed link the chain folds from and the HEAD of an empty log.
const translogGenesis = "tg-groundnet-translog/genesis/1"

// translogEntry is one folded position in the chain: the content-address subject and the running
// fold that commits to it and every prior entry.
type translogEntry struct {
	seq     int64
	subject string
	hash    string
}

// Translog is the local transparency log: an append-only hash-chain of registered statement
// content-addresses (TranslogDomain), TG's per-estate "blockchain of one" (the governance-ledger
// model) extended toward the federation. It implements BOTH TransparencyLog (Register -> Receipt)
// and ReplayGuard (RecordIfNew) over ONE chain, so a statement this node emitted OR has already
// ingested is caught as a replay. This is the in-memory core; T-021-8 backs it durably with the
// governance_ledger + ledger_anchor witness store. Safe for concurrent use.
type Translog struct {
	mu      sync.Mutex
	entries []translogEntry
	bySub   map[string]int64 // subject -> seq, for O(1) replay dedup
}

// NewTranslog returns an empty in-memory transparency log.
func NewTranslog() *Translog { return &Translog{bySub: make(map[string]int64)} }

// translogFold folds one entry into the chain: sha256 over (prev-hash, subject), length-prefixed so
// no field-boundary shift can make two different (prev, subject) pairs collide (the audit /
// corpus_chain discipline).
func translogFold(prev, subject string) string {
	sum := sha256.New()
	var n [8]byte
	wf := func(s string) {
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		sum.Write(n[:])
		sum.Write([]byte(s))
	}
	wf(prev)
	wf(subject)
	return hex.EncodeToString(sum.Sum(nil))
}

// appendLocked folds subject onto the chain and returns its Receipt. Caller holds mu.
func (t *Translog) appendLocked(subject string) Receipt {
	prev := translogGenesis
	if len(t.entries) > 0 {
		prev = t.entries[len(t.entries)-1].hash
	}
	seq := int64(len(t.entries)) + 1
	entryHash := translogFold(prev, subject)
	t.entries = append(t.entries, translogEntry{seq: seq, subject: subject, hash: entryHash})
	t.bySub[subject] = seq
	anchor := audit.ComputeAnchor(t.headStateLocked())
	return Receipt{
		Domain:    TranslogDomain,
		Seq:       seq,
		EntryHash: entryHash,
		Subject:   subject,
		HeadSeq:   anchor.Seq,
		HeadHash:  anchor.Hash,
		Digest:    anchor.Digest,
	}
}

// headStateLocked projects the CURRENT chain HEAD + trailing window for anchoring (REQ-2106). Caller
// holds mu.
func (t *Translog) headStateLocked() audit.HeadState {
	return t.headStateAtLocked(int64(len(t.entries)))
}

// headStateAtLocked reconstructs the HEAD state as of position uptoSeq (1..len). Because the chain is
// append-only, every past HEAD is exactly reproducible — which is what lets a Receipt's recorded HEAD
// anchor be re-derived and checked at any later time (REQ-2106). Caller holds mu.
func (t *Translog) headStateAtLocked(uptoSeq int64) audit.HeadState {
	n := int(uptoSeq)
	if n <= 0 || n > len(t.entries) {
		return audit.HeadState{}
	}
	window := audit.DefaultAnchorWindow
	start := n - window
	if start < 0 {
		start = 0
	}
	recent := make([]audit.RowRef, 0, n-start)
	for _, e := range t.entries[start:n] {
		recent = append(recent, audit.RowRef{Seq: e.seq, Hash: e.hash})
	}
	head := t.entries[n-1]
	return audit.HeadState{Seq: head.seq, Hash: head.hash, Recent: recent}
}

// Register implements TransparencyLog: it registers a Signed Statement by folding its content-address
// (sub) into the append-only chain and returns the marshaled Receipt (REQ-2105).
func (t *Translog) Register(_ context.Context, s *Statement) ([]byte, error) {
	h, err := s.Header()
	if err != nil {
		return nil, err
	}
	if h.Subject == "" {
		return nil, errors.New("groundnet: cannot register a statement with no subject")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return json.Marshal(t.appendLocked(h.Subject))
}

// RecordIfNew implements ReplayGuard: it ATOMICALLY records a statement (by its content-address
// subject) if it has not been seen, folding it into the same chain so a later replay is caught, and
// returns whether it was newly recorded (REQ-2115). A single locked op — no check-then-act window.
func (t *Translog) RecordIfNew(_ context.Context, subject string, _ []byte) (bool, error) {
	if subject == "" {
		return false, errors.New("groundnet: cannot record a statement with no subject")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, seen := t.bySub[subject]; seen {
		return false, nil
	}
	t.appendLocked(subject)
	return true, nil
}

// Len reports how many statements the log has recorded (test/introspection).
func (t *Translog) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.entries)
}
