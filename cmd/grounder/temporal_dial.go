package main

import (
	"context"
	"time"

	"go.temporal.io/sdk/client"
)

// THE GROUNDER'S TEMPORAL DIAL MUST SURVIVE A BOOT RACE (TG-565).
//
// The grounder's Temporal client is optional by design — the read-only API must serve without it — so a failed
// dial degrades ingest to validate-only (accept + normalize, NO triage session minted). Before this file that
// degrade was decided by ONE dial at boot and lasted for the life of the process. The 2026-09-18 reboot drill
// showed what that costs: after a host reboot dockerd starts every `restart: unless-stopped` container at once
// (it ignores depends_on), the grounder dialed Temporal ~2s before Temporal listened, and TG came back
// green-and-healthy while silently minting no triage for any alert — a quieter copy of the outage the drill
// was replaying. (The worker never had this: a failed dial is fatal there, so the restart policy retries it.)
//
// Two halves, so no path stays degraded while Temporal is reachable:
//   - dialTemporalPatiently retries with backoff for a SHORT budget before degrading — it absorbs the boot race
//     (seconds: in the drill Temporal listened ~2-4s after the grounder's first dial). It must stay short: it
//     runs BEFORE the HTTP listeners open, and the read-only API must not wait on Temporal. The budget plus one
//     in-flight dial stays inside the compose healthcheck's start_period (TestTemporalBootDialBudgetFits…).
//   - exitWhenTemporalReachable is the GUARANTEE, for any longer outage: a degraded grounder re-dials every 30s
//     and exits once Temporal answers, so the restart policy starts it again with the triage lane wired. (The
//     grounder has no graceful shutdown to skip — its normal end is log.Fatalf — and a live swap would have to
//     re-wire a dozen write backends.) A Temporal that FLAPS can cost one restart per flap, each ≤ ~20s of boot
//     wait; the watchdog only ever exits on a successful dial, so a Temporal that stays down never loops it.

// temporalDialer dials the Temporal frontend (client.Dial in production, a fake in tests).
type temporalDialer func() (client.Client, error)

const (
	// temporalBootDialBudget bounds how long boot waits for Temporal before degrading — and therefore how long
	// the read-only API's listeners wait. Short on purpose; the watchdog covers anything longer.
	temporalBootDialBudget = 15 * time.Second
	// temporalDialAttemptBound is how long ONE client.Dial can block: the SDK's GetSystemInfo health RPC timeout
	// (5s in go.temporal.io/sdk). The loop can start a last attempt just before the deadline, so the worst-case
	// boot wait is temporalBootDialBudget + temporalDialAttemptBound.
	temporalDialAttemptBound = 5 * time.Second
	// temporalRedialEvery is how often a DEGRADED grounder checks whether Temporal has become reachable.
	temporalRedialEvery = 30 * time.Second
	temporalBackoffCap  = 10 * time.Second
)

// dialTemporalPatiently dials until it succeeds or the budget is spent, backing off 1s, 2s, 4s … capped at 10s.
// It returns the client (or the last error) and how many attempts it took.
func dialTemporalPatiently(dial temporalDialer, budget time.Duration, sleep func(time.Duration), now func() time.Time,
	logf func(string, ...any)) (client.Client, int, error) {
	deadline := now().Add(budget)
	backoff := time.Second
	for attempts := 1; ; attempts++ {
		c, err := dial()
		if err == nil {
			if attempts > 1 {
				logf("temporal reachable after %d dial attempt(s) — the triage lane is wired (TG-565 boot race survived)", attempts)
			}
			return c, attempts, nil
		}
		if !now().Add(backoff).Before(deadline) {
			return nil, attempts, err
		}
		if attempts == 1 {
			logf("temporal dial failed (%v) — retrying for up to %s before degrading ingest to validate-only (TG-565: after a host reboot the grounder can start before Temporal listens)", err, budget)
		}
		sleep(backoff)
		if backoff *= 2; backoff > temporalBackoffCap {
			backoff = temporalBackoffCap
		}
	}
}

// exitWhenTemporalReachable is the degraded grounder's way back: every `every` it re-dials, and the first time
// Temporal answers it logs why and calls exit — the restart policy then starts a grounder whose boot dial wires
// the triage lane. It returns only when ctx is cancelled (or after calling exit, for tests).
func exitWhenTemporalReachable(ctx context.Context, dial temporalDialer, every time.Duration, exit func(int),
	logf func(string, ...any)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		c, err := dial()
		if err != nil {
			continue
		}
		c.Close()
		logf("temporal is reachable now but this grounder booted DEGRADED (validate-only: no triage sessions minted) — exiting so the restart policy restarts it with the triage lane wired (TG-565)")
		exit(3)
		return
	}
}
