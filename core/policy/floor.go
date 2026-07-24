package policy

// Execution deny-floor + loadable policy templates — spec/015 task T-015-7 (REQ-1511/1512/1513). This is the
// operator's OWN paranoia layer inside the policy engine, and it embodies the paradigm's "warn, don't block"
// rule: the operator owns the dial, so a floor entry (or a whole template) is REMOVABLE — but loudly, with an
// acknowledged double-confirmation that is recorded to the audit trail, never silently.
//
// TWO CONSTITUTIONALLY DISTINCT FLOORS — do not conflate them:
//
//   - The CONSTITUTIONAL never-auto floor (core/safety.IsNeverAuto / IsDestructiveOp / !Reversible, INV-09).
//     It is MECHANICAL, NON-configurable, and sits BENEATH this engine. NOTHING in this file — no template, no
//     removal, no double-confirmation — can weaken it. It is enforced in defense-in-depth at the classifier
//     and the actuation adapter regardless of any policy (see band.go's floor clamp, design.md step 0). This
//     file NEITHER reimplements it NOR touches core/safety; it only DOCUMENTS that it still clamps beneath and
//     leaves a test asserting the policy floor exposes no bypass of it.
//   - The POLICY execution deny-floor built HERE. It is an OPERATOR-configurable layer WITHIN the engine and
//     IS removable-with-warning (REQ-1513). Removing every policy floor entry does NOT and can NOT lift the
//     constitutional floor beneath it — a floor-class op still cannot auto-execute.
//
// EXECUTION floor, NOT a proposal floor (REQ-1511, the load-bearing semantic): in EVERY mode the pipeline runs
// identically — investigate → predict → classify → PROPOSE — and a floor-class action is STILL suggested with
// its rationale. The floor gates only whether that action may EXECUTE: ApplyFloor composes the proposal verdict
// with the floor and returns a floored EXECUTION verdict of `deny`, while PRESERVING the untouched proposal in
// its record so a human can still see it and override via the vote path. The deny bites only at auto-execute,
// never at proposal time.
//
// Fail-closed by construction (INV-09): a removal without an acknowledged confirmation is REFUSED (the floor
// stays); an unknown template name is an ERROR, never a silent allow-all; a malformed floor entry (one that
// constrains no dimension) DENIES everything rather than matching nothing. The `bare` allow-all template is
// permitted (the operator owns the dial) but ONLY behind a distinct red double-confirmation whose warning is
// recorded. Selectors reuse the ONE shared credential.Selector grammar via Match — there is no second grammar.
//
// Provenance: [O] INV-08/INV-09 · [R] paradigm-rule 4 (warn, don't block; the operator owns the dial) · [F]
// consolidates the predecessor safe-exec.sh guardrail. See spec/015-policy-engine requirements.md
// REQ-1511/1512/1513 and design.md (`policy.Floor`).

import (
	_ "embed"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------------------------------------
// Errors (typed, fail-closed). Every one keeps the floor in force / refuses the permissive change.
// ---------------------------------------------------------------------------------------------------------

var (
	// ErrFloorRemovalNotConfirmed is returned when a floor removal (a single entry or the whole conservative
	// template via `bare`) is attempted WITHOUT the required acknowledged double-confirmation. The removal is
	// refused and the floor stays in force (REQ-1513, fail closed) — this is the "loud, not silent" guarantee.
	ErrFloorRemovalNotConfirmed = errors.New("policy: floor removal requires an acknowledged double-confirmation — refused, floor stays")
	// ErrUnknownTemplate is returned by SelectTemplate for a name that is not a known template. It is an ERROR,
	// never a silent allow-all (REQ-1512, fail closed): an unknown template loads NOTHING and changes nothing.
	ErrUnknownTemplate = errors.New("policy: unknown policy template")
	// ErrFloorEntryNotFound is returned when RemoveFloorEntry names an entry that is not present (or already
	// removed). It changes nothing.
	ErrFloorEntryNotFound = errors.New("policy: floor entry not found")
)

// ---------------------------------------------------------------------------------------------------------
// Warning — the operator's acknowledged confirmation. "Warn, don't block" means the engine NEVER refuses a
// permissive operator choice for being permissive; it only requires the choice be explicit, acknowledged, and
// (for a floor-lowering change) double-confirmed, and it records the warning either way.
// ---------------------------------------------------------------------------------------------------------

// Warning is the acknowledged confirmation an operator supplies when lowering the paranoia dial (removing a
// floor entry, selecting the allow-all `bare` template). It is DATA the caller collects from a real UI
// double-confirmation; this package only checks that it is present and acknowledged and records its text.
type Warning struct {
	// Text is the human-readable acknowledgement the operator confirmed (recorded to the audit trail; must be
	// non-empty for the confirmation to count). It is NON-SECRET — a UI acknowledgement string, never a credential.
	Text string
	// Acknowledged is true when the operator explicitly acknowledged the warning (a single confirmation).
	Acknowledged bool
	// DoubleConfirm is true when the operator passed the DISTINCT red double-confirmation (REQ-1513) required to
	// remove a deny floor or load an allow-all policy. A floor-lowering change requires this, not merely a single
	// acknowledgement — the second, deliberate confirmation is what makes the lowering "loud".
	DoubleConfirm bool
}

// acknowledged reports whether the warning is a valid single acknowledgement: explicitly acknowledged AND
// carrying non-empty text. An empty or unacknowledged warning never counts (fail closed).
func (w Warning) acknowledged() bool { return w.Acknowledged && strings.TrimSpace(w.Text) != "" }

// redConfirmed reports whether the warning is a valid DISTINCT double-confirmation: acknowledged AND
// double-confirmed. This is the bar REQ-1513 sets for removing a deny floor or loading an allow-all policy.
func (w Warning) redConfirmed() bool { return w.acknowledged() && w.DoubleConfirm }

// ---------------------------------------------------------------------------------------------------------
// Records — NON-SECRET audit projections. Persistence is T-015-12; here they are returned/emitted only.
// ---------------------------------------------------------------------------------------------------------

// FloorRecord is the NON-SECRET projection of one ApplyFloor composition (REQ-1511) — the evidence that a
// floor gated an EXECUTION while leaving the PROPOSAL intact. It carries no argv, host, or credential: only the
// matched floor entry id, the two verdicts, and a reason. ProposalVerdict is the UNCHANGED proposal (what the
// pipeline suggested and a human may still override via the vote path); ExecutionVerdict is what the floor
// permits to actuate (deny when Floored).
type FloorRecord struct {
	Floored          bool    // true WHEN an active floor entry matched and floored the execution to deny.
	MatchedEntryID   string  // the id of the floor entry that floored (empty when nothing floored).
	ProposalVerdict  Verdict // the pipeline's proposal — UNCHANGED by the floor (the vote path still sees it).
	ExecutionVerdict Verdict // the floored execution verdict (deny when Floored; else == ProposalVerdict).
	Reason           string  // human-readable explanation for the console packet-tracer.
}

// FloorChangeRecord is the NON-SECRET audit record emitted on EVERY floor-lowering change — a removed entry or
// a selected `bare`/allow-all template (REQ-1513). It exists so no lowering is silent: entry/template, actor,
// the acknowledged warning text, whether the distinct red double-confirmation was given, and the timestamp.
// It contains no secret. A later leaf (T-015-12) appends it to the tamper-evident governance ledger; this leaf
// only returns/emits it.
type FloorChangeRecord struct {
	Change      string    // "remove-floor-entry" | "select-template:bare" | "select-template:conservative".
	EntryID     string    // the removed floor entry id, or the selected template name.
	Actor       string    // the operator who made the change (non-secret principal label).
	WarningText string    // the acknowledged warning text the operator confirmed (non-secret).
	RedConfirm  bool      // whether the distinct red double-confirmation accompanied the change.
	Lowering    bool      // whether the change LOWERED the paranoia dial (removed protection) — warned + audited.
	Timestamp   time.Time // when the change was recorded (audit metadata; the only non-deterministic field).
	Reason      string    // human-readable explanation for the audit trail / packet-tracer.
}

// ---------------------------------------------------------------------------------------------------------
// FloorEntry / Floor — the execution deny-floor.
// ---------------------------------------------------------------------------------------------------------

// FloorEntry is one execution-floor entry: a stable non-secret ID plus a Match reusing the SAME shared
// object-model + policy-dimension grammar as a Rule (credential.Selector via Match, op-class, argv-pattern,
// territory, reversible). An action the entry matches is floored to `deny` at execution UNLESS the entry has
// been removed-with-warning. A FloorEntry that constrains NO dimension is MALFORMED and denies EVERYTHING
// (fail closed) rather than matching nothing — the opposite of a Rule, because a floor must never fall open.
type FloorEntry struct {
	ID    string
	Match Match
}

// floorEntry is the internal, mutable-with-audit entry state the Floor owns: the operator FloorEntry, whether
// it was flagged malformed at construction (→ deny-all), and its removal record once removed-with-warning.
type floorEntry struct {
	entry     FloorEntry
	malformed bool
	removal   *FloorChangeRecord // non-nil ⇒ removed-with-warning; the entry no longer floors.
}

// matchesAction reports whether this entry floors the given action. A malformed entry (no dimension) denies
// everything (fail closed). A removed entry never floors (that is checked by the caller before this).
func (fe *floorEntry) matchesAction(in EvalInput) bool {
	if fe.malformed {
		return true // fail closed: an unparseable/empty floor entry denies everything rather than nothing.
	}
	return fe.entry.Match.matches(in)
}

// Floor is the operator's execution deny-floor: an ordered set of entries, each removable-with-warning. It is
// concurrency-safe. The read paths (AppliesTo / ApplyFloor) are pure over the current entry set; the only
// mutation path is RemoveFloorEntry / SelectTemplate, both of which REQUIRE an acknowledged confirmation and
// EMIT a FloorChangeRecord — there is NO exported path that removes an entry silently (the negative-control
// guarantee). Durable persistence of the entries + removals is a later leaf (T-015-12); this leaf holds them
// in memory. `now` is an injected clock (nil → time.Now) so the audit timestamp is testable.
type Floor struct {
	mu      sync.RWMutex
	entries []*floorEntry
	now     func() time.Time
	logf    func(format string, args ...any)
}

// NewFloor builds a Floor from operator entries, validating each fail-closed: an entry with an empty ID is
// rejected; an entry that constrains no dimension is accepted but FLAGGED MALFORMED so it denies everything
// (fail closed) rather than silently matching nothing. A duplicate id is rejected. logf is optional (nil →
// silent). The clock defaults to time.Now.
func NewFloor(entries ...FloorEntry) (*Floor, error) {
	f := &Floor{now: time.Now}
	seen := map[string]bool{}
	for i, e := range entries {
		if strings.TrimSpace(e.ID) == "" {
			return nil, fmt.Errorf("policy: floor entry[%d] has an empty id", i)
		}
		if seen[e.ID] {
			return nil, fmt.Errorf("policy: duplicate floor entry id %q", e.ID)
		}
		seen[e.ID] = true
		f.entries = append(f.entries, &floorEntry{
			entry:     e,
			malformed: !e.Match.specifiesAny(), // no dimension ⇒ fail-closed deny-all, not match-nothing.
		})
	}
	return f, nil
}

// WithClock sets the audit clock (for deterministic tests) and returns the floor.
func (f *Floor) WithClock(now func() time.Time) *Floor {
	f.mu.Lock()
	defer f.mu.Unlock()
	if now != nil {
		f.now = now
	}
	return f
}

// WithLogf sets the optional logger and returns the floor.
func (f *Floor) WithLogf(logf func(string, ...any)) *Floor {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logf = logf
	return f
}

// matchActiveLocked returns the first ACTIVE (not removed-with-warning) entry that floors the action, or nil.
// Caller holds f.mu (R or W). Order is deterministic — the entries' declared order.
func (f *Floor) matchActiveLocked(in EvalInput) *floorEntry {
	for _, fe := range f.entries {
		if fe.removal != nil {
			continue // removed-with-warning — this entry no longer floors.
		}
		if fe.matchesAction(in) {
			return fe
		}
	}
	return nil
}

// AppliesTo reports whether ANY active floor entry floors the given action. Pure over the current entry set;
// concurrency-safe. A removed-with-warning entry never applies.
func (f *Floor) AppliesTo(in EvalInput) bool {
	if f == nil {
		return false
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.matchActiveLocked(in) != nil
}

// ApplyFloor composes a PROPOSAL verdict with the execution deny-floor and returns the EXECUTION verdict plus a
// NON-SECRET FloorRecord (REQ-1511). It is the execution-floor semantics in one function:
//
//   - It NEVER changes the proposal. The action was already investigated/predicted/classified/proposed
//     identically in every mode; the ProposalVerdict is carried UNCHANGED in the record so a human can still
//     see it and override via the vote path.
//   - WHEN an active floor entry matches, the returned EXECUTION verdict is `deny` REGARDLESS of the proposal
//     (even a proposal of `auto`), and Floored is recorded — the deny bites only here, at auto-execute.
//   - WHEN no active entry matches (including when the matching entry was removed-with-warning), the execution
//     verdict is the proposal unchanged — the floor is not in force for this action.
//
// It is pure and deterministic (a nil floor floors nothing).
func (f *Floor) ApplyFloor(proposalVerdict Verdict, in EvalInput) (Verdict, FloorRecord) {
	rec := FloorRecord{ProposalVerdict: proposalVerdict, ExecutionVerdict: proposalVerdict}
	if f == nil {
		rec.Reason = "no policy floor configured — execution verdict is the proposal unchanged"
		return proposalVerdict, rec
	}
	f.mu.RLock()
	fe := f.matchActiveLocked(in)
	f.mu.RUnlock()

	if fe == nil {
		rec.Reason = "no active floor entry matched — execution verdict is the proposal unchanged (the proposal still stands for the vote path)"
		return proposalVerdict, rec
	}
	rec.Floored = true
	rec.MatchedEntryID = fe.entry.ID
	rec.ExecutionVerdict = VerdictDeny
	rec.Reason = fmt.Sprintf(
		"execution deny-floor entry %q matched — execution floored to deny (proposal %q is PRESERVED and still routes to the vote path for a human override); the constitutional never-auto floor (INV-09) applies beneath regardless",
		fe.entry.ID, proposalVerdict)
	return VerdictDeny, rec
}

// RemoveFloorEntry removes a single floor entry BEHIND an acknowledged double-confirmation (REQ-1513,
// warn-don't-block): the operator owns the dial, so removal is PERMITTED — but only loudly. It requires a
// distinct red double-confirmation (ack.redConfirmed()); a removal WITHOUT it is REFUSED and the floor stays
// (fail closed, ErrFloorRemovalNotConfirmed). On success the entry stops flooring and a FloorChangeRecord is
// returned/emitted for the audit trail. Removing a policy floor entry does NOT lift the constitutional
// never-auto floor beneath it — that mechanical floor still clamps floor-class ops.
func (f *Floor) RemoveFloorEntry(entryID, actor string, ack Warning) (FloorChangeRecord, error) {
	if !ack.redConfirmed() {
		// Fail closed: the operator MAY remove it, but not silently and not without the deliberate second
		// confirmation. The floor stays in force.
		return FloorChangeRecord{}, ErrFloorRemovalNotConfirmed
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	var target *floorEntry
	for _, fe := range f.entries {
		if fe.entry.ID == entryID && fe.removal == nil {
			target = fe
			break
		}
	}
	if target == nil {
		return FloorChangeRecord{}, fmt.Errorf("%w: %q", ErrFloorEntryNotFound, entryID)
	}

	rec := FloorChangeRecord{
		Change:      "remove-floor-entry",
		EntryID:     entryID,
		Actor:       actor,
		WarningText: ack.Text,
		RedConfirm:  true,
		Lowering:    true,
		Timestamp:   f.clock(),
		Reason: fmt.Sprintf(
			"floor entry %q removed by %s behind a red double-confirmation (warn-don't-block; the operator owns the dial). "+
				"The constitutional never-auto floor (INV-09) still clamps floor-class ops beneath the removed entry.",
			entryID, actor),
	}
	target.removal = &rec
	f.log("floor: entry %q removed-with-warning by %s", entryID, actor)
	return rec, nil
}

// Entries returns a read-only snapshot of the floor's entries and their removed state (for the console + tests).
// It is a COPY — the caller cannot mutate the floor through it (the only mutation path is RemoveFloorEntry /
// SelectTemplate, both audited).
func (f *Floor) Entries() []FloorEntrySnapshot {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]FloorEntrySnapshot, 0, len(f.entries))
	for _, fe := range f.entries {
		out = append(out, FloorEntrySnapshot{
			ID:        fe.entry.ID,
			Malformed: fe.malformed,
			Removed:   fe.removal != nil,
		})
	}
	return out
}

// FloorEntrySnapshot is the NON-SECRET read-only view of one floor entry's identity + state.
type FloorEntrySnapshot struct {
	ID        string
	Malformed bool // whether the entry constrains no dimension and therefore denies everything (fail closed).
	Removed   bool // whether the entry has been removed-with-warning (no longer floors).
}

func (f *Floor) clock() time.Time {
	if f.now != nil {
		return f.now()
	}
	return time.Now()
}

func (f *Floor) log(format string, args ...any) {
	if f.logf != nil {
		f.logf(format, args...)
	}
}

// ---------------------------------------------------------------------------------------------------------
// Templates — loadable starter RuleSets (rules-as-data). The two are the endpoints of the operator's dial.
// ---------------------------------------------------------------------------------------------------------

//go:embed templates/conservative.json
var conservativeTemplateJSON []byte

//go:embed templates/bare.json
var bareTemplateJSON []byte

// Template names (the wire + ledger spelling).
const (
	// TemplateConservative is the safe default: the predecessor safe-exec.sh argv deny-patterns loaded as
	// ordered deny rules plus a 30-executions-per-minute governor (REQ-1512). It is the tightest out-of-the-box
	// posture — everything not explicitly allowed still routes to the engine's fail-closed `approve` default.
	TemplateConservative = "conservative"
	// TemplateBare is the permissive posture: allow-all with a SINGLE access-list rule and NO operator denies
	// (REQ-1512). Selecting it LOWERS the dial (it removes the deny floor's protections), so it is permitted
	// ONLY behind the distinct red double-confirmation (REQ-1513) and always emits a warning record.
	TemplateBare = "bare"
)

// ConservativeTemplate returns the conservative starter policy as validated rule DATA (REQ-1512): the
// predecessor argv deny-patterns as ordered deny rules and a 30-per-minute governor on the global default. The
// embedded JSON is a FIXED, audited asset (like the fixed Rego module) — a parse failure can only be a build
// bug in that asset, never operator input, so it is returned as an error and the caller gets NO partial/empty
// (fail-open) RuleSet.
func ConservativeTemplate() (RuleSet, error) {
	return parseTemplate(TemplateConservative)
}

// BareTemplate returns the bare (allow-all, single-access-list, no operator denies) starter policy as validated
// rule DATA (REQ-1512). Loading it into an engine is a deliberate LOWERING of the dial — callers should apply
// it through SelectTemplate, which enforces the required red double-confirmation and records the warning.
func BareTemplate() (RuleSet, error) {
	return parseTemplate(TemplateBare)
}

// parseTemplate parses one embedded fixed template by name, fail-closed: an unknown name is ErrUnknownTemplate
// (never a silent allow-all), and a malformed embedded asset surfaces ParseRuleSet's ErrMalformedRule (never a
// partial RuleSet).
func parseTemplate(name string) (RuleSet, error) {
	switch name {
	case TemplateConservative:
		return ParseRuleSet(conservativeTemplateJSON)
	case TemplateBare:
		return ParseRuleSet(bareTemplateJSON)
	default:
		return RuleSet{}, fmt.Errorf("%w: %q", ErrUnknownTemplate, name)
	}
}

// ConservativeFloor builds the execution deny-floor seeded from the conservative template's argv deny-patterns
// — the SAME single source of truth as the conservative template (no duplicated pattern list). Each deny rule's
// argv-pattern becomes a floor entry, so the predecessor guardrail is enforceable both as rule DATA (the
// template) and as the execution floor. A parse failure of the fixed embedded asset is a build bug and returns
// an error (fail closed; NO partial floor).
func ConservativeFloor() (*Floor, error) {
	rs, err := ConservativeTemplate()
	if err != nil {
		return nil, err
	}
	entries := make([]FloorEntry, 0, len(rs.Rules))
	for _, r := range rs.Rules {
		if r.Verdict != VerdictDeny {
			continue // the floor is built from the deny side of the template.
		}
		entries = append(entries, FloorEntry{ID: r.ID, Match: r.Match})
	}
	return NewFloor(entries...)
}

// SelectTemplate applies a named starter template on the operator's request and returns its rule DATA plus a
// NON-SECRET FloorChangeRecord (REQ-1512/1513). It is the warn-don't-block gate for the dial's endpoints:
//
//   - conservative: the tightening safe default. No confirmation is required; it re-seeds the floor's entries
//     with the conservative deny-patterns and returns the conservative RuleSet. The record notes the tightening
//     (Lowering=false).
//   - bare: the allow-all posture. It LOWERS the dial (removes the floor's protections), so it REQUIRES the
//     distinct red double-confirmation (ack.redConfirmed()); WITHOUT it the change is REFUSED and NOTHING
//     changes (fail closed, ErrFloorRemovalNotConfirmed). WITH it, every active floor entry is
//     removed-with-warning, the bare RuleSet is returned, and a lowering warning record is emitted.
//   - any other name: ErrUnknownTemplate — never a silent allow-all; the floor is left untouched.
//
// The constitutional never-auto floor (INV-09) is UNAFFECTED by any template selection — it still clamps
// floor-class ops beneath the engine regardless of `bare`.
func (f *Floor) SelectTemplate(name, actor string, ack Warning) (RuleSet, FloorChangeRecord, error) {
	rs, err := parseTemplate(name)
	if err != nil {
		return RuleSet{}, FloorChangeRecord{}, err // unknown/malformed template — nothing changes (fail closed).
	}

	switch name {
	case TemplateBare:
		if !ack.redConfirmed() {
			// Warn-don't-block still means the LOWERING must be explicit + double-confirmed. Refuse without it.
			return RuleSet{}, FloorChangeRecord{}, ErrFloorRemovalNotConfirmed
		}
		f.mu.Lock()
		removed := 0
		for _, fe := range f.entries {
			if fe.removal == nil {
				rec := FloorChangeRecord{
					Change: "select-template:bare", EntryID: fe.entry.ID, Actor: actor,
					WarningText: ack.Text, RedConfirm: true, Lowering: true, Timestamp: f.clock(),
					Reason: fmt.Sprintf("floor entry %q removed by %s via the allow-all `bare` template (red double-confirmation)", fe.entry.ID, actor),
				}
				fe.removal = &rec
				removed++
			}
		}
		f.mu.Unlock()
		rec := FloorChangeRecord{
			Change: "select-template:bare", EntryID: TemplateBare, Actor: actor,
			WarningText: ack.Text, RedConfirm: true, Lowering: true, Timestamp: f.clock(),
			Reason: fmt.Sprintf(
				"`bare` allow-all template selected by %s behind a red double-confirmation — %d floor entr(y/ies) removed-with-warning; "+
					"the operator owns the dial (warn-don't-block). The constitutional never-auto floor (INV-09) STILL clamps floor-class ops beneath the removed policy floor.",
				actor, removed),
		}
		f.log("floor: `bare` allow-all selected by %s — %d entries removed-with-warning", actor, removed)
		return rs, rec, nil

	case TemplateConservative:
		// Re-seed the floor's entries with the conservative deny-patterns (tightening — no confirmation needed).
		seed, serr := ConservativeFloor()
		if serr != nil {
			return RuleSet{}, FloorChangeRecord{}, serr
		}
		f.mu.Lock()
		f.entries = seed.entries
		f.mu.Unlock()
		rec := FloorChangeRecord{
			Change: "select-template:conservative", EntryID: TemplateConservative, Actor: actor,
			WarningText: ack.Text, RedConfirm: ack.DoubleConfirm, Lowering: false, Timestamp: f.clock(),
			Reason: fmt.Sprintf("conservative (safe-default) template selected by %s — the predecessor deny-patterns + 30/min governor are re-seeded as the execution floor (tightening; no confirmation required)", actor),
		}
		f.log("floor: conservative template selected by %s", actor)
		return rs, rec, nil

	default:
		// parseTemplate already rejected unknown names; this is belt-and-suspenders (fail closed).
		return RuleSet{}, FloorChangeRecord{}, fmt.Errorf("%w: %q", ErrUnknownTemplate, name)
	}
}

// TemplateNames returns the known template names, sorted, for the console picker (T-015-12) and docs.
func TemplateNames() []string {
	names := []string{TemplateBare, TemplateConservative}
	sort.Strings(names)
	return names
}
