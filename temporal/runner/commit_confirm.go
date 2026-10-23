package runner

// spec/029 T-029-2 — the ARMED REVERT (REQ-2901, REQ-2906). For a commit-confirmed-eligible
// op-class the actuation arms a dead-man's switch BEFORE the effect executes: a durable
// commit_confirm row plus a CommitConfirmWorkflow child holding ONE Temporal timer (never an
// in-process timer). An armed revert that cannot be durably recorded refuses the forward effect
// — fail closed; an unarmed "armed revert" is the pve03 shape wearing a timer.
//
// Division of labour across the spec's tasks, fixed here so T-029-3 extends without breaking:
//   - T-029-2 (this file): eligibility read (registry data, T-029-1), the arm insert, the child
//     timer, the abort arm (the forward provably did not execute), and the interim elapse arm
//     (state elapsed_unconfirmed — recorded loudly, no inverse yet).
//   - T-029-3: the confirm-input signal sender (terminus verify's verdict routing, REQ-2902
//     HOLD+page on unverifiable), the elapse-time terminus consult, and the fired-inverse arm
//     through the FULL interceptor chain (RollbackWorkflow machinery, InvertsActionID).
//
// Single-writer discipline on the row: the parent's ArmCommitConfirmActivity is the ONLY insert;
// every later transition belongs to the child (ResolveCommitConfirmActivity), guarded in SQL to
// leave 'armed' exactly once — a lost or duplicate signal can never resurrect a resolved window.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/safety"
	tg "github.com/territory-grounder/grounder/temporal"
)

// CommitConfirmSignalName is the child's one resolution channel. The payload's Kind vocabulary is
// fixed here in full; T-029-2 acts on "abort", T-029-3 adds the "confirm-input" sender + handling.
const CommitConfirmSignalName = "commit-confirm-resolve"

// CommitConfirmResolve is the signal payload.
type CommitConfirmResolve struct {
	Kind     string // "abort" — the forward effect provably did not execute; "confirm-input" — T-029-3's terminus verdict routing
	Verdict  string // T-029-3: the mechanical verify's 3-value verdict
	Verified bool   // T-029-3: the TG-182 observedOK bit (REQ-2902 — unknown is not confirmed)
	Detail   string // human-legible reason, recorded on the row
}

// CommitConfirmRecorder is the narrow durable-state seam (satisfied by *db.CommitConfirmStore; the
// eval/acceptance harnesses wire a fake). Nil + an ELIGIBLE class is an arming failure — the
// workflow refuses the forward (REQ-2901 fail closed), never a silent skip.
type CommitConfirmRecorder interface {
	ArmCommitConfirm(ctx context.Context, r db.CommitConfirmRow) error
	Resolve(ctx context.Context, actionID, externalRef, state, detail, inverseActionID string) error
}

// ArmCommitConfirmInput carries the sealed action's identity into the arm write, plus the fired
// inverse's AUTHORIZATION BASIS captured at this instant (T-029-3, migration 0096): the forward's
// live classification band and whether a human vote approved it — known here because the arm runs
// after the vote block and with the fresh decision in scope. AlertRule is the incident signature
// the REQ-2902 hold-watch re-observes with.
type ArmCommitConfirmInput struct {
	ActionID    string
	ExternalRef string
	OpClass     string
	TargetHost  string
	Site        string
	PlanHash    string
	Band        string
	Approved    bool
	AlertRule   string
	// GuestParam is the sealed action's params["guest"] — the entity a guest-lifecycle
	// precondition is READ AGAINST. Threaded because Action.Target and params["guest"] are
	// independently LLM-populated and can diverge (the reason core/predict/gate.go and the
	// inverse seal both resolve guest-first with a Target fallback); the REQ-2908 no-op read
	// must ask about the same entity those gates do (round-2 review HIGH finding).
	GuestParam string
}

// ArmCommitConfirmResult reports eligibility (registry data, REQ-2904) and the armed window. The
// eligibility read lives INSIDE the activity — never in workflow code — so a registry change on
// redeploy can never make an old history replay differently: the recorded result is authoritative.
// MandateRefused (REQ-2905, T-029-4): the class holds a canary/staged posture but is NOT
// commit-confirmed eligible — the forward SHALL NOT execute without the armed revert, so the
// workflow refuses it exactly like an unarmable window.
type ArmCommitConfirmResult struct {
	Eligible       bool
	WindowSeconds  int64
	MandateRefused bool
	MandateReason  string
	// NoOpRefused (REQ-2908, T-029-5): the target already holds the desired end state at ARM time
	// — the effect resolves as a refused no-op before anything arms (a stand-down, not a failure).
	NoOpRefused bool
	NoOpReason  string
}

// ArmCommitConfirmActivity durably arms the revert window BEFORE the effect executes. A non-nil
// error means the window could NOT be durably recorded — the workflow converts that into
// refuse-forward. A class without a commit_confirmed declaration returns Eligible=false and
// touches nothing.
func (a *Activities) ArmCommitConfirmActivity(ctx context.Context, in ArmCommitConfirmInput) (ArmCommitConfirmResult, error) {
	spec, ok := opschema.Lookup(in.OpClass)
	if !ok || spec.CommitConfirmed == nil {
		// THE CANARY MANDATE (REQ-2905, T-029-4): while a class holds a canary/staged posture —
		// the deployment's pinned (host, op_class) allowlist, or the earned-but-observed
		// AUTO_NOTICE rung (the "first N graduated runs" phase; RungApprove is ordinary
		// pre-graduation polling, not a stage) — commit-confirmed is MANDATORY: an ineligible
		// class in that posture may not execute at all. It LAYERS ON the existing canary law
		// (the POLL forcing and its human vote are upstream and untouched); this is the last
		// line: no armed revert, no forward. Read here, in the activity, for the same
		// replay-safety reason as eligibility.
		if ok { // an UNREGISTERED class is refused upstream; the mandate concerns registered-but-ineligible ones
			if a.D.CanaryPinned != nil {
				if pinned, why := a.D.CanaryPinned(in.TargetHost, in.OpClass); pinned {
					return ArmCommitConfirmResult{MandateRefused: true,
						MandateReason: "canary-pinned (" + why + ") and not commit-confirmed eligible — the staged posture mandates the armed revert (REQ-2905)"}, nil
				}
			}
			if a.rungFor(in.OpClass) == RungAutoNotice {
				return ArmCommitConfirmResult{MandateRefused: true,
					MandateReason: "op-class holds the earned-but-observed AUTO_NOTICE rung and is not commit-confirmed eligible — the staged posture mandates the armed revert (REQ-2905)"}, nil
			}
		}
		return ArmCommitConfirmResult{Eligible: false}, nil
	}
	window := int64(spec.CommitConfirmed.WindowSeconds)
	// REQ-2908 (T-029-5): an EMPTY DIFF is a free no-op — where the class can compute one. For the
	// state-preconditioned classes the diff IS the target-state read: if the target already holds
	// the DESIRED end state at ARM time (a start-guest whose guest is RUNNING — it self-recovered
	// or someone raced us during the vote wait), the effect resolves as a refused no-op BEFORE
	// anything arms. This is the TG-378 seal-time precondition re-run at apply-time: the seal's
	// read can be minutes stale behind a human vote. Unestablished at arm is NOT a no-op (the
	// seal already established once; fail toward arming — the window itself is the protection).
	if want := spec.RequiresTargetState; want != "" && a.D.Gate != nil && a.D.Gate.GuestRunning != nil {
		// The SAME target resolution every precondition consumer uses (core/predict/gate.go:89,
		// SealCommitConfirmInverseActivity): params["guest"] first, Action.Target as the
		// fallback — the two are independently LLM-populated and can diverge, and a no-op
		// declared off the WRONG entity's state would refuse a genuinely-needed remediation.
		target := strings.TrimSpace(in.GuestParam)
		if target == "" {
			target = strings.TrimSpace(in.TargetHost)
		}
		if running, prov, ok := a.D.Gate.GuestRunning(ctx, target); ok {
			alreadyDesired := (want == opschema.RequiresNotRunning && running) ||
				(want == opschema.RequiresRunning && !running)
			if alreadyDesired {
				return ArmCommitConfirmResult{NoOpRefused: true,
					NoOpReason: fmt.Sprintf("target %s already holds the desired end state (%s) — empty diff, free no-op (REQ-2908)", target, prov)}, nil
			}
		}
	}
	if strings.TrimSpace(in.ActionID) == "" || strings.TrimSpace(in.ExternalRef) == "" {
		return ArmCommitConfirmResult{}, fmt.Errorf("arm commit-confirm: empty action identity (action_id %q, external_ref %q)", in.ActionID, in.ExternalRef)
	}
	if a.D.CommitConfirm == nil {
		return ArmCommitConfirmResult{}, fmt.Errorf("arm commit-confirm: no durable store wired for eligible class %s — refusing (REQ-2901 fail closed)", in.OpClass)
	}
	if err := a.D.CommitConfirm.ArmCommitConfirm(ctx, db.CommitConfirmRow{
		ActionID:        in.ActionID,
		ExternalRef:     in.ExternalRef,
		OpClass:         in.OpClass,
		TargetHost:      in.TargetHost,
		Site:            in.Site,
		PlanHash:        in.PlanHash,
		WindowSeconds:   window,
		ForwardBand:     in.Band,
		ForwardApproved: in.Approved,
		AlertRule:       in.AlertRule,
	}); err != nil {
		return ArmCommitConfirmResult{}, err
	}
	// REQ-2906: the arm appends to the tamper-evident ledger bound to the action_id. The append
	// follows the durable row write — a ledger entry for an unrecorded window would be the lie.
	if _, err := a.D.Ledger.Append(audit.GovDecision{
		Decision: "commit-confirm:armed",
		Reason:   fmt.Sprintf("class %s window %ds incident %s", in.OpClass, window, in.ExternalRef),
		ActionID: in.ActionID,
	}); err != nil {
		return ArmCommitConfirmResult{}, fmt.Errorf("arm commit-confirm %s: ledger append: %w", in.ActionID, err)
	}
	return ArmCommitConfirmResult{Eligible: true, WindowSeconds: window}, nil
}

// ResolveCommitConfirmInput moves an armed row to a resolution state (child-owned).
type ResolveCommitConfirmInput struct {
	ActionID        string
	ExternalRef     string
	State           string // a db.CommitConfirm* non-armed state
	Detail          string
	InverseActionID string // T-029-3: the fired inverse's sealed action id
}

// ResolveCommitConfirmActivity applies one row transition + its ledger append. A row already
// resolved (duplicate/late signal) is success — the earlier resolution stands and no second
// ledger entry is written.
func (a *Activities) ResolveCommitConfirmActivity(ctx context.Context, in ResolveCommitConfirmInput) error {
	if a.D.CommitConfirm == nil {
		return fmt.Errorf("resolve commit-confirm: no durable store wired")
	}
	if err := a.D.CommitConfirm.Resolve(ctx, in.ActionID, in.ExternalRef, in.State, in.Detail, in.InverseActionID); err != nil {
		if errors.Is(err, db.ErrCommitConfirmResolved) {
			// The row already left 'armed'. TWO cases, and the old silent early-return conflated
			// them (round-2 review finding #1: a ledger blip after the transition made the retry
			// land here and silently drop BOTH the ledger entry and the graduation feed — a
			// confirmed resolution invisible to the ladder forever):
			//   - a DIFFERENT state won: a true duplicate/late signal. The winning resolution's
			//     own attempt owns its ledger+feed; quiet nil is correct.
			//   - OUR OWN state: this activity's retry — the transition landed on a prior attempt
			//     whose tail (ledger append, feed) did not. RUN THE TAIL NOW. The residual is a
			//     rare benign duplicate ledger entry (activity timeout between append and return);
			//     for an audit surface, a duplicate trace beats a missing one. A re-fed PROMOTION
			//     is deduped by the ladder's external_ref claim key; a re-fed DEMOTION has no claim
			//     key but is idempotent by construction (the Unverified outcome is a pure counter
			//     reset) — precision owed to the T-029-4 round-2 review, which traced both halves.
			row, gerr := a.commitConfirmRowFor(ctx, in.ActionID, in.ExternalRef)
			if gerr != nil {
				return fmt.Errorf("resolve commit-confirm %s: already-resolved but cannot read the winner (retry): %w", in.ActionID, gerr)
			}
			if row.State != in.State {
				return nil
			}
			// fall through to the tail below
		} else {
			return err
		}
	}
	if _, err := a.D.Ledger.Append(audit.GovDecision{
		Decision: "commit-confirm:" + in.State,
		Reason:   in.Detail,
		ActionID: in.ActionID,
	}); err != nil {
		return fmt.Errorf("resolve commit-confirm %s → %s: ledger append: %w", in.ActionID, in.State, err)
	}
	// THE CONFIRMED-ONLY GRADUATION FEED (REQ-2907, T-029-4): the window's resolution — not the
	// session terminus — is what reaches the ladder for a commit-confirmed run. confirmed is the
	// ONLY promoting outcome; a fired revert and a failed revert are demotions (clean=false);
	// aborted / elapsed / held feed NOTHING (armed-never-counts — the spec/017 pending-never-counts
	// discipline applied to the armed state). The external_ref stays the exactly-once credit key
	// (the ladder dedups by it), so a duplicate resolve retry cannot double-feed. Runs AFTER the
	// transition landed — a credit for a resolution that did not persist would be the lie. The
	// terminus-side WITHHOLD for eligible classes lives in recordGraduationCredit.
	var clean, feed bool
	switch in.State {
	case db.CommitConfirmConfirmed:
		clean, feed = true, true
	case db.CommitConfirmReverted, db.CommitConfirmRevertFailed:
		clean, feed = false, true
	}
	if feed && a.D.RecordGraduation != nil {
		row, gerr := a.commitConfirmRowFor(ctx, in.ActionID, in.ExternalRef)
		if gerr != nil || strings.TrimSpace(row.OpClass) == "" {
			log.Printf("commit-confirm: graduation feed for %s skipped — cannot resolve the op-class: %v", in.ExternalRef, gerr)
		} else if gerr := a.D.RecordGraduation(ctx, row.OpClass, in.ExternalRef, clean); gerr != nil {
			// Best-effort like the terminus credit path: the resolution stands; the ladder miss is loud.
			log.Printf("commit-confirm: graduation feed %s (%s clean=%v) failed: %v", in.ExternalRef, row.OpClass, clean, gerr)
		}
	}
	return nil
}

// commitConfirmRowFor loads the resolved window's row (for its op-class). Split out so the
// graduation feed reads through the same recorder the rest of the lifecycle uses.
func (a *Activities) commitConfirmRowFor(ctx context.Context, actionID, externalRef string) (db.CommitConfirmRow, error) {
	getter, ok := a.D.CommitConfirm.(interface {
		Get(ctx context.Context, actionID, externalRef string) (db.CommitConfirmRow, error)
	})
	if !ok {
		return db.CommitConfirmRow{}, fmt.Errorf("recorder cannot load rows")
	}
	return getter.Get(ctx, actionID, externalRef)
}

// CommitConfirmExecutionReader is the consult's terminus read (satisfied by *db.ActionExecutionStore):
// the per-run action_execution row carrying the FRESH verdict + the TG-182 verified bit, recorded on
// BOTH paths by the interceptor (an unverifiable run writes NULL verdict + unverifiable=true).
type CommitConfirmExecutionReader interface {
	ExecutionFor(ctx context.Context, actionID, externalRef string) (db.ForwardExecution, bool, error)
}

// ConsultCommitConfirmInput asks the terminus how one (action, incident) run actually went.
type ConsultCommitConfirmInput struct {
	ActionID    string
	ExternalRef string
	// AlertRule is the incident signature (TG-461): the SERVICE-fault durable-substitute confirm scopes
	// RecoveredSince to it, so an unrelated rule recovering on the same host cannot confirm this incident.
	AlertRule string
}

// The consult's closed disposition vocabulary (REQ-2902 routed over DURABLE state — never a model
// token, never an in-band value a dead parent could have failed to deliver). There is deliberately
// NO "aborted" reading: the consult can prove an execution happened, but it can never prove one
// did NOT (the review-#1 lesson — a chain-gap run can lose every terminus write, and TOTAL absence
// is indistinguishable from never-ran). Provable non-execution arrives only via the parent's abort
// signal; absence at elapse fails toward the HOLD, never toward standing the dead-man down.
const (
	ConsultPending      = "pending"      // nothing at the terminus — pre-execute, record lag, or a fully-lost trace
	ConsultConfirmed    = "confirmed"    // fresh verdict match, verified=true — the ONLY confirming reading
	ConsultUnverifiable = "unverifiable" // executed with verified=false (verdict withheld) — REQ-2902's HOLD case
	ConsultDeviation    = "deviation"    // executed, verified, verdict partial/deviation — the inverse's case
)

// ConsultCommitConfirmResult is one terminus reading plus the live post-state observation the
// REQ-2902 hold-watch fires on.
type ConsultCommitConfirmResult struct {
	Disposition string
	Detail      string
	// ObservedAlerting: the target currently carries at least one active alert, read POSITIVELY from
	// the live post-state observer. nil = could not observe — and the hold-watch NEVER fires on a
	// failed read (the mirror of the forwardEffectPresent inversion trap: "not cleared" folds a read
	// error into "alerting", which would fire a revert on monitoring blindness).
	ObservedAlerting *bool
}

// ConsultCommitConfirmActivity reads the durable terminus for one run. Confirm-from-the-terminus-
// ONLY (the task's title): the per-run execution record is the sole confirm source; the manifest
// lifecycle chain is the executed-BELT for the record-write-failed control gap (the execution
// record is best-effort by design, the chain's executed stage is fail-loud).
func (a *Activities) ConsultCommitConfirmActivity(ctx context.Context, in ConsultCommitConfirmInput) (ConsultCommitConfirmResult, error) {
	if a.D.Executions == nil {
		return ConsultCommitConfirmResult{}, fmt.Errorf("consult commit-confirm: no execution reader wired")
	}
	exec, found, err := a.D.Executions.ExecutionFor(ctx, in.ActionID, in.ExternalRef)
	if err != nil {
		return ConsultCommitConfirmResult{}, err
	}
	chainExecuted := false
	var targetHost, site, opClass, guestParam string
	if a.D.Manifests != nil {
		if m, ok, merr := a.D.Manifests.Get(ctx, in.ActionID); merr == nil && ok && m != nil {
			targetHost = m.Action.Target // site rides the execution record; the manifest carries none
			opClass = m.Action.OpClass                        // TG-499: the durable-substitute confirm below
			guestParam = m.Action.Params[opschema.ParamGuest] // reads the class's state precondition + guest
			for _, st := range m.Stages {
				if st.Stage == manifest.StageExecuted {
					chainExecuted = true
					break
				}
			}
		}
	}
	res := ConsultCommitConfirmResult{}
	switch {
	case !found && !chainExecuted:
		res.Disposition, res.Detail = ConsultPending, "no execution at the terminus (no per-run record, no executed stage on the manifest chain)"
	case !found && chainExecuted:
		res.Disposition, res.Detail = ConsultUnverifiable, "executed per the manifest lifecycle chain but the per-run record is missing (record-write control gap) — outcome unknown"
	case exec.Unverifiable:
		res.Disposition, res.Detail = ConsultUnverifiable, "executed; post-state was unobservable (verdict withheld at the terminus)"
	case exec.Verdict == string(safety.VerdictMatch):
		res.Disposition, res.Detail = ConsultConfirmed, "terminus verdict match, verified — mechanical confirm"
	case exec.Verdict == string(safety.VerdictPartial) || exec.Verdict == string(safety.VerdictDeviation):
		res.Disposition, res.Detail = ConsultDeviation, "terminus verdict "+exec.Verdict+" — a non-confirm (REQ-2902)"
	default:
		// A recorded, verified run with an empty/unknown verdict should be unreachable; hold, never confirm.
		res.Disposition, res.Detail = ConsultUnverifiable, fmt.Sprintf("terminus record carries verdict %q — cannot confirm on it", exec.Verdict)
	}
	if found {
		targetHost, site = exec.TargetHost, exec.Site
	}
	// The live post-state, read positively (nil on any doubt).
	if a.D.PostStateObserve != nil && targetHost != "" {
		if alerts, ok := a.D.PostStateObserve(ctx, targetHost, site); ok {
			alerting := false
			for _, al := range alerts {
				if al.Host == targetHost {
					alerting = true
					break
				}
			}
			res.ObservedAlerting = &alerting
		}
	}
	// TG-499 (REQ-2902 durable-substitute confirm): a STATE-PRECONDITIONED guest heal whose terminus
	// post-state was unobservable (exec.Unverifiable — the actuate-plane LibreNMS read is 403-blind,
	// truncation-refused, or mid-poll) MAY still confirm on a FRESH, POSITIVE, mechanical guest_liveness
	// re-read that fails DIFFERENTLY than the monitor — the same durable-substitute move spec/012's
	// confirmed-clear belt makes when the LibreNMS re-pull lags (REQ-1122/REQ-1223). POSITIVE-ONLY +
	// fail-closed: GuestRunning returns ok=false on any stale/unreadable projection (guest_liveness.go),
	// and a target NOT in the desired end state does not confirm — either path leaves the disposition
	// Unverifiable → HOLD+page. Scoped to found+exec.Unverifiable (the record exists and is unobservable),
	// never the missing-record or absence case; mirrors the arm-time target resolution (REQ-2908).
	if res.Disposition == ConsultUnverifiable && found && exec.Unverifiable &&
		a.D.Gate != nil && a.D.Gate.GuestRunning != nil {
		if ocSpec, ok := opschema.Lookup(opClass); ok && ocSpec.RequiresTargetState != "" {
			target := strings.TrimSpace(guestParam) // guest param first, Action.Target fallback (the arm's resolution)
			if target == "" {
				target = strings.TrimSpace(targetHost)
			}
			if running, prov, obsOK := a.D.Gate.GuestRunning(ctx, target); obsOK {
				reachedDesiredEndState := (ocSpec.RequiresTargetState == opschema.RequiresNotRunning && running) ||
					(ocSpec.RequiresTargetState == opschema.RequiresRunning && !running)
				if reachedDesiredEndState {
					res.Disposition = ConsultConfirmed
					res.Detail = fmt.Sprintf("post-state unobservable at the terminus, but a fresh POSITIVE guest_liveness "+
						"re-read (%s) confirms the guest holds the desired end state — durable-substitute confirm (REQ-2902/TG-499)", prov)
				}
			}
		}
	}
	// TG-461 option-c (REQ-2902 durable-substitute confirm, SERVICE-FAULT slice): a commit-confirm-eligible
	// heal that is NOT state-preconditioned (a service-fault class — restart-service/start-service/…,
	// RequiresTargetState == "") whose terminus post-state was unobservable (exec.Unverifiable) MAY confirm
	// on a POSITIVE captured provider recovery. RecoveredSince reports whether TG durably CAPTURED an
	// ingest_transition 'recovery' row for THIS (host, rule-family) at/after the execution instant (spec/012
	// REQ-1122/1223; TG's OWN evidence, never the model's word, INV-11). POSITIVE-ONLY + fail-closed:
	// RecoveredSince returns (false, err) on an empty rule / no-alias / no-row / query-error, and a missing
	// recovery does NOT confirm — either path leaves the disposition Unverifiable → HOLD+page. It confirms on
	// a POSITIVE recovery RECORD, NEVER on the absence of an open incident (REQ-2902 "never the absence of
	// alerts alone"; reconciler.go "no close on asserted success"). Complements the guest_liveness slice
	// above; scoped to a NON-state-preconditioned class + a non-zero ExecutedAt, so the guest path is never
	// double-served and a zero execute-time can never match every prior recovery.
	if res.Disposition == ConsultUnverifiable && found && exec.Unverifiable &&
		a.D.RecoveredSince != nil && strings.TrimSpace(in.AlertRule) != "" && !exec.ExecutedAt.IsZero() {
		if ocSpec, ok := opschema.Lookup(opClass); ok && ocSpec.RequiresTargetState == "" {
			if recovered, rerr := a.D.RecoveredSince(ctx, targetHost, in.AlertRule, exec.ExecutedAt); rerr == nil && recovered {
				res.Disposition = ConsultConfirmed
				res.Detail = "post-state unobservable at the terminus, but TG durably CAPTURED a provider recovery " +
					"transition (ingest_transition) for the incident host at/after execute — positive durable-substitute confirm (REQ-2902/TG-461)"
			}
		}
	}
	return res, nil
}

// SealCommitConfirmInverseInput fires the compensating action for one failed window.
type SealCommitConfirmInverseInput struct {
	ActionID    string
	ExternalRef string
}

// SealCommitConfirmInverseResult carries the sealed inverse plus everything the actuate-queue
// execute step needs. Sealed=false with Reason is a REFUSAL (fail closed) — the child records
// revert_failed and pages: an unrevertable armed revert is an incident, not a no-op (REQ-2903).
type SealCommitConfirmInverseResult struct {
	Sealed          bool
	Reason          string
	InverseActionID string
	RollbackIn      RollbackInput
	ApprovedBasis   bool
}

// SealCommitConfirmInverseActivity derives and seals the fired inverse from DURABLE state only:
// the commit_confirm row (op-class, target, authorization basis captured at arm time) + the
// forward's sealed manifest (op, params, reversibility, band). The inverse manifest seals at the
// FORWARD's band — the envelope that admitted the forward is the basis its declared revert
// carries — and the interceptor still judges the request fresh (mode, floors, territory,
// necessity), so a basis the chain rejects refuses and pages rather than bypassing anything.
func (a *Activities) SealCommitConfirmInverseActivity(ctx context.Context, in SealCommitConfirmInverseInput) (SealCommitConfirmInverseResult, error) {
	if a.D.CommitConfirm == nil {
		return SealCommitConfirmInverseResult{}, fmt.Errorf("seal commit-confirm inverse: no durable store wired")
	}
	getter, ok := a.D.CommitConfirm.(interface {
		Get(ctx context.Context, actionID, externalRef string) (db.CommitConfirmRow, error)
	})
	if !ok {
		return SealCommitConfirmInverseResult{}, fmt.Errorf("seal commit-confirm inverse: recorder cannot load rows")
	}
	row, err := getter.Get(ctx, in.ActionID, in.ExternalRef)
	if err != nil {
		return SealCommitConfirmInverseResult{}, err
	}
	if a.D.Manifests == nil {
		return SealCommitConfirmInverseResult{Reason: "no manifest store — cannot derive the inverse from the sealed forward"}, nil
	}
	fm, found, err := a.D.Manifests.Get(ctx, in.ActionID)
	if err != nil {
		return SealCommitConfirmInverseResult{}, err
	}
	if !found || fm == nil {
		return SealCommitConfirmInverseResult{Reason: "forward manifest missing — cannot derive the inverse"}, nil
	}
	spec, ok := opschema.Lookup(row.OpClass)
	if !ok {
		return SealCommitConfirmInverseResult{Reason: "op-class " + row.OpClass + " is not registered"}, nil
	}
	// The blind-mutation guard for CLASS inverses (the stop-guest case): when the inverse class
	// declares a target-state precondition, ESTABLISH it here exactly as the forward seal gate
	// would — unknown is not running, an unwired reader refuses (TG-378 mirrored; never stop a
	// guest someone else already cycled).
	if invClass := strings.TrimSpace(spec.RollbackOpClass); invClass != "" {
		invSpec, invOK := opschema.Lookup(invClass)
		if !invOK {
			return SealCommitConfirmInverseResult{Reason: "inverse class " + invClass + " is not registered"}, nil
		}
		if want := invSpec.RequiresTargetState; want != "" {
			if a.D.Gate == nil || a.D.Gate.GuestRunning == nil {
				return SealCommitConfirmInverseResult{Reason: "inverse class " + invClass + " declares a state precondition and no state reader is wired — refusing a blind revert"}, nil
			}
			target := fm.Action.Params[opschema.ParamGuest]
			if target == "" {
				target = row.TargetHost
			}
			running, prov, obsOK := a.D.Gate.GuestRunning(ctx, target)
			if !obsOK {
				return SealCommitConfirmInverseResult{Reason: "inverse precondition unestablished for " + target + " (" + prov + ") — refusing a blind revert"}, nil
			}
			if (want == opschema.RequiresRunning && !running) || (want == opschema.RequiresNotRunning && running) {
				return SealCommitConfirmInverseResult{Reason: fmt.Sprintf("inverse precondition violated: %s requires %s, target %s observed otherwise (%s)", invClass, want, target, prov)}, nil
			}
		}
	}
	rin := RollbackInput{
		ForwardActionID:     in.ActionID,
		ForwardOpClass:      row.OpClass,
		ForwardOp:           fm.Action.Op,
		ForwardTarget:       row.TargetHost,
		ForwardParams:       fm.Action.Params,
		ForwardReversible:   fm.Action.Reversible,
		ForwardSite:         row.Site,
		ForwardExternalRef:  in.ExternalRef,
		RollbackExternalRef: "ccrevert-" + in.ExternalRef,
	}
	// Two inverse shapes (REQ-2903), each with its own derivation gate:
	//   - SELF-INVERSE (rollback_template / idempotent verb): the manual lane's rollbackArgvFor is
	//     the authority, exactly as TG-462 built it.
	//   - CLASS INVERSE (rollback_op_class, e.g. start-guest→stop-guest): the compensating action
	//     is a FIRST-CLASS action of the inverse class — its argv is that class's OWN compiled
	//     builder, never a rollback template (rollbackArgvFor would refuse it as "no safe inverse",
	//     which is correct for the MANUAL lane but wrong here: the registry's cross-ref pass
	//     validated this exact pairing at load). Caught by the class-inverse drill — the first cut
	//     gated BOTH shapes through rollbackArgvFor and the stop-guest fire could never happen.
	var inverse manifest.Action
	if invClass := strings.TrimSpace(spec.RollbackOpClass); invClass != "" {
		invSpec, invOK := opschema.Lookup(invClass)
		if !invOK {
			return SealCommitConfirmInverseResult{Reason: "inverse class " + invClass + " is not registered"}, nil
		}
		if invSpec.SafetyTier != opschema.TierLowReversible {
			return SealCommitConfirmInverseResult{Reason: fmt.Sprintf("inverse class %s is tier %q, not %q — refusing (the same tier discipline as the manual lane)", invClass, invSpec.SafetyTier, opschema.TierLowReversible)}, nil
		}
		if !rin.ForwardReversible {
			return SealCommitConfirmInverseResult{Reason: "forward action was not sealed reversible — refusing the inverse"}, nil
		}
		if argv, aerr := invSpec.Argv(rin.ForwardParams); aerr != nil || len(argv) == 0 {
			return SealCommitConfirmInverseResult{Reason: fmt.Sprintf("inverse class %s cannot build its argv from the forward's params: %v", invClass, aerr)}, nil
		}
		inverse = manifest.Action{
			Target:     rin.ForwardTarget,
			OpClass:    invClass,
			Op:         invSpec.Op,
			Params:     rin.ForwardParams,
			Reversible: true,
		}
	} else {
		// The reversibility/argv derivation gate — the same authority the manual lane consults first.
		if _, err := rollbackArgvFor(spec, rin.ForwardReversible, rin.ForwardParams); err != nil {
			return SealCommitConfirmInverseResult{Reason: err.Error()}, nil
		}
		inverse = inverseActionFor(rin)
	}
	planHash := PlanHash(rin.RollbackExternalRef, "")
	// The inverse seals at the FRESH band captured on the row at arm time — NEVER the forward
	// manifest's band: the manifest is content-addressed and sealed first-wins, so a re-classified
	// later incident of the same action shape would reload a stale frozen band (the TG-126 trap,
	// same reason the admission gate takes decision.Band). Parsing is fail-closed: an unknown or
	// empty stored band reads as POLL_PAUSE, the most restrictive.
	m, err := manifest.New(inverse, commitConfirmBandFromRow(row.ForwardBand), planHash, "")
	if err != nil {
		return SealCommitConfirmInverseResult{}, err
	}
	m = m.WithProvenance(manifest.Provenance{IncidentRef: rin.RollbackExternalRef})
	if a.D.ManifestSink != nil {
		if err := a.D.ManifestSink.Seal(ctx, m); err != nil {
			return SealCommitConfirmInverseResult{}, err
		}
	}
	// The forward's human vote extends ONLY to a SELF-INVERSE (rollback_template — the identical
	// action shape the vote authorized, re-run to compensate). A CLASS inverse (rollback_op_class
	// — a DIFFERENT action, e.g. start→stop) never inherits the vote: INV-12's "the vote binds
	// THIS action" stays narrow, and a class inverse earns its autonomy through its own
	// inverse_only policy rule or it polls/refuses+pages at the chain (review finding #2 —
	// closing the stale-vote widening for future classes structurally, not by convention).
	approvedBasis := row.ForwardApproved && strings.TrimSpace(spec.RollbackOpClass) == ""
	if _, err := a.D.Ledger.Append(audit.GovDecision{
		Decision: "commit-confirm:inverse-sealed",
		Reason:   fmt.Sprintf("inverse %s of %s (class %s, band basis %s, approved basis %v — self-inverse only)", m.ActionID, in.ActionID, row.OpClass, fm.Band, approvedBasis),
		ActionID: in.ActionID,
	}); err != nil {
		return SealCommitConfirmInverseResult{}, fmt.Errorf("seal commit-confirm inverse %s: ledger append: %w", in.ActionID, err)
	}
	return SealCommitConfirmInverseResult{Sealed: true, InverseActionID: m.ActionID, RollbackIn: rin, ApprovedBasis: approvedBasis}, nil
}

// commitConfirmBandFromRow parses the band string the arm captured (Band.String() form) back to
// the typed band, failing CLOSED: anything unrecognized — including the empty string on a pre-0096
// row — is POLL_PAUSE, the most restrictive, exactly the enum's own zero-value discipline.
func commitConfirmBandFromRow(s string) safety.Band {
	switch s {
	case safety.BandAuto.String():
		return safety.BandAuto
	case safety.BandAutoNotice.String():
		return safety.BandAutoNotice
	default:
		return safety.BandPollPause
	}
}

// TripMutationBreakerActivity trips the process-global mutation breaker (REQ-2906: a revert-FAILED
// state pages immediately AND trips the breaker). Nil seam = the trip cannot happen — that is an
// ERROR, not a skip: a deployment that can fire inverses but cannot halt on their failure is
// running without the half of the control that makes failure safe.
func (a *Activities) TripMutationBreakerActivity(ctx context.Context, reason string) error {
	if a.D.BreakerTrip == nil {
		return fmt.Errorf("trip mutation breaker: no breaker wired (reason was: %s)", reason)
	}
	return a.D.BreakerTrip(ctx, reason)
}

// CommitConfirmInput starts one child window.
type CommitConfirmInput struct {
	ActionID      string
	ExternalRef   string
	WindowSeconds int64
	// AlertRule is the incident signature threaded to the consult for the TG-461 service-fault
	// durable-substitute confirm (RecoveredSince scoping). The arm already carries it (ArmCommitConfirmInput).
	AlertRule string
}

// Hold-watch cadence (REQ-2902's amended unverifiable case): after HOLD+page the child stays on
// watch, re-consulting the live post-state; a POSITIVELY observed deviation fires the inverse,
// and an exhausted watch leaves the held row to the operator (the hold's ACTIVE period is
// bounded; the page said so).
const (
	commitConfirmHoldRechecks       = 6
	commitConfirmHoldRecheckMinutes = 10
)

// CommitConfirmWorkflow is the dead-man's switch: ONE durable Temporal timer for the confirm
// window, resolved by the first decisive event, with every resolution derived from the DURABLE
// terminus (confirm-from-the-terminus-only — the parent's confirm-input signal is a pure NUDGE
// carrying no trusted data, so a dead parent, a lost signal, and a sweep-adopted orphan all
// converge on the same consult). Started (and confirmed started) by the parent BEFORE the effect
// executes; abandoned at parent close so the window outlives the triage session.
//
// Dispositions (REQ-2901/2902/2903/2906):
//   - abort signal / consult "aborted"      → aborted (the effect provably never ran)
//   - consult "confirmed" (match+verified)  → confirmed — the ONLY confirming reading
//   - consult "deviation" (partial too)     → the inverse FIRES through the full chain
//   - consult "unverifiable"                → HOLD + page; bounded watch; a positively observed
//     deviation during the watch fires the inverse; an exhausted watch stays held (operator's)
//   - consult "pending" at ELAPSE           → aborted (nothing at the terminus by deadline)
func CommitConfirmWorkflow(ctx workflow.Context, in CommitConfirmInput) error {
	var a *Activities // nil receiver — activity-name resolution only
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		// Reads and idempotent Postgres writes: retry through transient blips, bounded — if the
		// store stays down the child FAILS and the row stays 'armed' (honest: the dead-man is
		// still hot, and the orphan sweep re-adopts it).
		RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 5},
	})
	if in.WindowSeconds <= 0 {
		return fmt.Errorf("commit-confirm window for %s/%s: non-positive window %d", in.ActionID, in.ExternalRef, in.WindowSeconds)
	}

	resolve := func(state, detail, inverseID string) error {
		return workflow.ExecuteActivity(ctx, a.ResolveCommitConfirmActivity, ResolveCommitConfirmInput{
			ActionID: in.ActionID, ExternalRef: in.ExternalRef,
			State: state, Detail: detail, InverseActionID: inverseID,
		}).Get(ctx, nil)
	}
	page := func(body string) {
		// Best-effort by design: the page is observability, and its failure must never mask the
		// state transition it reports — but it is never silent either (warn + the ledger already
		// carries the transition).
		if err := workflow.ExecuteActivity(ctx, a.NotifyActivity, NotifyInput{
			DecisionID: in.ActionID, Body: body, Approval: false,
		}).Get(ctx, nil); err != nil {
			workflow.GetLogger(ctx).Warn("commit-confirm: page delivery failed", "action_id", in.ActionID, "error", err.Error())
		}
	}

	sig := workflow.GetSignalChannel(ctx, CommitConfirmSignalName)
	timer := workflow.NewTimer(ctx, time.Duration(in.WindowSeconds)*time.Second)
	for {
		var rcv CommitConfirmResolve
		gotSignal := false
		elapsed := false
		sel := workflow.NewSelector(ctx)
		sel.AddFuture(timer, func(workflow.Future) { elapsed = true })
		sel.AddReceive(sig, func(c workflow.ReceiveChannel, _ bool) {
			c.Receive(ctx, &rcv)
			gotSignal = true
		})
		sel.Select(ctx)

		switch {
		case gotSignal && rcv.Kind == "abort":
			// The forward effect provably did not execute (the chain refused) — stand down.
			return resolve(db.CommitConfirmAborted, rcv.Detail, "")
		case gotSignal && rcv.Kind == "confirm-input":
			// The parent's post-verify NUDGE. Consult the terminus; a non-decisive reading keeps
			// the window armed until the next event (the timer is still running).
			done, err := commitConfirmConsultAndAct(ctx, a, in, false, resolve, page)
			if err != nil {
				return err
			}
			if done {
				return nil
			}
			continue
		case gotSignal:
			workflow.GetLogger(ctx).Warn("commit-confirm: unhandled resolve signal kind — window stays armed",
				"kind", rcv.Kind, "action_id", in.ActionID)
			continue
		case elapsed:
			done, err := commitConfirmConsultAndAct(ctx, a, in, true, resolve, page)
			if err != nil {
				return err
			}
			if done {
				return nil
			}
			// Unreachable by construction (elapse always routes), kept fail-safe.
			return nil
		}
	}
}

// commitConfirmConsultAndAct performs one terminus consult and acts on the reading. done=false
// only on a NON-decisive nudge-time reading (pending / not-yet); atElapse always decides.
func commitConfirmConsultAndAct(ctx workflow.Context, a *Activities, in CommitConfirmInput, atElapse bool,
	resolve func(state, detail, inverseID string) error, page func(string)) (bool, error) {
	var c ConsultCommitConfirmResult
	if err := workflow.ExecuteActivity(ctx, a.ConsultCommitConfirmActivity, ConsultCommitConfirmInput{
		ActionID: in.ActionID, ExternalRef: in.ExternalRef, AlertRule: in.AlertRule,
	}).Get(ctx, &c); err != nil {
		if !atElapse {
			// A failed nudge-time consult is not a resolution: stay armed, the timer still runs.
			workflow.GetLogger(ctx).Warn("commit-confirm: nudge-time consult failed — window stays armed",
				"action_id", in.ActionID, "error", err.Error())
			return false, nil
		}
		// A failed ELAPSE consult fails the child loudly: the row stays armed and the orphan
		// sweep re-adopts once the store recovers. Never guess a resolution.
		return true, err
	}

	switch c.Disposition {
	case ConsultPending:
		if !atElapse {
			return false, nil
		}
		// Nothing at the terminus by the deadline. This is NOT provable non-execution (review
		// finding #1: a chain-gap run can lose BOTH terminus writes — the interceptor now
		// back-writes the per-run row on that path, but a full store outage can still eat every
		// trace), and provable non-execution already resolves via the parent's abort signal. So
		// absence fails toward the HOLD: loud, paged, operator-owned — never a silent stand-down
		// over a possibly-live mutation.
		return true, commitConfirmHold(ctx, a, in,
			"window elapsed with NOTHING at the terminus — cannot distinguish never-executed from executed-but-untraced ("+c.Detail+"); the provably-not-executed case resolves via the abort signal, so this holds",
			resolve, page)
	case ConsultConfirmed:
		return true, resolve(db.CommitConfirmConfirmed, c.Detail, "")
	case ConsultDeviation:
		return true, commitConfirmFireInverse(ctx, a, in, c.Detail, resolve, page)
	case ConsultUnverifiable:
		if !atElapse {
			// Before the deadline an unverifiable reading is not yet a hold — the verify may still
			// land (the deferred-verify lane); the window simply stays armed.
			return false, nil
		}
		return true, commitConfirmHold(ctx, a, in, c.Detail, resolve, page)
	default:
		if !atElapse {
			return false, nil
		}
		// Fail toward the hold: an unknown reading at elapse must neither confirm nor fire.
		return true, commitConfirmHold(ctx, a, in, "unknown consult disposition "+c.Disposition, resolve, page)
	}
}

// commitConfirmHold is REQ-2902's amended unverifiable arm: HOLD armed (as held_unverifiable, the
// one non-terminal resolution), PAGE with the same worthiness as a fired revert, then keep an
// ACTIVE bounded watch — the inverse fires only on a subsequently OBSERVED deviation (positively
// observed; a failed read never fires anything). An exhausted watch leaves the held row to the
// operator, and says so on the page.
func commitConfirmHold(ctx workflow.Context, a *Activities, in CommitConfirmInput, why string,
	resolve func(state, detail, inverseID string) error, page func(string)) error {
	if err := resolve(db.CommitConfirmHeldUnverifiable, why, ""); err != nil {
		return err
	}
	page(fmt.Sprintf("commit-confirm HOLD on %s (incident %s): %s — the confirm window elapsed and the post-state could not be verified. "+
		"Holding armed; watching the live post-state for %d×%dm; the inverse fires only on an OBSERVED deviation. Resolving the hold is operator workflow.",
		in.ActionID, in.ExternalRef, why, commitConfirmHoldRechecks, commitConfirmHoldRecheckMinutes))
	for i := 0; i < commitConfirmHoldRechecks; i++ {
		if err := workflow.NewTimer(ctx, commitConfirmHoldRecheckMinutes*time.Minute).Get(ctx, nil); err != nil {
			return err
		}
		var c ConsultCommitConfirmResult
		if err := workflow.ExecuteActivity(ctx, a.ConsultCommitConfirmActivity, ConsultCommitConfirmInput{
			ActionID: in.ActionID, ExternalRef: in.ExternalRef, AlertRule: in.AlertRule,
		}).Get(ctx, &c); err != nil {
			workflow.GetLogger(ctx).Warn("commit-confirm: hold-watch consult failed — holding", "action_id", in.ActionID, "error", err.Error())
			continue
		}
		if c.ObservedAlerting != nil && *c.ObservedAlerting {
			return commitConfirmFireInverse(ctx, a, in,
				"observed deviation during the unverifiable hold (target alerting) — "+why, resolve, page)
		}
	}
	page(fmt.Sprintf("commit-confirm HOLD on %s (incident %s): watch exhausted (%d×%dm) with no observed deviation — "+
		"the row stays held_unverifiable and is operator-owned from here.",
		in.ActionID, in.ExternalRef, commitConfirmHoldRechecks, commitConfirmHoldRecheckMinutes))
	return nil
}

// commitConfirmFireInverse is REQ-2901's consequence and REQ-2903's discipline: the compensating
// action, sealed from durable state, executed through the FULL interceptor chain on the actuation
// queue with exactly one attempt (a mutation is never automatically retried). Every non-clean
// outcome — seal refusal, chain refusal, ambiguous execution, deviating inverse — resolves
// revert_failed, PAGES immediately, and TRIPS the mutation breaker (REQ-2906). A clean, verified
// inverse resolves reverted and still alerts (the same page-worthiness as a breaker trip).
func commitConfirmFireInverse(ctx workflow.Context, a *Activities, in CommitConfirmInput, why string,
	resolve func(state, detail, inverseID string) error, page func(string)) error {
	failed := func(detail, inverseID string) error {
		if err := resolve(db.CommitConfirmRevertFailed, detail, inverseID); err != nil {
			return err
		}
		page("commit-confirm REVERT FAILED on " + in.ActionID + " (incident " + in.ExternalRef + "): " + detail +
			" — an unrevertable armed revert is an incident (REQ-2903); the mutation breaker is being tripped.")
		if err := workflow.ExecuteActivity(ctx, a.TripMutationBreakerActivity,
			"commit-confirm revert failed on "+in.ActionID+": "+detail).Get(ctx, nil); err != nil {
			// The trip failing is itself trip-worthy news; it is already on the page and the log.
			workflow.GetLogger(ctx).Error("commit-confirm: breaker trip FAILED after a failed revert",
				"action_id", in.ActionID, "error", err.Error())
		}
		return nil
	}

	var seal SealCommitConfirmInverseResult
	if err := workflow.ExecuteActivity(ctx, a.SealCommitConfirmInverseActivity, SealCommitConfirmInverseInput{
		ActionID: in.ActionID, ExternalRef: in.ExternalRef,
	}).Get(ctx, &seal); err != nil {
		return failed("inverse seal errored: "+err.Error(), "")
	}
	if !seal.Sealed {
		return failed("inverse refused at seal: "+seal.Reason, "")
	}
	// The actuation queue, exactly one attempt — the same no-auto-retry discipline as the forward
	// execute (an at-least-once retry would run the estate change twice under one basis).
	execCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           tg.TaskQueueActuate,
		StartToCloseTimeout: 2 * time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	})
	var exec ExecuteResult
	if err := workflow.ExecuteActivity(execCtx, a.SealRollbackExecuteActivity, RollbackExecuteInput{
		In:              seal.RollbackIn,
		InverseActionID: seal.InverseActionID,
		AutoFired:       true,
		ApprovedBasis:   seal.ApprovedBasis,
	}).Get(ctx, &exec); err != nil {
		return failed("inverse execution ambiguous (activity error — it may or may not have run): "+err.Error(), seal.InverseActionID)
	}
	switch {
	case !exec.Executed:
		return failed("the chain refused the inverse: "+exec.Note, seal.InverseActionID)
	case exec.Verdict == string(safety.VerdictMatch):
		if err := resolve(db.CommitConfirmReverted, "inverse executed and verified match — "+why, seal.InverseActionID); err != nil {
			return err
		}
		page("commit-confirm REVERT FIRED on " + in.ActionID + " (incident " + in.ExternalRef + "): " + why +
			". The inverse " + seal.InverseActionID + " executed and verified clean. (REQ-2906: a fired revert alerts.)")
		return nil
	default:
		return failed("inverse executed but verified "+exec.Verdict+" — the estate did not return to the predicted state", seal.InverseActionID)
	}
}
