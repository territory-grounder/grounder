package safety

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ORACLES FOR THE CONCURRENCY-SCALED BURST BOUND (TG-571).
//
// The flat 150-reads-in-5-minutes alarm force-Shadowed TG four times in production (2026-09-30 00:54Z,
// 2026-10-04 11:26/11:28/11:31Z), every time on a mass outage: ~30 guests down at once, ~30 ordinary
// investigations at ~4 reads each. The bound now scales with the number of distinct investigations in the
// window — max(Burst, BurstPerSession × investigations) — so it measures reads PER INVESTIGATION, the quantity
// a sweep actually inflates. These oracles pin both directions: the outage no longer trips, a sweep still does.

// THE LIVE FALSE POSITIVE, REPLAYED. 32 investigations (one per downed guest) each read their own guest and two
// neighbours plus two log/metric pulls — 160 reads inside four minutes, above the flat 150.
//
// KILLING MUTATION: set DefaultReconBurstPerSession to 0 (the flat pre-TG-571 bound). RED — the shipped budget
// force-Shadows TG on the 2026-10-04 outage shape and refuses the outage's own triage reads.
func TestShippedReconBudgetDoesNotTripOnAMassOutage(t *testing.T) {
	kill := &recordingKill{}
	g, clock := newTestGovernor(DefaultReconBudget(), kill)
	reads := 0
	for s := 0; s < 32; s++ {
		sess := fmt.Sprintf("tg-liveness-guest%02d-1791113092", s)
		for i := 0; i < 5; i++ {
			if err := g.Admit(sess); err != nil {
				t.Fatalf("investigation %d read %d refused during a mass outage under the SHIPPED budget: %v", s, i, err)
			}
			g.Record(sess, "get-host-logs", fmt.Sprintf("guest%02d-n%d", s, i%3))
			reads++
		}
		clock.add(7 * time.Second) // 32 sessions inside ~4 minutes
	}
	if reads <= DefaultReconBurst {
		t.Fatalf("vacuity floor: the replay must exceed the flat burst floor (%d) to say anything; it made %d reads",
			DefaultReconBurst, reads)
	}
	if len(kill.reasons) != 0 {
		t.Fatalf("a mass outage of ordinary investigations force-Shadowed TG: %v", kill.reasons)
	}
	if s := g.Snapshot(); s.BurstInvestigations != 32 || s.BurstLimit != 32*DefaultReconBurstPerSession {
		t.Fatalf("the snapshot must publish the bound in force and what scaled it; got limit=%d investigations=%d",
			s.BurstLimit, s.BurstInvestigations)
	}
}

// A SWEEP STILL TRIPS. Six investigations spending their whole per-session cap (25 reads each, every read a
// different host) inside the window reach 150 = max(150, 6 × 12): the alarm fires once and reads are refused.
// Spread wider it still trips, because the scaled bound tracks reads PER investigation: twenty investigations
// at 13 distinct-host reads each are 260 reads against a bound of 240.
//
// KILLING MUTATIONS: (a) drop the burst check from Record. RED — a cap-spending sweep runs to the per-hour bound
// without ever forcing Shadow. (b) drop the episode freeze in burstState. RED — every investigation that joins
// the hot burst buys 12 more reads, re-closes the episode and re-fires the alarm (nine kills for the wide
// sweep), and the joining investigation's read is admitted mid-burst.
func TestASweepStillTripsTheScaledBurst(t *testing.T) {
	for _, tc := range []struct {
		name            string
		sessions, reads int
	}{
		{"cap-spending sweep in few investigations", 6, DefaultReconPerSession},
		{"wide sweep across many investigations", 20, DefaultReconBurstPerSession + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kill := &recordingKill{}
			g, _ := newTestGovernor(DefaultReconBudget(), kill)
			for s := 0; s < tc.sessions; s++ {
				sess := fmt.Sprintf("sweep-%d", s)
				for i := 0; i < tc.reads; i++ {
					if g.Admit(sess) != nil {
						break
					}
					g.Record(sess, "get-host-logs", fmt.Sprintf("host-%d-%d", s, i))
				}
			}
			if len(kill.reasons) != 1 {
				t.Fatalf("%d investigations × %d reads must trip the scaled burst exactly once; kills: %v",
					tc.sessions, tc.reads, kill.reasons)
			}
			if err := g.Admit("one-more"); !errors.Is(err, ErrReconRefused) {
				t.Fatalf("reads must be refused while the burst episode is hot, got %v", err)
			}
		})
	}
}

// HEADROOM IS EARNED ONLY BY A REAL INVESTIGATION. Reads with no session id share the "" bucket and reads
// adopted from the evidence ledger at boot are "<seeded>" — neither can say which investigations made them, so
// neither may raise the bound. Otherwise unstamped reads would buy their own headroom, and a restart would
// hand a burst a free investigation's worth of it.
//
// KILLING MUTATION: drop the `r.session != "" && r.session != seededSession` filter in burstState. RED — both
// the unstamped burst and the seeded window are admitted under a bound they raised themselves.
func TestUnstampedAndSeededReadsEarnNoBurstHeadroom(t *testing.T) {
	budget := ReconBudget{PerSession: 100, PerHour: 1000, Burst: 10, BurstWindow: time.Minute, BurstPerSession: 20}

	kill := &recordingKill{}
	g, _ := newTestGovernor(budget, kill)
	for i := 0; i < 15; i++ {
		g.Record("", "get-host-logs", fmt.Sprintf("host-%d", i))
	}
	if len(kill.reasons) != 1 {
		t.Fatalf("15 unstamped reads must trip the flat floor of 10 — the unstamped bucket earns no headroom; kills: %v", kill.reasons)
	}

	g2, clock := newTestGovernor(budget, nil)
	var at []time.Time
	for i := 0; i < 15; i++ {
		at = append(at, clock.t.Add(-10*time.Second))
	}
	if n, err := g2.SeedFromLedger(t.Context(), memLedger{at: at}); err != nil || n != 15 {
		t.Fatalf("vacuity floor: seed must adopt 15 reads, got %d (%v)", n, err)
	}
	var refusal *ReconRefusal
	// Asked from the unstamped bucket, so the only investigation that could raise the bound is the seeded one.
	if err := g2.Admit(""); !errors.As(err, &refusal) || refusal.Bound != "burst" || refusal.Limit != 10 {
		t.Fatalf("15 seeded reads must be refused against the flat floor of 10 (seeded reads earn no headroom); got %v", err)
	}
}

// THE BOUND AN ALARM CITES IS THE BOUND ADMIT REFUSES ON. Five investigations at 3 reads each (15) sit under
// max(10, 5 × 4) = 20 — above the flat floor, so every one of those reads is admitted only because the bound
// scaled. One investigation then reads on to 20: the kill reason must name bound 20 and the investigations,
// and the next refusal must carry Limit 20 — an alarm citing one number while the lane refuses on another is
// an alarm an operator cannot act on.
//
// KILLING MUTATION: refuse in Admit on g.budget.Burst (the floor) instead of the scaled bound. RED — Admit
// refuses read 11 while the alarm, which never fired, would say the bound is 20.
func TestTheScaledBoundIsTheOneAdmitRefusesOn(t *testing.T) {
	kill := &recordingKill{}
	g, _ := newTestGovernor(ReconBudget{PerSession: 100, PerHour: 1000, Burst: 10, BurstWindow: time.Minute, BurstPerSession: 4}, kill)
	read := func(sess string, i int) {
		t.Helper()
		if err := g.Admit(sess); err != nil {
			t.Fatalf("%s read %d refused below the scaled bound of 20: %v", sess, i, err)
		}
		g.Record(sess, "get-host-logs", fmt.Sprintf("h%d", i))
	}
	for s := 0; s < 5; s++ {
		for i := 0; i < 3; i++ {
			read(fmt.Sprintf("inv-%d", s), i)
		}
	}
	if len(kill.reasons) != 0 {
		t.Fatalf("15 reads from 5 investigations are under the scaled bound of 20; kills: %v", kill.reasons)
	}
	for i := 3; i < 8; i++ {
		read("inv-4", i)
	}
	if len(kill.reasons) != 1 || !strings.Contains(kill.reasons[0], "bound 20") || !strings.Contains(kill.reasons[0], "5 investigation(s)") {
		t.Fatalf("the alarm must fire at the scaled bound and cite it with its investigations; kills: %v", kill.reasons)
	}
	var refusal *ReconRefusal
	if err := g.Admit("inv-0"); !errors.As(err, &refusal) || refusal.Limit != 20 || refusal.Count != 20 {
		t.Fatalf("the refusal must carry the bound the alarm cited (20/20); got %v", err)
	}
}

// ZERO IS THE FLAT BOUND, NOT "UNLIMITED". BurstPerSession scales a guard UPWARD, so its off position is the
// stricter pre-TG-571 bound and sane() must not force it back on; a negative is clamped to that same 0.
//
// KILLING MUTATION: make sane() restore DefaultReconBurstPerSession on a non-positive value. RED — an operator
// who sets 0 to get the strict flat bound silently gets the scaled one.
func TestZeroBurstPerSessionIsTheFlatStricterBound(t *testing.T) {
	for _, v := range []int{0, -3} {
		kill := &recordingKill{}
		g, _ := newTestGovernor(ReconBudget{PerSession: 100, PerHour: 1000, Burst: 10, BurstWindow: time.Minute, BurstPerSession: v}, kill)
		if g.Budget().BurstPerSession != 0 {
			t.Fatalf("BurstPerSession %d must resolve to 0 (flat), got %d", v, g.Budget().BurstPerSession)
		}
		for s := 0; s < 10; s++ {
			g.Record(fmt.Sprintf("inv-%d", s), "get-host-logs", "h")
		}
		if len(kill.reasons) != 1 {
			t.Fatalf("with BurstPerSession=%d ten reads from ten investigations must trip the flat bound of 10; kills: %v", v, kill.reasons)
		}
	}
}

// AN INVESTIGATION COUNTS TOWARD THE BOUND IT ASKS UNDER. Admit runs before an investigation's first read lands
// in the window, and that read is what makes it an investigation — so a worker restarted mid-outage (its window
// re-seeded with reads no investigation owns) must still let a NEW investigation take its own headroom, while
// the unstamped bucket (TestUnstampedAndSeededReadsEarnNoBurstHeadroom) gets none.
//
// KILLING MUTATION: drop the `asking` investigation from burstState. RED — the first read of every new
// investigation is judged against a bound that leaves it out, and is refused at the flat floor.
func TestANewInvestigationCountsTowardTheBoundItAsksUnder(t *testing.T) {
	g, clock := newTestGovernor(ReconBudget{PerSession: 100, PerHour: 1000, Burst: 10, BurstWindow: time.Minute, BurstPerSession: 20}, nil)
	var at []time.Time
	for i := 0; i < 12; i++ {
		at = append(at, clock.t.Add(-10*time.Second))
	}
	if n, err := g.SeedFromLedger(t.Context(), memLedger{at: at}); err != nil || n != 12 {
		t.Fatalf("vacuity floor: seed must adopt 12 reads, got %d (%v)", n, err)
	}
	if err := g.Admit("tg-liveness-guest07-1791113092"); err != nil {
		t.Fatalf("a new investigation's first read must be judged under max(10, 20 × 1) = 20 with 12 reads in the "+
			"window; it was refused: %v", err)
	}
}
