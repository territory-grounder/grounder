package ledgeranchor

import (
	"context"
	"errors"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
)

type fakeChain struct {
	entries []audit.LedgerEntry
	err     error
}

func (f fakeChain) All(context.Context) ([]audit.LedgerEntry, error) { return f.entries, f.err }

func chainOfN(t *testing.T, n int) []audit.LedgerEntry {
	t.Helper()
	l := audit.NewLedger()
	for i := 0; i < n; i++ {
		if _, err := l.Append(audit.GovDecision{Decision: "classify:AUTO", Reason: "test", ActionID: "a"}); err != nil {
			t.Fatal(err)
		}
	}
	return l.Entries()
}

// The scheduled re-walk recomputes every row's CONTENT hash: an edited decision whose stored hash was left
// alone — invisible to the anchor comparisons, which only compare stored (seq, hash) pairs — is caught here.
func TestChainVerifyDetectsAnEditedRowTheAnchorsCannotSee(t *testing.T) {
	entries := chainOfN(t, 5)
	res, err, ok := ChainVerifyJob{Ledger: fakeChain{entries: entries}}.Run(context.Background())
	if err != nil || !ok || res.Broken != nil || res.Rows != 5 {
		t.Fatalf("a clean chain re-hashes clean: res=%+v err=%v ok=%v", res, err, ok)
	}
	tampered := append([]audit.LedgerEntry(nil), entries...)
	tampered[2].Decision = "classify:POLL_PAUSE"
	res, err, ok = ChainVerifyJob{Ledger: fakeChain{entries: tampered}}.Run(context.Background())
	if err != nil || !ok || res.Broken == nil || !errors.Is(res.Broken, audit.ErrChainBroken) {
		t.Fatalf("an edited row with its stored hash intact must read BROKEN: res=%+v err=%v ok=%v", res, err, ok)
	}
	// VerifyAgainstAnchors, by contrast, cannot see it: the stored (seq, hash) pairs are unchanged.
	if aerr := audit.VerifyAgainstAnchors(audit.RowRefsOf(tampered), nil); aerr != nil {
		t.Fatalf("control: the anchor comparison has nothing to compare and passes: %v", aerr)
	}
}

// A chain that cannot be READ is not a clean verdict.
func TestChainVerifyFailsSafeOnAReadError(t *testing.T) {
	_, err, ok := ChainVerifyJob{Ledger: fakeChain{err: errors.New("db down")}}.Run(context.Background())
	if ok || err == nil {
		t.Fatalf("an unreadable chain must report ok=false with the error, got ok=%v err=%v", ok, err)
	}
	if _, err, ok := (ChainVerifyJob{}).Run(context.Background()); ok || err == nil {
		t.Fatal("a job with no reader cannot certify anything")
	}
	// ABSENT IS NOT CLEAR: an empty ledger is unverifiable, never a clean pass — a wipe or a wrong-database
	// restore must not read as "0 rows, all intact".
	if _, err, ok := (ChainVerifyJob{Ledger: fakeChain{}}).Run(context.Background()); ok || !errors.Is(err, ErrEmptyChain) {
		t.Fatalf("an empty chain must be reported unverified (ErrEmptyChain), got ok=%v err=%v", ok, err)
	}
}
