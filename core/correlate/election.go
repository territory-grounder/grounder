package correlate

import (
	"sort"
	"strings"
	"time"
)

// This file is the CAUSAL ELECTION and the DURABLE CLUSTER KEY — the two halves TG-385/TG-376 add on top of
// the TG-169 correlation stage so a detected cascade collapses to ONE investigation instead of one per
// member.
//
// THE DEFECT. Correlation (Assess, correlate.go) correctly detected the 2026-08-06 pve03 cascade — 159
// decisions tagged correlated, peak 111 members / 54 hosts — but every member opened its own session (157
// alerts -> 157 sessions, 1.000 alerts/session, zero collapse). Two things were missing and are added here:
//
//   - No DURABLE cluster identity. Each subject recomputed its own +/-span window at its own arrival instant,
//     so the "cluster" was a function of WHEN a subject arrived, not of what broke. ClusterAnchor +
//     ClusterBucket give a storm ONE stable key (its first-seen alert) that every member joins, so all
//     members of one storm share one cluster id regardless of arrival order (persisted in alert_cluster,
//     migration 0085; joined by core/db.AlertClusterStore.Join).
//
//   - No ELECTION rule. Nothing said which member is the CAUSAL SUBJECT to investigate. Elect states it in
//     terms of causal position — the node the most things fall with — with an explicit tie-break chain, and
//     reads the FULL member set (never the MaxMembers-truncated audit list, so a causal parent whose ref
//     sorts past the cap is still electable).

// Topology is the estate-graph oracle the causal election reads. It is a deliberately narrow interface so
// core/correlate imports no estate/topology package and stays pure and unit-testable with a fake — the same
// discipline that keeps Assess a database-free rule. An adapter over the live estate graph
// (temporal/runner.GraphTopology) satisfies it in production; a map-backed fake satisfies it in tests.
type Topology interface {
	// InDegree reports how many DISTINCT estate entities depend on host — the causal weight "how many things
	// fall if this falls". A hypervisor carrying dozens of guests scores high; a guest scores ~none. An
	// unknown/unplaced host returns 0 (no claim of centrality, never a negative one).
	InDegree(host string) int
	// RunsOnParent returns the canonical runs_on parent (the cascading infra node host is placed on), or ""
	// when the graph knows none. Used by the second tie-break: the member whose host parents the most others.
	RunsOnParent(host string) string
}

// The controlled ELECTION-RULE vocabulary: which criterion in the tie-break chain actually decided the
// subject over the runner-up. Persisted on the routing decision (exec_class_decision.elect_rule) so a wrong
// election is reviewable rather than silent — never free text, so the population is groupable.
const (
	// ElectRuleIndegree: (i) the elected member's host has the highest ESTATE in-degree — the causal root of
	// the cascade (the thing the most members depend on). This is the primary, topology-grounded rule.
	ElectRuleIndegree = "estate-indegree"
	// ElectRuleParentFanout: (ii) in-degree did not separate them, but the elected member's host is the
	// runs_on PARENT of more of the OTHER members than any rival — a within-cluster containment measure.
	ElectRuleParentFanout = "cluster-parent-fanout"
	// ElectRuleEarliest: (iii) topology said nothing (both zero, both equal) — the elected member simply
	// ARRIVED FIRST. The honest fallback for a deployment whose estate graph is not yet seeded.
	ElectRuleEarliest = "earliest-ref"
	// ElectRuleRefOrder: everything above tied; the external_ref total order breaks it, so the election is
	// still deterministic (a replay elects the same subject forever).
	ElectRuleRefOrder = "ref-order"
	// ElectRuleSole: the cluster has one member — it is trivially its own subject.
	ElectRuleSole = "sole-member"
)

// IsCausalRule reports whether an election was decided by CAUSAL evidence — estate in-degree or runs_on
// parent-fanout — as opposed to a non-causal fallback (earliest arrival, ref order, or a sole member).
//
// IT IS A SILENCING SAFEGUARD, not a cosmetic distinction. The collapse (TG-376) DROPS the other members'
// investigations, so a wrong collapse SILENCES a real incident. A cluster elected by earliest-ref alone is a
// TIME COINCIDENCE — three hosts that merely alerted together, which is the exact shape of a TG-169
// false positive — and time-coincidence is not sufficient grounds to silence two genuine incidents. So the
// collapse gate demands a causal anchor: only a cluster whose subject was chosen by in-degree or parent-fanout
// collapses; a non-causal election leaves every member to investigate (the safe status quo). This makes the
// collapse effective for a genuine cascade (pve03 guests run_on the node ⇒ causal election ⇒ collapse) and
// inert-and-safe when the graph is unseeded and there is no causal anchor. The elect_rule is still recorded
// on the decision row either way, so the audit trail is unchanged.
func IsCausalRule(rule string) bool {
	return rule == ElectRuleIndegree || rule == ElectRuleParentFanout
}

// Election is the causal-subject decision for a cluster: which member investigates, the runner-up it was
// chosen over, and the controlled rule that decided. Everything is non-secret identifiers, safe to persist.
type Election struct {
	// Elected is the external_ref of the causal subject — the ONE member that opens an investigation session
	// (TG-376). Empty only for an empty member set.
	Elected string
	// ElectedHost is that subject's host (carried for the record and for the caller's evidence attachment).
	ElectedHost string
	// RunnerUp is the external_ref the election ranked second — recorded so "why THIS subject" is auditable.
	RunnerUp string
	// Rule is one of the ElectRule* constants: which tie-break decided Elected over RunnerUp.
	Rule string
}

// Elect chooses the cluster's CAUSAL investigation subject over the FULL, de-duplicated member set. It reads
// dedupMembers directly, NOT Verdict.Members — the MaxMembers cap is an audit-blob bound, and electing from a
// truncated list would make a causal parent whose ref sorts past the cap un-electable (exactly the failure a
// 40-member cluster whose parent sorts LAST is built to catch).
//
// THE TIE-BREAK CHAIN, stated in terms of causal position (TG-375 restored the containment edges this reads):
//
//	(i)  highest ESTATE IN-DEGREE among the members' hosts — the node the most things depend on; else
//	(ii) the member whose host is the runs_on PARENT of the most OTHER members; else
//	(iii) the EARLIEST-arriving member; else
//	     the external_ref order (a final total order, so the result is always deterministic).
//
// The comparator is lexicographic over (in-degree desc, parent-fanout desc, arrival asc, ref asc): each
// criterion is consulted only when the ones above it TIE, which is exactly what "else" means. The winner is
// the maximum; the runner-up is the second; Rule is the first criterion on which they differ. topo may be
// nil (no estate graph wired) — the topology criteria then contribute nothing and the election falls back to
// earliest-arrival, which still collapses the storm to one session, just not necessarily onto the causal root.
func Elect(subject Observation, w Window, topo Topology) Election {
	ms := dedupMembers(subject, w)
	if len(ms) == 0 {
		return Election{}
	}
	if len(ms) == 1 {
		return Election{Elected: ms[0].ExternalRef, ElectedHost: ms[0].Host, Rule: ElectRuleSole}
	}

	inDeg := func(h string) int {
		if topo == nil || h == "" {
			return 0
		}
		return topo.InDegree(h)
	}
	parentOf := func(h string) string {
		if topo == nil || h == "" {
			return ""
		}
		return topo.RunsOnParent(h)
	}

	// Per-member causal scores, computed ONCE over the full set.
	type scored struct {
		obs    Observation
		inDeg  int
		fanout int // how many OTHER members run on this member's host
	}
	sc := make([]scored, len(ms))
	for i := range ms {
		sc[i] = scored{obs: ms[i], inDeg: inDeg(ms[i].Host)}
	}
	for i := range sc {
		hk := hostKey(sc[i].obs.Host)
		if hk == "" {
			continue
		}
		for j := range ms {
			if j == i {
				continue
			}
			if hostKey(parentOf(ms[j].Host)) == hk {
				sc[i].fanout++
			}
		}
	}

	// less reports whether a ranks STRICTLY BEFORE b in the tie-break chain (a is the more causal subject).
	less := func(a, b scored) bool {
		if a.inDeg != b.inDeg {
			return a.inDeg > b.inDeg
		}
		if a.fanout != b.fanout {
			return a.fanout > b.fanout
		}
		if !a.obs.At.Equal(b.obs.At) {
			return a.obs.At.Before(b.obs.At)
		}
		return a.obs.ExternalRef < b.obs.ExternalRef
	}
	sort.SliceStable(sc, func(i, j int) bool { return less(sc[i], sc[j]) })

	winner, runnerUp := sc[0], sc[1]
	rule := ElectRuleRefOrder
	switch {
	case winner.inDeg != runnerUp.inDeg:
		rule = ElectRuleIndegree
	case winner.fanout != runnerUp.fanout:
		rule = ElectRuleParentFanout
	case !winner.obs.At.Equal(runnerUp.obs.At):
		rule = ElectRuleEarliest
	}
	return Election{
		Elected:     winner.obs.ExternalRef,
		ElectedHost: winner.obs.Host,
		RunnerUp:    runnerUp.obs.ExternalRef,
		Rule:        rule,
	}
}

// ClusterBucketWidth is how coarsely a cluster's anchor time is quantized for the durable key
// (window_bucket). Two members of one storm see the SAME first-seen alert inside their windows, so they
// already compute an identical anchor timestamp and land in the same bucket at any width; the bucket exists
// to give the (window_bucket, first_seen_ref) key numeric time-locality for indexing and to bound a future
// straddle-tolerant probe. It is a constant, not config, for the same reason the cluster thresholds are: it
// is part of what "one storm" MEANS, not a per-deployment knob.
const ClusterBucketWidth = 5 * time.Minute

// ClusterAnchor returns the storm's FIRST-SEEN alert over the full member set — the earliest arrival, ties
// broken by external_ref — as the cluster's durable identity (first_seen_ref, first_seen_at). Every member
// that can see the storm's origin inside its window computes the SAME anchor, so they collide on one
// alert_cluster row and share one id; two independent storms carry different anchors and stay separate. An
// empty member set returns the zero value.
func ClusterAnchor(subject Observation, w Window) (ref string, at time.Time) {
	ms := dedupMembers(subject, w)
	var anchor Observation
	found := false
	for _, o := range ms {
		if !found || o.At.Before(anchor.At) || (o.At.Equal(anchor.At) && o.ExternalRef < anchor.ExternalRef) {
			anchor = o
			found = true
		}
	}
	if !found {
		return "", time.Time{}
	}
	return anchor.ExternalRef, anchor.At
}

// ClusterBucket quantizes an anchor time to the durable window bucket (whole ClusterBucketWidth intervals
// since the epoch, in UTC). Deterministic and pure.
func ClusterBucket(at time.Time) int64 {
	if at.IsZero() {
		return 0
	}
	width := int64(ClusterBucketWidth / time.Second)
	if width <= 0 {
		width = 1
	}
	return at.UTC().Unix() / width
}

// hostKey is the light canonical form correlate uses to compare host names WITHOUT importing the estate
// package's canonName: domain-stripped and lower-cased, so "web01.dc1" and "WEB01" compare equal and a
// parent name returned by the topology oracle matches a member's host. It is intentionally simpler than the
// estate resolver (no alias/fuzzy tiers) — the election only needs equality between names that already refer
// to the same machine, and the oracle returns names in the graph's own canonical vocabulary.
func hostKey(name string) string {
	n := strings.TrimSpace(name)
	if i := strings.IndexByte(n, '.'); i >= 0 {
		n = n[:i]
	}
	return strings.ToLower(n)
}
