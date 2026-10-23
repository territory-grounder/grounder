// Package forensic assembles a CROSS-INCIDENT narrative from the corpora TG already writes.
//
// WHAT EXISTS AND WHAT DOES NOT. Per-incident reconstruction is complete and live: core/trace's Assemble
// joins eleven corpora for ONE external_ref and the console renders it. What has never existed is a view
// across incidents — nothing in the tree takes a time window (or a host, or a subject) and returns an
// ordered narrative spanning many sessions. Measured 2026-08-07: 15 registered Temporal workflows, none
// forensic; grep for timeline/IOC across non-test Go finds only Matrix room events and a per-key
// raise/clear walk. TG-168 calls the governance ledger "an untapped forensic corpus" and it is right —
// 9,719 hash-chained rows, of which only ~968 are boot-time reports.
//
// WHY THIS PACKAGE IS PURE. Merge takes per-corpus event slices and returns one stably-ordered stream. It
// holds no database handle, no clock and no model. That is deliberate: the interesting failure here is
// ORDERING — two corpora writing the same instant, a corpus with a coarser timestamp, a row with no time
// at all — and none of those need Postgres to provoke. The reader that fills these slices is a separate,
// bound, read-only concern.
//
// NOT IN SCOPE HERE, deliberately: IOC extraction, decoy-vs-real separation, and any model call. TG-168's
// own 2026-08-06 comment identifies deterministic corpus assembly as the half that is buildable without
// inference, and it is "what any model would consume anyway".
package forensic

import (
	"sort"
	"strings"
	"time"
)

// Source names the corpus an Event came from. A closed set, because the ordering tie-break depends on it
// and an unknown source would sort unpredictably — see Merge.
type Source string

const (
	SourceIngest     Source = "ingest_alert"
	SourceLedger     Source = "governance_ledger"
	SourceAgentStep  Source = "agent_step"
	SourceCredential Source = "credential_resolution"
	SourceExecClass  Source = "exec_class_decision"
)

// sourceRank fixes the tie-break order when two events share an instant. It is CAUSAL, not alphabetical:
// an alert arrives, it is classified, the agent investigates, credentials resolve, and the ledger records
// the decision. Two rows written in the same second by different corpora are almost always that sequence,
// so rendering them in this order reads as what happened rather than as an accident of naming.
var sourceRank = map[Source]int{
	SourceIngest:     0,
	SourceExecClass:  1,
	SourceAgentStep:  2,
	SourceCredential: 3,
	SourceLedger:     4,
}

// Event is one thing that happened, projected to NON-SECRET fields only (INV-13).
//
// Every field is a projection the corpus already exposes to the console or the trace. Nothing here is a
// credential, a key reference's value, or a model-authored claim presented as fact — Detail is prose the
// corpus itself wrote, and Actor is an attributed identity, never an assertion parsed out of narrative.
type Event struct {
	At     time.Time
	Source Source
	// Host is the estate host the event concerns, where the corpus records one. Empty is normal: a policy
	// decision is about an action, not a machine.
	Host string
	// SubjectRef is the external_ref this event belongs to, so a cross-incident stream can still be
	// grouped back into incidents without a second query.
	SubjectRef string
	// Actor is who acted, where an attributed identity exists. Never a model claim (INV-08).
	Actor string
	// Kind is the corpus's own verb — "classify:AUTO", "actuate:refuse", "agent-cycle". Kept verbatim
	// rather than mapped to a shared vocabulary: a lossy re-labelling is how a reader ends up reasoning
	// about a category the corpus never asserted.
	Kind   string
	Detail string
}

// Merge folds per-corpus slices into ONE stably-ordered stream.
//
// DETERMINISM IS THE POINT, not a nicety. A forensic narrative that renders differently on two runs over
// the same window cannot be cited — an operator comparing today's reconstruction with yesterday's would
// see differences that are artefacts of map iteration rather than of the estate. So the order is total:
// time, then the causal source rank, then subject ref, then kind, then detail. Every tie is broken by a
// field, never left to sort stability or input order.
//
// ZERO-TIME EVENTS ARE DROPPED, and that is a decision worth stating. A row whose timestamp did not
// resolve cannot be placed on a timeline; rendering it at the epoch would put it first, ahead of
// everything, and an operator reading a timeline trusts position to mean sequence. Dropping is
// destructive but honest, and the caller can count the difference between what it passed and what came
// back — which is why Merge returns the dropped count rather than swallowing it.
func Merge(groups ...[]Event) (stream []Event, dropped int) {
	for _, g := range groups {
		for _, e := range g {
			if e.At.IsZero() {
				dropped++
				continue
			}
			stream = append(stream, e)
		}
	}
	sort.SliceStable(stream, func(i, j int) bool {
		a, b := stream[i], stream[j]
		if !a.At.Equal(b.At) {
			return a.At.Before(b.At)
		}
		if ra, rb := rankOf(a.Source), rankOf(b.Source); ra != rb {
			return ra < rb
		}
		if a.SubjectRef != b.SubjectRef {
			return a.SubjectRef < b.SubjectRef
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Detail < b.Detail
	})
	return stream, dropped
}

// rankOf places an unknown source AFTER every known one rather than at 0. An unrecognised corpus is new
// or misspelled; sorting it first would silently reorder the causal sequence the known ranks encode.
func rankOf(s Source) int {
	if r, ok := sourceRank[s]; ok {
		return r
	}
	return len(sourceRank)
}

// Window bounds a reconstruction. Half-open [From, To): an event exactly at To belongs to the next
// window, so two adjacent windows tile without double-counting a boundary event — the property that makes
// paging through a long period safe.
type Window struct {
	From time.Time
	To   time.Time
}

// Contains reports whether an instant falls in the window.
func (w Window) Contains(t time.Time) bool {
	if t.Before(w.From) {
		return false
	}
	return w.To.IsZero() || t.Before(w.To)
}

// Valid reports whether the window is usable. A zero From is refused: an unbounded reconstruction over
// 9,719 ledger rows and 18,736 agent steps is not a forensic answer, it is a data dump, and the caller
// almost certainly meant a window it forgot to set.
func (w Window) Valid() bool {
	if w.From.IsZero() {
		return false
	}
	return w.To.IsZero() || w.To.After(w.From)
}

// Hosts returns the distinct hosts in a stream, sorted. Useful as the blast-radius line of a narrative:
// "between T1 and T2 these nine hosts appear" is the first question an operator asks.
func Hosts(stream []Event) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range stream {
		h := strings.TrimSpace(e.Host)
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}
