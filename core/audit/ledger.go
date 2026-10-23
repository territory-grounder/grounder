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
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
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

	// CreatedAt is the storage clock for the row (governance_ledger.created_at, DEFAULT now()). It is a
	// READ projection only: Persist never writes it, and it is deliberately NOT an input to entryHash.
	//
	// ★ It is therefore NOT covered by the chain. The persisted chain was computed over
	// {seq, decision, reason, action_id, withheld, prev_hash} across every historical row, so feeding a
	// timestamp into entryHash now would invalidate all of them at once — VerifyChain would report the
	// whole spine as tampered. An operator reading this time is reading Postgres's insert clock, which the
	// no-UPDATE/DELETE privilege boundary (migration 0015) protects, NOT the SHA-256 verification. Any
	// surface rendering it must not imply the stronger guarantee.
	//
	// Zero when the entry came from the in-memory Ledger rather than storage; readers must treat the zero
	// time as "unknown", never as an epoch.
	CreatedAt time.Time `json:"created_at"`
}

var (
	// ErrIncompleteDecision fails closed when a governance decision omits a required field.
	ErrIncompleteDecision = errors.New("audit: governance decision missing a required field (decision, action_id)")
	// ErrChainBroken is returned by VerifyChain when the hash chain does not verify (tampering).
	ErrChainBroken = errors.New("audit: governance ledger hash chain broken")
	// ErrChainBusy reports that the caller's deadline expired while WAITING for the chain gate — the append
	// never happened, so the chain is untouched and nothing is over- or under-recorded (TG-277). Distinct
	// from a sink failure, which means the write was attempted.
	ErrChainBusy = errors.New("audit: gave up waiting for the governance chain gate")
	// ErrDuplicateSeq reports that a durable sink rejected an append because the chain seq already exists —
	// a sibling writer advanced the shared governance_ledger head under this process's cached tail (TG-549).
	// A sink maps its unique-violation to this sentinel so AppendContext can RECOVER (re-read the head and
	// re-chain onto it) instead of wedging its governance lane; it is never a silent overwrite of an audit row.
	ErrDuplicateSeq = errors.New("audit: governance ledger seq already exists (durable head advanced under a cached tail)")
)

// maxSeqCollisionRetries bounds the re-chain loop when a sibling writer keeps advancing the shared head.
// A collision is normally cleared on the first re-read — two workers that seeded the same boot tail on one
// deploy de-sync as soon as each sees the other's first commit — and the generous ceiling only matters if a
// sibling is flushing a boot backlog. The caller's ctx bounds the total wall-clock underneath this count.
const maxSeqCollisionRetries = 16

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
	// The chain gate serializes appends. The hash chain is INHERENTLY sequential (each row's seq + prev_hash
	// depend on its predecessor), and the Ledger is SHARED across the worker's concurrent Temporal activities,
	// so serialization is required for correctness — without it concurrent Append races and produces a
	// non-monotonic, gap-broken chain with lost audit records. AppendRiskAudit also passes through it (it
	// calls Append).
	//
	// A one-slot CHANNEL rather than a sync.Mutex (TG-277), because the gate is held ACROSS the durable sink
	// write and that write can stall. A sync.Mutex wait is uncancellable: when Postgres stalled on
	// 2026-08-04 every other governance decision in the worker — classification, gating, mode transition,
	// config write — was queued behind one secret write with no deadline and no recourse. A channel lets a
	// caller that brought a deadline give up inside its OWN budget (ErrChainBusy) rather than joining an
	// unbounded queue, so one slow substrate write can no longer freeze the whole governance lane.
	gateOnce sync.Once
	gateCh   chan struct{}
}

// gate lazily builds the one-slot chain gate. sync.Once (rather than a constructor) keeps a ZERO-VALUE
// Ledger usable: NewLedger and NewLedgerFromTail both return composite literals that never set it, and a
// nil channel would block every append forever.
func (l *Ledger) gate() chan struct{} {
	l.gateOnce.Do(func() { l.gateCh = make(chan struct{}, 1) })
	return l.gateCh
}

// acquire takes the chain gate, giving up when ctx does. The non-blocking try comes first so an
// UNCONTENDED append never depends on the caller's context still being live — an already-expired context
// must not turn a write that would have succeeded instantly into a refusal.
func (l *Ledger) acquire(ctx context.Context) error {
	g := l.gate()
	select {
	case g <- struct{}{}:
		return nil
	default:
	}
	select {
	case g <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%w: %w", ErrChainBusy, ctx.Err())
	}
}

// hold takes the gate with no deadline — the read paths hold it only long enough to copy a slice.
func (l *Ledger) hold() { l.gate() <- struct{}{} }

// release returns the gate.
func (l *Ledger) release() { <-l.gate() }

// LedgerSink durably persists each appended ledger entry (a pgx-backed governance_ledger writer in
// production). The in-memory chain remains authoritative for the seq/hash computation; the sink is a
// write-through mirror, so a sink failure surfaces as the Append error rather than being silently dropped.
type LedgerSink interface {
	Persist(LedgerEntry) error
}

// LedgerSinkContext is a LedgerSink whose durable write honours the CALLER's deadline. AppendContext
// prefers it; a sink that does not implement it keeps the old unbounded behaviour, so nothing that exists
// today changes.
//
// THE DEFECT (TG-277, measured live on dc1tg01 2026-08-04): the pgx LedgerStore ran its INSERT with
// context.Background(), because Persist's signature carries no deadline to run it under. The write was
// therefore uncancellable. When the substrate stalled, the append sat inside the chain gate until the
// database answered — the calling activity burned its ENTIRE 15s StartToCloseTimeout in step one and
// surfaced a timeout that named nothing, and the operator read an intermittent 503 as "the secret store
// is unreliable". An optional interface rather than a signature change: all existing Append callers
// compile and behave identically.
type LedgerSinkContext interface {
	PersistContext(context.Context, LedgerEntry) error
}

// LedgerTailReader lets a durable Ledger re-read the CURRENT persisted head (seq, hash) so it can recover
// from a seq collision by re-chaining onto whatever a sibling writer committed (TG-549) rather than wedging.
// The pgx LedgerStore satisfies it with the very same Tail reader used to seed the chain at boot. A sink
// that does not implement it keeps the old fail-closed behaviour on a collision, so nothing today regresses.
type LedgerTailReader interface {
	Tail(context.Context) (int64, string, error)
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
	return l.AppendContext(context.Background(), d)
}

// AppendContext is Append under a caller deadline (TG-277). The deadline bounds BOTH halves of the wait
// that a stalled substrate can stretch without limit: queueing for the chain gate, and the durable sink
// write itself (when the sink implements LedgerSinkContext).
//
// It exists because a governed write is executed inside a Temporal activity with a StartToCloseTimeout and
// MaximumAttempts 1. Without the deadline reaching here, a stalled append consumes that entire budget and
// the refusal the operator finally sees names neither the step nor the cause — which is exactly how
// TG-277 came to blame a hash-chain append that measures ~12ms against a ledger at seq ~8800.
func (l *Ledger) AppendContext(ctx context.Context, d GovDecision) (LedgerEntry, error) {
	if d.Decision == "" || d.ActionID == "" {
		return LedgerEntry{}, ErrIncompleteDecision
	}
	if err := l.acquire(ctx); err != nil {
		return LedgerEntry{}, err
	}
	defer l.release()

	// build chains one entry onto the CURRENT cached tail (lastSeq/lastHash). Called once for the in-memory
	// path and once per attempt on the durable path — a re-read updates the tail between calls, so each
	// rebuild re-chains onto the real head.
	build := func() LedgerEntry {
		seq := l.lastSeq + 1
		prev := l.lastHash
		return LedgerEntry{
			Seq:      seq,
			Decision: d.Decision,
			Reason:   d.Reason,
			ActionID: d.ActionID,
			Withheld: d.Withheld,
			PrevHash: prev,
			Hash:     entryHash(seq, d.Decision, d.Reason, d.ActionID, d.Withheld, prev),
		}
	}

	if l.sink == nil {
		// No durable sink: the in-memory ledger IS its own record (Entries()/Verify() read it) and no other
		// writer shares its chain, so there is nothing to collide with. Retain the entry — a sink-backed
		// ledger deliberately does not (the DB is the record), or a long-running worker would leak.
		e := build()
		l.entries = append(l.entries, e)
		l.lastSeq = e.Seq
		l.lastHash = e.Hash
		return e, nil
	}

	// Durable sink: lastSeq/lastHash is only a CACHE of the shared governance_ledger head. A second writer —
	// the sibling worker that seeded the same tail on the same deploy (TG-549), a repair or restamp tool —
	// can advance the durable head under us, so seq = lastSeq+1 lands on an existing row and the INSERT
	// raises 23505, which the sink maps to ErrDuplicateSeq. Before TG-549 that failed closed PERMANENTLY:
	// lastSeq never moved, so every following append retried the same dead seq and the worker's whole
	// governance lane wedged until restart (and a simultaneous restart just re-raced). Recover instead:
	// re-read the real head, re-chain onto it, retry. Only the chain POSITION moves — the audit content is
	// unchanged — so the single global chain stays valid (VerifyChain passes). Bounded, and every attempt
	// honours ctx, so a contended head can neither spin unbounded nor outlive the caller's deadline.
	tr, canRecover := l.sink.(LedgerTailReader)
	var lastErr error
	for attempt := 0; attempt <= maxSeqCollisionRetries; attempt++ {
		e := build()
		err := l.persist(ctx, e)
		if err == nil {
			l.lastSeq = e.Seq
			l.lastHash = e.Hash
			return e, nil
		}
		lastErr = err
		if !errors.Is(err, ErrDuplicateSeq) || !canRecover {
			return LedgerEntry{}, err // not a recoverable collision (or the sink cannot re-read) — fail closed
		}
		ts, th, terr := tr.Tail(ctx)
		if terr != nil {
			return LedgerEntry{}, fmt.Errorf("audit: re-read ledger head after a seq collision: %w", terr)
		}
		l.lastSeq = ts
		l.lastHash = th
	}
	return LedgerEntry{}, fmt.Errorf("audit: governance ledger head stayed contended after %d re-chain attempts: %w",
		maxSeqCollisionRetries, lastErr)
}

// persist mirrors the entry to the durable sink under the caller's deadline when the sink can honour one
// (TG-277), and falls back to the deadline-less Persist otherwise.
func (l *Ledger) persist(ctx context.Context, e LedgerEntry) error {
	if cs, ok := l.sink.(LedgerSinkContext); ok {
		return cs.PersistContext(ctx, e)
	}
	return l.sink.Persist(e)
}

// Len returns the number of appended entries.
func (l *Ledger) Len() int {
	l.hold()
	defer l.release()
	return len(l.entries)
}

// Entries returns a copy of the persisted rows (safe to hand to a verifier or a read model).
func (l *Ledger) Entries() []LedgerEntry {
	l.hold()
	defer l.release()
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
