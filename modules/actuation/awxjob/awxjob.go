// Package awxjob is the AWX-job effect lane's actuator — the FIRST non-SSH mutating channel of the Actuation
// Regime Engine (spec/017 T-017-3, REQ-1704..1708, TG-110). It implements adapters/actuation.Actuator (the
// SAME interface the spec/013 SSH effect leaf implements), so it drops into the interceptor exactly like the
// native-ssh leaf: it is reachable ONLY through the awx-job Lane's UNEXPORTED effectLeaf, handed to
// actuate.Interceptor.Do, which runs the full wired chain (admission → never-auto floor → policy authorize →
// credential authenticate → mode chokepoint → execute → verify). This lane authorizes nothing, authenticates
// nothing, and lifts no floor of its own — "a lane is a channel, not a permission."
//
// WHY a job template is a NARROWER channel than raw SSH (the deliberate posture, threat-model.md): the
// argv-equivalent here is a FIXED job-template id plus typed, schema-validated `extra_vars` (extravars.go) —
// there is no free-form command string anywhere in the launch body, so the model cannot express an arbitrary
// shell command through this lane. It is safe ONLY because the platform beneath it stays in force:
//   - ALLOWLIST + POLICY (REQ-1704). The actuator launches ONLY a template on the operator-declared
//     TemplateAllowlist, and only when the op-class bound to that template matches the op-class the interceptor
//     already policy-authorized (carried in the launch spec, cross-checked here). A non-allowlisted template or
//     an op-class/template mismatch refuses — the model cannot name an arbitrary or attacker-chosen template.
//   - TYPED extra_vars (REQ-1705). Every launch variable is validated against the template's declared schema;
//     an undeclared key is rejected. A job template is not a shell escape.
//   - MUTATION STAYS OFF (REQ-1707). The launch is a MUTATING effect. It is reached ONLY under the mode
//     chokepoint (the interceptor refuses at Shadow — the default/only reachable mode — long before Exec), and
//     as defense in depth the actuator re-checks the SAME mode chokepoint at its own effect leaf: ReadOnly() is
//     true and Exec refuses BEFORE any network launch unless the mode currently permits actuation. So the
//     actuator is INERT at Shadow through two independent gates, and there is no launch path that skips the
//     interceptor (the composition invariant in core/regime keeps the leaf unexported).
//   - CREDENTIAL after policy, before launch (REQ-1706). The AWX launch token is resolved through a
//     core/config.SecretRef at launch time — AFTER the interceptor's policy verdict + mode chokepoint permit,
//     and BEFORE the POST. A resolved token is necessary, never sufficient. The launch token is DISTINCT from
//     the read-only sensor token the knowledge lane uses (REQ-1708); neither is ever a literal (INV-13).
//
// ASYNC (REQ-1709). An AWX launch returns immediately with a job HANDLE (job id) — the effect is not
// synchronously observable. The actuator hands the handle out (in the actuation.Result) so the GLOBAL
// deferred-verify channel (T-017-4) can poll the job to a terminal AWX status and compute the mechanical
// verdict. The actuator itself declares NO success at launch and implements NO double-launch guard —
// idempotency (action_id binding) is the deferred-verify channel's concern (T-017-4), deliberately NOT
// duplicated here so the two do not conflict.
//
// DISTROLESS-SAFE (INV-02). net/http only (client.go); NO subprocess, no awx-cli, no ansible binary.
//
// Provenance: [R] paradigm-rule 8 · [O] INV-02/INV-06/INV-09/INV-13/INV-21, spec/017 (REQ-1704..1708),
// TG-110.
package awxjob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	actuation "github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/safety"
)

// Capability is the declared capability slug this adapter provides; it matches the awx-job regime slug.
const Capability = "awx-job"

// LaunchVerb is the fixed, structural argv[0] the interceptor carries for an AWX-job launch. It is NOT a
// shell command — it names the effect, and the actuator refuses any argv that is not EXACTLY this single
// token. The plan-as-data (template id + typed extra_vars + limit + op-class) travels as the JSON LaunchSpec
// in the request's stdin, not as command arguments, so there is no command string to interpolate (INV-02).
const LaunchVerb = "awx-job-template-launch"

// TemplatePolicy binds one allowlisted AWX job template to the op-class the policy engine authorizes for it
// and the closed schema its launch-time `extra_vars` must conform to (REQ-1704/1705). It is the operator's
// declaration that a template is sanctioned, reviewed, and idempotent.
type TemplatePolicy struct {
	// OpClass is the op-class bound to this template — the class the policy engine (spec/015 Decide) authorizes
	// and the graduation ladder earns trust for. The launch's declared op-class must equal it (cross-checked in
	// Exec), so a template authorized for a benign class can never be launched under a different, denied class.
	OpClass string
	// ExtraVarsSchema is the CLOSED set of allowed launch-time variables + their declared primitive types.
	ExtraVarsSchema ExtraVarsSchema
}

// TemplateAllowlist is the operator-declared set of sanctioned AWX job templates, keyed by AWX
// job_template_id (REQ-1704). A template absent from it is NOT launchable through this lane — the actuator
// refuses. It is config-not-code: the worker builds it from operator configuration (a later wave), never a
// hardcoded set.
type TemplateAllowlist map[int]TemplatePolicy

var (
	// ErrLaunchGateClosed is returned when the actuator has no mode chokepoint wired, or the wired chokepoint
	// does not currently permit actuation (mode Shadow / un-bound / red preflight) — the effect-leaf defense in
	// depth that keeps mutation OFF even if Exec were reached directly (REQ-1707).
	ErrLaunchGateClosed = errors.New("awxjob: mode chokepoint does not permit actuation — refusing launch (mutation off)")
	// ErrNotLaunchArgv is returned when the argv is not EXACTLY the fixed LaunchVerb — a structural guard so no
	// free-form command shape can reach the launch path.
	ErrNotLaunchArgv = errors.New("awxjob: argv is not the fixed awx-job launch verb")
	// ErrTemplateNotAllowlisted is returned for a job_template id absent from the operator allowlist (REQ-1704).
	ErrTemplateNotAllowlisted = errors.New("awxjob: job_template is not on the operator allowlist — refusing")
	// ErrOpClassMismatch is returned when the launch's declared op-class does not match the op-class the
	// allowlist bound to the template — so a template authorized for one class cannot be launched under another
	// (the confused-deputy guard, REQ-1704).
	ErrOpClassMismatch = errors.New("awxjob: launch op-class does not match the template's allowlisted op-class — refusing")
	// ErrLaunchFieldIgnored is returned when AWX reports a prompt-on-launch field the client SENT under
	// ignored_fields — a dropped extra_var or host-limit is a refusal, never a silent no-op (REQ-1705).
	ErrLaunchFieldIgnored = errors.New("awxjob: AWX ignored a launch field we sent — refusing (effect would not match the prediction)")
	// ErrBadLaunchSpec is returned when the request stdin does not decode to a well-formed LaunchSpec.
	ErrBadLaunchSpec = errors.New("awxjob: request stdin is not a well-formed launch spec")
)

// LaunchSpec is the typed, bounded plan-as-data for ONE AWX job-template launch — the argv-equivalent of the
// SSH leaf's fixed command vector. It is NOT a command string: the effect is a fixed template id plus typed,
// schema-validated `extra_vars`. It travels as JSON in the interceptor request's stdin (see EncodeLaunch); the
// actuator decodes and validates it in Exec.
type LaunchSpec struct {
	// TemplateID is the AWX job_template id to launch. It must be allowlisted (REQ-1704).
	TemplateID int `json:"template_id"`
	// OpClass is the op-class the interceptor's policy engine authorized for this action (the caller sets it
	// from the sealed manifest). The actuator cross-checks it against the template's allowlisted op-class so the
	// policy-authorized class and the launched template can never be mismatched (REQ-1704).
	OpClass string `json:"op_class"`
	// ExtraVars are the typed launch variables, validated against the template's declared schema (REQ-1705).
	ExtraVars map[string]any `json:"extra_vars,omitempty"`
	// Limit is the AWX host limit (a single host / pattern) scoping the run. Optional.
	Limit string `json:"limit,omitempty"`
}

// EncodeLaunch turns a typed LaunchSpec into the (argv, stdin) the interceptor request carries. argv is the
// fixed LaunchVerb (no command string); stdin is the JSON-encoded spec. The governed caller (the worker /
// the deferred-verify driver, a later wave) builds an actuate.Request from these; the actuator decodes them
// in Exec. It fails closed on a non-positive template id.
func EncodeLaunch(spec LaunchSpec) (argv []string, stdin []byte, err error) {
	if spec.TemplateID <= 0 {
		return nil, nil, fmt.Errorf("%w: template_id must be positive", ErrBadLaunchSpec)
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return nil, nil, fmt.Errorf("awxjob: encode launch spec: %w", err)
	}
	return []string{LaunchVerb}, b, nil
}

// DecodeLaunched parses the actuation.Result stdout an executed launch returns back into the async job HANDLE.
// The GLOBAL deferred-verify channel (T-017-4) uses it to recover the job id it must poll to terminal.
func DecodeLaunched(stdout []byte) (Launched, error) {
	var l Launched
	if err := json.Unmarshal(stdout, &l); err != nil {
		return Launched{}, fmt.Errorf("awxjob: decode launched handle: %w", err)
	}
	return l, nil
}

// Actuator is the AWX-job effect leaf (an adapters/actuation.Actuator). It holds the launch client, the
// operator template-allowlist, and the process mode chokepoint it consults as defense in depth. Construct it
// with New and inject it into the awx-job Lane via core/regime.WithAWXActuator (the worker does this, a later
// wave). A nil/empty mode chokepoint or an empty allowlist makes it read-only — Exec fails closed.
type Actuator struct {
	client    *Client
	allowlist TemplateAllowlist
	// gate is the PROCESS mode chokepoint (core/safety). The actuator reads it to decide ReadOnly() and to
	// re-guard the launch (defense in depth). A nil gate ⇒ the actuator is read-only and Exec refuses — mutation
	// stays OFF. It is the SAME chokepoint the interceptor wires; this leaf never mutates it.
	gate *safety.Chokepoint
}

// Config constructs an Actuator. Client and Allowlist are required for a launch-capable actuator. ModeGate is
// the process mode chokepoint the actuator re-checks at its effect leaf; a nil gate yields a read-only
// actuator whose Exec fails closed (the fail-safe default — mutation is never on by omission).
type Config struct {
	Client    *Client
	Allowlist TemplateAllowlist
	ModeGate  *safety.Chokepoint
}

// New builds an AWX-job actuator. It fails closed if the launch client is missing. An empty allowlist or a
// nil mode gate is permitted at construction but leaves the actuator read-only (it can only refuse) — the
// safe posture until the operator declares an allowlist AND the mode is escalated at the owner-present flip.
func New(cfg Config) (*Actuator, error) {
	if cfg.Client == nil {
		return nil, fmt.Errorf("awxjob: actuator requires a launch client")
	}
	return &Actuator{client: cfg.Client, allowlist: cfg.Allowlist, gate: cfg.ModeGate}, nil
}

// Capability implements adapters/actuation.Actuator.
func (a *Actuator) Capability() string { return Capability }

// ReadOnly reports whether this actuator can only observe. It returns true UNLESS the mode chokepoint
// currently permits actuation AND the actuator carries a genuine launch path (a non-empty template allowlist).
// Mutation ships OFF — the mode is Shadow through all of Phase 0/1 — so this returns true in every production
// and test path except one that constructs a test-only actuating chokepoint. That is the structural proof
// the actuator cannot launch until the mode is escalated: the launch CODE PATH exists and is exercised, but
// it is unreachable while the mode is Shadow (mirrors the SSH module's gate-driven ReadOnly).
func (a *Actuator) ReadOnly() bool {
	return !(a.gate != nil && a.gate.MayActuate() && len(a.allowlist) > 0)
}

// Exec is the Actuator chokepoint the interceptor calls AFTER it has admitted, policy-authorized, and
// mode-permitted the action. It launches the allowlisted, typed AWX job template and returns the async job
// handle. It re-enforces the mode chokepoint, the allowlist, the op-class binding, and the typed schema HERE,
// at the effect leaf, as defense in depth: reached directly it still refuses at Shadow, refuses a
// non-allowlisted template or an op-class mismatch, and rejects any undeclared extra_var — before any network
// launch. It NEVER builds a command by string concatenation and NEVER spawns a shell.
func (a *Actuator) Exec(ctx context.Context, argv []string, stdin []byte) (actuation.Result, error) {
	// 1. Mode chokepoint (defense in depth, REQ-1707): a launch NEVER fires while the mode is out. A nil gate
	//    is a read-only actuator with no launch path.
	if a.gate == nil {
		return actuation.Result{}, ErrLaunchGateClosed
	}
	if err := a.gate.GuardMutation(); err != nil {
		return actuation.Result{}, fmt.Errorf("%w: %v", ErrLaunchGateClosed, err)
	}
	// 2. Structural argv guard: the argv MUST be exactly the fixed launch verb — no free-form command shape.
	if len(argv) != 1 || argv[0] != LaunchVerb {
		return actuation.Result{}, ErrNotLaunchArgv
	}
	// 3. Decode the typed plan-as-data from stdin.
	if len(stdin) == 0 {
		return actuation.Result{}, fmt.Errorf("%w: empty stdin", ErrBadLaunchSpec)
	}
	var spec LaunchSpec
	if err := json.Unmarshal(stdin, &spec); err != nil {
		return actuation.Result{}, fmt.Errorf("%w: %v", ErrBadLaunchSpec, err)
	}
	if spec.TemplateID <= 0 {
		return actuation.Result{}, fmt.Errorf("%w: non-positive template_id", ErrBadLaunchSpec)
	}
	// 4. Allowlist (REQ-1704): refuse a template the operator did not sanction.
	pol, ok := a.allowlist[spec.TemplateID]
	if !ok {
		return actuation.Result{}, fmt.Errorf("%w: template %d", ErrTemplateNotAllowlisted, spec.TemplateID)
	}
	// 4b. Op-class binding (REQ-1704, confused-deputy guard): the op-class the interceptor policy-authorized
	//     (carried in the spec) MUST equal the op-class the allowlist bound to this template, so a template
	//     sanctioned for a benign class can never be launched under a different, denied class.
	if strings.TrimSpace(spec.OpClass) != strings.TrimSpace(pol.OpClass) {
		return actuation.Result{}, fmt.Errorf("%w: launch op-class %q, template %d bound to %q",
			ErrOpClassMismatch, spec.OpClass, spec.TemplateID, pol.OpClass)
	}
	// 5. Typed extra_vars (REQ-1705): reject any key absent from the template's declared schema, or a type
	//    mismatch. No free-form command string is ever passed.
	if err := pol.ExtraVarsSchema.Validate(spec.ExtraVars); err != nil {
		return actuation.Result{}, err
	}
	// 6. Launch (REQ-1706): the launch token is resolved inside the client at this point — AFTER the
	//    interceptor's policy verdict + mode chokepoint, and BEFORE the POST. Returns the async job handle.
	launched, err := a.client.Launch(ctx, spec.TemplateID, spec.ExtraVars, spec.Limit)
	if err != nil {
		return actuation.Result{}, err
	}
	// 7. Hand the async job handle out (REQ-1709): the deferred-verify channel (T-017-4) polls it to terminal.
	//    The actuator declares NO success here — an AWX launch is a prediction, verified later.
	out, err := json.Marshal(launched)
	if err != nil {
		return actuation.Result{}, fmt.Errorf("awxjob: encode launched handle: %w", err)
	}
	return actuation.Result{Stdout: out}, nil
}

// compile-time proof the actuator satisfies the stable actuation interface (so it drops into the interceptor
// exactly like the SSH effect leaf).
var _ actuation.Actuator = (*Actuator)(nil)
