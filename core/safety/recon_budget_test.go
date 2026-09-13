package safety

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// ORACLES FOR THE READ-LANE VOLUME BOUND (TG-165).
//
// What is being pinned is not "a counter counts". It is the four measured facts of the ticket: reads had no
// cross-session bound at all, a recon burst reached no kill path, `/halt` never stopped recon, and a bound
// that fires must SAY SO loudly enough that a truncated investigation is never read as an empty estate.

// fakeClock drives the rolling windows without sleeping an hour.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }

// recordingKill is a ShadowForcer that records every kill it is handed — the /halt seam a recon burst must
// reach. (safety.FixedModeAuthority flips a flag; this one keeps the REASON, which is the part an operator
// reads at 03:00.)
type recordingKill struct{ reasons []string }

func (k *recordingKill) ForceShadow(reason string) { k.reasons = append(k.reasons, reason) }

// newTestGovernor builds a governor on a fake clock with a small, exactly-stated budget. The shipped
// defaults are deliberately un-hittable (that is their point), so the bounds are shrunk here and asserted
// separately against the defaults in TestShippedReconBudgetCannotTruncateARealInvestigation.
func newTestGovernor(b ReconBudget, kill ShadowForcer) (*ReconGovernor, *fakeClock) {
	c := &fakeClock{t: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
	return NewReconGovernor(b, kill, WithReconClock(c.now)), c
}

// THE HOLE THAT HAD NO BOUND AT ALL. Every previous limit on the read lane was per-call or per-session, and a
// fresh session is one Temporal retry away (agent/session.go mints a new id per Run). So an attacker never
// needed to defeat a bound — they needed a new session, ten times an hour, forever.
//
// KILLING MUTATION: delete the PerHour branch in Admit (or make it `> g.budget.PerHour`). RED — read 21
// lands on a brand-new session and the estate can be enumerated one fresh session at a time.
func TestReconBudgetBoundsReadsACROSSSessions(t *testing.T) {
	g, _ := newTestGovernor(ReconBudget{PerSession: 5, PerHour: 20, Burst: 1000, BurstWindow: time.Minute}, nil)
	// Four sessions, each spending exactly its per-session budget: every single read is within EVERY
	// pre-TG-165 bound, and together they are the whole hour.
	for s := 0; s < 4; s++ {
		sess := "sess-" + string(rune('a'+s))
		for i := 0; i < 5; i++ {
			if err := g.Admit(sess); err != nil {
				t.Fatalf("read %d of session %s refused inside every bound: %v", i, sess, err)
			}
			g.Record(sess, "get-host-logs", "host-0")
		}
	}
	err := g.Admit("sess-e-a-brand-new-session")
	if err == nil {
		t.Fatal("a BRAND-NEW session was admitted after the hour's budget was spent — this is the exact hole " +
			"TG-165 reports: no cross-session bound existed, so an attacker enumerates the estate one fresh " +
			"session at a time and every per-session and per-call limit still reads green")
	}
	if !errors.Is(err, ErrReconRefused) {
		t.Fatalf("refusal must match ErrReconRefused so callers need not match on text; got %v", err)
	}
	var ref *ReconRefusal
	if !errors.As(err, &ref) || ref.Bound != "hour" {
		t.Fatalf("want the per-hour bound named in the typed refusal, got %#v", err)
	}
	if ref.Count != 20 || ref.Limit != 20 {
		t.Fatalf("the refusal must carry the evidence for itself (count/limit), got count=%d limit=%d", ref.Count, ref.Limit)
	}
}

// A REFUSAL MUST SAY SO. A bound that quietly returns less produces a confident stand-down over an
// investigation that never happened — worse than either a refused read or a slow one. Every refusal message
// must name the bound AND state that the investigation is incomplete rather than empty.
//
// KILLING MUTATION: shorten ReconRefusal.Error to "estate read refused". RED — nothing tells the model or
// the operator that the estate was not actually quiet.
func TestEveryReconRefusalSaysTheInvestigationIsIncompleteNotEmpty(t *testing.T) {
	seen := 0
	for _, tc := range []struct {
		name string
		make func() error
	}{
		{"session", func() error {
			g, _ := newTestGovernor(ReconBudget{PerSession: 1, PerHour: 100, Burst: 100, BurstWindow: time.Minute}, nil)
			g.Record("s", "get-host-logs", "h1")
			return g.Admit("s")
		}},
		{"hour", func() error {
			g, _ := newTestGovernor(ReconBudget{PerSession: 100, PerHour: 1, Burst: 100, BurstWindow: time.Minute}, nil)
			g.Record("s", "get-host-logs", "h1")
			return g.Admit("s")
		}},
		{"burst", func() error {
			g, _ := newTestGovernor(ReconBudget{PerSession: 100, PerHour: 100, Burst: 1, BurstWindow: time.Minute}, nil)
			g.Record("s", "get-host-logs", "h1")
			return g.Admit("s")
		}},
		{"halt", func() error {
			g, _ := newTestGovernor(ReconBudget{PerSession: 100, PerHour: 100, Burst: 100, BurstWindow: time.Minute}, nil)
			g.Halt("operator POST /halt")
			return g.Admit("s")
		}},
	} {
		err := tc.make()
		if err == nil {
			t.Fatalf("%s: expected a refusal", tc.name)
		}
		msg := err.Error()
		if !strings.Contains(msg, "INCOMPLETE, not empty") {
			t.Errorf("%s refusal does not say the investigation is incomplete rather than empty: %q", tc.name, msg)
		}
		if !strings.Contains(msg, "conclude from the evidence already gathered") {
			t.Errorf("%s refusal does not tell the agent what to do instead: %q", tc.name, msg)
		}
		seen++
	}
	// Vacuity floor: the loop above proves nothing if it never produced a refusal to inspect.
	if seen != 4 {
		t.Fatalf("vacuity floor: inspected %d refusals, want all 4 bounds — the table is not exercising them", seen)
	}
}

// THE BURST IS AN ANOMALY, AND AN ANOMALY MUST REACH THE KILL SWITCH. Before TG-165 no read counter fed any
// kill path: recon volume was invisible to the safety core.
//
// KILLING MUTATION: drop the `kill.ForceShadow(reason)` call at the end of Record. RED — a full-rate estate
// sweep runs with the mutation posture left exactly as the attacker found it.
func TestReconBurstForcesTheModeToShadowAndFiresOncePerEpisode(t *testing.T) {
	kill := &recordingKill{}
	g, clock := newTestGovernor(ReconBudget{PerSession: 100, PerHour: 100, Burst: 5, BurstWindow: time.Minute}, kill)
	for i := 0; i < 5; i++ {
		g.Record("sweep", "get-host-logs", "host-"+string(rune('a'+i)))
	}
	if len(kill.reasons) != 1 {
		t.Fatalf("a recon burst must force the mode to Shadow exactly once; ForceShadow calls: %v", kill.reasons)
	}
	if !strings.Contains(kill.reasons[0], "recon burst") || !strings.Contains(kill.reasons[0], "distinct targets") {
		t.Errorf("the kill reason must name the anomaly and its fan-out (a poll of one host and a sweep of five "+
			"read identically as a count); got %q", kill.reasons[0])
	}
	// Sustained burst: the alarm does not re-fire per read (it would drown the log and re-kill a dead mode).
	for i := 0; i < 20; i++ {
		g.Record("sweep", "get-host-logs", "host-z")
	}
	if len(kill.reasons) != 1 {
		t.Fatalf("the burst alarm must fire ONCE per episode, not per read; got %d kills", len(kill.reasons))
	}
	// While the episode is hot, reads are refused.
	if err := g.Admit("sweep"); err == nil {
		t.Fatal("reads must be refused while the burst window is hot")
	}
	// AND THE BLINDING IS NOT PERMANENT. The operator's answer to a burst may well be "investigate it", which
	// needs reads. The window drains and the lane recovers on its own.
	clock.add(2 * time.Minute)
	if err := g.Admit("triage-after-the-burst"); err != nil {
		t.Fatalf("the burst window must DRAIN — a rate alarm that never releases is a permanent blinding of "+
			"triage, which is the failure this control is not allowed to cause: %v", err)
	}
	if s := g.Snapshot(); s.Bursts != 1 || s.Reads != 25 {
		t.Fatalf("the meter must publish what it counted (bursts=1 reads=25); got %+v", s)
	}
}

// `/halt` FLIPPED THE MUTATION CHOKEPOINT ONLY. Measured in the ticket: recon continued straight through a
// halt. Halt is the read-lane half, and it holds ForceShadow's contract — safe, idempotent, never refused,
// never re-enabling.
//
// KILLING MUTATION: make Halt a no-op (or add an un-halt path and call it). RED — a halted worker keeps
// serving estate reads, so the kill switch stops only the half Shadow had already stopped.
func TestHaltStopsTheReadLaneAndNeverReEnablesIt(t *testing.T) {
	g, clock := newTestGovernor(ReconBudget{PerSession: 100, PerHour: 100, Burst: 100, BurstWindow: time.Minute}, nil)
	if err := g.Admit("s"); err != nil {
		t.Fatalf("reads must be served before the halt: %v", err)
	}
	g.Halt("worker kill-switch: operator POST /halt")
	g.Halt("a second, idempotent halt")
	if !g.Halted() {
		t.Fatal("Halted() must report the stopped read lane — /metrics and the /halt response both read it")
	}
	err := g.Admit("s")
	if err == nil {
		t.Fatal("recon continued through a halt — the operator's kill switch left estate enumeration running")
	}
	var ref *ReconRefusal
	if !errors.As(err, &ref) || ref.Bound != "halt" {
		t.Fatalf("a halted read lane must refuse with the HALT bound, not a budget bound: %#v", err)
	}
	if !strings.Contains(ref.Error(), "operator POST /halt") {
		t.Errorf("the refusal must carry the FIRST halt reason so the cause is legible: %q", ref.Error())
	}
	// No amount of time, and no drained window, un-halts it.
	clock.add(2 * time.Hour)
	if err := g.Admit("s"); err == nil {
		t.Fatal("a halt must never expire — turning the read lane back on is an operator decision, not a timeout")
	}
}

// A SESSION'S SPEND IS ITS OWN. An exhausted investigation must not refuse an unrelated one — that would turn
// one long triage into an estate-wide outage of triage.
//
// KILLING MUTATION: key the per-session counter on a constant instead of `session`. RED.
func TestPerSessionSpendDoesNotRefuseAnotherInvestigation(t *testing.T) {
	g, _ := newTestGovernor(ReconBudget{PerSession: 3, PerHour: 100, Burst: 100, BurstWindow: time.Minute}, nil)
	for i := 0; i < 3; i++ {
		g.Record("incident-a", "get-host-logs", "host-a")
	}
	if err := g.Admit("incident-a"); err == nil {
		t.Fatal("the exhausted session must be refused")
	}
	if err := g.Admit("incident-b"); err != nil {
		t.Fatalf("an unrelated investigation must still be served — one busy triage is not an estate-wide "+
			"blackout of triage: %v", err)
	}
}

// THE SHIPPED BOUNDS MUST NOT BE ABLE TO TRUNCATE REAL WORK. A recon bound that ever bites a genuine
// investigation gets removed within a week, and an uninstalled control protects nobody. This asserts the
// DEFAULTS against the loop's own worst case: a full-cycle-budget investigation (10 reads, the hard
// agent.DefaultLimits ceiling), forty of them inside one hour.
//
// KILLING MUTATION: set DefaultReconPerHour to 100. RED — a normal busy hour starts refusing reads.
func TestShippedReconBudgetCannotTruncateARealInvestigation(t *testing.T) {
	g, clock := newTestGovernor(DefaultReconBudget(), nil)
	if DefaultReconPerSession <= 10 {
		t.Fatalf("the per-session bound (%d) must sit above the agent loop's own 10-cycle ceiling, or the "+
			"budget — not the loop — becomes the thing that ends investigations", DefaultReconPerSession)
	}
	served := 0
	for s := 0; s < 40; s++ { // 40 investigations in one hour, each spending the whole cycle budget
		sess := "incident-" + time.Duration(s).String()
		for i := 0; i < 10; i++ {
			if err := g.Admit(sess); err != nil {
				t.Fatalf("session %d read %d refused under the SHIPPED budget: %v — a bound that truncates real "+
					"triage will be turned off, and then it protects nothing", s, i, err)
			}
			g.Record(sess, "get-host-logs", "host-"+time.Duration(i).String())
			served++
		}
		clock.add(90 * time.Second) // ~40 investigations spread across the hour
	}
	if served != 400 {
		t.Fatalf("vacuity floor: served %d reads, want 400 — the loop is not exercising the budget at all", served)
	}
}

// memLedger is a ReconLedger twin: the durable read record (agent_step_evidence) the window is seeded from.
type memLedger struct {
	at  []time.Time
	err error
}

func (l memLedger) ReadsSince(_ context.Context, since time.Time) ([]time.Time, error) {
	if l.err != nil {
		return nil, l.err
	}
	var out []time.Time
	for _, t := range l.at {
		if !t.Before(since) {
			out = append(out, t)
		}
	}
	return out, nil
}

// A RESTART MUST NOT HAND OUT A FRESH HOUR. "Restart the worker" is not a step an intruder finds difficult,
// and an in-process meter that starts empty makes the whole per-hour bound a formality.
//
// KILLING MUTATION: make SeedFromLedger return (0, nil) without touching the window. RED — the post-restart
// worker admits a full fresh hour of reads while the durable record shows the hour already spent.
func TestSeedingTheRollingHourFromTheEvidenceLedgerSurvivesARestart(t *testing.T) {
	g, clock := newTestGovernor(ReconBudget{PerSession: 100, PerHour: 10, Burst: 100, BurstWindow: time.Minute}, nil)
	// Ten reads recorded in the last ten minutes, by the process that just died.
	var rows []time.Time
	for i := 0; i < 10; i++ {
		rows = append(rows, clock.t.Add(-time.Duration(i+1)*time.Minute))
	}
	n, err := g.SeedFromLedger(context.Background(), memLedger{at: rows})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if n != 10 {
		t.Fatalf("want 10 seeded reads, got %d", n)
	}
	if err := g.Admit("post-restart-session"); err == nil {
		t.Fatal("a restarted worker was handed a brand-new hour — the durable record already showed the hour " +
			"spent, so the per-hour bound would be defeated by the simplest action an intruder can take")
	}
	// The seed drains with real time, exactly as live reads do: an hour later the lane is open again.
	clock.add(time.Hour)
	if err := g.Admit("an-hour-later"); err != nil {
		t.Fatalf("seeded reads must age out of the window like any other: %v", err)
	}
}

// VACUITY FLOOR FOR THE SEED. An empty (or unseedable) ledger must bind NOTHING — otherwise a fresh install,
// or a worker whose DB read failed, would boot with a phantom hour of reads already spent.
func TestSeedingFromAnEmptyLedgerBindsNothing(t *testing.T) {
	g, _ := newTestGovernor(ReconBudget{PerSession: 100, PerHour: 10, Burst: 100, BurstWindow: time.Minute}, nil)
	n, err := g.SeedFromLedger(context.Background(), memLedger{})
	if err != nil || n != 0 {
		t.Fatalf("an empty ledger must seed nothing: n=%d err=%v", n, err)
	}
	if err := g.Admit("fresh-install"); err != nil {
		t.Fatalf("a fresh install must not boot with a phantom spent hour: %v", err)
	}
	if _, err := g.SeedFromLedger(context.Background(), memLedger{err: errors.New("db down")}); err == nil {
		t.Fatal("a ledger error must be RETURNED, not swallowed — booting with a cold window is the caller's " +
			"decision to log, not this function's silence")
	}
}

// A BLANK OR FAT-FINGERED BOUND MUST NOT MEAN "UNLIMITED". Every other bound in this repository (the
// syslog-ng session cap, the evidence retention clamp) restores its default on a non-positive value, because
// the alternative is a control silently removed by a typo in a config store.
//
// KILLING MUTATION: return b unchanged from sane(). RED — a zeroed budget admits everything forever.
func TestANonPositiveBoundRestoresTheDefaultRatherThanDisablingIt(t *testing.T) {
	g, _ := newTestGovernor(ReconBudget{}, nil)
	// The GATING bounds must never be disable-able by a blank/typo'd key: a zero budget restores each guard.
	if b := g.Budget(); b.PerSession != DefaultReconPerSession || b.PerHour != DefaultReconPerHour ||
		b.Burst != DefaultReconBurst || b.BurstWindow != DefaultReconBurstWindow {
		t.Fatalf("a zero budget must restore every GATING default, got %+v", b)
	}
	// FanoutObserve is the deliberate exception: it is OBSERVE-ONLY (never refuses a read), so a zero
	// legitimately DISABLES the fan-out flag — disabling an observation is not disabling a guard. Its default-on
	// comes from DefaultReconBudget()/the worker env default, not from sane() (TG-325).
	if b := g.Budget(); b.FanoutObserve != 0 {
		t.Fatalf("a zero FanoutObserve must stay 0 (observe-only, legitimately disable-able), got %d", b.FanoutObserve)
	}
	g2, _ := newTestGovernor(ReconBudget{PerHour: -1, Burst: -50}, nil)
	if b := g2.Budget(); b.PerHour != DefaultReconPerHour || b.Burst != DefaultReconBurst {
		t.Fatalf("negative bounds must fall back to the defaults, got %+v", b)
	}
}

// A NIL GOVERNOR IS THE PRE-TG-165 BEHAVIOUR, NOT A PANIC. Every caller that has not been wired (oracles, the
// offline eval harness, the grounder) must run exactly as it did before — house rule: no behaviour change for
// existing deployments without a safe default.
func TestANilGovernorAdmitsEverythingAndNeverPanics(t *testing.T) {
	var g *ReconGovernor
	if err := g.Admit("s"); err != nil {
		t.Fatalf("an unwired governor must admit: %v", err)
	}
	g.Record("s", "t", "h")
	g.Halt("x")
	if g.Halted() {
		t.Fatal("a nil governor has no read lane to halt")
	}
	if s := g.Snapshot(); s.Reads != 0 {
		t.Fatalf("a nil governor counts nothing, got %+v", s)
	}
}

// FAN-OUT IS REPORTED, NOT GATED ON. 500 reads of one host is a poll; 500 reads of 500 hosts is a sweep. The
// distinction is the operator's whole signal, and it lives in the snapshot rather than in a threshold nobody
// could size honestly today.
func TestSnapshotReportsCrossTargetFanOut(t *testing.T) {
	g, _ := newTestGovernor(ReconBudget{PerSession: 100, PerHour: 100, Burst: 100, BurstWindow: time.Minute}, nil)
	for i := 0; i < 12; i++ {
		g.Record("sweep", "get-host-logs", "host-"+string(rune('a'+i)))
	}
	for i := 0; i < 12; i++ {
		g.Record("poll", "get-host-logs", "host-a")
	}
	s := g.Snapshot()
	if s.TargetsHour != 12 {
		t.Fatalf("want 12 distinct estate objects in the hour, got %d (%+v)", s.TargetsHour, s)
	}
	if s.ReadsHour != 24 {
		t.Fatalf("want 24 reads in the hour, got %d", s.ReadsHour)
	}
}
