package tracker

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// MULTITRACKER — the four-verb contract over several configured trackers, routed by REF OWNERSHIP.
//
// WHY IT EXISTS. The worker bound the entry tracker only when EXACTLY ONE was enabled, with no else arm
// and no wiring seam. With two configured — a site running ServiceNow for ITSM and YouTrack for
// engineering work, which is the ordinary shape at an established estate — FOUR capabilities went dark
// at once, silently:
//
//  1. deps.TrackerRead   — the investigation stops reasoning with the incident's own ticket
//  2. deps.Tickets       — the TERMINAL reconcile close-out never transitions anything, ever
//  3. the learned scheduled-reboot lane loses its tracker
//  4. the dedup stage's OpenIssue check goes nil — and that one is not a degradation, it is WRONG:
//     core/suppression/dedup.go returned OutcomeSuppressed with the reason "duplicate of an open
//     incident within window" for a re-fire whose parent ticket had RESOLVED — silently dropping a
//     genuine new incident. FIXED in TG-354: the dedup stage now fails toward surfacing (a suppression
//     must be BACKED by a confirmed-open parent), so an openness check that does NOT confirm open ESCALATES.
//     TG-459 bounds that ONLY for the no-tracker case (gate.OpenIssue nil, genuinely unknowable openness): a
//     re-fire while the anchor is still within the short recency sub-window is a rapid duplicate of a
//     plausibly-still-open incident and is deduped on recency; a stale one escalates. A wired tracker that
//     answers non-open (RESOLVED, or unresolvable) escalates at any age (TG-354) — recency does not apply.
//
// Nothing logged any of it. The count loop had the same last-wins bug the notifier's did.
//
// ROUTED, NOT FANNED OUT — and the distinction is the whole safety argument. An external ref belongs to
// EXACTLY ONE tracker; "INC0010023" in ServiceNow and an identically-shaped id in another system are
// different tickets. So a write is never broadcast: the owner is resolved by reading, and only the owner
// is written to. Broadcasting a TransitionState would resolve an unrelated incident in a second system,
// which is a mutation of someone else's record made on a guess.
//
// This is why MultiTracker is SERIAL where MultiHistory is concurrent. There, every source contributes
// part of the answer, so all are queried. Here exactly one source has the answer and the rest have
// nothing, so asking them in a fixed order and stopping at the first hit is both cheaper and the only
// thing that makes ownership well-defined.
type MultiTracker struct {
	members []namedTracker
}

type namedTracker struct {
	name string
	t    Tracker
}

// NewMultiTracker builds the router over the given trackers, ordered by vendor slug so ownership
// resolution is deterministic across boots.
func NewMultiTracker(trackers map[string]Tracker) *MultiTracker {
	out := make([]namedTracker, 0, len(trackers))
	for name, t := range trackers {
		if t == nil {
			continue // a nil tracker owns nothing; counting it would only lengthen every error message
		}
		out = append(out, namedTracker{name: name, t: t})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return &MultiTracker{members: out}
}

// Len reports how many trackers this router will consult.
func (m *MultiTracker) Len() int {
	if m == nil {
		return 0
	}
	return len(m.members)
}

// Sources names them, in resolution order.
func (m *MultiTracker) Sources() []string {
	if m == nil {
		return nil
	}
	out := make([]string, 0, len(m.members))
	for _, s := range m.members {
		out = append(out, s.name)
	}
	return out
}

// SourceType names the router. Deliberately not a member's slug: a ticket transitioned through the
// router was not transitioned "by youtrack", and an audit line claiming so would misattribute the act.
func (m *MultiTracker) SourceType() string { return "multi" }

// owner resolves which tracker holds a ref, returning it with the issue it already had to read.
//
// The issue is returned rather than discarded because every caller needs one or the other and a second
// round-trip to re-read what was just read is a request an operator pays for twice.
func (m *MultiTracker) owner(ctx context.Context, id string) (namedTracker, Issue, error) {
	if m == nil || len(m.members) == 0 {
		return namedTracker{}, Issue{}, fmt.Errorf("tracker router: no tracker is configured")
	}
	if strings.TrimSpace(id) == "" {
		return namedTracker{}, Issue{}, fmt.Errorf("tracker router: empty issue id")
	}
	var tried []string
	for _, s := range m.members {
		iss, err := s.t.Read(ctx, id)
		if err == nil {
			return s, iss, nil
		}
		tried = append(tried, fmt.Sprintf("%s: %v", s.name, err))
	}
	// No tracker recognised the ref. This is an ERROR, never a zero Issue: a zero Issue has State "" —
	// neither Open nor Resolved — and every consumer that switches on state would take a branch chosen by
	// a value that means "we could not find out".
	return namedTracker{}, Issue{}, fmt.Errorf("tracker router: no configured tracker holds %q — %s",
		id, strings.Join(tried, "; "))
}

// Open reads the entry issue — the triage trigger — from whichever tracker holds it.
func (m *MultiTracker) Open(ctx context.Context, id string) (Issue, error) {
	_, iss, err := m.owner(ctx, id)
	return iss, err
}

// Read returns the current issue state by correlation key.
func (m *MultiTracker) Read(ctx context.Context, id string) (Issue, error) {
	_, iss, err := m.owner(ctx, id)
	return iss, err
}

// TransitionState moves the issue in the tracker that OWNS it, and in no other.
func (m *MultiTracker) TransitionState(ctx context.Context, id string, to State) error {
	own, _, err := m.owner(ctx, id)
	if err != nil {
		return err // a close-out that cannot find its ticket must fail loudly, not report success
	}
	if err := own.t.TransitionState(ctx, id, to); err != nil {
		return fmt.Errorf("%s: %w", own.name, err)
	}
	return nil
}

// Comment posts the terminal audit comment to the owning tracker only.
func (m *MultiTracker) Comment(ctx context.Context, id, body string) error {
	own, _, err := m.owner(ctx, id)
	if err != nil {
		return err
	}
	if err := own.t.Comment(ctx, id, body); err != nil {
		return fmt.Errorf("%s: %w", own.name, err)
	}
	return nil
}

var _ Tracker = (*MultiTracker)(nil)
