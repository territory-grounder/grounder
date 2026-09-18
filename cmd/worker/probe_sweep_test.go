package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/territory-grounder/grounder/temporal/moduletest"
)

type stubProber struct {
	mu      sync.Mutex
	calls   int
	fail    bool
	detail  string
	panics  bool
	lastOp  string
	summary string
}

func (p *stubProber) Probe(_ context.Context, req moduletest.Request) (string, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.lastOp = req.Operator
	if p.panics {
		panic("boom")
	}
	if p.fail {
		return "", p.detail, errors.New("probe failed")
	}
	return p.summary, "", nil
}

func (p *stubProber) n() int { p.mu.Lock(); defer p.mu.Unlock(); return p.calls }

// AN EMITTING PROBE IS NEVER RUN ON A TIMER.
//
// The notifier probe posts a real message into an operations room. A sweep that ran it every interval
// would page the one room that has to stay readable during an incident — monitoring that degrades the
// thing it monitors.
//
// KILLING MUTATION: delete the `if s.emitting[k]` skip. RED — the notifier stub gets called.
func TestTheSweepNeverRunsAnEmittingProbe(t *testing.T) {
	notifier := &stubProber{summary: "posted"}
	reader := &stubProber{summary: "read 12 devices"}
	s := newProbeSweep(
		map[string]moduletest.Prober{"notifier/matrix": notifier, "ingest/librenms": reader},
		map[string]bool{"notifier/matrix": true}, nil, func(string, ...any) {}, 0)

	res := s.run(context.Background())

	if notifier.n() != 0 {
		t.Errorf("the emitting probe was RUN %d time(s) by a scheduled sweep — that posts into the "+
			"approvals room on every tick", notifier.n())
	}
	if reader.n() != 1 {
		t.Errorf("the read-only probe ran %d time(s), want 1", reader.n())
	}
	if res.Skipped != 1 || res.Ran != 1 || res.OK != 1 {
		t.Errorf("result = %+v, want Ran=1 OK=1 Skipped=1", res)
	}
}

// NOTIFY ON TRANSITIONS, NOT ON STATE.
//
// A sweep that notified while a module was down would send the same message every interval until somebody
// fixed it, and a channel that repeats itself is one people learn to filter — worthless at the moment it
// finally matters.
//
// KILLING MUTATION: notify whenever a probe fails rather than on the passing→failing edge. RED — the
// second and third sweeps each send another notice.
func TestAPersistentFailureNotifiesExactlyOnce(t *testing.T) {
	p := &stubProber{fail: true, detail: "the token was rejected"}
	var sent []string
	var mu sync.Mutex
	s := newProbeSweep(map[string]moduletest.Prober{"tracker/jira": p}, nil,
		func(_ context.Context, body string) { mu.Lock(); sent = append(sent, body); mu.Unlock() },
		func(string, ...any) {}, 0)

	for i := 0; i < 3; i++ {
		s.run(context.Background())
	}
	mu.Lock()
	got := len(sent)
	first := ""
	if got > 0 {
		first = sent[0]
	}
	mu.Unlock()
	if got != 1 {
		t.Fatalf("a failure persisting across 3 sweeps sent %d notices, want exactly 1", got)
	}
	if !strings.Contains(first, "tracker/jira") || !strings.Contains(first, "the token was rejected") {
		t.Errorf("the notice does not name the module and its actionable detail: %q", first)
	}
	// It must also be unmistakable as a non-decision, like every other thing TG puts in that room.
	if !strings.Contains(first, "not a governance decision") {
		t.Errorf("the notice could be mistaken for a governance decision: %q", first)
	}

	// RECOVERY is its own single notice, and re-arms the failure edge.
	p.fail = false
	s.run(context.Background())
	mu.Lock()
	got, last := len(sent), sent[len(sent)-1]
	mu.Unlock()
	if got != 2 || !strings.Contains(last, "recovered") {
		t.Fatalf("recovery sent %d total notices, last=%q", got, last)
	}
	p.fail = true
	s.run(context.Background())
	mu.Lock()
	got = len(sent)
	mu.Unlock()
	if got != 3 {
		t.Errorf("a NEW failure after recovery must notify again; total notices = %d, want 3", got)
	}
}

// A panicking connector must not take the sweep — or the worker — down.
func TestAPanickingProbeIsAFailureNotACrash(t *testing.T) {
	s := newProbeSweep(map[string]moduletest.Prober{"cmdb/netbox": &stubProber{panics: true}}, nil, nil,
		func(string, ...any) {}, 0)
	res := s.run(context.Background())
	if res.Failed != 1 {
		t.Fatalf("a panicking probe was not counted as a failure: %+v", res)
	}
}

// The sweep names ITSELF as the operator, so anything a probe attributes points at the timer rather than
// at a person who did nothing.
func TestTheSweepAttributesItselfNotAPerson(t *testing.T) {
	p := &stubProber{summary: "ok"}
	s := newProbeSweep(map[string]moduletest.Prober{"cmdb/pve": p}, nil, nil, func(string, ...any) {}, 0)
	s.run(context.Background())
	p.mu.Lock()
	op := p.lastOp
	p.mu.Unlock()
	if !strings.Contains(op, "sweep") {
		t.Errorf("the sweep attributed its probe to %q — an automated run must not look operator-initiated", op)
	}
}

// Every run states its coverage, including the skipped count, so a reader never has to reconstruct what
// the monitor did not look at.
func TestEveryRunStatesItsOwnCoverage(t *testing.T) {
	var lines []string
	s := newProbeSweep(
		map[string]moduletest.Prober{"notifier/matrix": &stubProber{}, "cmdb/pve": &stubProber{summary: "ok"}},
		map[string]bool{"notifier/matrix": true}, nil,
		func(f string, a ...any) { lines = append(lines, strings.ToLower(f)) }, 0)
	s.run(context.Background())
	if len(lines) == 0 || !strings.Contains(lines[0], "skipped") {
		t.Errorf("the sweep summary does not state how much it skipped: %v", lines)
	}
}
