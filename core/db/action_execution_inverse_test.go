package db

import (
	"context"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/safety"
)

// TG-404 — the DURABLE half: an executed inverse is an action_execution row that names the forward action it
// undoes (inverts_action_id), so "did the rollback run, and did it succeed?" is a query, not a log-string
// parse. Runs against a REAL Postgres (TG_TEST_DSN) with migration 0071 applied: the whole risk is SQL — the
// column, the NULL-for-forward encoding, and the CHECK — none of which a pgx fake exercises.
//
// The three killing mutations the ticket's guard names live here at the storage layer, because that is where
// "the revert failed" would silently read as safe:
//  1. run an inverse and do not record it        → no row (Record not called) — asserted by the interceptor test
//  2. record without inverts_action_id           → TestForwardIsNullInverseIsSet (a NULL forward is a forward)
//  3. record SUCCESS when the inverse errored     → TestInverseVerdictIsStoredFaithfully (deviation stays deviation)
func TestForwardIsNullInverseIsSet(t *testing.T) {
	ctx := context.Background()
	p, err := Connect(ctx, skipWithoutDB(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()
	st := NewActionExecutionStore(p)
	clean := func() { _, _ = p.Exec(ctx, `DELETE FROM action_execution WHERE action_id LIKE 'tg404-%'`) }
	clean()
	defer clean()

	// A forward action: no inverse reference.
	if err := st.Record(ctx, "tg404-fwd", "inc-1", "host", "site", safety.VerdictMatch, true, ""); err != nil {
		t.Fatalf("record forward: %v", err)
	}
	// Its inverse: same incident, its OWN action_id, naming the forward it undoes.
	if err := st.Record(ctx, "tg404-inv", "inc-1", "host", "site", safety.VerdictMatch, true, "tg404-fwd"); err != nil {
		t.Fatalf("record inverse: %v", err)
	}

	var fwdInverts, invInverts *string
	if err := p.QueryRow(ctx, `SELECT inverts_action_id FROM action_execution WHERE action_id='tg404-fwd'`).Scan(&fwdInverts); err != nil {
		t.Fatalf("read forward row: %v", err)
	}
	if fwdInverts != nil {
		t.Errorf("the FORWARD row carries inverts_action_id=%q, want NULL — a forward action that reads as an "+
			"inverse would corrupt the loop-closure count in both directions", *fwdInverts)
	}
	if err := p.QueryRow(ctx, `SELECT inverts_action_id FROM action_execution WHERE action_id='tg404-inv'`).Scan(&invInverts); err != nil {
		t.Fatalf("read inverse row: %v", err)
	}
	if invInverts == nil || *invInverts != "tg404-fwd" {
		t.Fatalf("the INVERSE row carries inverts_action_id=%v, want \"tg404-fwd\" — without it the inverse is "+
			"indistinguishable from a forward action and 'has an inverse ever run?' stays unanswerable", invInverts)
	}
}

// TestInverseVerdictIsStoredFaithfully — the safety-critical one. "the inverse ran and succeeded" and "the
// inverse ran and FAILED" are opposite outcomes and must never be the same record. A deviation on an inverse
// (the revert did not restore the expected state) must be stored as a deviation, and an unverifiable inverse
// as a withheld (NULL) verdict — never laundered into a match.
func TestInverseVerdictIsStoredFaithfully(t *testing.T) {
	ctx := context.Background()
	p, err := Connect(ctx, skipWithoutDB(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()
	st := NewActionExecutionStore(p)
	clean := func() { _, _ = p.Exec(ctx, `DELETE FROM action_execution WHERE action_id LIKE 'tg404v-%'`) }
	clean()
	defer clean()

	// A FAILED revert: verdict deviation, still marked the inverse of its forward.
	if err := st.Record(ctx, "tg404v-devi", "inc-2", "host", "site", safety.VerdictDeviation, true, "tg404v-fwd"); err != nil {
		t.Fatalf("record deviating inverse: %v", err)
	}
	// A revert whose post-state could not be read: unverifiable, verdict withheld (NULL) — fail-closed (TG-182).
	if err := st.Record(ctx, "tg404v-unv", "inc-2", "host", "site", "", false, "tg404v-fwd"); err != nil {
		t.Fatalf("record unverifiable inverse: %v", err)
	}

	var verdict *string
	var unverifiable bool
	if err := p.QueryRow(ctx, `SELECT verdict, unverifiable FROM action_execution WHERE action_id='tg404v-devi'`).Scan(&verdict, &unverifiable); err != nil {
		t.Fatalf("read deviating inverse: %v", err)
	}
	if verdict == nil || *verdict != string(safety.VerdictDeviation) {
		t.Errorf("a FAILED revert stored verdict=%v, want deviation — the one outcome that must never read as "+
			"safe is a revert that did not work reading as a clean one", verdict)
	}
	if err := p.QueryRow(ctx, `SELECT verdict, unverifiable FROM action_execution WHERE action_id='tg404v-unv'`).Scan(&verdict, &unverifiable); err != nil {
		t.Fatalf("read unverifiable inverse: %v", err)
	}
	if verdict != nil || !unverifiable {
		t.Errorf("an unverifiable revert stored verdict=%v unverifiable=%v, want NULL/true — 'we reverted and "+
			"could not check' must be honest, not a false match", verdict, unverifiable)
	}
}

// TestBlankInverseReferenceIsRejected — the encoding invariant. A present-but-blank inverts_action_id would
// mark a row an inverse of NOTHING, a shape that neither a forward (NULL) nor a real inverse should ever
// take. The store maps "" to NULL, and the CHECK constraint rejects a whitespace-only value inserted around
// it, so the column is only ever NULL or a real reference.
func TestBlankInverseReferenceIsRejected(t *testing.T) {
	ctx := context.Background()
	p, err := Connect(ctx, skipWithoutDB(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()
	clean := func() { _, _ = p.Exec(ctx, `DELETE FROM action_execution WHERE action_id LIKE 'tg404b-%'`) }
	clean()
	defer clean()

	// The store maps a blank reference to NULL (a forward), never a blank string.
	if err := NewActionExecutionStore(p).Record(ctx, "tg404b-store", "inc-3", "host", "site", safety.VerdictMatch, true, "   "); err != nil {
		t.Fatalf("record with blank inverts: %v", err)
	}
	var inverts *string
	if err := p.QueryRow(ctx, `SELECT inverts_action_id FROM action_execution WHERE action_id='tg404b-store'`).Scan(&inverts); err != nil {
		t.Fatalf("read: %v", err)
	}
	if inverts != nil {
		t.Errorf("a blank inverse reference was stored as %q, not NULL — the store must not mint an inverse of nothing", *inverts)
	}

	// And a raw INSERT that bypasses the store must be refused by the CHECK, so the invariant does not rest on
	// the writer alone.
	_, rawErr := p.Exec(ctx, `INSERT INTO action_execution (action_id, external_ref, verdict, target_host, site, inverts_action_id)
		VALUES ('tg404b-raw', 'inc-3', 'match', 'host', 'site', '   ')`)
	if rawErr == nil {
		t.Error("the CHECK constraint accepted a whitespace-only inverts_action_id — a blank reference is neither " +
			"a forward (NULL) nor a real inverse, and the column must not hold it")
	} else if !strings.Contains(strings.ToLower(rawErr.Error()), "check") && !strings.Contains(strings.ToLower(rawErr.Error()), "constraint") {
		t.Errorf("the raw blank insert failed for the wrong reason (%v) — expected the CHECK constraint to reject it", rawErr)
	}
}

// TG-404 close-review follow-through: inverts_action_id is a content-addressed hash SHARED by every
// session proposing the identical action, so the sanctioned read demands the external_ref too — this test
// is the TG-142-class killing mutation: drop the `AND external_ref = $2` clause from InversesOf and the
// sibling incident's rollback appears in this incident's answer (want 1 row, would get 2).
func TestInversesOfScopesToTheIncident(t *testing.T) {
	ctx := context.Background()
	p, err := Connect(ctx, skipWithoutDB(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()
	st := NewActionExecutionStore(p)
	clean := func() { _, _ = p.Exec(ctx, `DELETE FROM action_execution WHERE action_id LIKE 'tg404s-%'`) }
	clean()
	defer clean()

	// Two incidents run the IDENTICAL forward action and the IDENTICAL rollback — same content hashes,
	// different external_refs. That is the live shape (TG-142's measurement: one hash, many incidents).
	for _, ref := range []string{"tg404s-incA", "tg404s-incB"} {
		if err := st.Record(ctx, "tg404s-fwd", ref, "host", "site", safety.VerdictMatch, true, ""); err != nil {
			t.Fatalf("record forward %s: %v", ref, err)
		}
		if err := st.Record(ctx, "tg404s-inv", ref, "host", "site", safety.VerdictMatch, true, "tg404s-fwd"); err != nil {
			t.Fatalf("record inverse %s: %v", ref, err)
		}
	}

	got, err := st.InversesOf(ctx, "tg404s-fwd", "tg404s-incA")
	if err != nil {
		t.Fatalf("InversesOf: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("InversesOf returned %d rows, want exactly 1 — more means the sibling incident's rollback "+
			"leaked across the content-hash (the TG-142 collision this read exists to prevent)", len(got))
	}
	if got[0].ExternalRef != "tg404s-incA" || got[0].ActionID != "tg404s-inv" {
		t.Fatalf("wrong row came back: %+v", got[0])
	}

	// A hash-only lookup is REFUSED, never answered: the refusal must not share a state with found-nothing.
	if _, err := st.InversesOf(ctx, "tg404s-fwd", ""); err == nil {
		t.Fatal("InversesOf with an empty external_ref answered instead of refusing — a hash-only lookup crosses incidents")
	}
	if _, err := st.InversesOf(ctx, "", "tg404s-incA"); err == nil {
		t.Fatal("InversesOf with an empty forward action_id answered instead of refusing")
	}

	// Found-nothing is its own honest state: a forward with no recorded inverse returns empty WITHOUT error.
	none, err := st.InversesOf(ctx, "tg404s-never-inverted", "tg404s-incA")
	if err != nil {
		t.Fatalf("InversesOf on a never-inverted action errored: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("expected no rows for a never-inverted action, got %d", len(none))
	}
}
