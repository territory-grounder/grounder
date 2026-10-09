package db

import (
	"context"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/ingest"
)

// GOLDEN-FIXTURE TEST FOR THE SUPPRESSION HISTORY READ.
//
// ingest.DecideSuppress is a pure function and already proven in isolation. Its verdict is only as good as
// the timeline it is handed, and building that timeline is a MERGE of two tables with different shapes —
// exactly the JOIN/ordering semantics a fake cannot reproduce and where this repository has been bitten
// before. So it runs against a real Postgres (TG_TEST_DSN).
//
// The failure that matters is ORDER. If a clear is emitted before the raise it closed, DecideSuppress reads
// "already closed" and ADMITS a repeat — noise. If a raise is emitted after a clear that actually preceded
// it, it reads "still open" and SUPPRESSES a live incident. The second is the dangerous direction, and the
// interleaving fixture below is what distinguishes them.

func seedAlertHistory(ctx context.Context, t *testing.T, p *Pool) (func(), time.Time) {
	t.Helper()
	const host, rule = "goldhist-host-a", "Device-Down"
	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)

	cleanup := func() {
		_, _ = p.Exec(ctx, `DELETE FROM ingest_alert WHERE host LIKE 'goldhist-%'`)
		_, _ = p.Exec(ctx, `DELETE FROM ingest_transition WHERE host LIKE 'goldhist-%'`)
	}
	cleanup()

	// A realistic INTERLEAVING: raise, repeat, clear, re-raise (the harness's inject/recover/re-inject
	// shape). A reader that concatenated the two tables instead of merging them would put both clears after
	// all raises and get the last question wrong.
	raise := func(at time.Time, h string) {
		if _, err := p.Exec(ctx, `
			INSERT INTO ingest_alert (external_ref, source_type, source_id, alert_rule, severity, host, received_at, observed_at)
			VALUES ($1,'librenms','lnms',$2,'critical',$3,$4,$4)`,
			"goldhist-"+at.Format("150405.000000")+"-"+h, rule, h, at); err != nil {
			t.Fatalf("seed alert: %v", err)
		}
	}
	clear := func(at time.Time, h string) {
		if _, err := p.Exec(ctx, `
			INSERT INTO ingest_transition (external_ref, kind, host, alert_rule, received_at, observed_at)
			VALUES ($1,'recovery',$2,$3,$4,$4)`,
			"goldhist-rec-"+at.Format("150405.000000"), h, rule, at); err != nil {
			t.Fatalf("seed recovery: %v", err)
		}
	}

	raise(base, host)                    // 0: incident opens
	raise(base.Add(2*time.Minute), host) // 1: repeat while open
	clear(base.Add(5*time.Minute), host) // 2: recovery closes it
	raise(base.Add(9*time.Minute), host) // 3: re-injection — a NEW incident
	// A different host must never appear in this key's history.
	raise(base.Add(3*time.Minute), "goldhist-host-b")
	clear(base.Add(6*time.Minute), "goldhist-host-b")

	return cleanup, base
}

// TestKeyHistoryMergesRaisesAndClearsInTimeOrder — the ordering property the verdict depends on.
func TestKeyHistoryMergesRaisesAndClearsInTimeOrder(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()
	cleanup, base := seedAlertHistory(ctx, t, p)
	defer cleanup()

	h, err := NewAlertHistoryStore(p).KeyHistory(ctx, "goldhist-host-a", "Device-Down", base.Add(-time.Minute))
	if err != nil {
		t.Fatalf("KeyHistory: %v", err)
	}
	if len(h) != 4 {
		t.Fatalf("history has %d entries, want the 4 seeded for this key (a 5th means another host's rows "+
			"leaked in, which would suppress across unrelated machines): %+v", len(h), h)
	}
	want := []bool{false, false, true, false} // raise, raise, clear, raise
	for i, w := range want {
		if h[i].Recovered != w {
			t.Errorf("entry %d recovered=%v, want %v — the merge is out of order, and DecideSuppress reads "+
				"this ordering directly: a clear placed before the raise it closed makes a live incident "+
				"look closed, and a raise placed after it makes a closed one look open", i, h[i].Recovered, w)
		}
		if i > 0 && h[i].At.Before(h[i-1].At) {
			t.Errorf("entry %d is EARLIER than entry %d — the result is not ascending", i, i-1)
		}
	}
}

// TestTheHistoryDrivesTheRightVerdictEndToEnd — the two halves together. This is the property the feature
// actually promises, and neither half proves it alone.
func TestTheHistoryDrivesTheRightVerdictEndToEnd(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()
	cleanup, base := seedAlertHistory(ctx, t, p)
	defer cleanup()

	s := NewAlertHistoryStore(p)
	full, err := s.KeyHistory(ctx, "goldhist-host-a", "Device-Down", base.Add(-time.Minute))
	if err != nil {
		t.Fatalf("KeyHistory: %v", err)
	}

	// After the re-raise at +9m, a further fire at +10m is a repeat of the NEW open incident -> suppress.
	if d := ingest.DecideSuppress(full, base.Add(10*time.Minute)); !d.Suppress {
		t.Errorf("a repeat of the re-opened incident was admitted (%s)", d.Reason)
	}

	// ...but the RE-RAISE ITSELF (the first alert after the recovery) must be admitted, or the second
	// injection scores as UNDETECTED and A1 collapses. Evaluate with only the history that existed at that
	// moment: raise, raise, clear.
	beforeReRaise := full[:3]
	if d := ingest.DecideSuppress(beforeReRaise, base.Add(9*time.Minute)); d.Suppress {
		t.Errorf("the first alert of a re-injection was SUPPRESSED (%s) — this is the A1-collapse case: the "+
			"detection-recall query finds no ingest_alert row to correlate and scores the fault as a miss",
			d.Reason)
	}
}

// TestAnEmptyKeyIsRejectedRatherThanMatchingEverything — an empty host in a LIKE-free equality still matches
// every row whose host is empty, and worse, it would collapse unrelated machines into one incident.
func TestAnEmptyKeyIsRejectedRatherThanMatchingEverything(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()

	if _, err := NewAlertHistoryStore(p).KeyHistory(ctx, "", "Device-Down", time.Now().Add(-time.Hour)); err == nil {
		t.Error("an empty host was accepted — a key that identifies nothing must not be allowed to " +
			"suppress across the estate")
	}
	if _, err := NewAlertHistoryStore(p).KeyHistory(ctx, "h", "", time.Now().Add(-time.Hour)); err == nil {
		t.Error("an empty alert_rule was accepted")
	}
}

// TestTheSQLShadowAgreesWithTheGoJudgement is the oracle that makes the shadow number worth acting on.
//
// ShadowSuppressionSince reimplements ingest.DecideSuppress in SQL (LAG over the merged timeline is the
// same "walk back to the immediately preceding event"). A shadow figure is going to be used to justify
// dropping REAL alerts, so "the query looks right" is not enough: the two independent implementations must
// produce the SAME verdict for every alert in the fixture. A divergence means one of them is wrong, and
// without this test it would be averaged into a plausible-looking percentage.
func TestTheSQLShadowAgreesWithTheGoJudgement(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()
	cleanup, base := seedAlertHistory(ctx, t, p)
	defer cleanup()

	s := NewAlertHistoryStore(p)
	since := base.Add(-time.Minute)

	sum, err := s.ShadowSuppressionSince(ctx, since, ingest.MaxOpenIncident)
	if err != nil {
		t.Fatalf("ShadowSuppressionSince: %v", err)
	}

	// Recompute the same verdicts in Go, per key, by replaying each key's own history.
	goSuppress, goAccepted := 0, 0
	for _, host := range []string{"goldhist-host-a", "goldhist-host-b"} {
		h, herr := s.KeyHistory(ctx, host, "Device-Down", since)
		if herr != nil {
			t.Fatalf("KeyHistory(%s): %v", host, herr)
		}
		for i, f := range h {
			if f.Recovered {
				continue // a recovery is a timeline entry, never an accepted alert
			}
			goAccepted++
			if ingest.DecideSuppress(h[:i], f.At).Suppress {
				goSuppress++
			}
		}
	}

	if sum.Accepted != goAccepted {
		t.Errorf("SQL counted %d accepted alerts, Go counted %d — the two implementations disagree on what "+
			"even IS an alert (a recovery must never be counted as one)", sum.Accepted, goAccepted)
	}
	if sum.WouldSuppress != goSuppress {
		t.Errorf("SQL says %d would be suppressed, Go says %d. These are two independent implementations of "+
			"the same rule and they MUST agree — a shadow number that disagrees with the judgement it "+
			"models cannot be used to justify dropping real alerts", sum.WouldSuppress, goSuppress)
	}
	// ...and the fixture must actually exercise both outcomes, or agreement is trivial.
	if goSuppress == 0 || goAccepted == goSuppress {
		t.Fatalf("fixture is degenerate: %d/%d suppressed — it must contain BOTH a suppressed repeat and an "+
			"admitted re-raise, or the two implementations agree for the wrong reason", goSuppress, goAccepted)
	}
}

// TestOpenIncidentCorroborationSeesWhatTheRecencyWindowMisses is the defect as an oracle.
//
// Measured live: 11 hosts hold an open incident, only 2 fall inside the 15-minute recency window, so NINE
// are genuinely down and invisible to common-cause corroboration. The fixture reproduces that shape — a host
// whose incident opened long ago and has been quiet since.
func TestOpenIncidentCorroborationSeesWhatTheRecencyWindowMisses(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()
	cleanup, base := seedAlertHistory(ctx, t, p)
	defer cleanup()

	now := base.Add(2 * time.Hour) // long after every seeded event
	s := NewAlertHistoryStore(p)

	open, err := s.ActiveByOpenIncident(ctx, []string{"goldhist-host-a", "goldhist-host-b"}, now, 6*time.Hour)
	if err != nil {
		t.Fatalf("ActiveByOpenIncident: %v", err)
	}
	// host-a's last event is the re-raise at +9m — an OPEN incident, two hours quiet.
	if !open["goldhist-host-a"] {
		t.Error("a host whose last event is a RAISE is not reported active — it has an open incident and " +
			"corroboration must see it however long ago it last announced itself")
	}
	// host-b's last event is a recovery — closed, must NOT corroborate.
	if open["goldhist-host-b"] {
		t.Error("a host whose last event is a RECOVERY is reported active — a closed incident is not " +
			"evidence of a shared-parent failure")
	}

	// The recency definition MISSES host-a at this moment — that contrast is the whole point, and without
	// it this test would pass even if the two definitions were identical.
	recent, err := NewAlertLogStore(p).ActiveHosts(ctx, []string{"goldhist-host-a"}, now.Add(-15*time.Minute))
	if err != nil {
		t.Fatalf("ActiveHosts: %v", err)
	}
	if recent["goldhist-host-a"] {
		t.Fatal("fixture is degenerate: the recency window still sees host-a, so it does not exercise the " +
			"blindness this change fixes")
	}
}

// TestOpenIncidentCorroborationIsImmuneToSuppression — the property that unblocks the suppression stack.
// Deleting every repeat (what suppression does) must not change the answer, because the verdict depends on
// the LATEST event's kind, not on how many times the incident re-announced itself.
func TestOpenIncidentCorroborationIsImmuneToSuppression(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()
	cleanup, base := seedAlertHistory(ctx, t, p)
	defer cleanup()

	now := base.Add(2 * time.Hour)
	s := NewAlertHistoryStore(p)
	before, err := s.ActiveByOpenIncident(ctx, []string{"goldhist-host-a"}, now, 6*time.Hour)
	if err != nil {
		t.Fatalf("ActiveByOpenIncident: %v", err)
	}

	// Simulate suppression: drop the REPEAT (the +2m fire), keeping the first raise and the recovery.
	if _, err := p.Exec(ctx, `DELETE FROM ingest_alert WHERE host='goldhist-host-a' AND received_at=$1`,
		base.Add(2*time.Minute)); err != nil {
		t.Fatalf("simulate suppression: %v", err)
	}
	after, err := s.ActiveByOpenIncident(ctx, []string{"goldhist-host-a"}, now, 6*time.Hour)
	if err != nil {
		t.Fatalf("ActiveByOpenIncident after suppression: %v", err)
	}
	if before["goldhist-host-a"] != after["goldhist-host-a"] {
		t.Errorf("suppressing a repeat CHANGED the corroboration verdict (%v -> %v). This definition exists "+
			"precisely so that dropping repeats cannot blind corroboration — 140 of 203 suppressed alerts "+
			"leave no in-window evidence under the recency definition",
			before["goldhist-host-a"], after["goldhist-host-a"])
	}
}

// TestAnIncidentWithNoRecoveryGoesStaleRatherThanCorroboratingForever — a lost recovery is a monitoring gap.
func TestAnIncidentWithNoRecoveryGoesStaleRatherThanCorroboratingForever(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()
	cleanup, base := seedAlertHistory(ctx, t, p)
	defer cleanup()

	s := NewAlertHistoryStore(p)
	// Far past the staleness bound: the open incident must stop corroborating.
	if got, err := s.ActiveByOpenIncident(ctx, []string{"goldhist-host-a"}, base.Add(48*time.Hour), 6*time.Hour); err != nil {
		t.Fatalf("ActiveByOpenIncident: %v", err)
	} else if got["goldhist-host-a"] {
		t.Error("an incident open for 48h with no recovery still corroborates — a lost recovery would then " +
			"inflate every predicted cascade on that host forever")
	}
}
