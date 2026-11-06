package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/modules/observability/syslogng"
)

// TG-315 — the collector that gives the correlator its first non-availability witness.
//
// `ingest_alert` holds 3,167 rows across three source types, all of which answer "is it up?", and ZERO
// rows carrying category=security-incident. Everything except the collector is built and live. These
// oracles drive the readable core against a fake Runner: no SSH, no database, no workflow engine.
//
// The load-bearing property is OFFERED vs PRODUCED. Four different situations produce zero admitted
// events and only one is healthy, so an oracle that asserted only "we got some events" would pass on a
// collector that had silently stopped reading.

type fakeAuthlogRunner struct {
	// byPath is the fake estate: path -> file content. A path absent from the map answers exit 1, which is
	// exactly what `tail` does for a host that ships no log today.
	byPath map[string]string
	err    error
	calls  []string
}

func (f *fakeAuthlogRunner) Run(_ context.Context, _ syslogng.Server, argv []string) (syslogng.RunResult, error) {
	p := argv[len(argv)-1]
	f.calls = append(f.calls, p)
	if f.err != nil {
		return syslogng.RunResult{}, f.err
	}
	body, ok := f.byPath[p]
	if !ok {
		return syslogng.RunResult{ExitCode: 1}, nil
	}
	return syslogng.RunResult{Stdout: []byte(body), ExitCode: 0}, nil
}

func authlogFixedClock() func() time.Time {
	t := time.Date(2026, 8, 7, 10, 5, 0, 0, time.UTC)
	return func() time.Time { return t }
}

const authlogFailureLines = `Aug  7 10:01:02 web01 sshd[111]: Failed password for root from 192.0.2.9 port 2222 ssh2
Aug  7 10:01:03 web01 sshd[111]: Failed password for root from 192.0.2.9 port 2223 ssh2
Aug  7 10:02:10 web01 sudo:  deploy : TTY=pts/0 ; PWD=/ ; USER=root ; COMMAND=/bin/systemctl restart nginx
`

func authlogServer() syslogng.Server {
	return syslogng.Server{Site: "nl", SSHHost: "dc1syslogng01", SSHUser: "root", BasePath: "/mnt/logs/syslog-ng"}
}

// THE HAPPY PATH, and it asserts the DENOMINATOR alongside the yield — not just that events appeared.
func TestTheCollectorReadsAuthEventsAndReportsItsDenominator(t *testing.T) {
	now := authlogFixedClock()
	paths := syslogng.ReadPathsFor("/mnt/logs/syslog-ng", "web01", now)
	f := &fakeAuthlogRunner{byPath: map[string]string{paths[0]: authlogFailureLines}}

	c := newAuthlogCollector([]syslogng.Server{authlogServer()}, []string{"web01"}, f, now)
	got := c.collectOnce(context.Background())

	if got.Offered != 1 {
		t.Errorf("offered = %d, want 1 — the denominator must count every (server,host) read ATTEMPTED, or "+
			"zero produced is indistinguishable from an unconfigured collector", got.Offered)
	}
	if got.Read != 1 {
		t.Errorf("read = %d, want 1", got.Read)
	}
	if got.Produced == 0 {
		t.Fatalf("produced 0 envelopes from %d parseable lines — with this failing, every other assertion "+
			"here is satisfied by a collector that reads nothing", strings.Count(authlogFailureLines, "\n"))
	}
	for _, e := range got.Envelopes {
		if e.Labels["category"] != "security-incident" {
			t.Errorf("envelope %q carries category %q — the whole point of this source is that it is a "+
				"SECOND KIND of witness, and core/risk keys the poll-forcing floor on that label",
				e.ExternalRef, e.Labels["category"])
		}
	}
}

// A HOST THAT SHIPS NO LOG IS NORMAL, NOT AN ERROR. 64 host directories exist on the real tree and 12 wrote
// on the day this was written. If an absent file counted as a failure the collector would look broken
// permanently.
func TestAHostWithNoLogIsOfferedButNotAFailure(t *testing.T) {
	now := authlogFixedClock()
	f := &fakeAuthlogRunner{byPath: map[string]string{}} // every path answers exit 1

	c := newAuthlogCollector([]syslogng.Server{authlogServer()}, []string{"web01", "web02"}, f, now)
	got := c.collectOnce(context.Background())

	if got.Offered != 2 {
		t.Errorf("offered = %d, want 2", got.Offered)
	}
	if got.Read != 0 || got.Produced != 0 {
		t.Errorf("read=%d produced=%d, want 0/0", got.Read, got.Produced)
	}
	if len(got.Failures) != 0 {
		t.Errorf("an absent log file was recorded as a FAILURE (%v) — a missing file and a refused read "+
			"must not share a status; TG-363 is the record of what that costs", got.Failures)
	}
}

// A TRANSPORT failure IS a failure, and it must not abort the other hosts.
func TestATransportFailureIsCountedAndDoesNotAbortTheOtherHosts(t *testing.T) {
	now := authlogFixedClock()
	paths := syslogng.ReadPathsFor("/mnt/logs/syslog-ng", "web02", now)
	broken := &fakeAuthlogRunner{byPath: map[string]string{paths[0]: authlogFailureLines}}
	broken.err = errors.New("dial tcp: connection refused")

	c := newAuthlogCollector([]syslogng.Server{authlogServer()}, []string{"web01"}, broken, now)
	got := c.collectOnce(context.Background())
	if len(got.Failures) == 0 {
		t.Error("a transport error produced no recorded failure — a poll that fails every read would " +
			"report identically to a quiet estate")
	}
	if got.Offered != 1 {
		t.Errorf("offered = %d, want 1 — a failed read is still an offered read", got.Offered)
	}
}

// THE CANDIDATE ORDER MUST BE THE SYSLOG-NG PACKAGE'S. Sites disagree about whether today.log exists (NL
// has one, GR does not), and a collector that resolved paths independently would drift from the tool that
// already reads these trees.
func TestTheCollectorFallsBackToTheDatedFileLikeTheInvestigationTool(t *testing.T) {
	now := authlogFixedClock()
	paths := syslogng.ReadPathsFor("/mnt/logs/syslog-ng", "dc2fw01", now)
	if len(paths) < 2 {
		t.Fatalf("expected a today.log candidate AND a dated fallback, got %v", paths)
	}
	// Only the DATED file exists — the GR shape.
	f := &fakeAuthlogRunner{byPath: map[string]string{paths[1]: authlogFailureLines}}

	c := newAuthlogCollector([]syslogng.Server{authlogServer()}, []string{"dc2fw01"}, f, now)
	got := c.collectOnce(context.Background())

	if got.Produced == 0 {
		t.Fatalf("the dated-file fallback produced nothing; paths tried = %v", f.calls)
	}
	if len(f.calls) < 2 || f.calls[0] != paths[0] {
		t.Errorf("candidate order wrong: tried %v, want today.log (%s) first", f.calls, paths[0])
	}
}

// THE YIELD REGISTER MUST EMIT AT ZERO. This is the assertion that keeps "quiet" and "never ran" apart.
func TestTheYieldRegisterEmitsBeforeAnyPoll(t *testing.T) {
	y := &authlogYield{}
	s := y.samples(time.Now())
	if len(s) != 7 {
		t.Fatalf("want all seven series before any poll, got %d", len(s))
	}
	var sawNever bool
	for _, x := range s {
		if x.Name == "tg_authlog_seconds_since_last_poll" {
			sawNever = x.Value == -1
		}
	}
	if !sawNever {
		t.Error("seconds_since_last_poll must be -1 before the first poll — 'never' and 'a long time ago' " +
			"are different facts and only one of them is a dead goroutine")
	}
}

// And the register must actually move, or it is decoration.
func TestTheYieldRegisterRecordsOfferedEvenWhenNothingIsProduced(t *testing.T) {
	y := &authlogYield{}
	y.record(time.Now(), authlogCollect{Offered: 5, Read: 0, Produced: 0})

	got := map[string]float64{}
	for _, s := range y.samples(time.Now()) {
		got[s.Name] = s.Value
	}
	if got["tg_authlog_reads_offered_total"] != 5 {
		t.Errorf("offered = %v, want 5 — without the denominator a zero yield cannot be read",
			got["tg_authlog_reads_offered_total"])
	}
	if got["tg_authlog_polls_total"] != 1 {
		t.Errorf("polls = %v, want 1", got["tg_authlog_polls_total"])
	}
}
