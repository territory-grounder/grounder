package db

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/holds"
	"github.com/territory-grounder/grounder/core/risk"
)

type controlFixture struct {
	p   *Pool
	ctx context.Context
	run string
	now time.Time
}

func openControlFixture(t *testing.T) controlFixture {
	t.Helper()
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to an empty database to run the control-window reads")
	}
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(p.Close)
	return controlFixture{p: p, ctx: ctx, now: time.Now().UTC(), run: fmt.Sprintf("tg700-%d", time.Now().UnixNano())}
}

func (f controlFixture) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := f.p.Exec(f.ctx, q, args...); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func (f controlFixture) session(t *testing.T, ref, host, outcome string, at time.Time) {
	t.Helper()
	f.exec(t, `INSERT INTO session_triage (external_ref, host, alert_rule, band, op, op_class, proposed, predicted, outcome, conclusion, step_count, decision_ms, mutated, confirmed_clear, actor_attribution, harness_fp, created_at)
		VALUES ($1,$2,'Service up/down','POLL_PAUSE','start','start-service',true,false,$3,'c',1,1,false,false,'','',$4)`, ref, host, outcome, at)
}

func (f controlFixture) window(t *testing.T, id, host string, from, to time.Time) {
	t.Helper()
	f.exec(t, `INSERT INTO control_window (control_id, host, starts_at, ends_at) VALUES ($1,$2,$3,$4)`, id, host, from, to)
}

func TestAControlWindowHidesItsSessionsFromEveryLearningReader(t *testing.T) {
	f := openControlFixture(t)
	guest, other := f.run+"-guest", f.run+"-other"
	inside, before, elsewhere := f.now.Add(-20*time.Minute), f.now.Add(-2*time.Hour), f.now.Add(-20*time.Minute)
	f.window(t, f.run+"-N3", guest, f.now.Add(-30*time.Minute), f.now.Add(-5*time.Minute))
	refs := map[string][2]any{
		f.run + "-control": {guest, inside},
		f.run + "-earlier": {guest, before},
		f.run + "-sibling": {other, elsewhere},
		f.run + "-later":   {guest, f.now.Add(-2 * time.Minute)},
	}
	for ref, v := range refs {
		f.session(t, ref, v[0].(string), "escalated:handoff", v[1].(time.Time))
	}
	has := func(got []string, want string) bool {
		for _, g := range got {
			if g == want {
				return true
			}
		}
		return false
	}
	check := func(t *testing.T, name string, got []string, control, earlier string) {
		t.Helper()
		if has(got, control) {
			t.Errorf("%s still returns the control session %s: %v", name, control, got)
		}
		if earlier != "" && !has(got, earlier) {
			t.Errorf("%s lost the ordinary session %s: %v", name, earlier, got)
		}
	}

	t.Run("prior sessions", func(t *testing.T) {
		rows, err := NewIncidentHistoryStore(f.p).PriorSessions(f.ctx, guest, 10)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, r := range rows {
			got = append(got, r.ExternalRef)
		}
		check(t, "PriorSessions", got, f.run+"-control", f.run+"-earlier")
		if !has(got, f.run+"-later") {
			t.Errorf("a session after the window must stay readable: %v", got)
		}
	})

	t.Run("conversation memory", func(t *testing.T) {
		key := "svc|" + guest
		store := NewConversationStore(f.p)
		for _, ref := range []string{f.run + "-control", f.run + "-earlier"} {
			if err := store.Append(f.ctx, key, ref, "digest "+ref, time.Hour); err != nil {
				t.Fatal(err)
			}
		}
		turns, err := store.Recent(f.ctx, key, "", 10)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, r := range turns {
			got = append(got, r.ExternalRef)
		}
		check(t, "Recent", got, f.run+"-control", f.run+"-earlier")
	})

	t.Run("escalation lessons", func(t *testing.T) {
		got, err := NewNotableIncidentStore(f.p, "skill", "dim", 200).NotableIncidents(f.ctx, 3*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		var refs []string
		for _, n := range got {
			refs = append(refs, n.ExternalRef)
		}
		check(t, "NotableIncidents", refs, f.run+"-control", f.run+"-earlier")
	})

	t.Run("caution lessons", func(t *testing.T) {
		for _, ref := range []string{f.run + "-control", f.run + "-earlier"} {
			f.exec(t, `INSERT INTO session_judgment (external_ref, dimension, score, comment, schema_version, rubric_version) VALUES ($1,'tg700dim',1,'low',1,'t')`, ref)
		}
		got, err := NewCautionCommentStore(f.p, "skill", "tg700dim", 2, 200).NotableIncidents(f.ctx, 3*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		var refs []string
		for _, n := range got {
			refs = append(refs, n.ExternalRef)
		}
		check(t, "CautionComments", refs, f.run+"-control", f.run+"-earlier")
	})

	t.Run("experience lessons", func(t *testing.T) {
		for _, ref := range []string{f.run + "-control", f.run + "-earlier"} {
			f.exec(t, `INSERT INTO session_experience (external_ref, host, alert_rule, verdict, cue, lesson, evidence, confidence, moments, model) VALUES ($1,$2,'r','v','c',$3,'e','high','[]','m')`, ref, guest, "lesson "+ref)
		}
		got, err := NewExperienceLessonSource(f.p, "skill", "dim", "high", 200).NotableIncidents(f.ctx, 3*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		var refs []string
		for _, n := range got {
			refs = append(refs, n.ExternalRef)
		}
		check(t, "ExperienceLessons", refs, f.run+"-control", f.run+"-earlier")
	})

	t.Run("the skill flywheel pool", func(t *testing.T) {
		if !strings.Contains(flywheelPoolCTE, "control_window") {
			t.Fatal("the flywheel pool must leave control sessions out")
		}
	})
}

func TestAControlHoldDoesNotHoldLaterFaultsButStaysVisible(t *testing.T) {
	f := openControlFixture(t)
	store := NewHoldStore(f.p)
	guest, rule := f.run+"-guest", "Service up/down"
	mk := func(ref string, at time.Time) holds.HoldDecision {
		return holds.HoldDecision{ExternalRef: ref, Host: guest, AlertRule: rule, ReasonKey: "server-derived-destructive-op", DecidedAt: at, ExpiresAt: f.now.Add(23 * time.Hour)}
	}
	f.window(t, f.run+"-N3", guest, f.now.Add(-30*time.Minute), f.now.Add(-5*time.Minute))
	if err := store.Record(f.ctx, mk(f.run+"-control", f.now.Add(-20*time.Minute))); err != nil {
		t.Fatal(err)
	}
	if _, open, err := store.OpenFor(f.ctx, guest, rule, f.now); err != nil || open {
		t.Fatalf("a control's hold must not hold later faults: open=%v err=%v", open, err)
	}
	listed, err := store.OpenHolds(f.ctx, f.now)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range listed {
		if h.ExternalRef == f.run+"-control" {
			found = true
		}
	}
	if !found {
		t.Fatal("a control's hold must stay visible to operators")
	}
	if err := store.Record(f.ctx, mk(f.run+"-real", f.now.Add(-2*time.Minute))); err != nil {
		t.Fatal(err)
	}
	if h, open, err := store.OpenFor(f.ctx, guest, rule, f.now); err != nil || !open || h.ExternalRef != f.run+"-real" {
		t.Fatalf("a real hold outside the window must still hold: %+v open=%v err=%v", h, open, err)
	}
}

func controlWindowWriteAttempts() []struct{ sql, what string } {
	return []struct{ sql, what string }{
		{`INSERT INTO control_window (control_id, host, starts_at, ends_at) VALUES ('tg706-forged', 'tg706-forged-host', now() - interval '1 day', now() + interval '1 day')`,
			"a worker role opened a control window, which hides real holds on that host from the prior-hold check"},
		{`UPDATE control_window SET ends_at = ends_at + interval '30 days' WHERE control_id = 'tg706-seed'`,
			"a worker role widened a registered control window over later holds"},
		{`DELETE FROM control_window WHERE control_id = 'tg706-seed'`,
			"a worker role deleted a registered control window"},
		{`TRUNCATE control_window`,
			"a worker role emptied the control window register"},
	}
}

func seedControlWindowRoles(t *testing.T, ctx context.Context, p *Pool) []string {
	t.Helper()
	const inheriting = "tg706_inherits_runtime"
	for _, stmt := range []string{
		`INSERT INTO control_window (control_id, host, starts_at, ends_at) VALUES ('tg706-seed', 'tg706-seed-host', now(), now() + interval '1 minute') ON CONFLICT (control_id) DO NOTHING`,
		"DO $do$ BEGIN CREATE ROLE " + pgIdent(inheriting) + " NOLOGIN INHERIT; EXCEPTION WHEN duplicate_object THEN NULL; END $do$;",
		"GRANT " + pgIdent(PlaneRoleRuntime) + " TO " + pgIdent(inheriting),
	} {
		if _, err := p.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	return []string{PlaneRoleRuntime, PlaneRoleTriage, PlaneRoleActuate, inheriting}
}

func assertNoControlWindowWrites(t *testing.T, ctx context.Context, p *Pool, roles []string) {
	t.Helper()
	for _, role := range roles {
		t.Run(role, func(t *testing.T) {
			conn := asRole(t, ctx, p, role)
			defer conn.Exec(ctx, "RESET ROLE")
			var n int
			if err := conn.QueryRow(ctx, "SELECT count(*) FROM control_window").Scan(&n); err != nil {
				t.Fatalf("%s must still read control windows or the learning and hold reads break: %v", role, err)
			}
			if n == 0 {
				t.Fatal("the seeded window is not visible, so UPDATE and DELETE below would prove nothing")
			}
			for _, priv := range []string{"INSERT", "UPDATE", "DELETE", "TRUNCATE"} {
				var has bool
				if err := conn.QueryRow(ctx, "SELECT has_table_privilege(current_user, 'control_window', $1)", priv).Scan(&has); err != nil {
					t.Fatalf("read %s privilege: %v", priv, err)
				}
				if has {
					t.Errorf("%s holds %s on control_window, directly or by inheritance", role, priv)
				}
			}
			for _, a := range controlWindowWriteAttempts() {
				_, err := conn.Exec(ctx, a.sql)
				denied(t, err, a.what)
			}
		})
	}
}

func TestNoWorkerRoleCanCreateOrChangeAControlWindow(t *testing.T) {
	ctx, p, _ := planeRoleFixture(t)
	assertNoControlWindowWrites(t, ctx, p, seedControlWindowRoles(t, ctx, p))
}

func TestGrantMirroringCannotReopenAControlWindow(t *testing.T) {
	ctx, p, dsn := planeRoleFixture(t)
	roles := seedControlWindowRoles(t, ctx, p)
	if _, err := ApplyPlaneGrants(ctx, dsn); err != nil {
		t.Fatalf("apply plane grants: %v", err)
	}
	assertNoControlWindowWrites(t, ctx, p, roles)

	if _, err := p.Exec(ctx, "GRANT INSERT, UPDATE, DELETE ON control_window TO "+pgIdent(PlaneRoleRuntime)); err != nil {
		t.Fatalf("mutate tg_runtime: %v", err)
	}
	t.Cleanup(func() {
		if _, err := p.Exec(ctx, "REVOKE INSERT, UPDATE, DELETE ON control_window FROM "+pgIdent(PlaneRoleRuntime)); err != nil {
			t.Errorf("restore tg_runtime: %v", err)
		}
	})
	if _, err := ApplyPlaneGrants(ctx, dsn); err != nil {
		t.Fatalf("apply plane grants after the mutation: %v", err)
	}
	assertNoControlWindowWrites(t, ctx, p, []string{PlaneRoleTriage, PlaneRoleActuate})
}

func TestEachWorkerPlaneReportsControlWindowsAsWithheld(t *testing.T) {
	ctx, _, dsn := planeRoleFixture(t)
	for _, tc := range []struct {
		role     string
		withheld []string
	}{
		{PlaneRoleTriage, WithheldFromTriage()},
		{PlaneRoleActuate, WithheldFromActuation()},
	} {
		a, err := roleAudit(ctx, dsn, tc.role, tc.withheld)
		if err != nil {
			t.Fatalf("audit %s: %v", tc.role, err)
		}
		found := false
		for _, tbl := range a.Checked {
			found = found || tbl == "control_window"
		}
		if !found {
			t.Errorf("%s boot audit did not examine control_window: %v", tc.role, a.Checked)
		}
		if !a.Split() {
			t.Errorf("%s can still write off-plane tables: %v", tc.role, a.Writable)
		}
	}
}

func TestNoDatabaseObjectGivesAWorkerAPathToControlWindows(t *testing.T) {
	ctx, p, _ := planeRoleFixture(t)
	for _, c := range []struct{ what, sql string }{
		{"function", `SELECT coalesce(string_agg(p.oid::regprocedure::text, ','), '') FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = 'public' AND p.prosrc ILIKE '%control_window%'`},
		{"trigger", `SELECT coalesce(string_agg(tgname, ','), '') FROM pg_trigger WHERE NOT tgisinternal AND tgrelid = 'control_window'::regclass`},
		{"rule", `SELECT coalesce(string_agg(rulename, ','), '') FROM pg_rules WHERE schemaname = 'public' AND (tablename = 'control_window' OR definition ILIKE '%control_window%')`},
		{"view", `SELECT coalesce(string_agg(viewname, ','), '') FROM pg_views WHERE schemaname = 'public' AND definition ILIKE '%control_window%'`},
	} {
		var got string
		if err := p.QueryRow(ctx, c.sql).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", c.what, err)
		}
		if got != "" {
			t.Errorf("a %s references control_window and could write it for a worker: %s", c.what, got)
		}
	}
}

func TestAWorkerCannotHideAPriorHoldWithAControlWindow(t *testing.T) {
	ctx, p, _ := planeRoleFixture(t)
	roles := seedControlWindowRoles(t, ctx, p)
	host := fmt.Sprintf("tg706-%d", time.Now().UnixNano())
	const rule = "Device-Down"
	decided := time.Now().UTC().Add(-40 * time.Minute)
	store := NewHoldStore(p)
	if err := store.Record(ctx, holds.HoldDecision{ExternalRef: host + "-ref", Host: host, AlertRule: rule, OpClass: "start-guest",
		ReasonKey: "actor-attributed-authorized", DecidedBy: "tg:fallback-approver", DecidedAt: decided, ExpiresAt: decided.Add(24 * time.Hour)}); err != nil {
		t.Fatalf("record the hold: %v", err)
	}
	classify := func(t *testing.T) (bool, string) {
		t.Helper()
		h, found, err := store.OpenFor(ctx, host, rule, time.Now().UTC())
		if err != nil {
			t.Fatalf("read the hold: %v", err)
		}
		in := risk.GatedInput{OpClass: "start-guest", Reversible: risk.Reversible, HasPrediction: true, PriorHoldOpen: found}
		if found {
			in.PriorHoldDetail = holds.Detail(h)
		}
		return found, risk.Classify(in).Signals["poll_reason"]
	}
	if found, reason := classify(t); !found || reason != holds.ReasonPriorHold {
		t.Fatalf("the hold must be open and decide the classification before any window exists: found=%v reason=%q", found, reason)
	}
	const window = `INSERT INTO control_window (control_id, host, starts_at, ends_at) VALUES ($1, $2, $3, $4)`
	from, to := decided.Add(-time.Minute), decided.Add(time.Minute)
	for _, role := range roles {
		func() {
			conn := asRole(t, ctx, p, role)
			defer conn.Exec(ctx, "RESET ROLE")
			_, err := conn.Exec(ctx, window, host+"-"+role, host, from, to)
			denied(t, err, role+" registered a window over a real hold")
		}()
		if found, reason := classify(t); !found || reason != holds.ReasonPriorHold {
			t.Fatalf("after %s tried to register a window the hold must still decide the classification: found=%v reason=%q", role, found, reason)
		}
	}
	if _, err := p.Exec(ctx, window, host+"-runner", host, from, to); err != nil {
		t.Fatalf("the control runner must still be able to register a window: %v", err)
	}
	if found, reason := classify(t); found || reason == holds.ReasonPriorHold {
		t.Fatalf("a window registered by the control runner must still apply, or this test does not measure the window: found=%v reason=%q", found, reason)
	}
}
