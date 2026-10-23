// Package policytrace runs the REAL worker policy Engine over a HYPOTHETICAL candidate action so an operator
// can ask "may TG act on host X with op-class Y?" and get back which rule matched, the composed verdict
// (auto/approve/deny), the effective band, and why — a FAITHFUL policy packet-tracer (TG-105 slice 1).
//
// WHY THE WORKER, NOT THE GROUNDER. The answer must come from the ONE engine the actuation interceptor
// consults (spec/015 T-015-13), never a grounder-side copy: a second engine is a second policy, and a
// packet-tracer that could DISAGREE with the live decision is worse than none. So the grounder starts THIS
// workflow over the existing Temporal channel and the worker's engine answers — exactly as opclassratify
// routes a ratify to the single ledger writer rather than forking a second grant path.
//
// WHAT IT DELIBERATELY IS NOT. The decider wired in cmd/worker is the BARE composed engine
// (policyEng.WithGraduation(policyGrad)) — NOT the audited engine, and WITHOUT the rate governor:
//
//   - No audit sink, so a hypothetical query appends NO policy_decision row. A trace must not pollute the
//     append-only decision history an operator reads as the record of REAL actuations.
//   - No rate governor, because the governor is STATEFUL runtime budget. Evaluating a trace must neither
//     consume rate budget nor let a trace's answer depend on how many real actions happened to run this
//     minute. The honest consequence is surfaced on every Result: RateGovernorSimulated is FALSE and the
//     Reason says so in words, because a composed `auto` here could still be rate-clamped to `approve` at
//     real actuation time.
//
// The engine is otherwise faithful: the SAME rule bundle, the SAME graduation ladder (a trace must reflect
// earned autonomy), the SAME Rego deny-overrides base, band composition, the constitutional never-auto floor,
// and the execution deny-floor. Read-only end to end: it evaluates and returns, actuating nothing.
package policytrace

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/safety"
)

// ErrNoDecider is returned if the activity is somehow reached with no engine wired. The worker only registers
// the lane when the decider is present (cmd/worker: policyTraceActs != nil), so this is defence in depth
// against a construction bug — a clean error, never a panic in a worker activity.
var ErrNoDecider = errors.New("policytrace: no policy decider wired")

// Request is the hypothetical candidate action to evaluate — a flattening of policy.EvalInput's matched and
// composed dimensions. The correlation/attribution keys (ActionID/ExternalRef/Principal) are DELIBERATELY
// absent: they compose no verdict, and their only consumer is the audit projection this lane never writes.
// Band and Mode ride as their canonical string spellings and are parsed at the activity (fail-closed on an
// empty/unknown value).
type Request struct {
	OpClass     string   // semantic op-class (the allow side of a match)
	Argv        string   // raw command string (the deny side of a match)
	Host        string   // canonical host name
	Resource    string   // named resource id
	Groups      []string // estate group memberships
	DeviceClass string   // device-class (e.g. "cisco-asa")
	Territory   string   // estate territory
	Reversible  bool     // whether the action is reversible

	Confidence float64 // bound model confidence (clamped in the confidence step)
	Band       string  // "AUTO" | "AUTO_NOTICE" | "POLL_PAUSE"; empty/unknown → POLL_PAUSE (fail closed)
	Mode       string  // "Shadow" | "HITL" | "Semi-auto" | "Full-auto"; empty/unknown → Shadow (fail closed)
}

// MatchedRule is one rule that matched the traced action, projected to its stable id + declared verdict — the
// deny-overrides provenance that makes the composed verdict explainable without changing it.
type MatchedRule struct {
	RuleID  string `json:"rule_id"`
	Verdict string `json:"verdict"`
}

// Result is the faithful trace of one policy evaluation — the packet-tracer answer.
type Result struct {
	Verdict        string        // composed auto/approve/deny
	MatchedRuleID  string        // the rule that determined the base verdict ("" = fail-closed default matched nothing)
	ComposedBand   string        // the action's composed risk band (POLL_PAUSE/AUTO_NOTICE/AUTO)
	ApproveBy      []string      // the matched rule's approve_by principals for a resolved `approve` (else empty)
	Mode           string        // the mode carried into the decision
	Reason         string        // human-readable packet-tracer explanation, with the rate-governor note appended
	NeverAutoFloor bool          // whether the constitutional never-auto floor applied to the action
	BundleVersion  string        // content-derived identity of the rule bundle this was evaluated over
	MatchedRules   []MatchedRule // the FULL matched-rule set (deny-overrides provenance)

	// RateGovernorSimulated is ALWAYS false: the trace decider carries NO rate governor (stateful runtime
	// budget), so this evaluation neither consumes nor reflects live rate state. The Reason repeats it in
	// words. A future faithful rate simulation would flip this true; until then honesty is a false + a note.
	RateGovernorSimulated bool
}

// rateGovNote is appended to every Result.Reason so the honest limit of the trace travels WITH the answer,
// not only in a field a caller might ignore.
const rateGovNote = " | NOTE: the rate-governor runtime state is NOT simulated — this trace evaluates policy " +
	"without the stateful rate governor, so a composed `auto` here may still be rate-clamped to `approve` at " +
	"real actuation time (RateGovernorSimulated=false)."

// PolicyDecider is the narrow evaluation seam — policy.Engine.Decide. Interface-typed so the activity is
// oracle-testable with a stub and never itself constructs a Rego engine. Same shape as actuate.PolicyDecider,
// kept LOCAL so this read-only lane imports no actuation package.
type PolicyDecider interface {
	Decide(ctx context.Context, in policy.EvalInput) (policy.PolicyDecision, error)
}

// Activities carries the worker-side decider for Temporal registration. Decider is EXPORTED because the
// worker in a different package constructs it (policytrace.Activities{Decider: ...}) — the same convention
// opclassratify.Activities{D: ...} follows.
type Activities struct{ Decider PolicyDecider }

// PolicyTraceActivity maps the hypothetical Request to a policy.EvalInput, runs the REAL engine's Decide, and
// projects the resulting PolicyDecision back to a Result. It writes nothing and actuates nothing.
func (a *Activities) PolicyTraceActivity(ctx context.Context, req Request) (Result, error) {
	if a == nil || a.Decider == nil {
		return Result{}, ErrNoDecider
	}
	decision, err := a.Decider.Decide(ctx, evalInputFrom(req))
	if err != nil {
		// Decide's fail-closed contract returns a DENY decision alongside an evaluator error; a Temporal
		// activity cannot return both a value and an error, so we surface the error (the grounder maps it to a
		// 503). A refused verdict (auto/approve/deny) is a nil-error path and maps through below.
		return Result{}, err
	}
	return resultFrom(decision), nil
}

// evalInputFrom flattens the request onto the engine's typed input. Band/Mode parse fail-closed.
func evalInputFrom(req Request) policy.EvalInput {
	return policy.EvalInput{
		OpClass:     req.OpClass,
		Argv:        req.Argv,
		Host:        req.Host,
		Resource:    req.Resource,
		Groups:      req.Groups,
		DeviceClass: req.DeviceClass,
		Territory:   req.Territory,
		Reversible:  req.Reversible,
		Confidence:  req.Confidence,
		Band:        parseBand(req.Band),
		Mode:        parseMode(req.Mode),
	}
}

// resultFrom projects a composed PolicyDecision to the wire Result, appending the rate-governor honesty note.
func resultFrom(d policy.PolicyDecision) Result {
	out := Result{
		Verdict:               string(d.Verdict()),
		MatchedRuleID:         d.MatchedRuleID(),
		ComposedBand:          d.ComposedBand().String(),
		ApproveBy:             d.ApproveBy(),
		Mode:                  d.Mode().String(),
		Reason:                d.Reason() + rateGovNote,
		NeverAutoFloor:        d.Audit().NeverAutoFloor,
		BundleVersion:         d.BundleVersion(),
		RateGovernorSimulated: false,
	}
	for _, r := range d.MatchedRules() {
		out.MatchedRules = append(out.MatchedRules, MatchedRule{RuleID: r.ID, Verdict: string(r.Verdict)})
	}
	return out
}

// parseBand maps the canonical band spelling to safety.Band, fail-closed to the most-restrictive POLL_PAUSE
// on an empty or unrecognised value (mirroring safety.Band's own String()/zero-value contract).
func parseBand(s string) safety.Band {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "AUTO":
		return safety.BandAuto
	case "AUTO_NOTICE":
		return safety.BandAutoNotice
	default:
		return safety.BandPollPause // "", "POLL_PAUSE", or any unknown → fail closed
	}
}

// parseMode maps the canonical mode spelling to policy.Mode, fail-closed to Shadow (read-only) on an empty or
// unrecognised value — the same posture the read surface takes for an absent/corrupt persisted mode.
func parseMode(s string) policy.Mode {
	if m, err := policy.ParseMode(strings.TrimSpace(s)); err == nil {
		return m
	}
	return policy.ModeShadow
}

// PolicyTraceWorkflow is the one-activity, READ-ONLY trace workflow. Distinctly named at the symbol
// (PolicyTraceWorkflow/Activity) for the same bare-function-name reason opclassratify documents: Temporal
// registers by bare function name, so a generic name would collide with another package's and panic the
// worker at boot (the 2026-07-17 boot-loop). It joins temporal/skilltrial's names guard.
//
// Short timeout, no retries: a trace is a synchronous question an operator is waiting on, and a policy
// refusal (deny) is a DECISION, not a transient to retry.
func PolicyTraceWorkflow(ctx workflow.Context, req Request) (Result, error) {
	opts := workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	}
	var res Result
	// Temporal dispatches by REGISTERED FUNCTION NAME: the zero-Decider receiver here only NAMES the activity;
	// the worker's registered instance (with the real engine) executes it.
	err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, opts), new(Activities).PolicyTraceActivity, req).Get(ctx, &res)
	return res, err
}
