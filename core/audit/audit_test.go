package audit

import (
	"errors"
	"sync"
	"testing"

	"github.com/territory-grounder/grounder/core/safety"
)

func TestLedgerAppendChainsAndVerifies(t *testing.T) {
	l := NewLedger()
	for i, d := range []GovDecision{
		{Decision: "classify:AUTO", Reason: "low", ActionID: "a1"},
		{Decision: "gate:deny", Reason: "no-prediction", ActionID: "a2", Withheld: true},
		{Decision: "verdict:match", Reason: "quiet", ActionID: "a2"},
	} {
		e, err := l.Append(d)
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if e.Seq != int64(i+1) {
			t.Fatalf("seq = %d, want %d", e.Seq, i+1)
		}
	}
	if l.Len() != 3 {
		t.Fatalf("len = %d, want 3", l.Len())
	}
	if err := l.Verify(); err != nil {
		t.Fatalf("intact chain must verify: %v", err)
	}
	// prev-hash linkage
	es := l.Entries()
	if es[0].PrevHash != "" || es[1].PrevHash != es[0].Hash || es[2].PrevHash != es[1].Hash {
		t.Fatalf("prev-hash linkage wrong: %+v", es)
	}
}

func TestLedgerRejectsIncompleteDecision(t *testing.T) {
	l := NewLedger()
	if _, err := l.Append(GovDecision{Decision: "", ActionID: "a1"}); !errors.Is(err, ErrIncompleteDecision) {
		t.Fatalf("empty decision must fail closed, got %v", err)
	}
	if _, err := l.Append(GovDecision{Decision: "x", ActionID: ""}); !errors.Is(err, ErrIncompleteDecision) {
		t.Fatalf("empty action_id must fail closed, got %v", err)
	}
}

func TestVerifyChainDetectsTampering(t *testing.T) {
	l := NewLedger()
	_, _ = l.Append(GovDecision{Decision: "classify:AUTO", Reason: "low", ActionID: "a1"})
	_, _ = l.Append(GovDecision{Decision: "verdict:match", Reason: "quiet", ActionID: "a1"})

	// content tamper
	rows := l.Entries()
	rows[0].Reason = "high" // altered after the fact
	if err := VerifyChain(rows); !errors.Is(err, ErrChainBroken) {
		t.Fatalf("content tamper must break the chain, got %v", err)
	}

	// deletion / reordering
	rows2 := l.Entries()
	rows2 = rows2[1:] // drop the first row → seq/linkage breaks
	if err := VerifyChain(rows2); !errors.Is(err, ErrChainBroken) {
		t.Fatalf("row deletion must break the chain, got %v", err)
	}
}

func TestAppendRiskAuditWritesOneRowAndChains(t *testing.T) {
	l := NewLedger()
	a := RiskAudit{
		ExternalRef:  "TG-1",
		RiskLevel:    "low",
		Band:         safety.BandAuto,
		AutoApproved: true,
		ActionID:     "action-abc",
		PlanHash:     "plan-1",
	}
	stamped, entry, err := AppendRiskAudit(l, a)
	if err != nil {
		t.Fatal(err)
	}
	if l.Len() != 1 {
		t.Fatalf("exactly one row per classification, got %d", l.Len())
	}
	if stamped.SchemaVersion <= 0 {
		t.Fatalf("audit row must be schema-stamped, got %d", stamped.SchemaVersion)
	}
	if stamped.AutoProceedOnTimeout {
		t.Fatalf("auto_proceed_on_timeout must always be false")
	}
	if entry.ActionID != "action-abc" || entry.Withheld {
		t.Fatalf("ledger entry not bound to the action / wrongly withheld: %+v", entry)
	}
	if err := l.Verify(); err != nil {
		t.Fatalf("ledger must verify: %v", err)
	}
}

func TestRiskAuditRejectsIncomplete(t *testing.T) {
	l := NewLedger()
	if _, _, err := AppendRiskAudit(l, RiskAudit{RiskLevel: "low", ActionID: "a"}); !errors.Is(err, ErrIncompleteAudit) {
		t.Fatalf("missing external_ref must fail closed, got %v", err)
	}
}

// A durable sink receives every appended entry (write-through); a FromTail ledger continues the chain, and
// the full sequence still verifies.
func TestLedgerSinkAndFromTail(t *testing.T) {
	var mirrored []LedgerEntry
	sink := sinkFunc(func(e LedgerEntry) error { mirrored = append(mirrored, e); return nil })

	l := NewLedger().WithSink(sink)
	e1, _ := l.Append(GovDecision{Decision: "classify:AUTO", ActionID: "a1"})
	e2, _ := l.Append(GovDecision{Decision: "gate:deny", ActionID: "a2"})
	if len(mirrored) != 2 || mirrored[1].Hash != e2.Hash {
		t.Fatalf("the sink must receive every entry, got %+v", mirrored)
	}

	// a restarted ledger seeded from the tail continues the chain (seq 3, prev = e2.Hash).
	l2 := NewLedgerFromTail(e2.Seq, e2.Hash)
	e3, _ := l2.Append(GovDecision{Decision: "verdict:match", ActionID: "a3"})
	if e3.Seq != 3 || e3.PrevHash != e2.Hash {
		t.Fatalf("a FromTail ledger must continue the chain, got seq=%d prev=%s", e3.Seq, e3.PrevHash)
	}
	// the full persisted chain (mirror + the continuation) verifies as one unbroken chain.
	full := []LedgerEntry{e1, e2, e3}
	if err := VerifyChain(full); err != nil {
		t.Fatalf("the continued chain must verify: %v", err)
	}
}

// A sink error fails the Append closed — the chain does not advance past an unpersisted decision.
func TestLedgerSinkErrorFailsClosed(t *testing.T) {
	l := NewLedger().WithSink(sinkFunc(func(LedgerEntry) error { return errors.New("sink down") }))
	if _, err := l.Append(GovDecision{Decision: "classify:AUTO", ActionID: "a1"}); err == nil {
		t.Fatal("a sink write failure must fail the Append")
	}
	if l.Len() != 0 {
		t.Fatal("the chain must not advance when the durable write failed")
	}
}

type sinkFunc func(LedgerEntry) error

func (f sinkFunc) Persist(e LedgerEntry) error { return f(e) }

// A sink-backed (durable) ledger does NOT retain entries in memory — the DB is the record, so a long-running
// worker cannot leak. An in-memory ledger DOES retain them (it is its own record).
func TestSinkLedgerDoesNotRetainInMemory(t *testing.T) {
	durable := NewLedger().WithSink(sinkFunc(func(LedgerEntry) error { return nil }))
	for i := 0; i < 100; i++ {
		if _, err := durable.Append(GovDecision{Decision: "classify:AUTO", ActionID: "a"}); err != nil {
			t.Fatal(err)
		}
	}
	if durable.Len() != 0 {
		t.Fatalf("a durable ledger must not retain entries in memory (leak), got %d", durable.Len())
	}
	// the chain still advanced correctly (seq 101 next).
	e, _ := durable.Append(GovDecision{Decision: "gate:deny", ActionID: "a"})
	if e.Seq != 101 {
		t.Fatalf("the chain must still advance, got seq %d", e.Seq)
	}

	inmem := NewLedger()
	for i := 0; i < 5; i++ {
		inmem.Append(GovDecision{Decision: "classify:AUTO", ActionID: "a"})
	}
	if inmem.Len() != 5 {
		t.Fatalf("an in-memory ledger must retain its entries, got %d", inmem.Len())
	}
}

// AppendRiskAudit persists the full row through the risk sink (write-through), fails closed on a sink error,
// and still appends the ledger entry on success.
func TestRiskAuditSink(t *testing.T) {
	var rows []RiskAudit
	l := NewLedger().WithRiskSink(riskSinkFunc(func(a RiskAudit) error { rows = append(rows, a); return nil }))
	ra := RiskAudit{ExternalRef: "TG-1", RiskLevel: "low", Band: safety.BandAuto, ActionID: "a1"}
	if _, _, err := AppendRiskAudit(l, ra); err != nil {
		t.Fatalf("append: %v", err)
	}
	if len(rows) != 1 || rows[0].ExternalRef != "TG-1" || rows[0].SchemaVersion == 0 {
		t.Fatalf("the risk sink must receive the STAMPED row, got %+v", rows)
	}
	if l.Len() != 1 {
		t.Fatalf("the ledger entry must still be appended, got %d", l.Len())
	}
	// a sink error fails the whole append closed — no ledger entry.
	l2 := NewLedger().WithRiskSink(riskSinkFunc(func(RiskAudit) error { return errors.New("db down") }))
	if _, _, err := AppendRiskAudit(l2, ra); err == nil {
		t.Fatal("a risk-sink failure must fail the append")
	}
	if l2.Len() != 0 {
		t.Fatal("no ledger entry must be recorded when the risk-audit write failed")
	}
}

type riskSinkFunc func(RiskAudit) error

func (f riskSinkFunc) PersistRiskAudit(a RiskAudit) error { return f(a) }

// ADVERSARIAL (INV-22, concurrent inputs): the ledger is shared across the worker's concurrent Temporal
// activities. Concurrent Append must remain race-free and keep the hash chain monotonic + gap-free with no
// lost records — the chain is inherently sequential, so it must serialize.
func TestLedgerConcurrentAppendIsChainSafe(t *testing.T) {
	l := NewLedger()
	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := l.Append(GovDecision{Decision: "classify:AUTO", ActionID: "a"}); err != nil {
				t.Errorf("concurrent append errored: %v", err)
			}
		}()
	}
	wg.Wait()
	if l.Len() != n {
		t.Fatalf("appends were lost under concurrency: got %d want %d", l.Len(), n)
	}
	if err := l.Verify(); err != nil {
		t.Fatalf("the chain must remain intact under concurrency: %v", err)
	}
}
