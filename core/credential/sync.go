package credential

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------------------------------------
// SYNC ORCHESTRATION + CROSS-SOURCE RESOLUTION (spec/016 task T-016-7).
//
// SyncEngine composes the native resolver core (the standalone config-not-code store) with zero or more
// registered CredentialSources. It:
//
//   - drives each source's read-only Sync incrementally and idempotently, keyed by (source_id, native_id),
//     recording last-synced + drift per run (REQ-1607/1608/1615);
//   - resolves a Target by a deterministic operator-declared SOURCE precedence, recording the winning and
//     shadowed sources, and fails closed when precedence does not disambiguate (REQ-1609);
//   - falls back to the native store WHERE no synced source covers the target (REQ-1610).
//
// It never falls open to a default identity: every unmatched, ambiguous, or unresolvable path returns the
// zero Resolution with ErrUnresolved/ErrAmbiguous (REQ-1602), and holds under an empty or partially-synced
// store (a source that has not synced, or a failed sync, contributes nothing but never a blank bundle).
// ---------------------------------------------------------------------------------------------------------

// SyncOutcome is the terminal state of one Sync run. A failed sync leaves the prior converged state intact.
type SyncOutcome string

const (
	// SyncOK — the pull succeeded and the source converged to the new upstream state.
	SyncOK SyncOutcome = "ok"
	// SyncFailed — the pull errored (unreachable/denied/malformed); prior converged state is retained.
	SyncFailed SyncOutcome = "failed"
)

// SyncRun is the immutable record of one Sync (REQ-1615, the persistence contract's credential_sync_run
// shape). It carries the drift indicator — the counts of upstream objects added / changed / removed since
// the prior successful sync — plus last-synced and outcome. It holds NO secret value (only counts and
// non-secret metadata), so it is safe to append to the governance ledger and surface in the console.
type SyncRun struct {
	SourceID     string
	Plane        Plane
	StartedAt    time.Time
	LastSyncedAt time.Time // the last SUCCESSFUL sync time; unchanged by a failed run
	Added        int
	Changed      int
	Removed      int
	Outcome      SyncOutcome
	Err          string // non-secret error text, empty on SyncOK
}

// Drifted reports whether the run observed any upstream change (added/changed/removed > 0) — the drift
// indicator the console surfaces (REQ-1615).
func (r SyncRun) Drifted() bool { return r.Added+r.Changed+r.Removed > 0 }

// Resolution is the outcome of a cross-source resolve: the winning Bundle plus non-secret provenance —
// which source won, whether it came from the native fallback, and which sources were shadowed (REQ-1609).
// It is a distinct return type so the Bundle itself stays untouched; the provenance is safe to log/persist.
type Resolution struct {
	Bundle   Bundle
	Source   string   // winning source id; "" when resolved from the native fallback
	Native   bool     // true when resolved from the native store (REQ-1610)
	Shadowed []string // lower-precedence sources that also matched, sorted; nil when none
}

// sourceSlot is a registered source plus its precedence and its current converged entry set.
type sourceSlot struct {
	src        CredentialSource
	precedence int                    // lower value = higher precedence (declared order); ties fail closed
	entries    map[string]SourceEntry // converged state keyed by NativeID (idempotent upsert target)
	rules      []Rule                 // entries compiled to resolver Rules (most-specific-wins within source)
	synced     bool
	lastRun    SyncRun
	lastSynced time.Time // last successful sync time
}

// SyncEngine is the multi-source resolver. Construct with NewSyncEngine; register sources with
// RegisterSource; drive pulls with Sync / SyncAll; resolve with Resolve. Its zero-source form resolves
// purely from the native fallback, and its no-native form refuses everything a source does not cover — both
// fail closed by construction.
type SyncEngine struct {
	// mu guards slots, byID, and every mutable sourceSlot field (entries/rules/synced/lastSynced/lastRun).
	// Writers (RegisterSource, the convergence phase of Sync) take Lock; readers (Resolve, LastRun) take
	// RLock. The read-only source pull in Sync runs OUTSIDE the lock so a slow network sync never blocks the
	// actuation-critical Resolve path. native is a static config-not-code store (immutable after build), so
	// its own Resolve needs no lock here.
	mu     sync.RWMutex
	native *Engine
	slots  []*sourceSlot
	byID   map[string]*sourceSlot

	// membership indexes host↔group reconciliation (REQ-1605): every source that also implements
	// MembershipSource contributes a host→[groups] map here at Sync time, and Resolve consults it to populate
	// Target.Groups so a group-selector bundle (e.g. an AWX inventory bundle) matches a host in that group. It
	// has its OWN mutex (never taken while another goroutine holds it before se.mu), so the ordering is always
	// se.mu → membership.mu; GroupsFor takes only membership.mu. It holds NON-SECRET data only.
	membership *membershipStore
}

// NewSyncEngine builds a SyncEngine over a native resolver core (the standalone fallback, REQ-1610). native
// may be nil, in which case there is no fallback and only synced sources can resolve a target (still fail
// closed for anything uncovered).
func NewSyncEngine(native *Engine) *SyncEngine {
	return &SyncEngine{native: native, byID: map[string]*sourceSlot{}, membership: newMembershipStore()}
}

// RegisterSource registers a source at the given precedence (lower value wins; equal values among matching
// sources fail closed at resolution, REQ-1609). It rejects a nil source, an empty/duplicate source id, and
// a source declaring an unknown plane — a misconfigured source is refused rather than silently admitted.
func (se *SyncEngine) RegisterSource(src CredentialSource, precedence int) error {
	if se == nil {
		return errors.New("credential: RegisterSource on a nil SyncEngine")
	}
	if src == nil {
		return errors.New("credential: RegisterSource: nil source")
	}
	id := src.ID()
	if id == "" {
		return errors.New("credential: RegisterSource: source has an empty id")
	}
	if !src.Plane().valid() {
		return fmt.Errorf("credential: source %q declares unknown plane %q", id, src.Plane())
	}
	se.mu.Lock()
	defer se.mu.Unlock()
	if _, dup := se.byID[id]; dup {
		return fmt.Errorf("credential: source %q already registered", id)
	}
	slot := &sourceSlot{src: src, precedence: precedence, entries: map[string]SourceEntry{}}
	se.slots = append(se.slots, slot)
	se.byID[id] = slot
	return nil
}

// Sync runs one source's read-only pull and converges the store incrementally and idempotently (REQ-1608).
// On success it replaces the source's converged entry set (removed upstream entries disappear — no orphan;
// duplicate native ids collapse — no duplication) and records drift + last-synced. On failure it retains
// the prior converged state and records a failed run. It returns the run record and any pull error.
func (se *SyncEngine) Sync(ctx context.Context, sourceID string) (SyncRun, error) {
	se.mu.RLock()
	slot, ok := se.byID[sourceID]
	se.mu.RUnlock()
	if !ok {
		return SyncRun{}, fmt.Errorf("credential: Sync: unknown source %q (register it first)", sourceID)
	}
	started := time.Now().UTC()
	plane := slot.src.Plane()

	// The read-only source pull runs OUTSIDE the lock — a slow network sync must not block Resolve.
	entries, err := slot.src.Sync(ctx)
	if err != nil {
		se.mu.Lock()
		run := SyncRun{SourceID: sourceID, Plane: plane, StartedAt: started, LastSyncedAt: slot.lastSynced,
			Outcome: SyncFailed, Err: err.Error()}
		slot.lastRun = run
		se.mu.Unlock()
		return run, fmt.Errorf("credential: source %q sync failed: %w", sourceID, err)
	}

	// OPTIONAL estate host↔group membership (REQ-1605): a source that also knows which hosts belong to which
	// groups (AWX inventory→host today) contributes a host→[groups] map so a group-selector bundle resolves
	// for a host in that group. This runs OUTSIDE the lock (a second read-only network round, like the entries
	// pull) and is BEST-EFFORT: a membership-fetch failure retains the prior indexed membership and NEVER fails
	// the credential sync (no membership just means no extra groups → resolution unchanged, fail-closed). The
	// map is NON-SECRET (host + group names only).
	var membership map[string][]string
	var haveMembership bool
	if ms, ok := slot.src.(MembershipSource); ok && plane == PlaneMachine {
		if m, merr := ms.Membership(ctx); merr == nil {
			membership, haveMembership = m, true
		}
	}

	// Build the new converged set keyed by NativeID; validate every entry (fail closed on a bad one).
	// This is pure over the pulled entries — no shared state — so it stays outside the lock. The two-plane
	// router (validateEntryPlane) refuses any entry that carries the other plane's payload (REQ-1611), so a
	// machine-plane sync can never populate an approver and a human-plane sync can never populate a bundle.
	next := make(map[string]SourceEntry, len(entries))
	for _, e := range entries {
		if e.NativeID == "" {
			se.mu.Lock()
			run := failedRun(sourceID, plane, started, slot.lastSynced, "an entry has an empty native id")
			slot.lastRun = run
			se.mu.Unlock()
			return run, fmt.Errorf("credential: source %q: %s", sourceID, run.Err)
		}
		if err := validateEntryPlane(plane, e); err != nil {
			se.mu.Lock()
			run := failedRun(sourceID, plane, started, slot.lastSynced, err.Error())
			slot.lastRun = run
			se.mu.Unlock()
			return run, fmt.Errorf("credential: source %q: %s", sourceID, run.Err)
		}
		next[e.NativeID] = e // last write for a duplicate native id wins → no duplication
	}

	// Converge under the write lock: diff against the prior set, then replace the entry set + recompile
	// rules atomically (removed entries are gone → no orphan; no Resolve observes a half-applied state).
	se.mu.Lock()
	added, changed, removed := drift(slot.entries, next)
	slot.entries = next
	// Route by plane (REQ-1611): ONLY a machine-plane source compiles resolver Rules that Resolve can match
	// (host-credential resolution). A human-plane source leaves rules nil — its entries are approver
	// identities, reachable ONLY through ResolveApprovers — so a human sync can never populate host
	// resolution and Resolve can never observe an approver.
	if plane == PlaneMachine {
		slot.rules = rulesFromEntries(sourceID, next)
	} else {
		slot.rules = nil
	}
	// Reconcile this source's host↔group membership (REQ-1605) atomically with its converged entry set. Only
	// on a successful membership fetch — a failed one retains the prior membership (see above).
	if haveMembership {
		se.membership.replace(sourceID, membership)
	}
	slot.synced = true
	slot.lastSynced = started
	run := SyncRun{SourceID: sourceID, Plane: plane, StartedAt: started, LastSyncedAt: started,
		Added: added, Changed: changed, Removed: removed, Outcome: SyncOK}
	slot.lastRun = run
	se.mu.Unlock()
	return run, nil
}

// SyncAll runs every registered source's pull — the operation a scheduled tick invokes (the on-schedule
// counterpart to a single on-demand Sync). It returns each run in registration order and the first pull
// error, if any; a failure in one source does not abort the others (each fails closed independently).
func (se *SyncEngine) SyncAll(ctx context.Context) ([]SyncRun, error) {
	se.mu.RLock()
	ids := make([]string, len(se.slots))
	for i, slot := range se.slots {
		ids[i] = slot.src.ID()
	}
	se.mu.RUnlock()
	runs := make([]SyncRun, 0, len(ids))
	var firstErr error
	for _, id := range ids {
		run, err := se.Sync(ctx, id)
		runs = append(runs, run)
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return runs, firstErr
}

// LastRun returns the most recent SyncRun for a source (REQ-1615) — the console's last-synced + drift feed.
func (se *SyncEngine) LastRun(sourceID string) (SyncRun, bool) {
	se.mu.RLock()
	defer se.mu.RUnlock()
	slot, ok := se.byID[sourceID]
	if !ok {
		return SyncRun{}, false
	}
	return slot.lastRun, slot.lastRun.SourceID != ""
}

// Resolve maps a Target to exactly one Bundle across all synced sources and the native fallback, or fails
// closed. Procedure (REQ-1609/1610):
//
//  1. Collect every synced source whose rules match the target, resolving within each by most-specific-wins.
//  2. If NO source matches → resolve from the native store as the standalone fallback (REQ-1610).
//  3. Otherwise apply the operator-declared source precedence: the single highest-precedence matching source
//     wins and the rest are recorded as shadowed. If two matching sources share the top precedence, or the
//     top source is internally ambiguous (equal-specificity), the precedence does not disambiguate →
//     ErrAmbiguous (fail closed, REQ-1609/1606).
//
// It NEVER returns a default, global, or last-used identity.
func (se *SyncEngine) Resolve(t Target) (Resolution, error) {
	if se == nil {
		return Resolution{}, ErrUnresolved
	}
	se.mu.RLock()
	defer se.mu.RUnlock()

	// ESTATE RECONCILIATION (REQ-1605): enrich the target with the estate groups it belongs to (the AWX
	// inventories a host is a member of, etc.) BEFORE matching, so a group-selector bundle resolves for a host
	// in that group. Additive + fail-closed: an unknown host adds no groups → the target resolves EXACTLY as
	// before (host / host-glob / native rules unchanged); this only ADDS the ability to match a group selector.
	// The SAME seam serves the read-only investigation path (hostdiag) TODAY and the future actuation effect
	// leaf — both resolve through this one core, so neither has to know about membership.
	if t.Host != "" && se.membership != nil {
		t.Groups = mergeGroups(t.Groups, se.membership.groupsFor(t.Host))
	}

	type cand struct {
		id         string
		precedence int
		bundle     Bundle
		ambiguous  bool // matched but internally equal-specificity → cannot yield a single bundle
	}
	var cands []cand
	for _, slot := range se.slots {
		// ISOLATION (REQ-1611): a machine-plane resolution NEVER draws on a human-plane source. Human slots
		// carry nil rules, but guard on the plane explicitly so this holds independently of rule compilation.
		if slot.src.Plane() != PlaneMachine || !slot.synced || len(slot.rules) == 0 {
			continue
		}
		r, err := selectRule(slot.rules, t)
		switch {
		case err == nil:
			cands = append(cands, cand{id: slot.src.ID(), precedence: slot.precedence, bundle: r.Bundle})
		case errors.Is(err, ErrAmbiguous):
			cands = append(cands, cand{id: slot.src.ID(), precedence: slot.precedence, ambiguous: true})
		default:
			// plain ErrUnresolved → this source does not cover the target; skip it.
		}
	}

	// REQ-1610: no synced source covers the target → native standalone fallback.
	if len(cands) == 0 {
		if se.native == nil {
			return Resolution{}, ErrUnresolved
		}
		b, err := se.native.Resolve(t)
		if err != nil {
			return Resolution{}, err
		}
		return Resolution{Bundle: b, Native: true}, nil
	}

	// REQ-1609: apply source precedence. Lower value = higher precedence.
	topPrec := cands[0].precedence
	for _, c := range cands {
		if c.precedence < topPrec {
			topPrec = c.precedence
		}
	}
	var top []cand
	var shadowed []string
	for _, c := range cands {
		if c.precedence == topPrec {
			top = append(top, c)
		} else {
			shadowed = append(shadowed, c.id)
		}
	}
	// Two sources tie at the top precedence → precedence did not disambiguate → refuse.
	if len(top) > 1 {
		ids := make([]string, 0, len(top))
		for _, c := range top {
			ids = append(ids, c.id)
		}
		sort.Strings(ids)
		return Resolution{}, fmt.Errorf("%w: target %q covered by equal-precedence sources %v",
			ErrAmbiguous, targetLabel(t), ids)
	}
	winner := top[0]
	// The sole top source is internally ambiguous (equal-specificity rules) → refuse (REQ-1606).
	if winner.ambiguous {
		return Resolution{}, fmt.Errorf("%w: winning source %q has an equal-specificity conflict for target %q",
			ErrAmbiguous, winner.id, targetLabel(t))
	}

	sort.Strings(shadowed)
	return Resolution{Bundle: winner.bundle, Source: winner.id, Shadowed: shadowed}, nil
}

// GroupsFor returns the estate groups a host is a member of, drawn from every registered MembershipSource
// (REQ-1605). Resolve consults it to populate Target.Groups so a group-selector bundle resolves for a host
// in that group. It is exported so a caller that builds its own Target (an actuation effect leaf) can enrich
// the target the same way Resolve does. An unknown host or a nil engine yields nil (no extra groups).
func (se *SyncEngine) GroupsFor(host string) []string {
	if se == nil || se.membership == nil {
		return nil
	}
	return se.membership.groupsFor(host)
}

// rulesFromEntries compiles a source's converged entry set into resolver Rules. The rule ID is the entry's
// NativeID so the winning-rule provenance names the upstream object.
func rulesFromEntries(sourceID string, entries map[string]SourceEntry) []Rule {
	if len(entries) == 0 {
		return nil
	}
	// Deterministic order (by NativeID) so equal-specificity ties are detected stably.
	ids := make([]string, 0, len(entries))
	for id := range entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Rule, 0, len(ids))
	for _, id := range ids {
		e := entries[id]
		out = append(out, Rule{ID: e.NativeID, Selector: e.Selector, Bundle: e.Bundle})
	}
	return out
}

// drift computes the added / changed / removed counts between the prior and next converged entry sets,
// keyed by NativeID — the per-source drift indicator (REQ-1615). A repeated sync of unchanged data yields
// (0, 0, 0): the store converges with no duplicate and no orphan (REQ-1608).
func drift(prev, next map[string]SourceEntry) (added, changed, removed int) {
	for id, ne := range next {
		pe, ok := prev[id]
		switch {
		case !ok:
			added++
		case !sameEntry(pe, ne):
			changed++
		}
	}
	for id := range prev {
		if _, ok := next[id]; !ok {
			removed++
		}
	}
	return added, changed, removed
}

// sameEntry reports whether two entries carry the same selector and the same non-secret + SecretRef
// identity (a change to any matched field or secret REFERENCE is drift). It compares references, never
// secret values (nothing is resolved here).
func sameEntry(a, b SourceEntry) bool {
	return a.Selector == b.Selector &&
		a.Bundle.user == b.Bundle.user &&
		a.Bundle.port == b.Bundle.port &&
		a.Bundle.scheme == b.Bundle.scheme &&
		a.Bundle.sshKeyRef == b.Bundle.sshKeyRef &&
		a.Bundle.apiTokenRef == b.Bundle.apiTokenRef &&
		a.Bundle.become == b.Bundle.become &&
		sameApprover(a.Approver, b.Approver) // human-plane entries: an approver-identity change is drift
}

func failedRun(sourceID string, plane Plane, started, lastSynced time.Time, msg string) SyncRun {
	return SyncRun{SourceID: sourceID, Plane: plane, StartedAt: started, LastSyncedAt: lastSynced,
		Outcome: SyncFailed, Err: msg}
}
