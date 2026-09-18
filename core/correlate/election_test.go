package correlate

import (
	"fmt"
	"testing"
	"time"
)

// fakeTopology is a hand-built estate oracle: guests depend on ONE hypervisor, which therefore has a high
// in-degree and is every guest's runs_on parent. It is deliberately not the estate graph — the election is a
// pure rule over an interface, and a map here proves the RULE without dragging the graph into core/correlate.
type fakeTopology struct {
	inDeg  map[string]int
	parent map[string]string
}

func (f fakeTopology) InDegree(host string) int        { return f.inDeg[host] }
func (f fakeTopology) RunsOnParent(host string) string { return f.parent[host] }

// cascade40 builds a 40-member storm: ONE causal parent (a hypervisor) plus 39 guests that run on it. The
// parent's ref sorts LAST (zz-...) and the guests' refs sort first (guest-00..guest-38), so:
//
//   - The cluster EXCEEDS MaxMembers (40 > 32) — the vacuity guard. Verdict.Members is sorted asc and
//     truncated at 32, so it keeps guest-00..guest-31 and DROPS the parent entirely. An election that read
//     the truncated audit list could not even SEE the parent, so a test on a 5-member never-truncated cluster
//     would prove nothing about whether truncation was handled. Elect must read the full member set.
//   - The causal parent is the one to elect (highest estate in-degree), and it is NOT the lexicographically
//     first ref (that is guest-00) — so "elect sorted[0]" is a wrong answer this fixture distinguishes.
//
// Every member arrives inside the span, parent first, so all 40 see one another. Returns the parent ref, the
// window (all 40 observations), and the topology oracle.
func cascade40(base time.Time, span time.Duration) (parentRef string, w Window, topo fakeTopology) {
	const parentHost = "dc1pve03"
	parentRef = "zz-pve03-node-down" // sorts AFTER every guest-NN ref, and past the MaxMembers=32 cut
	topo = fakeTopology{inDeg: map[string]int{}, parent: map[string]string{}}

	obs := make([]Observation, 0, 40)
	// The parent fires FIRST (a cascade's root goes down before its guests notice).
	obs = append(obs, Observation{ExternalRef: parentRef, Host: parentHost, SourceType: "librenms", AlertRule: "pve-node-down", At: base})
	topo.inDeg[parentHost] = 39 // 39 guests depend on it — the causal weight the election reads
	for i := 0; i < 39; i++ {
		host := fmt.Sprintf("dc1vm%02d", i)
		ref := fmt.Sprintf("guest-%02d-down", i)
		// Guests trickle in over the first fifth of the span — all still well within it.
		at := base.Add(time.Duration(i+1) * span / 200)
		obs = append(obs, Observation{ExternalRef: ref, Host: host, SourceType: "librenms", AlertRule: "guest-down", At: at})
		topo.parent[host] = parentHost // each guest runs_on the parent
		topo.inDeg[host] = 0
	}
	return parentRef, Window{Span: span, Observations: obs}, topo
}

// THE KILLING TEST for the CAUSAL ELECTION (TG-385/TG-376): over a 40-member cascade whose causal parent's
// ref sorts LAST and past the MaxMembers cap, Elect must choose the PARENT — the node the most members
// depend on — as the one subject to investigate, from EVERY member's point of view (the result must not
// depend on which member happens to be the subject of the call).
//
// KILLING MUTATION 1 (executed, RED): replace the causal tie-break with `return sorted-by-ref[0]`. The
// elected subject becomes guest-00 (lexicographically first), the assertion `elected == parent` goes RED,
// and the whole point — investigate the CAUSE, not the alphabetically-first symptom — is lost.
//
// VACUITY GUARD: the fixture has 40 members > MaxMembers(32), and the parent is the one member truncated OUT
// of Verdict.Members. So this test also fails if Elect ever reads the truncated audit list instead of the
// full set — a 5-member cluster could never catch that.
func TestElect_CausalParentOverLexicographic_40Members(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	span := 10 * time.Minute
	parentRef, w, topo := cascade40(base, span)

	// VACUITY FLOOR — the fixture must actually exceed the cap, or the truncation half of the guard is moot.
	if got := len(w.Observations); got != 40 {
		t.Fatalf("fixture built %d members, want 40 (the >MaxMembers=%d vacuity guard)", got, MaxMembers)
	}
	// The parent MUST be truncated out of the verdict's audit list, or "election reads the full set" is
	// untested: if the parent survived into Verdict.Members, an election off the truncated list could still
	// pass by luck.
	v := Assess(w.Observations[10], w) // any member as subject; the verdict's Members is sorted+capped
	if len(v.Members) != MaxMembers {
		t.Fatalf("verdict carried %d members, want the cap %d — fixture not exercising truncation", len(v.Members), MaxMembers)
	}
	for _, m := range v.Members {
		if m == parentRef {
			t.Fatalf("the causal parent %q survived into the %d-capped audit list — the vacuity guard needs it "+
				"truncated OUT so electing from that list is provably impossible", parentRef, MaxMembers)
		}
	}

	// Elect from EVERY member's point of view — the elected subject must be the parent, every time, by the
	// estate-indegree rule. Subject-independence is the property the collapse relies on: whichever member's
	// workflow runs the election, they must all agree on who investigates.
	elected := map[string]int{}
	for _, subj := range w.Observations {
		el := Elect(subj, w, topo)
		elected[el.Elected]++
		if el.Elected != parentRef {
			t.Fatalf("subject %q elected %q, want the causal parent %q — the election is not choosing the node "+
				"the cascade fell from (rule=%s runnerUp=%q)", subj.ExternalRef, el.Elected, parentRef, el.Rule, el.RunnerUp)
		}
		if el.Rule != ElectRuleIndegree {
			t.Fatalf("elected the parent by rule %q, want %q — the estate in-degree is what makes this causal, "+
				"and recording the wrong rule makes a right answer unreviewable", el.Rule, ElectRuleIndegree)
		}
	}
	if len(elected) != 1 {
		t.Fatalf("the 40 members did not agree on ONE subject: %v — the collapse would open more than one session", elected)
	}
}

// The estate tie-break comes FIRST, but the fixture also proves the chain falls through correctly: with the
// topology removed (a deployment whose estate graph is not yet seeded), the election must still pick ONE
// subject deterministically — the earliest arrival — so the storm still collapses to one session.
//
// KILLING MUTATION (documented): drop the earliest-arrival tie-break so ties fall straight to ref order —
// the elected subject flips from the parent (which arrived first) to guest-00 (lexicographically first),
// which is the wrong subject for a topology-blind deployment and reddens `elected == parentRef` here.
func TestElect_FallsBackToEarliestArrival_NoTopology(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	span := 10 * time.Minute
	parentRef, w, _ := cascade40(base, span)

	el := Elect(w.Observations[5], w, nil) // nil topology — no in-degree, no parent
	if el.Elected != parentRef {
		t.Fatalf("with no topology the election picked %q, want the earliest arrival %q — the parent fired "+
			"first, and earliest-arrival is the honest topology-blind fallback (rule=%s)", el.Elected, parentRef, el.Rule)
	}
	if el.Rule != ElectRuleEarliest {
		t.Fatalf("topology-blind election recorded rule %q, want %q", el.Rule, ElectRuleEarliest)
	}
}

// The second tie-break (parent-fanout) is reachable when in-degree does NOT separate the members — e.g. a
// deployment whose graph carries runs_on containment but no dependents indexed as in-degree. Here every
// member has in-degree 0 but the parent still runs_on-parents the guests, so fanout elects it.
func TestElect_ParentFanoutTieBreak(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	span := 10 * time.Minute
	parentRef, w, topo := cascade40(base, span)
	topo.inDeg = map[string]int{} // erase in-degree, keep runs_on parents ⇒ the (ii) rule must carry it

	// Make a GUEST arrive earliest so the earliest-arrival fallback would pick the wrong subject; only the
	// parent-fanout rule elects the parent here.
	w.Observations[1].At = base.Add(-time.Minute) // guest-00 now earliest
	el := Elect(w.Observations[0], w, topo)
	if el.Elected != parentRef {
		t.Fatalf("parent-fanout election picked %q, want the parent %q (rule=%s) — the member whose host "+
			"parents the most others is the causal subject when in-degree is flat", el.Elected, parentRef, el.Rule)
	}
	if el.Rule != ElectRuleParentFanout {
		t.Fatalf("recorded rule %q, want %q", el.Rule, ElectRuleParentFanout)
	}
}

// ClusterAnchor is the durable identity's key: the storm's FIRST-SEEN alert, computed identically from ANY
// member's point of view, so two members that both see the origin collide on one cluster row. This is the
// half that stops the cluster from being a function of arrival order.
//
// KILLING MUTATION 2 lives against the DB side (core/db) where "all members share ONE cluster id" is
// asserted over persisted rows; here we prove the pure key is subject-independent and time-bucketed.
func TestClusterAnchor_IsSubjectIndependent(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	span := 10 * time.Minute
	parentRef, w, _ := cascade40(base, span)

	// The parent fired first, so it is the anchor — and every member computes the SAME anchor + bucket.
	var wantBucket int64
	for i, subj := range w.Observations {
		ref, at := ClusterAnchor(subj, w)
		if ref != parentRef {
			t.Fatalf("subject %q computed anchor %q, want the first-seen ref %q — a per-subject anchor is exactly "+
				"the arrival-time dependence TG-385 removes", subj.ExternalRef, ref, parentRef)
		}
		b := ClusterBucket(at)
		if i == 0 {
			wantBucket = b
		} else if b != wantBucket {
			t.Fatalf("subject %q bucketed the anchor to %d, want %d — the durable key must not vary by who asks", subj.ExternalRef, b, wantBucket)
		}
	}
	if wantBucket == 0 {
		t.Fatal("anchor bucket computed as 0 for a non-zero time — the key would collide every storm into bucket 0")
	}
}
