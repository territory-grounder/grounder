// Package gitopsmr is the GitOps merge-request effect lane's actuator (TG-122, spec/017 — the `gitops-mr`
// regime). It implements adapters/actuation.Actuator (the SAME interface the native-ssh and awx-job effect
// leaves implement), so it drops into the interceptor exactly like them: it is reachable ONLY through the
// gitops-mr Lane's UNEXPORTED effectLeaf, handed to actuate.Interceptor.Do, which runs the full wired chain
// (admission → never-auto floor → policy authorize → credential authenticate → mode chokepoint → execute →
// verify). This lane authorizes nothing, authenticates nothing, and lifts no floor — "a lane is a channel,
// not a permission."
//
// WHY an MR is the correct, SAFEST channel for a GitOps-managed target: a direct kubectl/helm/tofu apply
// DRIFTS the desired state and is auto-reverted by the reconciler (split-brain). The MR routes a declarative
// change through the platform's own reconciler (Atlantis plan/apply, Argo sync). For this lane, "EXECUTE" =
// open (never merge, never `atlantis apply`, never touch the cluster API) — exactly two REST calls, then STOP.
//
// It is safe ONLY because the platform beneath it stays in force, mirroring awx-job's posture (there is no
// free-form file content anywhere — the model chooses which allowlisted op-class, which target, which
// in-schema new-value, never repo bytes):
//   - ALLOWLIST + POLICY. The actuator writes ONLY to a repo on the operator RepoAllowlist, and only when the
//     op-class bound to that repo matches the op-class the interceptor already policy-authorized (carried in
//     the ProposeSpec, cross-checked here — the confused-deputy guard). A non-allowlisted repo or an
//     op-class/repo mismatch refuses.
//   - TYPED, REGISTRY-GATED edits. The MR body is DERIVED by a structured Renderer from typed FieldEdits that
//     each name ONE closed FieldRule on the repo policy — never regex/sed, never free-form file bytes. An edit
//     that resolves to ≠ exactly one field is refused (diff-minimality is the review contract).
//   - SECRET VALUES NEVER ENTER GIT. A pre-write guard scans the rendered patch for decoded secret values and
//     hard-fails; only reference/plumbing edits are MR-safe.
//   - MUTATION STAYS OFF. Opening an MR is a MUTATING external effect (branch+commit+MR+pipeline+reviewers).
//     It is reached ONLY under the mode chokepoint (the interceptor refuses at Shadow — the default/only
//     reachable mode — before Exec), and as defense in depth the actuator re-checks the SAME chokepoint at its
//     leaf: ReadOnly() is true and Exec refuses BEFORE any REST call unless the mode currently permits.
//   - CREDENTIAL after policy, before write. The api-scoped GitLab PAT is resolved through a config.SecretRef
//     inside the Opener at write time — AFTER the interceptor's policy verdict + mode chokepoint, and BEFORE
//     the write. A resolved token is necessary, never sufficient; never a literal (INV-13).
//
// ASYNC (spec/017 REQ-1709). Opening an MR returns a HANDLE (`<repoID>!<iid>`), not a completed outcome — the
// real effect (merge → Atlantis apply / Argo sync → reconcile) lands minutes-to-days later, human-gated. The
// actuator hands the handle out (in the actuation.Result) for the GLOBAL deferred-verify channel to poll to a
// terminal, reconcile-observed verdict. It declares NO success at open time and implements NO double-open
// guard — idempotency (action_id binding) is the deferred-verify channel's concern (REQ-1712), deliberately
// NOT duplicated here. On the synchronous verify path the lane is structurally REFUSED
// (returnsHandleNotOutcome(gitops-mr), core/regime) until that producer is wired — injecting this actuator
// does not arm it.
//
// DISTROLESS-SAFE (INV-02): net/http only (via the injected Opener); NO subprocess, no git/glab binary.
//
// Provenance: [R] paradigm-rule 8 · [O] INV-02/INV-09/INV-13/INV-21, spec/017, TG-122 (design
// docs/design/TG-122-gitops-mr-lane.md, owner-approved TG-488 B25).
package gitopsmr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	actuation "github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/safety"
)

// Capability is the declared capability slug this adapter provides; it matches the gitops-mr regime slug
// (core/regime.RegimeGitOpsMR).
const Capability = "gitops-mr"

// ProposeVerb is the fixed, structural argv[0] the interceptor carries for a gitops-mr open. It is NOT a shell
// command — it names the effect, and the actuator refuses any argv that is not EXACTLY this single token. The
// plan-as-data (repo id + op-class + typed field edits + rationale) travels as the JSON ProposeSpec in the
// request stdin, so there is no command string to interpolate (INV-02).
const ProposeVerb = "gitops-mr-open"

// FieldRule is ONE closed, operator-declared locate rule the Renderer may target: a single addressable field
// in a single file. The model selects a rule by its RuleID and supplies only the new scalar — it can never
// name a free-form file/path. File is repo-relative; Selector is the structured, renderer-interpreted address
// of exactly one field within it (e.g. an hcl block+attribute path, or a yaml field path). The Renderer
// refuses a selector that resolves to ≠ exactly one field.
type FieldRule struct {
	RuleID   string `json:"rule_id"`  // stable id the ProposeSpec's FieldEdit names
	File     string `json:"file"`     // repo-relative path (e.g. k8s/_core/foo/main.tf, argocd-apps/x.yaml)
	Selector string `json:"selector"` // structured single-field address the Renderer interprets (never regex)
}

// RepoPolicy binds one allowlisted target repo to the op-class the policy engine authorizes for it and the
// CLOSED set of field rules the Renderer may target (the awxjob.TemplatePolicy analogue). BaseURL/TokenRef are
// per-repo config — the NL and GR infra repos live on SEPARATE per-site GitLab instances, so neither is ever
// hardcoded (design §3c).
type RepoPolicy struct {
	BaseURL      string           // per-site GitLab base — never a literal; NL vs GR
	ProjectPath  string           // operator config, e.g. infrastructure/<site>/production — never hardcoded
	TargetBranch string           // e.g. main
	BranchPrefix string           // reserved TG prefix, e.g. "tg/" (so the sensor never fights renovate/* or Atlantis)
	TokenRef     config.SecretRef // api-scoped PAT, sealed; resolved by the Opener at write time; never a literal
	OpClass      string           // the op-class this repo+change-class is authorized for (confused-deputy cross-check)
	FieldRules   []FieldRule      // the CLOSED set of locate rules the Renderer may target
}

// RepoAllowlist is the operator-declared set of sanctioned target repos, keyed by repo id (the
// awxjob.TemplateAllowlist analogue). A repo absent from it is NOT writable through this lane — the actuator
// refuses. Config-not-code: the worker builds it from operator configuration (Slice 4), never a hardcoded set.
type RepoAllowlist map[string]RepoPolicy

// FieldEdit is one typed field change: it names ONE FieldRule on the repo policy and supplies the new scalar.
// There is NO free-form file content — the Renderer produces the bytes from the rule + the value.
type FieldEdit struct {
	FieldRuleID string `json:"field_rule_id"`
	NewValue    string `json:"new_value"`
}

// ProposeSpec is the typed, bounded plan-as-data for ONE gitops-mr open — the argv-equivalent of the SSH
// leaf's fixed command vector and the awx-job LaunchSpec. It travels as JSON in the interceptor request stdin
// (see EncodePropose); the actuator decodes and validates it in Exec.
type ProposeSpec struct {
	RepoID    string      `json:"repo_id"`   // MUST be on the operator RepoAllowlist
	OpClass   string      `json:"op_class"`  // cross-checked vs the repo policy's OpClass (confused-deputy)
	Edits     []FieldEdit `json:"edits"`     // typed field edits — NO free-form file content
	Rationale string      `json:"rationale"` // templated MR prose; the secret guard rejects a decoded secret here too
}

// OpenedMR is the async handle an opened MR returns — the deferred-verify channel polls it to a terminal,
// reconcile-observed verdict. Handle is `<repoID>!<iid>` (the same shape awx-job returns its job id).
type OpenedMR struct {
	Handle string `json:"handle"` // "<repoID>!<iid>"
	RepoID string `json:"repo_id"`
	IID    int    `json:"iid"` // the MR internal id
	Branch string `json:"branch"`
	URL    string `json:"url,omitempty"`
}

// Renderer derives the MR file changes from the typed edits against a repo policy's closed FieldRules. It is
// structured (hclwrite for *.tf/helm_release.set, kyaml for argocd-apps/*.yaml — comment/order preserving),
// NEVER regex/sed, and MUST refuse an edit that resolves to ≠ exactly one field or names an unknown FieldRule.
// The concrete renderers are a follow-on within Slice 1; the actuator depends only on this seam so its safety
// structure (allowlist, op-class binding, secret guard, mode gate) is complete and testable now.
type Renderer interface {
	// Render returns, for each edited repo-relative file, its FULL new content (the atomic branch+commit writes
	// these). It fails closed on an unknown FieldRuleID, a selector resolving to ≠1 field, or a rule whose File
	// is not repo-relative.
	Render(pol RepoPolicy, edits []FieldEdit) (files map[string][]byte, err error)
}

// Opener performs the effect's EXACTLY TWO REST calls and STOPS: an atomic branch+commit (GitLab Commits API
// `actions[]` on a new tg/ branch) then create-merge-request — never the Files API, never merge, never
// `atlantis apply`. It resolves the repo policy's TokenRef at write time (after policy + mode). The concrete
// GitLab impl is a follow-on within Slice 1; the actuator depends only on this seam.
type Opener interface {
	OpenMR(ctx context.Context, pol RepoPolicy, branch, title, body string, files map[string][]byte) (OpenedMR, error)
}

var (
	// ErrOpenGateClosed is returned when the actuator has no mode chokepoint wired, or the wired chokepoint does
	// not currently permit actuation (mode Shadow / un-bound / red preflight) — the effect-leaf defense in depth
	// that keeps the write refused even if Exec were reached directly.
	ErrOpenGateClosed = errors.New("gitopsmr: mode chokepoint does not permit actuation — refusing to open MR (mutation off)")
	// ErrNotProposeArgv is returned when the argv is not EXACTLY the fixed ProposeVerb — a structural guard so no
	// free-form command shape can reach the write path.
	ErrNotProposeArgv = errors.New("gitopsmr: argv is not the fixed gitops-mr open verb")
	// ErrRepoNotAllowlisted is returned for a repo id absent from the operator allowlist.
	ErrRepoNotAllowlisted = errors.New("gitopsmr: repo is not on the operator allowlist — refusing")
	// ErrOpClassMismatch is returned when the ProposeSpec's op-class does not match the op-class the allowlist
	// bound to the repo (the confused-deputy guard) — a repo authorized for one class can't be written under another.
	ErrOpClassMismatch = errors.New("gitopsmr: propose op-class does not match the repo's allowlisted op-class — refusing")
	// ErrNoEdits is returned when a ProposeSpec carries zero field edits — an empty MR is never opened.
	ErrNoEdits = errors.New("gitopsmr: propose carries no field edits — refusing (an empty MR is never opened)")
	// ErrSecretInPatch is returned when the rendered patch (or the rationale) contains a decoded secret value —
	// only references live in Git; a decoded value would leak a literal or be a no-op (design §4).
	ErrSecretInPatch = errors.New("gitopsmr: rendered patch contains a decoded secret value — refusing (only SecretRefs belong in Git)")
	// ErrBadProposeSpec is returned when the request stdin does not decode to a well-formed ProposeSpec.
	ErrBadProposeSpec = errors.New("gitopsmr: request stdin is not a well-formed propose spec")
)

// EncodePropose turns a typed ProposeSpec into the (argv, stdin) the interceptor request carries: argv is the
// fixed ProposeVerb (no command string), stdin is the JSON-encoded spec. Fails closed on an empty repo id or
// zero edits.
func EncodePropose(spec ProposeSpec) (argv []string, stdin []byte, err error) {
	if strings.TrimSpace(spec.RepoID) == "" {
		return nil, nil, fmt.Errorf("%w: empty repo_id", ErrBadProposeSpec)
	}
	if len(spec.Edits) == 0 {
		return nil, nil, ErrNoEdits
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return nil, nil, fmt.Errorf("gitopsmr: encode propose spec: %w", err)
	}
	return []string{ProposeVerb}, b, nil
}

// DecodeOpened parses the actuation.Result stdout an executed open returns back into the async MR handle. The
// GLOBAL deferred-verify channel uses it to recover the MR it must poll to reconcile-convergence.
func DecodeOpened(stdout []byte) (OpenedMR, error) {
	var o OpenedMR
	if err := json.Unmarshal(stdout, &o); err != nil {
		return OpenedMR{}, fmt.Errorf("gitopsmr: decode opened handle: %w", err)
	}
	return o, nil
}

// Actuator is the gitops-mr effect leaf (an adapters/actuation.Actuator). It holds the injected Renderer +
// Opener, the operator RepoAllowlist, and the process mode chokepoint it consults as defense in depth.
// Construct it with New and inject it into the gitops-mr Lane via core/regime.WithGitOpsMRActuator (the worker
// does this, Slice 4). A nil gate or empty allowlist makes it read-only — Exec fails closed.
type Actuator struct {
	opener    Opener
	renderer  Renderer
	allowlist RepoAllowlist
	// gate is the PROCESS mode chokepoint (core/safety). A nil gate ⇒ read-only, Exec refuses — mutation stays
	// OFF. It is the SAME chokepoint the interceptor wires; this leaf never mutates it.
	gate *safety.Chokepoint
}

// Config constructs an Actuator. Opener and Renderer are required for a write-capable actuator. ModeGate is the
// process mode chokepoint the actuator re-checks at its leaf; a nil gate yields a read-only actuator whose Exec
// fails closed (the fail-safe default — mutation is never on by omission).
type Config struct {
	Opener    Opener
	Renderer  Renderer
	Allowlist RepoAllowlist
	ModeGate  *safety.Chokepoint
}

// New builds a gitops-mr actuator. It fails closed if the Opener or Renderer is missing. An empty allowlist or
// a nil mode gate is permitted at construction but leaves the actuator read-only (it can only refuse) — the
// safe posture until the operator declares an allowlist AND the mode is escalated at the owner-present flip.
func New(cfg Config) (*Actuator, error) {
	if cfg.Opener == nil {
		return nil, fmt.Errorf("gitopsmr: actuator requires an MR opener")
	}
	if cfg.Renderer == nil {
		return nil, fmt.Errorf("gitopsmr: actuator requires a renderer")
	}
	return &Actuator{opener: cfg.Opener, renderer: cfg.Renderer, allowlist: cfg.Allowlist, gate: cfg.ModeGate}, nil
}

// Capability implements adapters/actuation.Actuator.
func (a *Actuator) Capability() string { return Capability }

// ReadOnly reports whether this actuator can only observe. It returns true UNLESS the mode chokepoint currently
// permits actuation AND the actuator carries a genuine write path (a non-empty repo allowlist). Mutation ships
// OFF, so this returns true in every production and test path except one that constructs a test-only actuating
// chokepoint — the structural proof the actuator cannot write until the mode is escalated (mirrors awx-job/ssh).
func (a *Actuator) ReadOnly() bool {
	return !(a.gate != nil && a.gate.MayActuate() && len(a.allowlist) > 0)
}

// Exec is the Actuator chokepoint the interceptor calls AFTER it has admitted, policy-authorized, and
// mode-permitted the action. It renders the typed edits and opens an MR (exactly two REST calls, then STOP),
// returning the async MR handle. It re-enforces the mode chokepoint, the allowlist, the op-class binding, the
// ≠1-field diff-minimality, and the secret guard HERE, at the effect leaf, as defense in depth: reached
// directly it still refuses at Shadow, refuses a non-allowlisted repo or an op-class mismatch, and refuses a
// decoded secret — before any REST call. It NEVER merges, never comments `atlantis apply`, never touches the
// cluster API.
func (a *Actuator) Exec(ctx context.Context, argv []string, stdin []byte) (actuation.Result, error) {
	// 1. Mode chokepoint (defense in depth): an open NEVER fires while the mode is out. A nil gate is a
	//    read-only actuator with no write path.
	if a.gate == nil {
		return actuation.Result{}, ErrOpenGateClosed
	}
	if err := a.gate.GuardMutation(); err != nil {
		return actuation.Result{}, fmt.Errorf("%w: %v", ErrOpenGateClosed, err)
	}
	// 2. Structural argv guard: the argv MUST be exactly the fixed propose verb — no free-form command shape.
	if len(argv) != 1 || argv[0] != ProposeVerb {
		return actuation.Result{}, ErrNotProposeArgv
	}
	// 3. Decode the typed plan-as-data from stdin.
	if len(stdin) == 0 {
		return actuation.Result{}, fmt.Errorf("%w: empty stdin", ErrBadProposeSpec)
	}
	var spec ProposeSpec
	if err := json.Unmarshal(stdin, &spec); err != nil {
		return actuation.Result{}, fmt.Errorf("%w: %v", ErrBadProposeSpec, err)
	}
	if len(spec.Edits) == 0 {
		return actuation.Result{}, ErrNoEdits
	}
	// 4. Allowlist: refuse a repo the operator did not sanction.
	pol, ok := a.allowlist[spec.RepoID]
	if !ok {
		return actuation.Result{}, fmt.Errorf("%w: repo %q", ErrRepoNotAllowlisted, spec.RepoID)
	}
	// 4b. Op-class binding (confused-deputy guard): the op-class the interceptor policy-authorized (carried in
	//     the spec) MUST equal the op-class the allowlist bound to this repo.
	if strings.TrimSpace(spec.OpClass) != strings.TrimSpace(pol.OpClass) {
		return actuation.Result{}, fmt.Errorf("%w: propose op-class %q, repo %q bound to %q",
			ErrOpClassMismatch, spec.OpClass, spec.RepoID, pol.OpClass)
	}
	// 5. Render the typed edits into file bytes (structured; the Renderer refuses ≠1-field / unknown rule).
	files, err := a.renderer.Render(pol, spec.Edits)
	if err != nil {
		return actuation.Result{}, fmt.Errorf("gitopsmr: render: %w", err)
	}
	if len(files) == 0 {
		return actuation.Result{}, fmt.Errorf("%w: renderer produced no file change", ErrNoEdits)
	}
	// 6. Secret guard (design §4): no decoded secret value ever enters Git — scan the rendered patch AND the
	//    rationale. Only references (SecretRef / remoteRef plumbing) are MR-safe.
	if err := guardNoSecretValues(files, spec.Rationale); err != nil {
		return actuation.Result{}, err
	}
	// 7. Open the MR (credential resolved inside the Opener now — after policy + mode, before the write). Exactly
	//    two REST calls, then STOP. Returns the async MR handle; the actuator declares NO success — an open is a
	//    prediction, verified later by the deferred-verify channel.
	branch := branchName(pol, spec)
	title, body := mrText(pol, spec)
	opened, err := a.opener.OpenMR(ctx, pol, branch, title, body, files)
	if err != nil {
		return actuation.Result{}, err
	}
	// The Opener returns the MR iid/branch/url; the Actuator stamps the repo identity it holds (the allowlist
	// key) so the async handle is `<repoID>!<iid>` — the shape the deferred-verify poller splits back apart.
	opened.RepoID = spec.RepoID
	opened.Handle = MakeHandle(spec.RepoID, opened.IID)
	out, err := json.Marshal(opened)
	if err != nil {
		return actuation.Result{}, fmt.Errorf("gitopsmr: encode opened handle: %w", err)
	}
	return actuation.Result{Stdout: out}, nil
}

// compile-time proof the actuator satisfies the stable actuation interface (so it drops into the interceptor
// exactly like the SSH and awx-job effect leaves).
var _ actuation.Actuator = (*Actuator)(nil)
