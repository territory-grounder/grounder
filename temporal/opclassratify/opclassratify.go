// Package opclassratify executes the earned op-class operator verbs in the WORKER — the ledger's single
// writer (spec/028 REQ-2813; the same rule spec/027 REQ-2703 imposes on the manifest lane and spec/014
// REQ-1311 on skill versions). The grounder's review surface never appends to the hash chain itself: it
// starts this workflow and waits, so a concurrent grounder can never fork the chain.
//
// WHY THIS LANE IS THE MOST CONSEQUENTIAL OF THE THREE. An adopted manifest entry materializes a TARGET the
// leaf will accept. A ratified op-class materializes the COMMAND — an argv template that runs as root. Every
// other write path in the system decides what may be touched; this one decides what may be done. That is
// why the lane owns no state machine and no grant logic of its own: lifecycle verbs land in
// opclasscat.Transition, the grant lands in the overlay's append-only writer, and this package's whole job
// is to run one of them under the worker's ledger. A second copy of either would be a second grant path.
package opclassratify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/opclasscat"
	"github.com/territory-grounder/grounder/core/policy"
)

// Verb is the CLOSED operator verb set (REQ-2813). A sixth value is a design change, not a new constant.
type Verb string

const (
	VerbRatify      Verb = "ratify"
	VerbDismiss     Verb = "dismiss"
	VerbDemote      Verb = "demote"
	VerbRevoke      Verb = "revoke"
	VerbExportEmbed Verb = "export-embed"
)

// Request is the typed order. Approver is SERVER-DERIVED at the surface and never client-supplied: a client
// that could name its own approver could launder a grant through someone else's identity.
//
// Spec is what the OPERATOR authored. It has already passed opschema.ValidateRatification at the surface —
// including the laundering tripwire, which needs the candidate's model text and so must run where that text
// is readable. It is re-validated HERE anyway (see ratify): the surface can be bypassed by a future caller;
// the worker is the authority.
type Request struct {
	Verb             Verb
	CandidateKey     string
	OpClass          string
	Spec             opschema.OpClassSpec
	PromoteThreshold int
	Rationale        string
	Approver         string
}

// Result is what the console re-renders from.
type Result struct {
	CandidateKey string
	OpClass      string
	Status       string
	Level        string
	LedgerSeq    int64
	EntryHash    string
	Artifact     string
}

var (
	// ErrNotFound: the key names no candidate. A DECISION, not a transient — surfaced as 404, never retried.
	ErrNotFound = errors.New("opclassratify: unknown op-class candidate")
	// ErrNotGranted: the class has no live grant for this verb to act on.
	ErrNotGranted = errors.New("opclassratify: op-class has no live grant")
	// ErrUnknownVerb: defence in depth behind the surface's closed table.
	ErrUnknownVerb = errors.New("opclassratify: unknown verb")
	// ErrAutoBarredTier: a barred candidate offered at an auto-eligible tier. A DECISION (422), never
	// retried — the operator must re-author at a never-auto tier (TG-227 blocker 4).
	ErrAutoBarredTier = errors.New("opclassratify: auto-barred candidate at an auto-eligible tier")
)

// Loader reads the candidate the verb applies to. Split from opclasscat.Store because the state machine
// takes a Candidate, not a key: the worker loads, then transitions, so the row the ledger describes is the
// row the worker actually read.
type Loader interface {
	CandidateByKey(ctx context.Context, key string) (opclasscat.Candidate, bool, error)
	// Occurrences supplies the model text for the worker's OWN tripwire check.
	Occurrences(ctx context.Context, key string, since time.Time) ([]opclasscat.Occurrence, error)
}

// Grant is one ratification as the overlay writer receives it. A struct rather than eleven positional
// arguments because two of them are `approver` and `rationale`, two more are `family` and `tier`, and a
// transposition at the call site would silently record the wrong human against the wrong capability.
type Grant struct {
	OpClass          string
	Spec             opschema.OpClassSpec
	EntryHash        string
	Family           string
	Tier             string
	PromoteThreshold int
	CandidateKey     string
	Approver         string
	Rationale        string
	LedgerSeq        int64
}

// Overlay is the append-only grant writer (core/db.OpClassRatifiedStore, adapted at the composition root).
// The lane holds it as an interface so the oracles can drive the real code path without a database.
type Overlay interface {
	Ratify(ctx context.Context, g Grant) error
	Revoke(ctx context.Context, opClass, approver, rationale string, ledgerSeq int64) error
	IsLive(ctx context.Context, opClass string) (bool, error)
}

// Ladder is the per-class graduation state the demote verb resets. DISTINCT from the estate-wide breaker
// (REQ-2810): one misbehaving class must not silently mute the whole estate, and muting the estate must not
// read as a verdict on one class.
type Ladder interface {
	Load(ctx context.Context, opClass string) (policy.ClassState, error)
	Save(ctx context.Context, st policy.ClassState) error
}

// Exporter renders the embed-export MR body. Injected rather than imported so the lane does not depend on a
// main package; the real implementation is tools/embedexport's Render.
type Exporter interface {
	MRBody(spec opschema.OpClassSpec) (string, error)
}

// Deps are the worker-side collaborators.
type Deps struct {
	Loader  Loader
	Store   opclasscat.Store
	Ledger  opclasscat.Ledger
	Overlay Overlay
	Ladder  Ladder
	Export  Exporter
	// Refreshed, when non-nil, is fired after an overlay WRITE commits (ratify, revoke) so the composed
	// registry converges on a kick instead of waiting out the refresh TTL — an operator watching their
	// grant go live should wait seconds, not a polling interval. Fired AFTER the row lands, never before:
	// a kick that races the commit would refresh into the pre-write state and read as "grant lost".
	// Nil-safe; the durable row + periodic refresh remain the source of truth if the kick is dropped.
	Refreshed func()
}

// notifyRefreshed fires the post-write refresh kick, if one is wired.
func (d Deps) notifyRefreshed() {
	if d.Refreshed != nil {
		d.Refreshed()
	}
}

// reasonWithApprover encodes WHO decided into the ledger reason.
//
// audit.GovDecision has no actor field — the chain's subject is the action a decision is bound to, not the
// person — so the approver rides in the reason text, in the exact "[name] rationale" shape
// worldmodel.Transition already uses. Replicating the FORM matters: a reader grepping the chain for who
// ratified a capability should not have to know that op-classes encode it differently from manifest entries.
func reasonWithApprover(approver, rationale string) string {
	if a := strings.TrimSpace(approver); a != "" {
		return "[" + a + "] " + rationale
	}
	return rationale
}

// Activities carries Deps for Temporal registration.
type Activities struct{ D Deps }

// OpClassVerbActivity dispatches the closed verb set. Every arm ends in an EXISTING chokepoint.
func (a *Activities) OpClassVerbActivity(ctx context.Context, req Request) (Result, error) {
	if strings.TrimSpace(req.Rationale) == "" {
		return Result{}, opclasscat.ErrRationaleRequired
	}
	switch req.Verb {
	case VerbRatify:
		return a.ratify(ctx, req)
	case VerbDismiss:
		return a.dismiss(ctx, req)
	case VerbDemote:
		return a.demote(ctx, req)
	case VerbRevoke:
		return a.revoke(ctx, req)
	case VerbExportEmbed:
		return a.exportEmbed(ctx, req)
	default:
		return Result{}, fmt.Errorf("%w: %q", ErrUnknownVerb, req.Verb)
	}
}

// ratify is the grant. THREE things happen in a fixed order and the order is the safety property.
//
//  1. The tripwire runs AGAIN, here, against the model text this worker reads for itself. The surface
//     already ran it, and that check is the one an operator sees — but it can be bypassed by any future
//     caller that starts this workflow directly, and "the caller already checked" is exactly the assumption
//     that turns a defence into decoration. Cheap to repeat, and it is the last place the system can still
//     refuse.
//
//  2. opclasscat.Transition moves ratify_ready -> ratified. It appends the opclass:ratify GovDecision
//     BEFORE the row (the ledger-before-row contract), so a crash leaves an over-recorded ledger rather
//     than an unrecorded grant.
//
//  3. The overlay row is written LAST, carrying the ledger seq from step 2. A crash between 2 and 3 leaves
//     a ledgered decision with no capability — the operator re-ratifies and the second attempt is refused
//     as a bad transition, which is visible and recoverable. The opposite order would leave a LIVE
//     capability with no chain entry explaining it, which is neither.
func (a *Activities) ratify(ctx context.Context, req Request) (Result, error) {
	c, found, err := a.D.Loader.CandidateByKey(ctx, req.CandidateKey)
	if err != nil {
		return Result{}, err
	}
	if !found {
		return Result{}, ErrNotFound
	}
	occs, err := a.D.Loader.Occurrences(ctx, req.CandidateKey, time.Time{})
	if err != nil {
		return Result{}, err
	}
	// The model's authored PROSE only — see the httpapi surface's modelTextOf for why the bare op verb is
	// excluded (it is the operation's identity, not model text, and including it would make every
	// service-lifecycle class unratifiable). Flagged there as a spec deviation for review.
	modelText := make([]string, 0, len(occs)*2)
	for _, o := range occs {
		modelText = append(modelText, o.Rationale, o.UndoSketch)
	}
	// THE BARRED/TIER COUPLING (TG-227 blocker 4's enforcement half). The cron's screen stamped
	// auto_barred from the candidate's OBSERVED ops and targets; here that verdict binds the grant: a
	// barred class is ratifiable ONLY at a never-auto tier. Visible forever, silent never. This sits at
	// the single ratify writer — beneath the console, beneath any future workflow starter — and the
	// graduation tier floor (already law) is the second, independent control downstream.
	if c.AutoBarred && opschema.AutoEligible(strings.ToLower(strings.TrimSpace(req.Spec.SafetyTier))) {
		return Result{}, fmt.Errorf("%w: candidate %q is auto-barred (screened from its observed ops/targets); "+
			"it may be ratified only at a never-auto tier (irreversible or vendor-critical), not %q",
			ErrAutoBarredTier, req.CandidateKey, req.Spec.SafetyTier)
	}
	spec, err := opschema.ValidateRatification(req.Spec, modelText)
	if err != nil {
		return Result{}, err
	}
	// The threshold may only RISE. An operator ratifying and simultaneously lowering the ladder bar would be
	// deciding the grant and the speed of its promotion in one click; migration 0049 CHECKs >= 5, and the
	// tier default is the floor.
	threshold := policy.PromoteThresholdForTier(spec.SafetyTier)
	if req.PromoteThreshold > threshold {
		threshold = req.PromoteThreshold
	}
	hash, err := opschema.CanonicalHash(spec)
	if err != nil {
		return Result{}, err
	}
	// TG-177 — FAIL-CLOSED op-class trust inheritance. `spec.OpClass` is OPERATOR-AUTHORED and is NOT bound
	// to the candidate's cluster slug (`c.OpClass`). Two boundary moves ride this seam: a RENAME/split
	// (authored slug ≠ cluster slug) and a REUSE (authored slug already carries graduation — a
	// revoked-then-re-ratified name, or a stale ladder row). Trust keyed on a mutable string, under a system
	// that actuates the estate, is a privilege-escalation path through ordinary refactoring: the most likely
	// way it fires is someone doing good work, splitting an over-broad class into precise ones and silently
	// handing an unearned auto-actuate capability to a name that never earned it. Default inheritance is
	// NOTHING. Record the boundary move on the ratify ledger entry (the chain gains a who/when trace for
	// every rename), and reset the ladder to approve so a split/renamed/reused class re-earns from zero.
	// The DURABLE row is reset here; the composed-registry refresher (cmd/worker/opclass_overlay_refresh.go,
	// WithLadderEvict) carries that reset into the per-process ENFORCEMENT ladder cache — it evicts the slug
	// when it (re)admits the row, in this process on the ratify kick and in every other within one refresh
	// interval, so GraduatedVerdict cannot serve a warm pre-reset level. (The general TG-146 S3/S4 cache
	// coherence for demotions written OUTSIDE the ratify/overlay path stays a separate, owner-deferred
	// concern; this closes only the re-ratify inheritance path, which is the one TG-177 owns.)
	lin, err := a.gradInheritance(ctx, c.OpClass, spec.OpClass)
	if err != nil {
		return Result{}, err
	}
	out, err := opclasscat.Transition(ctx, a.D.Store, a.D.Ledger, c, opclasscat.StatusRatified,
		lineageReason(req.Approver, req.Rationale, lin))
	if err != nil {
		return Result{}, err
	}
	// The reset lands AFTER the ledger records the boundary and BEFORE the grant goes live — the same
	// ledger-before-capability order the grant itself follows. A crash between here and the overlay write
	// leaves an ungraduated slug with no grant (the operator re-ratifies; the candidate is already
	// `ratified`, so the retry is refused as a bad transition and is visible), never a live grant carrying
	// inherited autonomy.
	if lin.reset {
		if err := a.D.Ladder.Save(ctx, policy.ClassState{OpClass: spec.OpClass, Level: policy.LevelApprove}); err != nil {
			return Result{}, fmt.Errorf("ratify: reset inherited graduation for %q (was %s): %w", spec.OpClass, lin.fromLvl, err)
		}
	}
	if err := a.D.Overlay.Ratify(ctx, Grant{
		OpClass: spec.OpClass, Spec: spec, EntryHash: hash, Family: spec.Family, Tier: spec.SafetyTier,
		PromoteThreshold: threshold, CandidateKey: req.CandidateKey, Approver: req.Approver,
		Rationale: req.Rationale, LedgerSeq: out.LedgerSeq,
	}); err != nil {
		return Result{}, err
	}
	a.D.notifyRefreshed()
	return Result{
		CandidateKey: out.CandidateKey, OpClass: spec.OpClass, Status: string(out.Status),
		LedgerSeq: out.LedgerSeq, EntryHash: hash,
	}, nil
}

// lineage is the boundary-move decision for one ratify (TG-177): how the operator-authored slug relates to
// the candidate's cluster slug, and whether that slug carried graduation the reused name never earned.
type lineage struct {
	parent  string // the candidate's cluster slug (normalized), where the evidence was clustered
	child   string // the operator-authored ratified slug (normalized), where the grant lands
	kind    string // "new" (same slug, no prior trust) | "rename" (boundary moved) | "reuse" (prior trust reset)
	reset   bool   // a prior graduation row above the fail-closed floor exists and is being reset to approve
	fromLvl string // the level being reset FROM, for the boundary trace (only meaningful when reset)
}

// gradInheritance reads any prior graduation for the AUTHORED slug and decides the fail-closed inheritance
// outcome. It NEVER promotes: the only durable write it authorizes (performed by the caller) is a reset to
// approve. An absent prior row is the normal case for a genuinely new class and is not an error; any other
// load failure is fatal to the ratify (a grant must not proceed while its inheritance is unknown — fail
// closed toward refusing, not toward assuming clean).
//
// A prior row counts as inherited trust if it is anything other than a PRISTINE approve — a promoted level
// OR a mid-climb approve streak. A reused name must re-earn every clean run, so a class sitting at
// approve with CleanRunCount 4 (one short of promotion) is reset too, not only a fully promoted one.
func (a *Activities) gradInheritance(ctx context.Context, candidateSlug, authoredSlug string) (lineage, error) {
	lin := lineage{
		parent: strings.ToLower(strings.TrimSpace(candidateSlug)),
		child:  strings.ToLower(strings.TrimSpace(authoredSlug)),
		kind:   "new",
	}
	if lin.child != lin.parent {
		lin.kind = "rename"
	}
	prior, err := a.D.Ladder.Load(ctx, authoredSlug)
	if err != nil {
		if errors.Is(err, policy.ErrClassAbsent) {
			return lin, nil // no prior state — the class starts fresh by default, nothing to reset
		}
		return lineage{}, fmt.Errorf("ratify: load prior graduation for %q: %w", authoredSlug, err)
	}
	if prior.Level != policy.LevelApprove || prior.CleanRunCount > 0 || prior.NoticeRunCount > 0 {
		lin.reset = true
		lin.kind = "reuse"
		lin.fromLvl = prior.Level.String()
	}
	return lin, nil
}

// lineageReason folds the boundary trace into the ratify ledger reason so the hash chain records the split /
// rename / reuse next to WHO ratified it — "no silent rename path remains" (TG-177). The shape mirrors
// reasonWithApprover's "[approver] rationale" so a reader grepping the chain need not learn a second format.
func lineageReason(approver, rationale string, lin lineage) string {
	trace := fmt.Sprintf(" | lineage: parent=%s child=%s kind=%s", lin.parent, lin.child, lin.kind)
	if lin.reset {
		trace += " reset_from=" + lin.fromLvl
	}
	return reasonWithApprover(approver, rationale) + trace
}

func (a *Activities) dismiss(ctx context.Context, req Request) (Result, error) {
	c, found, err := a.D.Loader.CandidateByKey(ctx, req.CandidateKey)
	if err != nil {
		return Result{}, err
	}
	if !found {
		return Result{}, ErrNotFound
	}
	out, err := opclasscat.Transition(ctx, a.D.Store, a.D.Ledger, c, opclasscat.StatusDismissed,
		reasonWithApprover(req.Approver, req.Rationale))
	if err != nil {
		return Result{}, err
	}
	return Result{CandidateKey: out.CandidateKey, OpClass: out.OpClass, Status: string(out.Status), LedgerSeq: out.LedgerSeq}, nil
}

// demote drops ANY rung to approve and resets BOTH streaks (REQ-2810).
//
// Both, not one, because the two climbs are separately counted and a demotion that cleared only the second
// would leave a class one clean run from re-entering auto_notice immediately after an operator judged it
// unsafe. The ledger row is appended BEFORE the ladder write, the same direction as every other decision
// here: an over-recorded demotion is a paperwork problem, an unrecorded one is a class quietly operating at
// a rung nobody granted it.
func (a *Activities) demote(ctx context.Context, req Request) (Result, error) {
	st, err := a.D.Ladder.Load(ctx, req.OpClass)
	if err != nil {
		return Result{}, err
	}
	entry, err := a.D.Ledger.Append(audit.GovDecision{
		Decision: "opclass:demote",
		Reason:   reasonWithApprover(req.Approver, req.Rationale),
		ActionID: req.OpClass,
	})
	if err != nil {
		return Result{}, err
	}
	st.OpClass = req.OpClass
	st.Level = policy.LevelApprove
	st.CleanRunCount = 0
	st.NoticeRunCount = 0
	// An operator demotion is AUTHORITATIVE — it must win over any concurrent worker Record, exactly like the
	// ratify reset above. Save unconditionally (version 0), NOT under the TG-146 S3/S4 optimistic-concurrency
	// guard: this activity runs MaximumAttempts=1 (no engine retry, line ~466), so a compare-and-set miss on a
	// benign race would fail the operator's demotion outright — leaving the class at the autonomous rung the
	// operator just judged unsafe — rather than dropping it to approve. Version 0 makes the store apply this
	// write unconditionally and bump the version, so a racing Record's stale CAS fails and it reloads approve.
	st.Version = 0
	if err := a.D.Ladder.Save(ctx, st); err != nil {
		return Result{}, err
	}
	return Result{OpClass: req.OpClass, Level: policy.LevelApprove.String(), LedgerSeq: entry.Seq}, nil
}

// revoke withdraws a live grant. The class falls to rung 0 — registry absence — because the overlay loader
// excludes revoked rows, not because anything writes a "revoked" level somewhere.
func (a *Activities) revoke(ctx context.Context, req Request) (Result, error) {
	live, err := a.D.Overlay.IsLive(ctx, req.OpClass)
	if err != nil {
		return Result{}, err
	}
	if !live {
		return Result{}, ErrNotGranted
	}
	entry, err := a.D.Ledger.Append(audit.GovDecision{
		Decision: opclasscat.DecisionRevoke,
		Reason:   reasonWithApprover(req.Approver, req.Rationale),
		ActionID: req.OpClass,
	})
	if err != nil {
		return Result{}, err
	}
	if err := a.D.Overlay.Revoke(ctx, req.OpClass, req.Approver, req.Rationale, entry.Seq); err != nil {
		return Result{}, err
	}
	// The kick matters MORE here than on ratify: until the refresh lands, a revoked class is still being
	// served by the composed registry — the unsafe direction of staleness.
	a.D.notifyRefreshed()
	return Result{OpClass: req.OpClass, Level: policy.LevelApprove.String(), LedgerSeq: entry.Seq}, nil
}

// exportEmbed renders the promotion MR body and ledgers the INTENT. It changes nothing.
//
// It is ledgered anyway (REQ-2820) because an operator declaring that a class should move into the embedded
// tamper domain — the one where actions run with nobody watching — is a governance-relevant act even when
// the immediate output is only text. The merge that follows is the grant; this records who asked for it.
func (a *Activities) exportEmbed(ctx context.Context, req Request) (Result, error) {
	live, err := a.D.Overlay.IsLive(ctx, req.OpClass)
	if err != nil {
		return Result{}, err
	}
	if !live {
		return Result{}, ErrNotGranted
	}
	spec, ok := opschema.Lookup(req.OpClass)
	if !ok {
		return Result{}, ErrNotGranted
	}
	body, err := a.D.Export.MRBody(spec)
	if err != nil {
		return Result{}, err
	}
	entry, err := a.D.Ledger.Append(audit.GovDecision{
		Decision: "opclass:export-embed",
		Reason:   reasonWithApprover(req.Approver, req.Rationale),
		ActionID: req.OpClass,
	})
	if err != nil {
		return Result{}, err
	}
	return Result{OpClass: req.OpClass, LedgerSeq: entry.Seq, Artifact: body}, nil
}

// OpClassVerbWorkflow is the one-activity verb workflow. Named DISTINCTLY — Temporal registers by BARE
// function name, so a generic `TransitionWorkflow` here would collide with skillwrite's and panic the worker
// at boot (the 2026-07-17 boot-loop; manifestwrite carries the same note). Both join temporal/skilltrial's
// names guard.
//
// No retries: a refused verb (bad transition, missing rationale, a template that byte-matches model text) is
// a DECISION, not a transient. Retrying would re-attempt against a row the first attempt may already have
// moved, and would turn one refused ratification into several identical ledger rows.
func OpClassVerbWorkflow(ctx workflow.Context, req Request) (Result, error) {
	opts := workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	}
	var res Result
	// Temporal dispatches by REGISTERED FUNCTION NAME: the zero-Deps receiver here only names the activity;
	// the worker's registered instance (with the real collaborators) executes it.
	err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, opts), new(Activities).OpClassVerbActivity, req).Get(ctx, &res)
	return res, err
}
