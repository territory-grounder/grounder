package main

import (
	"sync/atomic"
	"time"

	"github.com/territory-grounder/grounder/core/metrics"
)

// THE DETECTOR THAT CANNOT SAY WHETHER IT IS WORKING (TG-350 follow-through).
//
// pve-liveness is TG's FASTEST detector — it polls guest status every ~37s and mints a triage on
// running→stopped, ahead of the ~6–11 minute LibreNMS device-down push. TG-350 fixed the credential it
// reads with. What no fix addressed is that the source publishes NOTHING about its own yield.
//
// MEASURED 2026-08-07. `tg_ingest_source_last_seen_seconds{source_id="pve-liveness"}` = 147 HOURS; the
// last row it wrote to ingest_alert is 2026-07-31 23:40. Meanwhile it is bound, polling, and its module
// self-test passes — `ingest/pve-liveness` is in the "can prove itself" list every boot. Six days of
// silence from a healthy-looking detector.
//
// AND THE SILENCE IS NOT INNOCENT. dc1pve03 lost its NVMe at 02:54Z on 2026-08-06. Four of the
// twenty guests pve-liveness watches live on it (perplexica01, wallos01, wallos-mylab01, sftpgo01), and
// during that window ingest_alert took 12 alerts naming those exact guests — every one from LibreNMS or
// Alertmanager, NONE from pve-liveness. The detector whose entire purpose is to beat LibreNMS to that
// transition contributed nothing to the one cascade it existed for.
//
// WHY THAT COULD NOT BE DIAGNOSED. There are ZERO tg_* series for this source. The poll loop logs only
// when it mints something (`if minted > 0 || already > 0`), so all four of these look identical from
// outside:
//
//  1. the loop is running and the estate is genuinely quiet   (healthy)
//  2. the loop is running and FetchActive returns nothing ever (blind)
//  3. the loop's fetch fails every tick                        (broken — logs, but nothing aggregates it)
//  4. the goroutine died                                       (dead)
//
// Only (1) is acceptable and it is the one that produces no evidence. These samples separate them: a
// poll counter that only advances while the loop lives, split by outcome, plus what the last poll
// actually SAW and what it produced.
type pveLivenessYield struct {
	pollsOK     atomic.Int64
	pollsFailed atomic.Int64
	minted      atomic.Int64
	alreadyOpen atomic.Int64
	// lastSeenDown is what the most recent SUCCESSFUL poll observed — a gauge, so it returns to 0 when
	// the estate recovers rather than latching.
	lastSeenDown atomic.Int64
	// watched is the denominator: how many guests this detector was configured to watch. Without it a
	// zero down-count is ambiguous between "nothing is down" and "nothing is being watched".
	watched atomic.Int64
	// lastPollUnix is the loop's heartbeat. A counter that stops advancing is only visible as an absence
	// of change; an explicit timestamp makes "the goroutine died" a readable fact.
	lastPollUnix atomic.Int64
}

func (y *pveLivenessYield) recordFailure(now time.Time) {
	y.pollsFailed.Add(1)
	y.lastPollUnix.Store(now.Unix())
}

func (y *pveLivenessYield) recordSuccess(now time.Time, seenDown, minted, already int) {
	y.pollsOK.Add(1)
	y.lastSeenDown.Store(int64(seenDown))
	y.minted.Add(int64(minted))
	y.alreadyOpen.Add(int64(already))
	y.lastPollUnix.Store(now.Unix())
}

// samples renders the register. ALWAYS EMITTED, including at zero — a series that appears only once the
// detector fires makes "quiet" and "gone" the same observation, which is the defect this closes.
func (y *pveLivenessYield) samples(now time.Time) []metrics.Sample {
	if y == nil {
		return nil
	}
	var sinceLastPoll float64
	if last := y.lastPollUnix.Load(); last > 0 {
		sinceLastPoll = now.Sub(time.Unix(last, 0)).Seconds()
	} else {
		// The loop is wired but has not completed a single tick. -1 is deliberately not a duration: it
		// says "never", which is a different fact from "a long time ago" and the one that distinguishes a
		// detector that never started from one that stopped.
		sinceLastPoll = -1
	}
	return []metrics.Sample{
		{
			Name: "tg_pve_liveness_polls_total", Kind: metrics.Counter,
			Help: "successful pve-liveness polls. TG's fastest detector had NO series at all until now: " +
				"its loop logs only when it mints, so a quiet estate, a blind fetch and a dead goroutine " +
				"were one observation. Advances only while the loop lives.",
			Value: float64(y.pollsOK.Load()),
		},
		{
			Name: "tg_pve_liveness_poll_failures_total", Kind: metrics.Counter,
			Help: "pve-liveness polls whose fetch FAILED. Read beside tg_pve_liveness_polls_total: a " +
				"failure rate near 1 is a broken credential or unreachable PVE, which otherwise appears " +
				"only as repeated log lines nothing aggregates.",
			Value: float64(y.pollsFailed.Load()),
		},
		{
			Name: "tg_pve_liveness_guests_watched", Kind: metrics.Gauge,
			Help: "guests this detector watches (TG_PVE_LIVENESS_GUESTS). The DENOMINATOR: without it a " +
				"zero down-count cannot be told apart from watching nothing at all.",
			Value: float64(y.watched.Load()),
		},
		{
			Name: "tg_pve_liveness_guests_down", Kind: metrics.Gauge,
			Help: "guests observed stopped by the most recent SUCCESSFUL poll. A gauge, so it returns to " +
				"0 on recovery instead of latching.",
			Value: float64(y.lastSeenDown.Load()),
		},
		{
			Name: "tg_pve_liveness_minted_total", Kind: metrics.Counter,
			Help: "triages this detector opened. Measured 2026-08-07 the source had written nothing since " +
				"2026-07-31 while 12 alerts about the guests it watches arrived from OTHER sources during " +
				"the pve03 cascade — the gap between polls_total and minted_total is where that lives.",
			Value: float64(y.minted.Load()),
		},
		{
			Name: "tg_pve_liveness_already_open_total", Kind: metrics.Counter,
			Help: "guest-down detections that found a triage already running (push, or a prior tick). " +
				"Beside minted_total this separates 'detected nothing' from 'detected, and something else " +
				"got there first' — the second is the detector working and losing a race, not failing.",
			Value: float64(y.alreadyOpen.Load()),
		},
		{
			Name: "tg_pve_liveness_seconds_since_poll", Kind: metrics.Gauge,
			Help: "seconds since the loop last completed a tick, or -1 if it never has. The heartbeat: a " +
				"counter that stops advancing is only visible as an absence of change, and -1 distinguishes " +
				"a detector that never started from one that stopped.",
			Value: sinceLastPoll,
		},
	}
}
