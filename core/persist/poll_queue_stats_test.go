package persist

import (
	"testing"
	"time"
)

func openAt(ref, site, action string, reversible bool, at time.Time) PendingDecision {
	return PendingDecision{
		ExternalRef: ref, ActionID: "act-" + ref, Band: "POLL_PAUSE",
		Approaches: []string{action}, Site: site, Reversible: reversible, OpenedAt: at,
	}
}

// THE CASE THE TICKET IS ABOUT. Depth alone cannot tell a busy estate from a flood, so the pair must.
func TestOneFaultFanningOutIsDistinguishableFromABusyEstate(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

	flood := make([]PendingDecision, 0, 60)
	for i := 0; i < 60; i++ {
		flood = append(flood, openAt(string(rune('a'+i%26))+itoa(i), "dc1",
			"restart the kubelet on the affected node", true, now.Add(-time.Minute)))
	}
	fs := ComputeQueueStats(flood, now)

	busy := make([]PendingDecision, 0, 60)
	for i := 0; i < 60; i++ {
		busy = append(busy, openAt("b"+itoa(i), "dc1", "distinct remediation "+itoa(i), true, now.Add(-time.Minute)))
	}
	bs := ComputeQueueStats(busy, now)

	if fs.Open != bs.Open {
		t.Fatalf("the two scenarios must have EQUAL depth or this test proves nothing: %d vs %d", fs.Open, bs.Open)
	}
	if fs.DistinctShapes != 1 {
		t.Errorf("60 identical proposals collapsed to %d shapes, want 1 — a rule written on this cannot "+
			"tell repetition from variety, which is the whole distinction", fs.DistinctShapes)
	}
	if fs.LargestShape != 60 {
		t.Errorf("largest shape = %d, want 60 — this is the number that says how many waiting polls ONE "+
			"review would settle", fs.LargestShape)
	}
	if bs.DistinctShapes != 60 {
		t.Errorf("60 genuinely different proposals collapsed to %d shapes. Over-collapsing is the "+
			"dangerous direction: it renders a real multi-fault incident as one repetitive flood and "+
			"invites an operator to dismiss it in bulk.", bs.DistinctShapes)
	}
}

// The same remediation at two sites is TWO decisions — different blast radius, and an operator may approve
// one and refuse the other.
func TestTheSameActionAtTwoSitesDoesNotCollapse(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	st := ComputeQueueStats([]PendingDecision{
		openAt("1", "dc1", "restart the unit", true, now),
		openAt("2", "dc2", "restart the unit", true, now),
	}, now)
	if st.DistinctShapes != 2 {
		t.Errorf("two sites collapsed to %d shape(s). Collapsing across sites would let one approval read "+
			"as though it settled the estate, when the second site's blast radius was never considered.",
			st.DistinctShapes)
	}
}

// A shape key built on external_ref would make every flood look fully distinct and the ratio a constant 1.
func TestTheShapeKeyIsNotJustTheUniqueRef(t *testing.T) {
	now := time.Now().UTC()
	same := []PendingDecision{
		openAt("ref-A", "dc1", "Restart  The   Unit", true, now),
		openAt("ref-B", "dc1", "restart the unit", true, now),
	}
	if st := ComputeQueueStats(same, now); st.DistinctShapes != 1 {
		t.Errorf("two identical proposals under different refs counted as %d shapes. Keyed on the ref "+
			"(unique per session by construction) the ratio is always 1 and the flood signal is a constant.",
			st.DistinctShapes)
	}
}

// Rows with no proposal text must NOT be treated as duplicates of one another. A row that cannot be
// compared is its own question; folding them together would under-report a flood of unreadable proposals.
func TestUnreadableProposalsDoNotCollapseIntoOne(t *testing.T) {
	now := time.Now().UTC()
	st := ComputeQueueStats([]PendingDecision{
		{ExternalRef: "x", Site: "dc1", OpenedAt: now},
		{ExternalRef: "y", Site: "dc1", OpenedAt: now},
	}, now)
	if st.DistinctShapes != 2 {
		t.Errorf("two proposals with no action text collapsed to %d shape(s), want 2 — silently counting "+
			"incomparable rows as duplicates under-reports exactly the queue nobody can review",
			st.DistinctShapes)
	}
}

// An unset opened_at is UNKNOWN, not ancient. Inventing an age from the zero time yields ~2000 years and
// pages for a backlog that does not exist.
func TestAnUnsetOpenedAtDoesNotFabricateAnAge(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	st := ComputeQueueStats([]PendingDecision{
		{ExternalRef: "dated", Approaches: []string{"a"}, OpenedAt: now.Add(-3 * time.Hour)},
		{ExternalRef: "undated", Approaches: []string{"b"}},
	}, now)
	if st.OldestAge != 3*time.Hour {
		t.Errorf("oldest age = %v, want 3h. An undated row must contribute NO age; treating the zero time "+
			"as an opening date reports a two-thousand-year-old poll and pages immediately.", st.OldestAge)
	}
	if st.Open != 2 {
		t.Errorf("the undated row vanished from the depth (open=%d, want 2) — it is still waiting for a "+
			"human whether or not it knows when it started", st.Open)
	}
}

// The empty queue must be honestly empty rather than reporting a stale or invented age.
func TestAnEmptyQueueReportsZeroesNotSilence(t *testing.T) {
	st := ComputeQueueStats(nil, time.Now())
	if st.Open != 0 || st.DistinctShapes != 0 || st.LargestShape != 0 || st.OldestAge != 0 || st.Irreversible != 0 {
		t.Errorf("empty queue produced %+v, want all zero", st)
	}
}

// ORDERING: irreversible first, then oldest. First-in-first-out under flood means the one proposal that
// cannot be undone is reviewed after ninety that can, by someone the preceding ninety have trained to
// click approve.
func TestIrreversibleProposalsSurfaceAheadOfOlderReversibleOnes(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	in := []PendingDecision{
		openAt("old-reversible", "dc1", "restart", true, now.Add(-5*time.Hour)),
		openAt("older-reversible", "dc1", "restart", true, now.Add(-9*time.Hour)),
		openAt("new-irreversible", "dc1", "delete the volume", false, now.Add(-1*time.Minute)),
	}
	got := OrderForReview(in)
	if got[0].ExternalRef != "new-irreversible" {
		t.Errorf("first for review is %q, want the irreversible one. Under flood, FIFO puts the "+
			"unrecoverable decision behind every recoverable one.", got[0].ExternalRef)
	}
	if got[1].ExternalRef != "older-reversible" {
		t.Errorf("within the reversible group the order is not oldest-first (got %q)", got[1].ExternalRef)
	}
	// The caller's slice must be untouched — the console reads this list more than once.
	if in[0].ExternalRef != "old-reversible" {
		t.Error("OrderForReview mutated its input")
	}
	if len(got) != len(in) {
		t.Fatalf("ordering LOST rows: %d in, %d out. Prioritisation must never drop a poll — a hidden "+
			"poll still blocks its action, and a visible backlog becomes a silently stuck one.",
			len(in), len(got))
	}
}

// VACUITY FLOOR for the ordering guard: if every fixture shared one reversibility, the test above would
// pass against an OrderForReview that ignored the field entirely.
func TestTheOrderingFixtureActuallyExercisesBothBranches(t *testing.T) {
	now := time.Now()
	in := []PendingDecision{
		openAt("a", "s", "x", true, now),
		openAt("b", "s", "y", false, now),
	}
	var rev, irrev int
	for _, d := range in {
		if d.Reversible {
			rev++
		} else {
			irrev++
		}
	}
	if rev == 0 || irrev == 0 {
		t.Fatal("the ordering fixtures are all one reversibility, so the irreversible-first rule is never " +
			"exercised and the guard above would pass against an ordering that ignores it")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
