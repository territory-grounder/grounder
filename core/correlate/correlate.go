// Package correlate is the incident CORRELATION stage: it decides whether an incident is one system in
// trouble or SEVERAL, from the alerts TG itself admitted around it — cross-source, cross-host, and
// time-windowed. Its answer is the `Correlated` signal the execution-topology classifier routes on
// (core/execclass.Input.Correlated).
//
// WHAT IT REPLACES, AND WHY THAT WAS NOT CORRELATION AT ALL (TG-169). The flag was set by one line in the
// workflow:
//
//	if env.Severity == ingest.SeverityCritical { in.Correlated = true }
//
// Severity is a property of ONE alert. It cannot answer a question about the RELATIONSHIP between alerts,
// and the live numbers show what happens when it is asked to: 2,434 of 2,995 admitted alerts are critical,
// so 81% of incidents asserted that they "span multiple systems" — a claim TG had no evidence for, on a
// record an operator is expected to review. The reverse error is the expensive one. A genuine multi-host
// cascade assembled from WARNINGS — many weak signals, no single critical, which is the shape of a real
// compromise unfolding — set Correlated=false and was routed to the CHEAPEST reasoning available.
//
// The rules below are the smallest honest statement of "this looks like more than one system":
//
//   - MULTI-HOST: three or more DISTINCT hosts alerting inside one window. Two hosts is ordinary estate
//     noise in a homelab this size; three simultaneous is a pattern.
//   - CROSS-SOURCE: two or more distinct INGEST SOURCES reporting trouble on two or more distinct hosts.
//     Two independent observers is stronger evidence than three hosts seen by one noisy poller, so it
//     clears at a lower host count — this is the rule the security telemetry connectors (EDR, auth logs,
//     Falco, osquery — a SEPARATE ticket, none of them wired yet) will land into, since their whole value
//     is being a second, independent witness to what the availability plane already sees.
//
// SEVERITY IS DELIBERATELY NOT AN INPUT. It rides along on each Observation for the persisted record and
// for a human reading it afterwards, and no rule consults it. Re-admitting it would re-create exactly the
// conflation this package exists to end: a lone critical is one system in trouble, and three warning hosts
// are a cascade, whatever their labels say.
//
// THE STAGE IS PURE AND DETERMINISTIC (INV-08): no model token decides the topology, and the same window
// always yields the same verdict, so a Temporal replay and an after-the-fact review agree. Its ROUTING is
// FAIL-SAFE UP — `Correlated` only ever routes the CLUSTER's investigation to the more thorough class
// (execclass routes Correlated ⇒ DEEP_INVESTIGATION), never to a shortcut, so a permissive window is a cost
// and a missed one is a risk. The caller supplies the missing fact when the window could not be read at all
// (see Verdict.Reason / ReasonUnavailable) rather than letting this package invent one.
//
// THE COLLAPSE (TG-376) IS THE ONE PLACE CORRELATION CAN DROP AN INVESTIGATION, AND IT IS GUARDED. A
// correlated member elected the cluster's CAUSAL subject (by estate in-degree or runs_on parent-fanout —
// see election.go / IsCausalRule) is investigated ONCE on the cluster's behalf; its co-members attach as
// evidence and open no session. But a cluster with NO causal anchor — elected only by earliest-ref because
// the estate graph is unseeded — is a TIME COINCIDENCE, and silencing a real incident on time-coincidence
// alone is the wrong direction. So a non-causal cluster is NOT collapsed: every member investigates, exactly
// as before. Collapse is thus fail-safe too — it drops a session only with causal evidence of who the parent is.
package correlate

import (
	"sort"
	"time"
)

// Observation is one admitted alert as the correlator sees it — the NON-SECRET projection of an
// `ingest_alert` row (migration 0033), which is the only durable, append-only record of what the front
// door actually admitted. Never a raw payload, never a credential (INV-13).
//
// At is the FRONT-DOOR ARRIVAL time (received_at), not the provider's own observed_at, and that is the
// deliberate choice: the window is a statement about what TG saw together, and provider clocks skew
// independently of each other. Two sources whose clocks disagree by minutes would otherwise fall out of
// one another's windows precisely when the cross-source rule is the one that matters.
type Observation struct {
	ExternalRef string
	SourceType  string
	Host        string
	AlertRule   string
	Severity    string
	At          time.Time
}

// Window is the evidence the correlator was given: the span it was asked for, and the observations found
// inside it. Span is carried WITH the observations so the persisted decision records the question that was
// asked, not only the answer — a verdict of "isolated" over a 30-second window and one over a 10-minute
// window are different claims, and a reviewer cannot tell them apart from the observation list alone.
type Window struct {
	Span         time.Duration
	Observations []Observation
}

// The cluster thresholds. Constants rather than configuration on purpose: they are the DEFINITION of what
// this stage calls a cascade, and a per-deployment knob able to lower them is a way to silently return to
// "everything is correlated" — the state TG-169 exists to leave. The window SPAN is the operator-tunable
// dimension (it is set at the composition root and travels on Window.Span), because that is a property of
// how fast the estate's failures propagate, not of what the word "cascade" means.
const (
	// MinClusterHosts is how many DISTINCT hosts must alert inside the window to call it multi-host.
	MinClusterHosts = 3
	// MinCrossSourceHosts / MinCrossSources are the cross-source rule: two independent ingest sources
	// reporting trouble on two distinct hosts.
	MinCrossSourceHosts = 2
	MinCrossSources     = 2
	// MaxMembers bounds the member-ref list carried on the verdict (and thence into the persisted
	// decision row). A wide cascade must not write an unbounded blob into an append-only table.
	MaxMembers = 32
)

// The controlled reason vocabulary. A reason is a CATEGORY, never free text: it is persisted on the
// routing decision and read by a human (and by any later aggregate over "why did TG go deep"), so it has
// to be groupable.
const (
	// ReasonIsolated: the window held one system's trouble. NOT correlated.
	ReasonIsolated = "isolated"
	// ReasonMultiHost: MinClusterHosts or more distinct hosts inside the window.
	ReasonMultiHost = "multi-host-window"
	// ReasonCrossSource: MinCrossSources or more distinct ingest sources across MinCrossSourceHosts or
	// more distinct hosts.
	ReasonCrossSource = "cross-source-window"
	// ReasonUnavailable: the window could NOT be read (no reader wired, or the read failed). The
	// correlator produced no answer at all and the caller fell back — see the note on Verdict.Degraded.
	ReasonUnavailable = "window-unavailable"
)

// Verdict is the correlation stage's answer plus the evidence it rests on. Everything here is
// non-secret identifiers and counts, so the whole struct is safe to persist and to show an operator.
type Verdict struct {
	// Correlated is the signal execclass.Input.Correlated is fed from.
	Correlated bool
	// Reason is one of the controlled constants above.
	Reason string
	// Span is the window the verdict was reached over (echoed from Window.Span).
	Span time.Duration
	// Hosts / Sources are the DISTINCT non-empty hosts and ingest sources found in the window, sorted.
	Hosts   []string
	Sources []string
	// Members are the external_refs of the alerts in the cluster, sorted and capped at MaxMembers —
	// the audit trail for "which alerts made this incident look multi-system".
	Members []string
	// MemberCount is the FULL member count before the MaxMembers cap, so a truncated list is still
	// honest about how large the cluster was.
	MemberCount int
	// Degraded is true only for a verdict the correlator did not actually reach (ReasonUnavailable).
	// It is NOT a synonym for "not correlated": the difference between "TG looked and saw one system"
	// and "TG could not look" is exactly the difference a reviewer needs, and collapsing them is how a
	// broken reader becomes indistinguishable from a quiet estate.
	Degraded bool
}

// Assess is the correlation rule. It is pure: same subject + same window ⇒ same verdict, forever.
//
// The subject is counted as a member of its own cluster — an incident is part of the picture it is being
// assessed against. Dedup is by external_ref, so the subject counts once even though the window normally
// already contains its own front-door row (that overlap is deliberate; see the ordering note in the body),
// and a re-delivered webhook collapses for the same reason.
//
// A host that fires five different rules contributes ONE host, and a source that fires fifty alerts
// contributes ONE source. That is what makes this a statement about BREADTH rather than about volume: a
// single chatty host cannot manufacture a cascade, which is the failure mode a naive alert COUNT would
// have (and which the old severity heuristic had in a different disguise).
func Assess(subject Observation, w Window) Verdict {
	v := Verdict{Reason: ReasonIsolated, Span: w.Span}

	hosts := map[string]bool{}
	sources := map[string]bool{}
	ms := dedupMembers(subject, w)
	members := make([]string, 0, len(ms))
	for _, o := range ms {
		members = append(members, o.ExternalRef)
		if o.Host != "" {
			hosts[o.Host] = true
		}
		if o.SourceType != "" {
			sources[o.SourceType] = true
		}
	}

	v.Hosts = sortedKeys(hosts)
	v.Sources = sortedKeys(sources)
	sort.Strings(members)
	v.MemberCount = len(members)
	if len(members) > MaxMembers {
		members = members[:MaxMembers]
	}
	v.Members = members

	switch {
	case len(v.Hosts) >= MinClusterHosts:
		v.Correlated, v.Reason = true, ReasonMultiHost
	case len(v.Sources) >= MinCrossSources && len(v.Hosts) >= MinCrossSourceHosts:
		v.Correlated, v.Reason = true, ReasonCrossSource
	}
	return v
}

// Unavailable is the verdict for a session whose window could not be read at all. It carries the caller's
// fallback answer for Correlated rather than inventing one, and marks itself Degraded so the persisted
// record says "TG could not look" instead of quietly claiming "TG looked and saw nothing".
func Unavailable(correlated bool) Verdict {
	return Verdict{Correlated: correlated, Reason: ReasonUnavailable, Degraded: true}
}

// dedupMembers is the cluster's FULL, de-duplicated member set — the observations Assess counts breadth
// over and Elect (election.go) elects a causal subject from. It is extracted into ONE place so those two
// can never disagree about who is in the cluster: the verdict's MaxMembers-truncated audit list and the
// election's causal ranking are both derived from THIS set, and the election reads the untruncated slice
// directly (the truncation is an audit-blob bound, never an input to who investigates — TG-385).
//
// The order is load-bearing, not stylistic (unchanged from the original Assess): the window is admitted
// FIRST, then the subject, and dedup-by-ref lets whichever copy arrives first win. The front door has
// already written the subject's own ingest_alert row (source_type included) by the time this runs, so the
// window normally CONTAINS the subject; letting the durable row win keeps every source slug in ONE
// vocabulary — the caller holds only the authenticated ingest SourceID ("prometheus-dc1"), not a
// source_type ("librenms"), so feeding it in would make one observer look like two. When the durable row
// is genuinely missing, the subject still contributes its HOST, so the stage UNDER-counts breadth rather
// than inventing a second witness. The DEFENSIVE window bound keeps membership a function of the arguments
// alone, so the pure oracle and the SQL cannot silently disagree about "inside the window"; a zero Span
// disables it (a caller that supplied observations without declaring a span gets them all).
func dedupMembers(subject Observation, w Window) []Observation {
	seenRef := map[string]bool{}
	out := make([]Observation, 0, len(w.Observations)+1)
	admit := func(o Observation) {
		if o.ExternalRef == "" || seenRef[o.ExternalRef] {
			return
		}
		if w.Span > 0 && !subject.At.IsZero() && !o.At.IsZero() {
			d := o.At.Sub(subject.At)
			if d < 0 {
				d = -d
			}
			if d > w.Span {
				return
			}
		}
		seenRef[o.ExternalRef] = true
		out = append(out, o)
	}
	for _, o := range w.Observations {
		admit(o)
	}
	admit(subject)
	return out
}

func sortedKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
