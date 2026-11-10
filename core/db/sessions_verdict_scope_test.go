package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// THE COMMAND LIST'S VERDICT COLUMN MUST BE PER SESSION.
//
// It read `LEFT JOIN action_verdict v ON v.action_id = a.action_id`. action_id is content-addressed over the
// operation alone (INV-07) and action_verdict is keyed by it, first-wins — so ONE row is stamped onto every
// session that ever proposed that operation. Observed straight off the live Command view on 2026-07-29: three
// sessions sharing action 47d1d005 all reading `match`, three sharing 957f5d4d all reading `deviation`.
//
// This is the first surface an operator sees, and it is the one that makes a failed heal disappear behind an
// earlier success — or brands two healthy runs with one old deviation.
//
// THE TEST IS BUILT SO THE OLD JOIN CANNOT PASS IT: two sessions run the SAME action shape and get DIFFERENT
// outcomes. A single shape-keyed row cannot represent that, so any implementation reading action_verdict fails
// regardless of which row it happens to pick.
//
// Gated on TG_TEST_POSTGRES_DSN (CI provides it).
func TestSessionsListVerdictIsPerSession(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the per-session verdict test")
	}
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer p.Close()

	uniq := fmt.Sprintf("slv-%d", os.Getpid())
	actionID := uniq + "-shape"
	good, bad, unver, never := uniq+"-good", uniq+"-bad", uniq+"-unver", uniq+"-never"
	base := time.Date(2026, 7, 29, 1, 0, 0, 0, time.UTC)

	defer func() {
		for _, q := range []string{
			`DELETE FROM action_execution WHERE action_id = $1`,
			`DELETE FROM action_verdict WHERE action_id = $1`,
		} {
			if _, err := p.Exec(ctx, q, actionID); err != nil {
				t.Errorf("cleanup %q: %v", q, err)
			}
		}
		for _, ref := range []string{good, bad, unver, never} {
			if _, err := p.Exec(ctx, `DELETE FROM session_risk_audit WHERE external_ref = $1`, ref); err != nil {
				t.Errorf("cleanup audit %s: %v", ref, err)
			}
		}
	}()

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := p.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	audit := func(ref string, at time.Time) {
		mustExec(`INSERT INTO session_risk_audit (external_ref, risk_level, band, action_id, schema_version, created_at)
		          VALUES ($1,'low','AUTO',$2,1,$3)`, ref, actionID, at)
	}
	exec := func(ref string, verdict any, unverifiable bool, at time.Time) {
		mustExec(`INSERT INTO action_execution (action_id, external_ref, verdict, unverifiable, target_host, site, schema_version, executed_at)
		          VALUES ($1,$2,$3,$4,'dc1mealie01','dc1',1,$5)`, actionID, ref, verdict, unverifiable, at)
	}

	// The shape's first-ever verdict — the row the old join would have stamped on ALL FOUR sessions.
	mustExec(`INSERT INTO action_verdict (action_id, plan_hash, verdict, target_host, site, schema_version, created_at)
	          VALUES ($1,$2,'match','dc1mealie01','dc1',1,$3)`, actionID, uniq+"-plan", base)

	audit(good, base.Add(1*time.Minute))
	exec(good, "match", false, base.Add(1*time.Minute))

	audit(bad, base.Add(2*time.Minute)) // SAME shape, opposite outcome
	exec(bad, "deviation", false, base.Add(2*time.Minute))

	audit(unver, base.Add(3*time.Minute)) // executed, post-state unreadable (TG-182 fail-closed)
	exec(unver, nil, true, base.Add(3*time.Minute))

	audit(never, base.Add(4*time.Minute)) // proposed the shape, never executed

	rows, err := NewSessionReadStore(p).Recent(ctx, 200)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.ExternalRef] = r.Verdict
	}

	for _, c := range []struct{ ref, want, why string }{
		{good, "match", "this session executed and matched"},
		{bad, "deviation", "this session ran the SAME shape as another and DEVIATED — reporting the other's " +
			"`match` is how a failed heal disappears behind an earlier success"},
		{unver, "unverifiable", "executed but the post-state could not be read (TG-182 fail-closed). It must " +
			"NOT read as a clean verdict, and must not collapse into the same blank as a session that " +
			"never executed"},
		{never, "", "this session never executed, so it has no outcome of its own to report"},
	} {
		if g, ok := got[c.ref]; !ok {
			t.Errorf("session %s missing from the list entirely", c.ref)
		} else if g != c.want {
			t.Errorf("session %s verdict = %q, want %q — %s", c.ref, g, c.want, c.why)
		}
	}
}
