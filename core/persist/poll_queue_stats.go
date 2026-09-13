package persist

import (
	"sort"
	"strings"
	"time"
)

// THE HUMAN GATE HAS NO DEPTH GAUGE (TG-173, OWASP Agentic T10 — overwhelming-HITL).
//
// TG's rate governor answers load by clamping auto -> APPROVE. It routes MORE decisions to the human under
// exactly the conditions where the human is least able to keep up, and nothing measures the result. Counted
// on 2026-08-06: 47 published tg_* metric families, and not one of them is the depth of the poll queue —
// the queue that every residual safety property of Full-auto rests on.
//
// CountOpen already exists and is served on /v1/stats as pending_polls. That is a number rendered to an
// operator who is ALREADY LOOKING at the console. The failure this addresses is the operator not knowing to
// look: a genuine alert storm, or an attacker manufacturing many low-value in-grammar proposals, degrades
// the human gate into rubber-stamping and hides one real malicious proposal in the noise. A count visible
// only on the page nobody has open is not an instrument.
//
// WHY THE SHAPE RATIO IS THE SIGNAL, NOT THE DEPTH. Depth alone cannot separate a busy estate from a flood:
// 200 open polls across 200 distinct proposals is a real incident and the queue is doing its job; 200 open
// polls across 3 distinct proposals is one fault fanning out, and reviewing them individually is a waste of
// the only reviewer. The second is also what a manufactured flood looks like, because manufacturing variety
// is harder than manufacturing volume. Publishing both, plus the largest single shape, lets one rule say
// "the operator is being drowned by repetition" rather than "the operator is busy".
//
// NOTHING HERE SHEDS OR CAPS THE QUEUE, DELIBERATELY. pending_decision is a READ PROJECTION with no
// authority: the Runner workflow is the sole authority on whether an action is paused, and it waits for a
// vote regardless of what this table says. Dropping a row would not relieve the human — it would hide a
// poll that still blocks an action, converting a visible backlog into a silently stuck one. Backpressure
// that can be applied safely belongs upstream, on the inbound proposal rate; what belongs here is seeing.
type QueueStats struct {
	// Open is how many polls are waiting for a human right now.
	Open int
	// DistinctShapes is how many genuinely different proposals those polls represent. Open==DistinctShapes
	// means every waiting decision is its own question; DistinctShapes==1 with Open large means one fault
	// is fanning out across the estate.
	DistinctShapes int
	// LargestShape is the size of the biggest near-duplicate group. It is the headline number for a flood:
	// it says how many of the waiting polls one review would actually settle.
	LargestShape int
	// OldestAge is how long the longest-waiting poll has been open. Zero when the queue is empty.
	//
	// Read beside Open: a shallow queue whose oldest entry is a day old is a DIFFERENT failure from a deep
	// queue that is minutes old. The first is an operator who has stopped voting (or a poll whose workflow
	// died — see ReapAbandoned); the second is a storm in progress.
	OldestAge time.Duration
	// Irreversible is how many of the waiting polls bind an action that cannot be undone. Under flood these
	// are the ones that must not be rubber-stamped, and they are the ones ordering surfaces first.
	Irreversible int
}

// ComputeQueueStats summarises the open poll queue as of `now`.
//
// A pure function over the rows so the flood arithmetic is testable without a database — and so the shape
// rule can be argued about in one place rather than being embedded in SQL where it cannot be exercised.
func ComputeQueueStats(open []PendingDecision, now time.Time) QueueStats {
	st := QueueStats{Open: len(open)}
	shapes := map[string]int{}
	for _, d := range open {
		if d.Reversible == false {
			st.Irreversible++
		}
		shapes[proposalShape(d)]++
		// A row with no opened_at contributes no age rather than an enormous one. An unset timestamp is
		// UNKNOWN, and inventing an age from it would page for a backlog that is not there.
		if !d.OpenedAt.IsZero() {
			if age := now.Sub(d.OpenedAt); age > st.OldestAge {
				st.OldestAge = age
			}
		}
	}
	st.DistinctShapes = len(shapes)
	for _, n := range shapes {
		if n > st.LargestShape {
			st.LargestShape = n
		}
	}
	return st
}

// proposalShape is the near-duplicate key: what makes two waiting polls the SAME question.
//
// Site plus the proposed action, not external_ref — external_ref is unique per session by construction, so
// keying on it would report every flood as fully distinct and the ratio would be a constant 1. The action
// text is what the human actually reads and decides about.
//
// The site is part of the key on purpose: the same remediation on two sites is two decisions, because the
// blast radius differs and an operator may well approve one and refuse the other.
func proposalShape(d PendingDecision) string {
	action := ""
	if len(d.Approaches) > 0 {
		action = strings.Join(strings.Fields(strings.ToLower(d.Approaches[0])), " ")
	}
	if action == "" {
		// No proposal text is its own shape, keyed by ref so these never collapse together. A row that
		// cannot be compared must not be silently counted as a duplicate of another one.
		return "\x00noaction\x00" + d.ExternalRef
	}
	return strings.ToLower(strings.TrimSpace(d.Site)) + "\x00" + action
}

// OrderForReview sorts open decisions the way a flooded operator needs them: what cannot be undone first,
// then what has waited longest.
//
// The previous order was opened_at alone. That is the right default for a calm queue and the wrong one for
// the case this ticket is about — under flood, first-in-first-out means the irreversible proposal is
// reviewed after ninety reversible ones, by a reviewer who has been trained by the preceding ninety to
// click approve. Ordering is the one piece of prioritisation that costs nothing and cannot lose a row.
//
// Sorts a copy; the caller's slice is left alone.
func OrderForReview(open []PendingDecision) []PendingDecision {
	out := make([]PendingDecision, len(open))
	copy(out, open)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Reversible != out[j].Reversible {
			return !out[i].Reversible // irreversible first
		}
		if !out[i].OpenedAt.Equal(out[j].OpenedAt) {
			return out[i].OpenedAt.Before(out[j].OpenedAt) // then oldest
		}
		return out[i].ExternalRef < out[j].ExternalRef // deterministic
	})
	return out
}
