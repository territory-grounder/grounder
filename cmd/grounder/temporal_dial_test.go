package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"gopkg.in/yaml.v3"
)

// TG-565: after a host reboot the grounder started ~2s before Temporal listened, its ONE boot dial failed, and
// it stayed validate-only (no triage sessions) for the life of the process — green, healthy, and triaging
// nothing. These oracles pin the patient dial, the degraded grounder's way back, and main()'s use of both.

type fakeTemporalClient struct {
	client.Client // nil: only Close is called
	closed        *bool
}

func (f fakeTemporalClient) Close() { *f.closed = true }

// scriptedTemporalDial fails `failures` times, then succeeds. The returned counter reads under the same lock
// the dialer writes under (the watchdog dials from its own goroutine).
func scriptedTemporalDial(failures int) (temporalDialer, func() int, *bool) {
	calls, closed := 0, false
	var mu sync.Mutex
	count := func() int { mu.Lock(); defer mu.Unlock(); return calls }
	return func() (client.Client, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls <= failures {
			return nil, errors.New("dial tcp temporal:7233: connect: connection refused")
		}
		return fakeTemporalClient{closed: &closed}, nil
	}, count, &closed
}

type fakeBootClock struct{ t time.Time }

func (c *fakeBootClock) now() time.Time        { return c.t }
func (c *fakeBootClock) sleep(d time.Duration) { c.t = c.t.Add(d) }

// THE BOOT RACE: Temporal refuses the first dials, then listens. The grounder must wait for it and wire the
// triage lane, not degrade. KILLING MUTATION: return on the first failure (no retry) → degraded, and this fails.
func TestDialTemporalPatientlySurvivesTheBootRace(t *testing.T) {
	dial, calls, _ := scriptedTemporalDial(3)
	clk := &fakeBootClock{t: time.Unix(0, 0)}
	var slept []time.Duration
	sleep := func(d time.Duration) { slept = append(slept, d); clk.sleep(d) }
	var logs []string
	c, attempts, err := dialTemporalPatiently(dial, temporalBootDialBudget, sleep, clk.now,
		func(f string, a ...any) { logs = append(logs, f) })
	if err != nil || c == nil {
		t.Fatalf("a Temporal that comes up during boot must be dialed, got err=%v — the grounder would boot DEGRADED (validate-only) and mint no triage", err)
	}
	if attempts != 4 || calls() != 4 {
		t.Fatalf("attempts=%d calls=%d, want 4 (3 refusals then success)", attempts, calls())
	}
	if want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}; len(slept) != 3 || slept[0] != want[0] || slept[1] != want[1] || slept[2] != want[2] {
		t.Fatalf("backoff = %v, want %v", slept, want)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "retrying") || !strings.Contains(strings.Join(logs, "\n"), "reachable after") {
		t.Fatalf("the wait and the recovery must both be logged; got %q", logs)
	}
}

func TestDialTemporalPatientlyDoesNotWaitWhenTemporalIsUp(t *testing.T) {
	dial, _, _ := scriptedTemporalDial(0)
	clk := &fakeBootClock{t: time.Unix(0, 0)}
	sleeps := 0
	_, attempts, err := dialTemporalPatiently(dial, temporalBootDialBudget, func(time.Duration) { sleeps++ }, clk.now, func(string, ...any) {})
	if err != nil || attempts != 1 || sleeps != 0 {
		t.Fatalf("err=%v attempts=%d sleeps=%d, want a single immediate dial", err, attempts, sleeps)
	}
}

// Temporal genuinely down: the dial gives up within the budget (the read-only API must still come up), with
// the backoff capped. KILLING MUTATION: drop the cap → a 16s+ sleep appears and this fails; drop the budget
// check → it never returns (the test times out).
func TestDialTemporalPatientlyDegradesWithinItsBudget(t *testing.T) {
	dial, calls, _ := scriptedTemporalDial(1 << 30)
	clk := &fakeBootClock{t: time.Unix(0, 0)}
	start := clk.now()
	var maxSleep time.Duration
	sleep := func(d time.Duration) {
		if d > maxSleep {
			maxSleep = d
		}
		clk.sleep(d)
	}
	c, _, err := dialTemporalPatiently(dial, temporalBootDialBudget, sleep, clk.now, func(string, ...any) {})
	if err == nil || c != nil {
		t.Fatalf("an unreachable Temporal must degrade (return the error), got c=%v err=%v", c, err)
	}
	if spent := clk.now().Sub(start); spent > temporalBootDialBudget {
		t.Fatalf("waited %s, beyond the %s budget — the read-only API would be held down", spent, temporalBootDialBudget)
	}
	if maxSleep > temporalBackoffCap {
		t.Fatalf("a single backoff reached %s, beyond the %s cap", maxSleep, temporalBackoffCap)
	}
	// It must USE the budget, not give up early: it stops only when the NEXT wait would cross the deadline, and a
	// wait is at most temporalBackoffCap, so it has spent at least budget-cap and dialed at least three times.
	if spent := clk.now().Sub(start); spent < temporalBootDialBudget-temporalBackoffCap || calls() < 3 {
		t.Fatalf("gave up early: %d dial attempt(s) over %s of a %s budget", calls(), spent, temporalBootDialBudget)
	}
}

// The backoff cap only binds on a budget longer than the boot one (1+2+4+8 < 15s), so exercise it directly: over
// a long budget no single wait may exceed temporalBackoffCap. KILLING MUTATION: drop the cap → a 16s+ wait.
func TestDialTemporalPatientlyCapsItsBackoff(t *testing.T) {
	dial, _, _ := scriptedTemporalDial(1 << 30)
	clk := &fakeBootClock{t: time.Unix(0, 0)}
	var maxSleep time.Duration
	sleep := func(d time.Duration) {
		if d > maxSleep {
			maxSleep = d
		}
		clk.sleep(d)
	}
	if _, _, err := dialTemporalPatiently(dial, 2*time.Minute, sleep, clk.now, func(string, ...any) {}); err == nil {
		t.Fatal("an unreachable Temporal must degrade")
	}
	if maxSleep != temporalBackoffCap {
		t.Fatalf("longest wait = %s over a 2m budget, want exactly the %s cap", maxSleep, temporalBackoffCap)
	}
}

// The degraded grounder's way back: once Temporal answers it exits (code 3) so the restart policy re-wires the
// triage lane. KILLING MUTATION: exit without a successful dial → the "never exits while down" test fails;
// never exit → this one times out.
func TestDegradedGrounderExitsOnceTemporalIsReachable(t *testing.T) {
	dial, calls, closed := scriptedTemporalDial(2)
	code := make(chan int, 1)
	done := make(chan struct{})
	go func() {
		exitWhenTemporalReachable(context.Background(), dial, time.Millisecond, func(c int) { code <- c }, func(string, ...any) {})
		close(done)
	}()
	select {
	case c := <-code:
		if c == 0 {
			t.Fatal("the degraded grounder must exit NON-zero so the restart is visible")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Temporal became reachable but the degraded grounder never exited — it would stay validate-only for good")
	}
	<-done
	if calls() != 3 || !*closed {
		t.Fatalf("calls=%d closed=%v, want the 3rd dial to succeed and its probe client to be closed", calls(), *closed)
	}
}

func TestDegradedGrounderNeverExitsWhileTemporalIsDown(t *testing.T) {
	dial, calls, _ := scriptedTemporalDial(1 << 30)
	ctx, cancel := context.WithCancel(context.Background())
	exited := false
	done := make(chan struct{})
	go func() {
		exitWhenTemporalReachable(ctx, dial, time.Millisecond, func(int) { exited = true }, func(string, ...any) {})
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if calls() >= 5 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if exited {
		t.Fatal("the grounder exited while Temporal was still down — a restart loop that re-wires nothing")
	}
}

// main() must use both halves; a revert to one bare client.Dial reintroduces the permanent degrade.
// KILLING MUTATION: restore `client.Dial(client.Options{HostPort: tport})` in the if-statement, or delete the
// `go exitWhenTemporalReachable(` line — a pin below names the regression.
func TestGrounderTemporalDialSurvivesTheBootRace(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	var code []string
	for line := range strings.SplitSeq(string(b), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" && !strings.HasPrefix(trimmed, "//") {
			code = append(code, line)
		}
	}
	src := strings.Join(code, "\n")
	if !strings.Contains(src, "dialTemporalPatiently(dialTemporal, temporalBootDialBudget") {
		t.Error("main() no longer dials Temporal patiently — one refused dial at boot degrades ingest for the life of the process (TG-565)")
	}
	if !strings.Contains(src, "go exitWhenTemporalReachable(") {
		t.Error("a degraded grounder no longer exits when Temporal comes back — it stays validate-only (no triage) until someone restarts it (TG-565)")
	}
}

// The boot wait runs BEFORE the HTTP listeners, so it is bounded by the read-only API's own contract: the worst
// case (budget + one in-flight dial) must fit inside the grounder's compose healthcheck start_period, or a
// grounder correctly degrading on a Temporal outage is reported unhealthy (and a 2-minute wait, the first draft
// of this fix, held the read-only API dark that long). KILLING MUTATION: raise temporalBootDialBudget to 2m →
// this fails; lower the compose start_period below 20s → this fails.
func TestTemporalBootDialBudgetFitsTheHealthcheckGrace(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/docker-compose.yml")
	if err != nil {
		t.Fatalf("read compose: %v", err)
	}
	var doc struct {
		Services map[string]struct {
			Healthcheck struct {
				StartPeriod string `yaml:"start_period"`
			} `yaml:"healthcheck"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse compose: %v", err)
	}
	g, ok := doc.Services["grounder"]
	if !ok || g.Healthcheck.StartPeriod == "" {
		t.Fatal("the grounder service has no healthcheck start_period — the boot-wait bound has nothing to be checked against")
	}
	grace, err := time.ParseDuration(g.Healthcheck.StartPeriod)
	if err != nil {
		t.Fatalf("grounder start_period %q: %v", g.Healthcheck.StartPeriod, err)
	}
	if worst := temporalBootDialBudget + temporalDialAttemptBound; worst >= grace {
		t.Fatalf("worst-case boot wait for Temporal is %s, not inside the grounder healthcheck start_period %s — the read-only API would be held dark and a correctly degrading grounder reported unhealthy", worst, grace)
	}
}
