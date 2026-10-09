package db

// ORACLES FOR EXACTLY-ONCE LADDER CREDIT (TG-266, REQ-2804). Against a REAL Postgres: the whole mechanism
// IS the unique constraint plus ON CONFLICT DO NOTHING, and a fake would happily return whatever it was
// told. Migration 0050 also REVOKEs UPDATE/DELETE on this table, so the append-only property is the
// database's, not this code's — only a real connection can show that.

import (
	"context"
	"errors"
	"testing"
)

func creditFixture(ctx context.Context, t *testing.T) (*GraduationCreditStore, func()) {
	t.Helper()
	dsn := skipWithoutDB(t)
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	clean := func() {
		_, _ = p.Exec(ctx, `DELETE FROM graduation_credit WHERE op_class LIKE 'gold-credit-%'`)
		_, _ = p.Exec(ctx, `DELETE FROM action_execution WHERE action_id LIKE 'gold-credit-exec-%'`)
	}
	clean()
	// GROUND THE FIXTURE'S INCIDENTS (TG-321, migration 0064). A credit now requires an action_execution row
	// for its external_ref, so these rows are what make the EXACTLY-ONCE tests test exactly-once rather than
	// re-testing the grounding trigger. Every ref these tests claim against is seeded here; a ref that is
	// deliberately UNgrounded is not, and TestCreditIsRefusedWithoutARecordedExecution relies on that.
	for _, ref := range []string{"incident-1", "incident-2", "incident-x", "incident-y"} {
		if _, err := p.Exec(ctx, `
			INSERT INTO action_execution (action_id, external_ref, verdict, target_host, site)
			VALUES ($1, $2, 'match', 'gold-host', 'gold')`,
			"gold-credit-exec-"+ref, ref); err != nil {
			t.Fatalf("seed action_execution for %s: %v", ref, err)
		}
	}
	return NewGraduationCreditStore(p), func() { clean(); p.Close() }
}

// THE CREDIT MUST NAME A RUN THAT ACTUALLY HAPPENED (TG-321).
//
// The ladder decides which op-classes may mutate the estate without a human vote, and both of its writers
// run on the TRIAGE queue — so the credential plane split has to grant the triage plane INSERT here. A
// compromised triage worker therefore cannot execute an action, but could credit one and promote the class,
// and the promotion changes what a LATER, legitimate proposal is allowed to do unattended.
//
// KILLING MUTATION: drop the graduation_credit_grounded trigger (migration 0064's down file does exactly
// this). RED — an op-class reaches `auto` on a run that never occurred.
//
// Against a REAL Postgres deliberately: the control IS the trigger. A fake would return whatever it was told
// and would prove that the test author believes in the trigger, which is not the same as it existing.
func TestCreditIsRefusedWithoutARecordedExecution(t *testing.T) {
	ctx := context.Background()
	st, done := creditFixture(ctx, t)
	defer done()

	// "incident-never-ran" is deliberately NOT seeded in creditFixture.
	claimed, err := st.Claim(ctx, "gold-credit-forged", "incident-never-ran", "verified_clean")
	if claimed {
		t.Fatal("credit was granted for an incident with NO action_execution row — a triage-plane writer " +
			"can advance an op-class toward `auto` on a run that never happened")
	}
	if !errors.Is(err, ErrCreditUngrounded) {
		t.Fatalf("the refusal did not surface as ErrCreditUngrounded, so a caller cannot tell a permanent, "+
			"correct refusal from a transient store outage and will retry it forever: %v", err)
	}
	// NEGATIVE CONTROL. The same claim against a GROUNDED incident must succeed — otherwise the refusal
	// above proves only that Claim is broken, not that the grounding check is what refused.
	ok, err := st.Claim(ctx, "gold-credit-forged", "incident-1", "verified_clean")
	if err != nil {
		t.Fatalf("a grounded claim errored: %v", err)
	}
	if !ok {
		t.Fatal("a grounded claim was also refused — the test above demonstrates nothing about grounding")
	}
	// And the refused row must not be sitting in the table.
	var n int
	if err := st.p.QueryRow(ctx, `SELECT count(*) FROM graduation_credit WHERE external_ref = 'incident-never-ran'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("the refused credit was written anyway (%d row(s)) — the trigger reported an error and did "+
			"not prevent the insert", n)
	}
}

// KILLING MUTATION: drop the ON CONFLICT clause (a duplicate then errors instead of reporting
// not-claimed), or return true unconditionally. RED — the second claim is the replayed session that
// would otherwise take a second step toward autonomy from one incident.
func TestCreditIsClaimableExactlyOncePerIncident(t *testing.T) {
	ctx := context.Background()
	st, done := creditFixture(ctx, t)
	defer done()

	first, err := st.Claim(ctx, "gold-credit-restart", "incident-1", "verified_clean")
	if err != nil {
		t.Fatal(err)
	}
	if !first {
		t.Fatal("the FIRST claim was refused — a real verified-clean run must credit")
	}
	second, err := st.Claim(ctx, "gold-credit-restart", "incident-1", "verified_clean")
	if err != nil {
		t.Fatalf("a repeat claim errored instead of reporting not-claimed: %v", err)
	}
	if second {
		t.Fatal("the SAME incident credited twice — one incident would advance the class two runs toward " +
			"autonomy (REQ-2804)")
	}
}

// KILLING MUTATION: key the claim on op_class alone (or on external_ref alone). RED both ways — the first
// would let one incident block every other class, the second would let one class block every incident.
func TestTheClaimKeyIsThePairNotEitherHalf(t *testing.T) {
	ctx := context.Background()
	st, done := creditFixture(ctx, t)
	defer done()

	if ok, err := st.Claim(ctx, "gold-credit-a", "incident-x", "verified_clean"); err != nil || !ok {
		t.Fatalf("baseline claim: ok=%v err=%v", ok, err)
	}
	// same incident, DIFFERENT class — a second class genuinely earned a run from this incident
	if ok, err := st.Claim(ctx, "gold-credit-b", "incident-x", "verified_clean"); err != nil || !ok {
		t.Fatalf("a different op-class was blocked by another class's credit for the same incident: ok=%v err=%v", ok, err)
	}
	// same class, DIFFERENT incident — the next real run
	if ok, err := st.Claim(ctx, "gold-credit-a", "incident-y", "verified_clean"); err != nil || !ok {
		t.Fatalf("a new incident was blocked by an earlier credit for the same class: ok=%v err=%v", ok, err)
	}
}

// KILLING MUTATION: accept an empty key half (claim with opClass="" or ref=""). RED — an empty key would
// collapse every unkeyed run onto one row, so the FIRST unkeyed credit would silently block all others.
func TestAnEmptyKeyHalfIsRefusedRatherThanCredited(t *testing.T) {
	ctx := context.Background()
	st, done := creditFixture(ctx, t)
	defer done()

	if ok, err := st.Claim(ctx, "", "incident-1", "verified_clean"); ok || err == nil {
		t.Fatalf("empty op_class: ok=%v err=%v — want refused with an error", ok, err)
	}
	if ok, err := st.Claim(ctx, "gold-credit-c", "", "verified_clean"); ok || err == nil {
		t.Fatalf("empty external_ref: ok=%v err=%v — want refused with an error", ok, err)
	}
}

// The append-only property is the DATABASE's: migration 0050 revokes UPDATE and DELETE from tg_runtime
// ("credit that can be rewritten is not credit"). This pins that the claim path uses an INSERT form that
// survives those grants — a DO UPDATE rewrite would fail against the production role.
//
// KILLING MUTATION: change ON CONFLICT DO NOTHING to DO UPDATE. RED under tg_runtime's grants.
func TestTheClaimUsesAnAppendOnlyForm(t *testing.T) {
	ctx := context.Background()
	st, done := creditFixture(ctx, t)
	defer done()

	if _, err := st.Claim(ctx, "gold-credit-append", "incident-1", "verified_clean"); err != nil {
		t.Fatal(err)
	}
	// A second claim must be a no-op INSERT, never an UPDATE of the stored outcome.
	if _, err := st.Claim(ctx, "gold-credit-append", "incident-1", "unverified"); err != nil {
		t.Fatalf("repeat claim errored: %v", err)
	}
	var outcome string
	if err := st.p.QueryRow(ctx, `SELECT outcome FROM graduation_credit
		WHERE op_class = $1 AND external_ref = $2`, "gold-credit-append", "incident-1").Scan(&outcome); err != nil {
		t.Fatal(err)
	}
	if outcome != "verified_clean" {
		t.Fatalf("stored outcome is %q — the repeat claim REWROTE the original credit; credit that can be "+
			"rewritten is not credit (migration 0050)", outcome)
	}
}

// TG-261: the un-set. KILLING MUTATION: make Delete a no-op returning true, or drop it. RED — an operator
// who saved a wrong-but-valid value would have no way back except surgery on Postgres, and after TG-260
// made the worker READ these rows that is a live trap.
func TestAConfigOverrideCanBeTakenBack(t *testing.T) {
	ctx := context.Background()
	dsn := skipWithoutDB(t)
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	st := NewCPConfigStore(p)
	const key = "module.gold.clear.field"
	_, _ = p.Exec(ctx, `DELETE FROM control_plane_config WHERE key = $1`, key)
	defer func() { _, _ = p.Exec(ctx, `DELETE FROM control_plane_config WHERE key = $1`, key) }()

	if err := st.Upsert(ctx, key, "37s", "why", "tester", 1, 1); err != nil {
		t.Fatal(err)
	}
	ov, err := st.Overrides(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ov[key] != "37s" {
		t.Fatalf("fixture: override not stored (%q)", ov[key])
	}

	existed, err := st.Delete(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if !existed {
		t.Fatal("Delete reported no row removed for a key that was just written")
	}
	ov, err = st.Overrides(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, still := ov[key]; still {
		t.Fatal("the override survived the clear — absence IS what 'no override' means, so the resolver " +
			"must now fall through to env")
	}

	// Clearing an absent key is a reported no-op, never an error: the end state the operator asked for
	// is the end state they have.
	again, err := st.Delete(ctx, key)
	if err != nil {
		t.Fatalf("clearing an absent key errored: %v", err)
	}
	if again {
		t.Fatal("clearing an absent key claimed to remove a row")
	}
}
