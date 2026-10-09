package audit

import (
	"testing"
	"time"
)

// THE LEDGER'S TIMESTAMP MUST NEVER BE ABLE TO INVALIDATE THE SPINE.
//
// The console's ledger TIME column read blank for every row because governance_ledger.created_at — which has
// existed in the DDL since migration 0003 — was never SELECTed, never carried on the DTO, and was hardcoded
// to "" in the console. Projecting it is a one-line fix. Folding it into the hash would be a catastrophe:
// the persisted chain over every historical row was computed from {seq, decision, reason, action_id,
// withheld, prev_hash} ALONE, so hashing a timestamp now would make VerifyChain report the entire spine as
// tampered — a fail-closed control firing on 4 800+ untampered rows, which is indistinguishable from a real
// compromise at the moment an operator most needs to trust it.
//
// So the load-bearing property is NOT "the time is displayed". It is "the time is inert to the chain".
// These oracles pin that, and the mutation control adds CreatedAt to entryHash and proves they go RED.

func chainOfThree(t *testing.T) *Ledger {
	t.Helper()
	l := NewLedger()
	for _, d := range []GovDecision{
		{Decision: "AUTO", Reason: "graduated op-class", ActionID: "a1"},
		{Decision: "POLL_PAUSE", Reason: "ood-novel-incident", ActionID: "a2", Withheld: true},
		{Decision: "AUTO_NOTICE", Reason: "canary-policy-pinned", ActionID: "a3"},
	} {
		if _, err := l.Append(d); err != nil {
			t.Fatalf("seeding the chain failed: %v", err)
		}
	}
	return l
}

// TestChainVerifiesRegardlessOfCreatedAt is the oracle that protects the historical spine. Storage stamps a
// time on rows the chain was computed without; re-walking must not care what that time says.
func TestChainVerifiesRegardlessOfCreatedAt(t *testing.T) {
	entries := chainOfThree(t).Entries()
	if err := VerifyChain(entries); err != nil {
		t.Fatalf("fixture chain does not verify before any timestamp is applied: %v", err)
	}

	// Storage hands back wildly different clocks — a backfilled row, a zero row, a future row.
	entries[0].CreatedAt = time.Date(2019, 3, 1, 4, 5, 6, 0, time.UTC)
	entries[1].CreatedAt = time.Time{}
	entries[2].CreatedAt = time.Date(2099, 12, 31, 23, 59, 59, 0, time.UTC)

	if err := VerifyChain(entries); err != nil {
		t.Fatalf("VerifyChain rejected an untampered chain because of created_at (%v) — the timestamp is "+
			"NOT one of the hashed fields, so it must be inert here. If this fails, every persisted row "+
			"just became unverifiable", err)
	}
}

// NOTE — a second oracle was written here and DELETED, because it could not fail. It appended two entries,
// assigned different CreatedAt values afterwards, and asserted the hashes matched. But Append computes the
// hash BEFORE the assignment, and CreatedAt is zero at that moment in both cases, so the hashes are equal by
// construction whether or not entryHash reads the field. `go vet` flagged the giveaway (an unused write).
// TestChainVerifiesRegardlessOfCreatedAt above is the real oracle: VerifyChain RECOMPUTES each hash from the
// entry's fields, so a CreatedAt that leaked into entryHash makes the recomputation disagree with the stored
// hash and the chain reports as broken. Assert through the recomputation, never against a string that was
// frozen before the mutation could reach it.

// TestAppendLeavesCreatedAtUnset — the in-memory ledger has no clock of its own, and must not invent one.
// A fabricated timestamp on an unpersisted entry would be indistinguishable from a stored one in the API
// response, which is exactly the "fixture rendered as live" defect this console work exists to remove.
func TestAppendLeavesCreatedAtUnset(t *testing.T) {
	e, err := NewLedger().Append(GovDecision{Decision: "AUTO", Reason: "r", ActionID: "a1"})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if !e.CreatedAt.IsZero() {
		t.Errorf("Append stamped CreatedAt = %v — the in-memory ledger does not persist, so it has no "+
			"storage clock to report; a reader must be able to tell an unstored entry by its zero time",
			e.CreatedAt)
	}
}
