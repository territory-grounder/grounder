package runner

import (
	"context"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/suppression"
)

// TestLiveSuppressGateDedup proves the live recent-triage log drives (host, rule)-window dedup: a first fire
// is investigated and recorded; a re-fire within the window is deduped; a different (host, rule) is not; and
// a re-fire AFTER the window is a genuine new incident (the log evicted the stale anchor).
func TestLiveSuppressGateDedup(t *testing.T) {
	at := time.Date(2026, 7, 16, 3, 0, 0, 0, time.UTC)
	window := 10 * time.Minute
	// This test triages each fire AT its observation time (no arrival lag), so pin the dedup eval clock to the
	// same `when` — it exercises the window arithmetic, not the eval-vs-observation split (that is
	// TestLiveSuppressGateDedupUsesEvalClock below).
	var evalWhen time.Time
	// The recent-triage log stamps every anchor with IssueRef = the incident's ExternalRef, so post-TG-354 a
	// dedup suppression requires the parent be CONFIRMED open. This test exercises the window arithmetic, not
	// openness, so confirm every anchor open — otherwise an undeterminable parent would (correctly) escalate.
	gate := &LiveSuppressGate{Window: window, Ledger: audit.NewLedger(), Log: NewRecentTriageLog(window),
		Clock: func() time.Time { return evalWhen }, OpenIssue: func(string) bool { return true }}

	fire := func(ref, host, rule string, when time.Time) suppression.Decision {
		evalWhen = when
		// A non-critical, KNOWN severity so the severity floor does not short-circuit before the dedup stage.
		d, err := gate.Decide(context.Background(), suppression.Alert{ExternalRef: ref, Host: host, AlertRule: rule, Severity: ingest.SeverityWarning, ObservedAt: when}, when)
		if err != nil {
			t.Fatalf("decide %s: %v", ref, err)
		}
		return d
	}

	if fire("i1", "web01", "NginxDown", at).Outcome.Suppressing() {
		t.Fatal("first fire has no prior anchor — must not be deduped")
	}
	if !fire("i2", "web01", "NginxDown", at.Add(2*time.Minute)).Outcome.Suppressing() {
		t.Fatal("a re-fire of the same (host, rule) within the window must be deduped (no second session)")
	}
	if fire("i3", "web01", "DiskFull", at.Add(3*time.Minute)).Outcome.Suppressing() {
		t.Fatal("a different (host, rule) must NOT be deduped")
	}
	if fire("i4", "web01", "NginxDown", at.Add(20*time.Minute)).Outcome.Suppressing() {
		t.Fatal("a re-fire AFTER the window is a genuine new incident — must not be deduped")
	}
}

// TestLiveSuppressGateDedupUsesEvalClock is the TG-377 oracle: the dedup window is measured on the worker's
// EVALUATION clock (when it triaged), not the alert's ObservedAt. During the 2026-08-06 pve03 cascade, 171
// alerts hit 171 parallel workflows out of order and with ingestion lag, so a re-fire's ObservedAt could be
// EARLIER than a prior's — on the alert clock the prior reads as future-dated (negative age) and dedup
// suppressed 0 of 171. Both alerts are triaged inside one worker window here (eval clock = 14:45), and the
// re-fire is observed EARLIER than the prior; it must still dedup.
// Both alerts are OBSERVED well outside the 10-min window relative to the eval clock (they fired ~45min ago,
// an ingestion-lagged stale pair) but are TRIAGED together in one worker window (eval clock = 14:45). This
// fixture discriminates all THREE eval-clock sites at once:
//   - drop `if !s.EvalAt.IsZero(){ now = s.EvalAt }` in DedupStage.Evaluate → the re-fire evaluates on its
//     stale ObservedAt, reads the prior as future-dated, fails open, escalates → RED.
//   - revert `Log.Record` LoggedAt from evalNow back to the observation `now` → the prior is stamped at 14:00,
//     falls outside the 10-min window/retention when the re-fire queries at 14:45, is evicted, no anchor → RED.
//   - revert the `Recent(evalNow,…)` query clock likewise → the prior is filtered out as out-of-window → RED.
// All three must be on the eval clock for the re-fire to dedup.
func TestLiveSuppressGateDedupUsesEvalClock(t *testing.T) {
	wall := time.Date(2026, 8, 6, 14, 45, 0, 0, time.UTC) // the single worker window in which both are triaged
	window := 10 * time.Minute
	gate := &LiveSuppressGate{Window: window, Ledger: audit.NewLedger(), Log: NewRecentTriageLog(window),
		// Confirm anchors open: this test pins the eval clock (TG-377), not openness (TG-354).
		Clock: func() time.Time { return wall }, OpenIssue: func(string) bool { return true }}
	dec := func(ref string, observedAt time.Time) suppression.Decision {
		d, err := gate.Decide(context.Background(), suppression.Alert{ExternalRef: ref, Host: "pve03", AlertRule: "GuestDown", Severity: ingest.SeverityWarning, ObservedAt: observedAt}, observedAt)
		if err != nil {
			t.Fatalf("decide %s: %v", ref, err)
		}
		return d
	}
	// Prior alert: FIRED 14:00 (45min before the eval clock — a lagged/stale delivery), triaged now (14:45).
	if dec("i1", wall.Add(-45*time.Minute)).Outcome.Suppressing() {
		t.Fatal("first fire has no prior anchor — must not be deduped")
	}
	// Re-fire FIRED 13:58 (out of order, also stale) but triaged in the same worker window. On the eval clock
	// both were triaged seconds apart, so it is a duplicate within the 10-min window and must be suppressed —
	// the log stamp, the Recent query, and the age test must ALL be on that eval clock.
	if !dec("i2", wall.Add(-47*time.Minute)).Outcome.Suppressing() {
		t.Fatal("a re-fire triaged within the worker's dedup window must be deduped on the EVAL clock, regardless of a stale/out-of-order ObservedAt (TG-377)")
	}
}

// TestLiveSuppressGateFreezeUsesEventTime guards the other half of the TG-377 fix: moving the DEDUP clock to
// eval time must NOT leak into the freeze gate. Freeze matches on when the alert FIRED (its ObservedAt / the
// `now` passed to Decide), so an alert that fired inside a declared maintenance window stays suppressed even
// when the worker triages it well AFTER the window closed (ingestion lag). If freeze wrongly used the eval
// clock, this alert — triaged 3h after the window — would escalate.
func TestLiveSuppressGateFreezeUsesEventTime(t *testing.T) {
	winStart := time.Date(2026, 8, 6, 2, 0, 0, 0, time.UTC)
	fg := &suppression.FreezeGate{}
	fg.Replace([]suppression.FreezeWindow{{Start: winStart, End: winStart.Add(time.Hour), Scope: "pve03", Reason: "declared maintenance"}})
	gate := &LiveSuppressGate{Freeze: fg, Ledger: audit.NewLedger(), Log: NewRecentTriageLog(time.Minute),
		Clock: func() time.Time { return winStart.Add(3 * time.Hour) }} // triaged long after the window closed
	firedInWindow := winStart.Add(30 * time.Minute)
	d, err := gate.Decide(context.Background(), suppression.Alert{ExternalRef: "i", Host: "pve03", AlertRule: "GuestDown", Severity: ingest.SeverityWarning, ObservedAt: firedInWindow}, firedInWindow)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if !d.Outcome.Suppressing() {
		t.Fatal("an alert that FIRED inside the freeze window must stay frozen even when triaged after it — freeze uses event time, not the dedup eval clock (TG-377 guard)")
	}
}

// TestLiveSuppressGateKnownPattern proves the operator-declared known-transient stage: a confident,
// keyword-transient pattern suppresses; a standing fault (no transient keyword) never does; a below-floor
// confidence never does; and a rule with no declared pattern is investigated.
func TestLiveSuppressGateKnownPattern(t *testing.T) {
	at := time.Date(2026, 7, 16, 3, 0, 0, 0, time.UTC)
	gate := &LiveSuppressGate{
		Patterns: []suppression.TransientPattern{
			{AlertRule: "BGPSessionFlapping", Confidence: 0.8}, // "flap" keyword + confident → suppresses
			{AlertRule: "DiskFull", Confidence: 0.9},           // no transient keyword → never suppresses
			{AlertRule: "LinkFlap", Confidence: 0.5},           // below the 0.7 floor → never suppresses
		},
		Ledger: audit.NewLedger(),
		Log:    NewRecentTriageLog(time.Minute),
	}
	suppressed := func(rule string) bool {
		d, err := gate.Decide(context.Background(), suppression.Alert{ExternalRef: "i", Host: "h", AlertRule: rule, Severity: ingest.SeverityWarning, ObservedAt: at}, at)
		if err != nil {
			t.Fatalf("decide %s: %v", rule, err)
		}
		return d.Outcome.Suppressing()
	}
	if !suppressed("BGPSessionFlapping") {
		t.Fatal("a confident, keyword-transient declared pattern must suppress")
	}
	if suppressed("DiskFull") {
		t.Fatal("a standing fault (no transient keyword) must NOT be auto-suppressed")
	}
	if suppressed("LinkFlap") {
		t.Fatal("a below-floor-confidence pattern must NOT suppress")
	}
	if suppressed("UnrelatedRule") {
		t.Fatal("a rule with no declared pattern must NOT be suppressed")
	}
}

// TestIsRebootClass proves the classifier the ingest activity actually uses (Deps.RebootRules, whose ZERO
// VALUE is the compiled default set) is narrow: host-down/reboot/restart/unreachable rules are reboot-class;
// standing faults are not (so a real incident on a scheduled host is never swept up).
func TestIsRebootClass(t *testing.T) {
	rules := Deps{}.RebootRules // the zero value production falls back to when nothing is configured
	for _, r := range []string{"HostDown", "NodeReboot", "web01 unreachable", "PowerCycle", "systemd-restart"} {
		if !rules.IsReboot(r) {
			t.Errorf("%q should be reboot-class", r)
		}
	}
	for _, r := range []string{"DiskFull", "HighLatency", "CertExpiring", "BGPFlapping"} {
		if rules.IsReboot(r) {
			t.Errorf("%q should NOT be reboot-class", r)
		}
	}
}

// TestLiveSuppressGateScheduledReboot proves the operator-declared recurring reboot schedule: a reboot-class
// alert on the scheduled host inside the cron window is suppressed; out of the window, a non-reboot alert, or
// an unscheduled host are all investigated.
func TestLiveSuppressGateScheduledReboot(t *testing.T) {
	validFrom := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	validUntil := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	gate := &LiveSuppressGate{
		Schedules: []suppression.Schedule{
			{Host: "web01", Cron: "0 3 * * *", Timezone: "UTC", Status: suppression.SchLive, ValidFrom: validFrom, ValidUntil: validUntil},
		},
		RebootPreBuffer: 10 * time.Minute,
		RebootWindow:    10 * time.Minute,
		Ledger:          audit.NewLedger(),
		Log:             NewRecentTriageLog(time.Minute),
	}
	at3 := time.Date(2026, 7, 16, 3, 0, 0, 0, time.UTC) // daily 3am window
	at4 := time.Date(2026, 7, 16, 4, 0, 0, 0, time.UTC) // outside the window
	dec := func(host, rule string, reboot bool, when time.Time) bool {
		d, err := gate.Decide(context.Background(), suppression.Alert{ExternalRef: "i", Host: host, AlertRule: rule, Severity: ingest.SeverityWarning, IsReboot: reboot, ObservedAt: when}, when)
		if err != nil {
			t.Fatalf("decide: %v", err)
		}
		return d.Outcome.Suppressing()
	}
	if !dec("web01", "HostDown", true, at3) {
		t.Fatal("a reboot-class alert on the scheduled host IN the window must be suppressed")
	}
	if dec("web01", "HostDown", true, at4) {
		t.Fatal("outside the reboot window must NOT be suppressed")
	}
	if dec("web01", "DiskFull", false, at3) {
		t.Fatal("a non-reboot alert must NOT be suppressed by a reboot schedule")
	}
	if dec("web99", "HostDown", true, at3) {
		t.Fatal("a host with no schedule must NOT be suppressed")
	}
}

// TestLiveSuppressGateBlastRadiusFold proves the operator-declared blast-radius fold: a matching CHILD alert
// within the valid window is folded (posted as a notice, no session); outside the window it is investigated;
// and a non-matching alert is investigated. Operator policies are always-fresh (no learned staleness).
func TestLiveSuppressGateBlastRadiusFold(t *testing.T) {
	at := time.Date(2026, 7, 16, 3, 0, 0, 0, time.UTC)
	gate := &LiveSuppressGate{
		Folds: []suppression.SuppressionPolicy{
			{HostScope: "cache01", RuleScope: "HighLatency", ValidFrom: at.Add(-time.Hour), ValidUntil: at.Add(time.Hour), LastVerifiedAt: at},
		},
		FoldFreshness: 100 * 365 * 24 * time.Hour,
		Ledger:        audit.NewLedger(),
		Log:           NewRecentTriageLog(time.Minute),
	}
	dec := func(host, rule string, when time.Time) suppression.Decision {
		d, err := gate.Decide(context.Background(), suppression.Alert{ExternalRef: "i", Host: host, AlertRule: rule, Severity: ingest.SeverityWarning, ObservedAt: when}, when)
		if err != nil {
			t.Fatalf("decide: %v", err)
		}
		return d
	}
	if !dec("cache01", "HighLatency", at).Outcome.Suppressing() {
		t.Fatal("a matching child alert in the valid window must be folded (suppressed as a notice)")
	}
	if dec("cache01", "HighLatency", at.Add(2*time.Hour)).Outcome.Suppressing() {
		t.Fatal("outside the policy's valid window must NOT be folded")
	}
	if dec("web01", "HighLatency", at).Outcome.Suppressing() {
		t.Fatal("a non-matching host must NOT be folded")
	}
}

// TestLiveSuppressGateDedupOpenIncident proves the open-incident guard: a re-fire is deduped only WHILE the
// anchor incident is still open; once its parent ticket resolves, a re-fire is a genuine new incident and is
// investigated (never silently deduped).
func TestLiveSuppressGateDedupOpenIncident(t *testing.T) {
	at := time.Date(2026, 7, 16, 3, 0, 0, 0, time.UTC)
	open := map[string]bool{}
	gate := &LiveSuppressGate{
		Window:    10 * time.Minute,
		OpenIssue: func(ref string) bool { return open[ref] },
		Ledger:    audit.NewLedger(),
		Log:       NewRecentTriageLog(10 * time.Minute),
	}
	fire := func(ref string, when time.Time) suppression.Decision {
		d, err := gate.Decide(context.Background(), suppression.Alert{ExternalRef: ref, Host: "web01", AlertRule: "NginxDown", Severity: ingest.SeverityWarning, ObservedAt: when}, when)
		if err != nil {
			t.Fatalf("decide %s: %v", ref, err)
		}
		return d
	}
	fire("TG-1", at)    // first fire → escalates, recorded as anchor with IssueRef=TG-1
	open["TG-1"] = true // the anchor incident is open
	if !fire("TG-2", at.Add(time.Minute)).Outcome.Suppressing() {
		t.Fatal("a re-fire while the anchor incident is OPEN must be deduped")
	}
	open["TG-1"] = false // the anchor incident has resolved
	if fire("TG-3", at.Add(2*time.Minute)).Outcome.Suppressing() {
		t.Fatal("a re-fire after the anchor incident RESOLVED must NOT be deduped — it is a genuine new incident")
	}
}

// TestLiveSuppressGateCounts proves the gate tallies its decisions by outcome for observability: two
// distinct escalations and one dedup'd re-fire are counted as 2 escalate + 1 suppressed.
func TestLiveSuppressGateCounts(t *testing.T) {
	at := time.Date(2026, 7, 16, 3, 0, 0, 0, time.UTC)
	// The dedup'd re-fire (i3) needs a CONFIRMED-open parent to suppress post-TG-354 (the log stamps
	// IssueRef = ExternalRef), so confirm anchors open — this test is about the outcome tally, not openness.
	gate := &LiveSuppressGate{Window: 10 * time.Minute, Ledger: audit.NewLedger(), Log: NewRecentTriageLog(10 * time.Minute),
		OpenIssue: func(string) bool { return true }}
	fire := func(ref, rule string, when time.Time) {
		if _, err := gate.Decide(context.Background(), suppression.Alert{ExternalRef: ref, Host: "h", AlertRule: rule, Severity: ingest.SeverityWarning, ObservedAt: when}, when); err != nil {
			t.Fatalf("decide %s: %v", ref, err)
		}
	}
	fire("i1", "A", at)                  // escalate (no prior)
	fire("i2", "B", at)                  // escalate (different rule)
	fire("i3", "A", at.Add(time.Minute)) // dedup of i1 → suppressed
	c := gate.Counts()
	if c["escalate"] != 2 {
		t.Errorf("want 2 escalate, got %d (%v)", c["escalate"], c)
	}
	if c["suppressed"] != 1 {
		t.Errorf("want 1 suppressed, got %d (%v)", c["suppressed"], c)
	}
}

// TestRecentTriageLogEviction proves the log stays bounded: entries past retention are evicted on read.
func TestRecentTriageLogEviction(t *testing.T) {
	at := time.Date(2026, 7, 16, 3, 0, 0, 0, time.UTC)
	l := NewRecentTriageLog(5 * time.Minute)
	l.Record(suppression.TriageEntry{Host: "a", AlertRule: "r", LoggedAt: at})
	l.Record(suppression.TriageEntry{Host: "b", AlertRule: "r", LoggedAt: at.Add(time.Minute)})
	// 4 minutes later: both within 5m retention; a 3m window sees only the newer one.
	if got := l.Recent(at.Add(4*time.Minute), 3*time.Minute); len(got) != 1 || got[0].Host != "b" {
		t.Fatalf("3m window at +4m must see only host b, got %+v", got)
	}
	// 10 minutes later: both past retention → evicted, none returned, and the log is emptied.
	if got := l.Recent(at.Add(10*time.Minute), 10*time.Minute); len(got) != 0 {
		t.Fatalf("all entries past retention must be evicted, got %+v", got)
	}
	if got := l.Recent(at.Add(10*time.Minute), 10*time.Minute); len(got) != 0 {
		t.Fatal("the log must be emptied after eviction")
	}
}
