package suppressionshadow

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	coreingest "github.com/territory-grounder/grounder/core/ingest"
)

type fakeHist struct {
	fires []coreingest.Fire
	err   error
	calls int
	mu    sync.Mutex
}

func (f *fakeHist) KeyHistory(_ context.Context, _, _ string, _ time.Time) ([]coreingest.Fire, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return f.fires, f.err
}

type recLog struct {
	mu    sync.Mutex
	lines []string
}

func (r *recLog) logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, strings.ToLower(format))
	_ = args
}
func (r *recLog) all() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, "\n")
}

// settle waits for the observer's goroutine. The method is fire-and-forget BY DESIGN, so a test that read
// results synchronously would be testing a different function than production calls.
func settle() { time.Sleep(120 * time.Millisecond) }

// A REPEAT OF A STILL-OPEN INCIDENT IS SCORED AS "WOULD SUPPRESS" — AND NOTHING IS DROPPED.
func TestARepeatIsScoredAndNothingIsDropped(t *testing.T) {
	at := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	h := &fakeHist{fires: []coreingest.Fire{
		{At: at.Add(-10 * time.Minute)}, // the still-open incident
		{At: at},                        // the alert just accepted, already stored by the reader
	}}
	lg := &recLog{}
	New(h, lg.logf).ObserveAccepted("guest-a", "Device-Down", at)
	settle()
	if !strings.Contains(lg.all(), "would suppress") {
		t.Errorf("a repeat of an open incident was not scored as suppressible: %s", lg.all())
	}
	if !strings.Contains(lg.all(), "nothing was dropped") {
		t.Error("the shadow must state that nothing was dropped — this measurement runs while suppression is " +
			"NOT enabled, and a log that reads like an action would be read as one")
	}
}

// ★ THE ALERT MUST NOT BE A REPEAT OF ITSELF. The reader stores the accepted alert before the observation
// runs, so counting the whole history would report suppression of EVERY alert — a 100% figure that looks
// like a spectacular result and is an off-by-one.
func TestAnAlertIsNotARepeatOfItself(t *testing.T) {
	at := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	h := &fakeHist{fires: []coreingest.Fire{{At: at}}} // ONLY the just-accepted alert
	lg := &recLog{}
	New(h, lg.logf).ObserveAccepted("guest-a", "Device-Down", at)
	settle()
	if strings.Contains(lg.all(), "would suppress") {
		t.Errorf("a FIRST alert was scored as a repeat of itself — the shadow would report near-100%% "+
			"suppression and the enable decision would rest on it: %s", lg.all())
	}
}

// A FAILING HISTORY READ MUST NOT BE SCORED AS "ADMIT". Treating an unreadable history as "no prior fires"
// silently reclassifies repeats as new alerts and UNDERSTATES what suppression would have caught — the
// direction that makes enabling look less useful than it is, on evidence that is simply missing.
func TestAFailedHistoryReadIsReportedNotScored(t *testing.T) {
	lg := &recLog{}
	New(&fakeHist{err: errors.New("db down")}, lg.logf).ObserveAccepted("guest-a", "Device-Down", time.Now())
	settle()
	got := lg.all()
	if !strings.Contains(got, "history read failed") {
		t.Errorf("a failed history read was not reported: %s", got)
	}
	if strings.Contains(got, "would admit") || strings.Contains(got, "would suppress") {
		t.Errorf("a failed read produced a VERDICT (%s) — an unreadable history is missing evidence, and "+
			"scoring it either way contaminates the number", got)
	}
}

// IT MUST NEVER PANIC OR BLOCK ON THE INGEST PATH. A nil store, a nil logger, or empty keys are all
// reachable in production wiring; an observation that takes the front door down is worse than no measurement.
func TestTheObservationCanNeverTakeTheFrontDoorDown(t *testing.T) {
	for _, c := range []struct {
		name string
		s    *Shadow
		host string
		rule string
	}{
		{"nil shadow", nil, "h", "r"},
		{"nil history", New(nil, func(string, ...any) {}), "h", "r"},
		{"nil logger", New(&fakeHist{}, nil), "h", "r"},
		{"empty host", New(&fakeHist{}, func(string, ...any) {}), "", "r"},
		{"empty rule", New(&fakeHist{}, func(string, ...any) {}), "h", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer func() {
					if p := recover(); p != nil {
						t.Errorf("%s panicked on the ingest path: %v", c.name, p)
					}
				}()
				c.s.ObserveAccepted(c.host, c.rule, time.Now())
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Errorf("%s BLOCKED — ObserveAccepted takes no context precisely so it cannot delay ingest", c.name)
			}
		})
	}
}

// An empty key must not even reach the store: a blank host or rule cannot identify an incident, and querying
// on it would scan history for a key that matches nothing while still costing a round trip per alert.
func TestAnEmptyKeyNeverReachesTheStore(t *testing.T) {
	h := &fakeHist{}
	s := New(h, func(string, ...any) {})
	s.ObserveAccepted("", "rule", time.Now())
	s.ObserveAccepted("host", "", time.Now())
	settle()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.calls != 0 {
		t.Errorf("an unidentifiable alert hit the history store %d time(s)", h.calls)
	}
}
