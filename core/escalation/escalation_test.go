package escalation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/persist"
)

var eNow = time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

// The in-memory oracle must satisfy the store seam the controller drives (the durable pgx twin is asserted
// against the same seam in core/db). If either drifts from the interface, this stops compiling.
var _ Store = (*persist.EscalationQueue)(nil)

type fakeCondition struct{ active map[string]bool }

func (c fakeCondition) StillActive(_ context.Context, ref string) (bool, error) {
	return c.active[ref], nil
}

type fakePager struct{ paged []string } // "ref@tier"

func (p *fakePager) Page(_ context.Context, ref, tier string) error {
	p.paged = append(p.paged, ref+"@"+tier)
	return nil
}

// newController returns a controller over an in-memory queue plus that concrete queue, so a test can assert
// on the append-only history (Len) the Store seam does not expose.
func newController(active map[string]bool, cap int) (*Controller, *fakePager, *persist.EscalationQueue) {
	pager := &fakePager{}
	q := persist.NewEscalationQueue()
	c := NewController(q, fakeCondition{active: active}, pager, cap)
	return c, pager, q
}

// faultyStore wraps the in-memory queue and fails MarkFired (a stuck durable store) to exercise FireDue's
// bounded/isolated error handling.
type faultyStore struct {
	*persist.EscalationQueue
	failMark bool
}

func (s *faultyStore) MarkFired(ctx context.Context, seq int64) error {
	if s.failMark {
		return errors.New("boom: markfired")
	}
	return s.EscalationQueue.MarkFired(ctx, seq)
}

// faultyPager errors when paging any ref in failRefs, to exercise per-row isolation.
type faultyPager struct {
	paged    []string
	failRefs map[string]bool
}

func (p *faultyPager) Page(_ context.Context, ref, tier string) error {
	if p.failRefs[ref] {
		return errors.New("boom: page " + ref)
	}
	p.paged = append(p.paged, ref+"@"+tier)
	return nil
}

// BOUNDED: a stuck store (MarkFired always fails) must NOT page — the row is marked consumed BEFORE it is
// re-entered, so "no mark ⇒ no page", never a paged-but-unmarked row that re-pages the approver graph forever.
func TestFireDueMarkFailureProducesNoPageStorm(t *testing.T) {
	store := &faultyStore{EscalationQueue: persist.NewEscalationQueue(), failMark: true}
	pager := &fakePager{}
	c := NewController(store, fakeCondition{active: map[string]bool{"TG-1": true}}, pager, 3)
	if _, err := store.Enqueue(context.Background(), "TG-1", 1, eNow.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	n, err := c.FireDue(context.Background(), eNow)
	if n != 0 || err == nil {
		t.Fatalf("a stuck MarkFired must fire nothing and surface the error, got n=%d err=%v", n, err)
	}
	if len(pager.paged) != 0 {
		t.Fatalf("a row whose MarkFired fails must NOT be paged (no mark ⇒ no page), got %v", pager.paged)
	}
}

// ISOLATED: a failure on one due row must not head-of-line-block the rest of the batch.
func TestFireDuePerRowIsolation(t *testing.T) {
	q := persist.NewEscalationQueue()
	pager := &faultyPager{failRefs: map[string]bool{"TG-1": true}} // TG-1's page errors every time
	c := NewController(q, fakeCondition{active: map[string]bool{"TG-1": true, "TG-2": true}}, pager, 3)
	// TG-1 is enqueued first (lower seq, returned first by DuePending), TG-2 second.
	if _, err := q.Enqueue(context.Background(), "TG-1", 1, eNow.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Enqueue(context.Background(), "TG-2", 1, eNow.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	n, err := c.FireDue(context.Background(), eNow)
	if err == nil {
		t.Fatal("the poisoned TG-1 must surface an error")
	}
	// TG-2 still fired and paged despite TG-1's failure (before the fix, FireDue aborted at TG-1).
	if n != 1 || len(pager.paged) != 1 || pager.paged[0] != "TG-2@approver-graph" {
		t.Fatalf("TG-2 must fire despite TG-1 being poisoned, got n=%d paged=%v", n, pager.paged)
	}
}

func TestScheduleReCheckEnqueues(t *testing.T) {
	c, _, q := newController(nil, 3)
	out, err := c.ScheduleReCheck(context.Background(), "TG-1", 0, eNow.Add(time.Hour))
	if err != nil || out != Scheduled {
		t.Fatalf("unanswered poll must schedule a re-check, got %v %v", out, err)
	}
	if q.Len() != 1 {
		t.Fatalf("a re-check row must be appended, len=%d", q.Len())
	}
}

func TestFireStillActiveReEscalates(t *testing.T) {
	c, pager, _ := newController(map[string]bool{"TG-1": true}, 3)
	_, _ = c.ScheduleReCheck(context.Background(), "TG-1", 0, eNow.Add(-time.Minute)) // due
	n, err := c.FireDue(context.Background(), eNow)
	if err != nil || n != 1 {
		t.Fatalf("one due re-check should fire, got %d %v", n, err)
	}
	if len(pager.paged) != 1 || pager.paged[0] != "TG-1@approver-graph" {
		t.Fatalf("a still-active condition must re-escalate and page the approver graph, got %v", pager.paged)
	}
}

func TestFireRecoveredDefers(t *testing.T) {
	c, pager, _ := newController(map[string]bool{"TG-1": false}, 3)
	_, _ = c.ScheduleReCheck(context.Background(), "TG-1", 0, eNow.Add(-time.Minute))
	_, _ = c.FireDue(context.Background(), eNow)
	if len(pager.paged) != 0 {
		t.Fatalf("a recovered condition must defer to the autocloser, not page: %v", pager.paged)
	}
	// the last recorded outcome is Deferred
	res := c.Results()
	if res[len(res)-1].Outcome != Deferred {
		t.Fatalf("recovered condition must defer, got %v", res[len(res)-1].Outcome)
	}
}

func TestCapStandsDownToHuman(t *testing.T) {
	c, pager, q := newController(nil, 3)
	// attempts already at the cap ⇒ stand down instead of enqueuing another re-check
	out, err := c.ScheduleReCheck(context.Background(), "TG-1", 3, eNow.Add(time.Hour))
	if err != nil || out != StoodDown {
		t.Fatalf("reaching the cap must stand down, got %v %v", out, err)
	}
	if q.Len() != 0 {
		t.Fatalf("stand-down must not enqueue another re-check, len=%d", q.Len())
	}
	if len(pager.paged) != 1 || pager.paged[0] != "TG-1@fallback-approver" {
		t.Fatalf("stand-down must page the fallback approver, got %v", pager.paged)
	}
}
