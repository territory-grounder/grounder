package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/httpapi"
)

// TestRecoveryReconcilesOpenSessionsAndProposals drives the REAL pgx ingest path (TransitionLogStore.Append)
// and proves TG-387: a captured recovery transition is a TERMINAL signal that, at ingest time and independent
// of any live workflow, closes the incident's still-open triage sessions and obsoletes its still-open
// POLL_PAUSE proposals — matched on the NATURAL KEY (host, alert_rule) through the shared rule-family
// authority, because external_ref can never match a recovery to its fault (LibreNMS re-mints the alert id on
// clear). It asserts the four contract properties:
//
//	(1) a recovery CLOSES a matching open session (confirmed_clear flips true, the closing transition id is
//	    recorded) and OBSOLETES a matching open proposal — including a family-sibling label (the pveliveness
//	    "Device-Down" incident a "Devices-up/down" recovery must still close);
//	(2) a recovery for a DIFFERENT (host, alert_rule) leaves the session OPEN — no false close, on either a
//	    different host or a rule in a different family;
//	(3) a recovery does NOT close a session whose incident is NEWER than the recovery (the timestamp guard);
//	(4) IDEMPOTENCY — re-applying the same recovery changes 0 rows (no re-close, no re-stamp, no re-resolve).
//
// Gated on TG_TEST_POSTGRES_DSN (an empty database; it calls Migrate() itself), like the other core/db oracles.
// RED-confirm: neutralise the reconcileRecovery call in Append and property (1) fails — the session stays open.
func TestRecoveryReconcilesOpenSessionsAndProposals(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to an empty database to run the recovery-reconcile oracle")
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
	s := NewTransitionLogStore(p)

	// pid-namespaced so parallel worktrees on the shared box never collide.
	pid := os.Getpid()
	host := fmt.Sprintf("tg387-host-%d", pid)
	otherHost := host + "-other"
	const recoveryRule = "Devices-up/down" // in the device-down family (rulefamily.json)
	const familyRule = "Device-Down"       // pveliveness's OWN label — same family, must still be closed
	const unrelatedRule = "DiskSpaceLow"   // NOT in the device-down family — a genuinely different condition

	defer func() {
		refLike := fmt.Sprintf("tg387-%d-%%", pid)
		_, _ = p.Exec(ctx, "DELETE FROM ingest_transition WHERE host = $1 OR host = $2", host, otherHost)
		_, _ = p.Exec(ctx, "DELETE FROM pending_decision WHERE external_ref LIKE $1", refLike)
		_, _ = p.Exec(ctx, "DELETE FROM session_triage WHERE host = $1 OR host = $2", host, otherHost)
	}()

	recovery := time.Now().UTC()
	before := recovery.Add(-10 * time.Minute) // incident predates the recovery
	after := recovery.Add(5 * time.Minute)    // incident NEWER than the recovery

	seedSession := func(ref, h, r string, createdAt time.Time) {
		t.Helper()
		if _, err := p.Exec(ctx, `
			INSERT INTO session_triage (external_ref, host, alert_rule, outcome, confirmed_clear, created_at)
			VALUES ($1, $2, $3, 'failed:investigate', false, $4)`,
			ref, h, r, createdAt); err != nil {
			t.Fatalf("seed session %s: %v", ref, err)
		}
	}
	seedDecision := func(ref, actionID string, openedAt time.Time) {
		t.Helper()
		if _, err := p.Exec(ctx, `
			INSERT INTO pending_decision (external_ref, action_id, opened_at, status)
			VALUES ($1, $2, $3, 'open')`,
			ref, actionID, openedAt); err != nil {
			t.Fatalf("seed decision %s: %v", ref, err)
		}
	}

	refMatch := fmt.Sprintf("tg387-%d-match", pid)         // same host+rule, predates      -> CLOSE + OBSOLETE
	refFamily := fmt.Sprintf("tg387-%d-family", pid)       // same host, family sibling rule -> CLOSE
	refOtherHost := fmt.Sprintf("tg387-%d-otherhost", pid) // different host                 -> stays open
	refOtherRule := fmt.Sprintf("tg387-%d-otherrule", pid) // different family               -> stays open
	refNewer := fmt.Sprintf("tg387-%d-newer", pid)         // newer than the recovery        -> stays open

	seedSession(refMatch, host, recoveryRule, before)
	seedDecision(refMatch, "act-match", before)
	seedSession(refFamily, host, familyRule, before)
	seedSession(refOtherHost, otherHost, recoveryRule, before)
	seedSession(refOtherRule, host, unrelatedRule, before)
	seedSession(refNewer, host, recoveryRule, after)
	seedDecision(refNewer, "act-newer", after) // a poll OPENED after the recovery — must stay open

	// APPLY the recovery through the real ingest seam. Its external_ref deliberately differs from every seeded
	// session's — proving the join is the natural key, never external_ref.
	s.Append(ctx, httpapi.TransitionRecord{
		ExternalRef: fmt.Sprintf("tg387-%d-recovery-newid", pid), Host: host, Site: "nl",
		AlertRule: recoveryRule, ReceivedAt: recovery,
	})

	var firstID int64
	if err := p.QueryRow(ctx,
		`SELECT id FROM ingest_transition WHERE host=$1 AND kind='recovery' ORDER BY id DESC LIMIT 1`, host).
		Scan(&firstID); err != nil {
		t.Fatalf("read transition id: %v", err)
	}

	readSession := func(ref string) (cleared bool, closedBy *int64) {
		t.Helper()
		if err := p.QueryRow(ctx,
			`SELECT confirmed_clear, closed_by_transition_id FROM session_triage WHERE external_ref=$1`, ref).
			Scan(&cleared, &closedBy); err != nil {
			t.Fatalf("read session %s: %v", ref, err)
		}
		return cleared, closedBy
	}
	readDecision := func(ref string) (status, outcome string) {
		t.Helper()
		if err := p.QueryRow(ctx,
			`SELECT status, outcome FROM pending_decision WHERE external_ref=$1`, ref).
			Scan(&status, &outcome); err != nil {
			t.Fatalf("read decision %s: %v", ref, err)
		}
		return status, outcome
	}

	// (1) matching session CLOSED, closer recorded — the core assertion the RED-confirm neutralises.
	if cleared, closedBy := readSession(refMatch); !cleared || closedBy == nil || *closedBy != firstID {
		t.Fatalf("matching session: confirmed_clear=%v closed_by=%v, want true + closer=%d — a recovery MUST close its own incident",
			cleared, closedBy, firstID)
	}
	// (1) matching proposal OBSOLETED, with the distinct outcome (never conflated with human:timeout).
	if status, outcome := readDecision(refMatch); status != "resolved" || outcome != "obsolete:subject-recovered" {
		t.Fatalf("matching proposal: status=%q outcome=%q, want resolved / obsolete:subject-recovered", status, outcome)
	}
	// (1) family-sibling label CLOSED — the pveliveness "Device-Down" incident a "Devices-up/down" recovery
	// must still reconcile (the whole reason the match goes through the rule-family authority, not string=).
	if cleared, closedBy := readSession(refFamily); !cleared || closedBy == nil || *closedBy != firstID {
		t.Fatalf("family-sibling session: confirmed_clear=%v closed_by=%v, want true + closer=%d — a family-sibling recovery MUST close it",
			cleared, closedBy, firstID)
	}
	// (2a) different HOST — untouched.
	if cleared, closedBy := readSession(refOtherHost); cleared || closedBy != nil {
		t.Fatalf("different-host session: confirmed_clear=%v closed_by=%v, want false/nil — a recovery on another host must NOT close it",
			cleared, closedBy)
	}
	// (2b) different rule FAMILY — untouched.
	if cleared, closedBy := readSession(refOtherRule); cleared || closedBy != nil {
		t.Fatalf("different-rule session: confirmed_clear=%v closed_by=%v, want false/nil — a recovery of a different condition must NOT close it",
			cleared, closedBy)
	}
	// (3) NEWER incident — session and its poll both untouched (the timestamp guard).
	if cleared, closedBy := readSession(refNewer); cleared || closedBy != nil {
		t.Fatalf("newer-incident session: confirmed_clear=%v closed_by=%v, want false/nil — a recovery must NOT close an incident newer than itself",
			cleared, closedBy)
	}
	if status, _ := readDecision(refNewer); status != "open" {
		t.Fatalf("newer-incident proposal: status=%q, want open — a poll opened after the recovery must NOT be obsoleted", status)
	}

	// (4) IDEMPOTENCY — re-apply the same recovery (a NEW ingest_transition id, since a re-fire+re-clear share
	// no id). It must change 0 rows: the closed session is not re-stamped, the resolved proposal not re-resolved.
	s.Append(ctx, httpapi.TransitionRecord{
		ExternalRef: fmt.Sprintf("tg387-%d-recovery-newid-2", pid), Host: host, Site: "nl",
		AlertRule: recoveryRule, ReceivedAt: recovery.Add(1 * time.Minute),
	})
	var secondID int64
	if err := p.QueryRow(ctx,
		`SELECT id FROM ingest_transition WHERE host=$1 AND kind='recovery' ORDER BY id DESC LIMIT 1`, host).
		Scan(&secondID); err != nil {
		t.Fatalf("read second transition id: %v", err)
	}
	if secondID == firstID {
		t.Fatalf("second recovery did not create a distinct transition row (id=%d)", secondID)
	}
	var reclosed int
	if err := p.QueryRow(ctx,
		`SELECT count(*) FROM session_triage WHERE closed_by_transition_id = $1`, secondID).Scan(&reclosed); err != nil {
		t.Fatalf("count reclosed: %v", err)
	}
	if reclosed != 0 {
		t.Fatalf("idempotency: the re-applied recovery closed %d session(s), want 0", reclosed)
	}
	// The matching session's closer is STILL the first recovery — never overwritten by the re-apply.
	if _, closedBy := readSession(refMatch); closedBy == nil || *closedBy != firstID {
		t.Fatalf("idempotency: matching session closer=%v, want unchanged first closer=%d", closedBy, firstID)
	}
}
