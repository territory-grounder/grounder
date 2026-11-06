// Package opschema is the SINGLE SOURCE OF TRUTH for Territory Grounder's actuation op-class schemas: for
// each actuatable op_class, its structured-parameter contract AND the deterministic argv the orchestrator
// builds from those params. Before this registry the same fact — "restart-service needs a `unit`, whose argv
// is [systemctl restart <unit>]" — lived in ~4 places that could drift (the agent prompt/example, the
// proposal parser, the runner's sealedArgv, the ssh effect leaf); a proposal that OMITTED the unit was
// accepted at every earlier layer and only failed at EXECUTE-time with an opaque "empty argv (no program)".
// Collapsing the fact to ONE declared schema lets the parser/interceptor screen it at PROPOSE-time, the
// runner build the argv from it, the effect leaf re-derive the same argv, and the agent prompt render it —
// all reading the same data.
//
// Load-bearing property (INV-08): NOTHING here becomes control flow. Lookup matches an EXACT normalized
// op_class slug (never a model-token-driven branch); the ParamSpecs screen/steer params and render prompt
// DATA; the ArgvBuilder is a fixed argv vector, never a shell/string-built command (INV-02). A poka-yoke
// validator is only safe when it is EXACTLY as tolerant as the reader it guards, so ValidateArgs and the
// argv builder share one tolerance (proven by the validator-tolerance == builder-tolerance test): a params
// set the builder accepts always passes ValidateArgs, and one it rejects always fails — no stricter, no
// looser.
//
// Provenance: [O] INV-02 (fixed argv, no shell), INV-06 (one grammar; this is the op-class SCHEMA the single
// parser reads, not a second grammar), INV-08 (schema is data, dispatch is an exact validated name lookup),
// spec/013 · [F] "deterministic orchestrator owns the effect channel" · design-wisdom #5 (ACI: a caller acts
// "from the description and parameters alone"), the ACI-validation-tolerance lesson (a validator must match
// the reader it guards).
package opschema

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// ParamUnit is the structured param key for the systemd unit an op-class restarts. It is the ONE spelling
// the restart-service schema, the runner's argv builder, and the ssh effect leaf all use, so the key can
// never drift between the producer and the consumer.
const ParamUnit = "unit"

// ParamContainer is the structured param key for the docker container an op-class restarts. Like ParamUnit it
// is the ONE spelling the restart-container schema, the runner's argv builder, and the ssh effect leaf share,
// so the container name can never drift between the producer and the consumer.
const ParamContainer = "container"

// ParamGuest is the structured param key for the Proxmox VE guest (VM / LXC) a lifecycle op targets. It is the
// ONE spelling the start-guest schema, its argv builder, and the proxmox effect leaf share, so the guest name
// can never drift between the producer and the consumer.
const ParamGuest = "guest"

// EffectKind is the closed set of effect CHANNELS an op-class's params translate into. It decides HOW the
// runner turns a proposal into the sealed effect the interceptor executes — NOT a model-token branch (INV-08):
// the kind is a fixed field of the loadable schema, resolved by an EXACT op-class lookup, never chosen from
// the model's output. The default (absent/blank) is EffectSSHArgv so the existing loadable schema need not
// stamp the SSH default on every class (behavior-preserving).
const (
	// EffectSSHArgv — the params build a FIXED argv vector via the op-class's COMPILED ArgvBuilder, executed by
	// a native-ssh / local effect leaf (restart-service, restart-container, reload-service). INV-02: a fixed
	// vector, never a shell/string-built command.
	EffectSSHArgv = "ssh-argv"
	// EffectAWXLaunch — the params are the typed extra_vars of an AWX job-template launch. There is NO argv
	// builder here (the effect is NOT an SSH command): the runner encodes a LaunchSpec (template id from the
	// operator's op-class→template config + the params as extra_vars) into the interceptor request's argv
	// (=[LaunchVerb]) + stdin, and the awx-job effect leaf validates + launches it. Still INV-02: the launch is
	// a fixed template id + typed extra_vars, never a command string.
	EffectAWXLaunch = "awx-launch"
	// EffectProxmoxLifecycle — a Proxmox VE guest LIFECYCLE op (start / stop). Like ssh-argv it is ARGV-encoded
	// (its params build a FIXED argv, e.g. [start, <guest>], via a compiled builder); UNLIKE ssh-argv it routes
	// by KIND to the proxmox lane (the guest is proxmox-mediated for start/stop even though it is native-ssh for
	// a service restart). So ENCODING (argv) and ROUTING (by kind) are orthogonal: an effect kind is argv-encoded
	// (ssh-argv, proxmox-lifecycle — has a compiled builder) OR launch-encoded (awx-launch — no builder), and
	// separately routes by target (ssh-argv) or by kind (awx-launch, proxmox-lifecycle, decided in the runner).
	EffectProxmoxLifecycle = "proxmox-lifecycle"
	// EffectK8sDeclarative — a change to a GitOps-managed declarative source (a Helm value, a manifest field on
	// a target whose desired state lives in Git). LAUNCH-encoded like awx-launch (NO compiled builder): the
	// runner translates the op-class + its typed params into a gitops-mr ProposeSpec (repo + closed field
	// edits, never free-form file bytes) and the gitops-mr actuator opens an MR — it NEVER touches the cluster
	// API. Routes by KIND to the gitops-mr lane (a declarative k8s change is Git-mediated regardless of the
	// target's own management regime — TG-122). The MR is an ASYNC effect: the lane answers with a handle, so
	// it is producer-reserved + deferred-verified (slice 0), never adjudicated on the synchronous path.
	EffectK8sDeclarative = "k8s-declarative"
)

// argvEncoded reports whether an effect kind's effect is a FIXED argv built by a compiled ArgvBuilder (ssh-argv,
// proxmox-lifecycle) rather than a non-argv launch the runner encodes elsewhere (awx-launch → a LaunchSpec). It
// is the single predicate that decides which kinds REQUIRE a compiled builder (mustBuildRegistry) and which have
// an SSH-style argv (Argv). An unknown kind is not argv-encoded (mustBuildRegistry rejects it explicitly first).
func argvEncoded(kind string) bool {
	switch kind {
	case EffectSSHArgv, EffectProxmoxLifecycle:
		return true
	default:
		return false
	}
}

// ParamSpec is the typed schema of ONE structured parameter an op-class accepts. It MIRRORS agent.ParamSpec
// (name/type/required/enum/example/description) — the same ACI contract the read-only investigation tools
// publish — but lives here, dependency-free, so the parser, interceptor, runner, effect leaf, AND the
// agent-prompt renderer can all read ONE schema without an import cycle (agent imports opschema; opschema
// imports nothing from the repo). No field here becomes control flow (INV-08): the schema screens/steers a
// param and renders prompt DATA; it is never a dispatch key.
type ParamSpec struct {
	Name        string   `json:"name"`                  // the param key the argv builder reads from the proposal's params
	Type        string   `json:"type"`                  // a human-facing type hint rendered to the model (e.g. "string")
	Required    bool     `json:"required"`              // WHEN true, an absent/blank value is an actionable error, not a silent pass
	Enum        []string `json:"enum,omitempty"`        // WHEN non-empty, the value MUST be one of these (else an actionable error)
	Example     string   `json:"example,omitempty"`     // a concrete example value that steers the model toward a valid, complete proposal
	Description string   `json:"description,omitempty"` // one line: what this param selects
}

// ArgvBuilder deterministically translates an op-class's structured params into the FIXED argv vector (no
// shell, no string-built command — INV-02). It returns an error when a required param is missing or blank —
// the SAME tolerance ValidateArgs screens for, so a params set ValidateArgs passes always builds and one it
// rejects never does (validator-tolerance == builder-tolerance). It is a pure function of the declared
// schema and the supplied params.
type ArgvBuilder func(params map[string]string) ([]string, error)

// OpClassSpec is one actuatable op-class's declared contract: its canonical op_class slug, a human op hint,
// its structured params, and the deterministic argv builder. It is registry DATA, never a dispatch key —
// Lookup resolves it by an EXACT normalized slug (INV-08).

// CommitConfirmedSpec is the per-class commit-confirmed declaration (spec/029 REQ-2904), pure data.
type CommitConfirmedSpec struct {
	// WindowSeconds is the confirm window: the armed revert fires when it elapses without a
	// positive mechanical confirm (HOLD+page on unverifiable — REQ-2902 as signed off). Floors are
	// validated at load: ≥ commitConfirmWindowFloorSeconds for every class, and STRICTLY GREATER
	// than AwxWindowFloorSeconds for awx-launch classes, whose terminal outcome may legitimately
	// arrive as late as the deferred-verify bound (spec/017 REQ-1711).
	WindowSeconds int `json:"window_seconds"`
}

// commitConfirmWindowFloorSeconds is REQ-2904's conservative default floor: minutes, at least 2× the
// monitoring poll cycle (the estate polls at ≤150s), so a confirm that depends on one poll landing
// can always receive it.
const commitConfirmWindowFloorSeconds = 300

// AwxWindowFloorSeconds mirrors core/regime.DefaultVerificationBound (30m). It is REPEATED here as a
// literal because importing core/regime from opschema would cycle; the two constants are pinned equal
// by TestAwxWindowFloorMatchesTheDeferredVerifyBound in core/regime (the drift guard).
const AwxWindowFloorSeconds = 1800

type OpClassSpec struct {
	OpClass    string      `json:"op_class"`              // the canonical op_class slug (e.g. "restart-service")
	Op         string      `json:"op"`                    // a human op hint rendered to the model (e.g. "restart")
	EffectKind string      `json:"effect_kind,omitempty"` // the effect CHANNEL (see Kind); "" ⇒ EffectSSHArgv (behavior-preserving)
	Family     string      `json:"family"`                // the graduation/authorization GROUP this class belongs to (see Family)
	SafetyTier string      `json:"safety_tier"`           // how dangerous this class is (see SafetyTier); floors what band it may reach
	Params     []ParamSpec `json:"params"`                // the structured params this op-class requires/accepts
	// ArgvTemplate is an OPERATOR-AUTHORED argv template — the DATA alternative to a compiled builder, for
	// the ~90% of verbs that are a fixed shape with typed slots. See renderTemplate for the substitution
	// rules and why it does not weaken INV-02. Exactly one of {build, ArgvTemplate} may be set.
	ArgvTemplate []string `json:"argv_template,omitempty"`
	// RollbackTemplate is the class's COMPENSATING argv, as data, under the same rules as ArgvTemplate. It is
	// optional: absent means the rollback is a re-run of the forward argv, which is correct for the idempotent
	// reconvergence verbs (a restart's compensating action is a restart). It is REQUIRED in spirit wherever the
	// forward is not its own inverse — `start` is the standing example, whose compensating action is `stop` and
	// emphatically not another `start`. Declaring it here rather than in the effect leaf keeps the pair in ONE
	// place: a rollback that lives beside the forward cannot drift from it.
	RollbackTemplate []string `json:"rollback_template,omitempty"`
	// RequiresTargetState is the class's STATE PRECONDITION (TG-378), a CLOSED enum: "" (none) or
	// "not-running" — the target must be OBSERVED not running before a manifest for this class may seal.
	// Declared here, per class, because the pve03 cascade proved the alternative: TG sealed `start`
	// manifests for guests with 2,000-hour uptimes, and the only gate that stopped them was an unrelated
	// global band. The prediction gate enforces it fail-closed: an action whose precondition cannot be
	// ESTABLISHED (unknown state, unwired reader, stale projection) refuses — unknown is not not-running.
	// omitempty keeps every existing overlay's CanonicalHash byte-stable (a ratified entry_hash must not
	// move under a vocabulary addition it does not use).
	RequiresTargetState string      `json:"requires_target_state,omitempty"`
	// CommitConfirmed opts this class into commit-confirmed actuation (spec/029 REQ-2904; owner
	// sign-off TG-488 B5): the forward effect executes only WITH a pre-armed self-revert, and
	// survives only if the mechanical verifier confirms inside the window. ELIGIBILITY IS DATA:
	// absent means the class is NOT eligible and nothing arms. Validation at load is the
	// conservative floor — an eligible class MUST declare its inverse (rollback_template for argv
	// classes, rollback_op_class for builder/lifecycle classes) and a window ≥ the floor
	// (awx-launch classes: > the spec/017 deferred-verify bound — see AwxWindowFloorSeconds).
	// omitempty: a nil pointer serializes to nothing, so every pre-029 overlay CanonicalHash is
	// byte-stable (the requires_target_state precedent).
	CommitConfirmed *CommitConfirmedSpec `json:"commit_confirmed,omitempty"`
	// RollbackOpClass names the COMPENSATING op-class for classes whose effect is not argv-encoded
	// (a proxmox-lifecycle start's inverse is the stop CLASS, not an argv). Validated at registry
	// load to resolve to an existing class; dangling references refuse the whole registry (fail
	// closed). Exactly one of {RollbackTemplate, RollbackOpClass} may accompany CommitConfirmed.
	RollbackOpClass string `json:"rollback_op_class,omitempty"`
	build               ArgvBuilder `json:"-"` // the deterministic params → fixed argv translation (COMPILED)
}

// The CLOSED set of state preconditions. "" means the class declares none.
// "running" joined the set with stop-guest (spec/029 T-029-3): the commit-confirmed inverse of a
// start must observe its target RUNNING before it may seal — the mirror of the pve03 guard, and
// the blind-stop protection REQ-2902's hold case worries about (never stop a guest someone else
// already cycled). Same fail-closed semantics: unknown is not running, an unwired reader refuses.
const (
	RequiresNotRunning = "not-running"
	RequiresRunning    = "running"
)

// The CLOSED set of op-class FAMILIES. A family is the unit a graduation ladder will be keyed on: verbs that
// share an operational shape, a blast radius and a rollback story earn autonomy together rather than one slug
// at a time. Keyed as data (not a dispatch branch) and matched EXACTLY after normalization (INV-08).
//
// Why a closed set: an unrecognised family would silently create a NEW ladder nobody is watching — a class
// could then reach autonomy through a group that no operator ever reviewed. Unknown ⇒ fail closed at init.
const (
	FamilyServiceLifecycle   = "service-lifecycle"   // systemd units: start/stop/restart/reload/enable/disable
	FamilyContainerLifecycle = "container-lifecycle" // docker/compose: restart/start/stop/recreate/pull
	FamilyGuestLifecycle     = "guest-lifecycle"     // Proxmox VM/LXC: start/stop/reboot/shutdown
	FamilyDiskReclaim        = "disk-reclaim"        // free space: prune/vacuum/rotate/trim — mostly IRREVERSIBLE
	FamilyResourceResize     = "resource-resize"     // grow disk / set memory / set cores
	FamilyK8sWorkload        = "k8s-workload"        // pod delete, rollout restart, scale, cordon/drain
	FamilyNetworkDevice      = "network-device"      // Cisco/pfSense/APC — vendor gear, can PARTITION the estate
	FamilyStorage            = "storage"             // ZFS/LVM/Ceph/Synology volume actions
	FamilyProcess            = "process"             // kill/renice a runaway process
	FamilyPackage            = "package"             // package install/upgrade/clean
)

// The CLOSED set of SAFETY TIERS, ordered by how much damage a wrong call does. The tier FLOORS the band an
// op-class may reach: a tier is never a reason to ALLOW more, only ever to allow less (safe-direction only,
// mirroring the never-auto floor).
const (
	// TierLowReversible — a clean inverse exists and running it twice is harmless (start↔stop, restart).
	TierLowReversible = "low-reversible"
	// TierMedium — recoverable but disruptive (recreate a container, reboot a guest); brief outage is expected.
	TierMedium = "medium"
	// TierIrreversible — DESTROYS state with no inverse (prune, vacuum, delete, truncate). May never auto.
	TierIrreversible = "irreversible"
	// TierVendorCritical — production network/NAS gear. A wrong call can PARTITION the estate and cut the agent
	// off from the hosts it is fixing. Never auto-promotable, and its drill is separate from every other tier.
	TierVendorCritical = "vendor-critical"
)

var knownFamilies = map[string]bool{
	FamilyServiceLifecycle: true, FamilyContainerLifecycle: true, FamilyGuestLifecycle: true,
	FamilyDiskReclaim: true, FamilyResourceResize: true, FamilyK8sWorkload: true,
	FamilyNetworkDevice: true, FamilyStorage: true, FamilyProcess: true, FamilyPackage: true,
}

var knownTiers = map[string]bool{
	TierLowReversible: true, TierMedium: true, TierIrreversible: true, TierVendorCritical: true,
}

// AutoEligible reports whether a tier may EVER reach autonomy. Irreversible and vendor-critical verbs are
// floored at human approval by construction — no accrual of clean runs can lift them, because the question a
// clean run answers ("did it work?") is not the question these tiers pose ("what if it does not?").
func AutoEligible(tier string) bool {
	t := strings.ToLower(strings.TrimSpace(tier))
	return t == TierLowReversible || t == TierMedium
}

// Kind returns the op-class's effect kind, defaulting an absent/blank value to EffectSSHArgv — so the loadable
// schema need not stamp the SSH default on every existing class, and an older schema (no effect_kind field)
// keeps its ssh-argv meaning. Normalized (trim + lowercase) so a case/whitespace variant cannot dodge the
// exact-match dispatch the runner does on it.
// SafelyCompensatable is the STATIC half of the reversibility authority (spec/030 REQ-3004): does this
// class carry a compensating action that actually UNDOES its forward? True only for a low-reversible
// class that either DECLARES a rollback_template or whose op is a genuine idempotent-reconvergence verb
// (restart/reload). Everything else — medium/irreversible tiers, and the start-with-no-declared-inverse
// shape whose "rollback" would silently RE-RUN the forward — is not compensatable, fail closed. The
// runtime half (params in hand, forward sealed reversible, non-empty argv) stays with the rollback
// executor (temporal/runner rollbackArgvFor), which enforces this same static criterion per call; the
// transaction-plan registry consults THIS predicate at build time so an uncompensatable step can never
// join a recipe.
func (s OpClassSpec) SafelyCompensatable() bool {
	if s.SafetyTier != TierLowReversible {
		return false
	}
	op := strings.ToLower(strings.TrimSpace(s.Op))
	return len(s.RollbackTemplate) > 0 || op == "restart" || op == "reload"
}

func (s OpClassSpec) Kind() string {
	k := strings.ToLower(strings.TrimSpace(s.EffectKind))
	if k == "" {
		return EffectSSHArgv
	}
	return k
}

// opschemaJSON is the LOADABLE op-class schema document — the op_class/op/params contract the model reads,
// proposes against, and the prompt catalog renders — shipped as the SAME rules-as-data JSON an operator would
// author (the prose-loadable rule: "what the model READS is loadable; code that TOUCHES the estate is
// compiled"). It is embedded (not read from disk) so it is tamper-evident with the binary, and the argv
// BUILDERS below stay compiled — a loaded schema can describe/screen/steer a proposal but can NEVER define
// what actuates.
//
//go:embed opschema.json
var opschemaJSON []byte

// builders are the COMPILED params→argv translations, keyed by normalized op_class. Argv construction is code
// that TOUCHES the estate (INV-02: a fixed vector, never a shell/string-built command), so it is NEVER loaded
// from data — a new actuatable op-class REQUIRES a compiled builder here, no operator-supplied schema can add
// an execution path. A schema in opschema.json with no builder here is fail-closed at init (unactuatable); a
// builder here with no schema is unreachable — mustBuildRegistry rejects both (fail closed). Each builder
// screens ONLY required-param presence, exactly as tolerant as ValidateArgs screens the declared schema
// (validator-tolerance == builder-tolerance, proven by the tolerance test) — so a params set ValidateArgs
// accepts always builds, and one it rejects never does.
var builders = map[string]ArgvBuilder{
	// start-guest — a Proxmox VE guest START (bring a DOWN VM/LXC that should be up back up). ARGV-encoded
	// [start, <guest>] but effect_kind=proxmox-lifecycle so it routes to the PROXMOX lane (not the ssh leaf).
	// Reversible (start↔stop); the harsher lifecycle verbs (reboot/shutdown/reset/destroy) are floored in the
	// proxmox actuator. The guest-name shape + the operator allowed-guests allowlist are the effect leaf's runtime
	// controls (modules/actuation/proxmox); this builder screens only presence. The proxmox actuator resolves the
	// guest name → its node/vmid against the PVE cluster.
	"start-guest": func(params map[string]string) ([]string, error) {
		guest := strings.TrimSpace(params[ParamGuest])
		if guest == "" {
			return nil, fmt.Errorf("op-class %q: missing required param %q (%s) — %s; e.g. %q",
				"start-guest", ParamGuest, "string", "the Proxmox guest (VM/LXC) to start", "librespeed01")
		}
		return []string{"start", guest}, nil
	},
	// stop-guest — start-guest's declared commit-confirmed INVERSE (spec/029 T-029-3): the armed
	// revert's compensating action when a started guest fails its confirm window. ARGV-encoded
	// [stop, <guest>], proxmox lane, requires_target_state=running (the blind-stop guard: never
	// stop a guest someone else already cycled). COMPILED here like every actuatable class — the
	// inverse is a first-class mutation (REQ-2903), never a special path.
	"stop-guest": func(params map[string]string) ([]string, error) {
		guest := strings.TrimSpace(params[ParamGuest])
		if guest == "" {
			return nil, fmt.Errorf("op-class %q: missing required param %q (%s) — %s; e.g. %q",
				"stop-guest", ParamGuest, "string", "the Proxmox guest (VM/LXC) to stop", "librespeed01")
		}
		return []string{"stop", guest}, nil
	},
}

// registry is the immutable set of actuatable op-class schemas, keyed by NORMALIZED op_class slug: the LOADED
// schema (opschema.json) with its COMPILED builder attached. It is UNEXPORTED and reachable only through
// Lookup/Specs/Catalog (mirroring safety.neverAutoFloor) so no package can mutate a live schema during a
// canary — an exported map could be reassigned/deleted, silently redefining what an op-class actuates.
var registry = mustBuildRegistry(schemaForProfile(), builders)

// DayZeroEnv opts a deployment into the EMPTY embedded catalog — ADR-0016's day-zero posture, where TG is a
// full-capability shadow adviser that can execute nothing and every capability must be EARNED (spec/026,
// spec/028). It is the configuration the predecessor runs in by construction: an open-world adviser with no
// hand-authored catalog at all.
//
// It is a deployment PROFILE, not a feature flag: read once at init, never re-read, and it can only ever
// REMOVE capability. Setting it cannot make anything execute that would not have executed anyway, because an
// absent op-class is rung 0 (registry absence), which the existing chain already refuses at four independent
// points — nil sealedArgv, the empty-argv leaf refusal, the never-auto floor, and the mode chokepoint.
const DayZeroEnv = "TG_DAYZERO_EMPTY_CATALOG"

// DayZero reports whether this process runs the empty-catalog profile.
func DayZero() bool { return os.Getenv(DayZeroEnv) == "1" }

// schemaForProfile returns the embedded catalog, or an EMPTY one under the day-zero profile.
//
// Until this existed, ADR-0016's central promise — "day-zero TG (empty catalog) is a full-capability shadow
// adviser and can execute nothing" — had NO reachable code path: emptying opschema.json panicked at init,
// because every one of the seven compiled builders became a builder with no schema. The claim the whole
// earned-ladder epic rests on could not be exercised, let alone measured against the predecessor.
func schemaForProfile() []byte {
	if DayZero() {
		return []byte(`{"op_classes":[]}`)
	}
	return opschemaJSON
}

// mustBuildRegistry parses the embedded loadable schema and binds each op-class to its compiled builder, failing
// CLOSED (panic at init — the actuation surface must never boot half-defined) on any drift: malformed JSON, a
// schema op-class with no compiled builder (unactuatable), a duplicate op-class, or a compiled builder with no
// schema (unreachable). The bidirectional check keeps the loadable schema and the compiled builders in lockstep
// so neither can silently diverge from the other.
func mustBuildRegistry(schemaJSON []byte, builders map[string]ArgvBuilder) map[string]OpClassSpec {
	var doc struct {
		OpClasses []OpClassSpec `json:"op_classes"`
	}
	if err := json.Unmarshal(schemaJSON, &doc); err != nil {
		panic(fmt.Sprintf("opschema: cannot parse embedded opschema.json (fail closed): %v", err))
	}
	m := make(map[string]OpClassSpec, len(doc.OpClasses))
	for _, s := range doc.OpClasses {
		key := normalize(s.OpClass)
		if _, dup := m[key]; dup && key != "" {
			panic(fmt.Sprintf("opschema: duplicate op-class %q in opschema.json (fail closed)", s.OpClass))
		}
		// THE ADMISSION CHECKS LIVE IN ValidateSpec (spec/028 REQ-2814, overlay.go). They were written as
		// init-time panics because the embedded registry is compiled in: a malformed entry is a build defect
		// and dying at boot is the right answer. Ratification changed the threat model — the same rules must
		// now judge a spec that arrives at RUNTIME from an operator form, where a panic is a denial of
		// service. So the rules moved to an error-returning function and THIS caller re-raises them as the
		// same init panic. Embedded behaviour is unchanged; the rules simply became reusable.
		b, hasBuilder := builders[key]
		spec, err := ValidateSpec(s, hasBuilder && b != nil)
		if err != nil {
			panic(fmt.Sprintf("opschema: %v", err))
		}
		if argvEncoded(spec.Kind()) {
			spec.build = b
		}
		m[normalize(spec.OpClass)] = spec
	}
	// COMMIT-CONFIRMED cross-reference pass (spec/029 REQ-2904): a rollback_op_class must resolve to a
	// class that EXISTS in this registry — a dangling inverse would arm a revert that dies at fire time,
	// which is the revert-failed incident REQ-2906 pages about, built in at load. Fail closed instead.
	for key, s := range m {
		if s.CommitConfirmed == nil || strings.TrimSpace(s.RollbackOpClass) == "" {
			continue
		}
		if _, ok := m[normalize(s.RollbackOpClass)]; !ok {
			panic(fmt.Sprintf("opschema: op-class %q declares rollback_op_class %q which does not exist "+
				"in the registry — an unresolvable inverse cannot self-revert (fail closed; REQ-2904) [%s]",
				s.OpClass, s.RollbackOpClass, key))
		}
	}
	// Every compiled builder must back an ARGV-encoded schema: a builder with no schema is unreachable, and a
	// builder keyed to a launch-encoded class is a contradiction (that class encodes its effect elsewhere). Fail closed.
	for key := range builders {
		s, ok := m[key]
		if !ok {
			// Under the day-zero profile EVERY builder is orphaned BY DESIGN — that is what an empty
			// catalog means — so the orphan check is suspended here and nowhere else. The check exists to
			// catch a BUILD DEFECT (a builder someone forgot to give a schema); under an explicitly
			// requested empty catalog an orphan is the requested state, not a defect, and panicking would
			// make ADR-0016's day-zero posture unreachable (it was, until this branch existed).
			//
			// Suspending it removes no protection: an orphaned builder is unreachable precisely BECAUSE
			// nothing can look it up. Execution needs a spec, Lookup returns none, and the four independent
			// refusals downstream (nil sealedArgv, empty-argv leaf refusal, never-auto floor, mode
			// chokepoint) each independently stop an actuation. The profile can only ever REMOVE capability.
			if DayZero() {
				continue
			}
			panic(fmt.Sprintf("opschema: compiled builder %q has no schema in opschema.json (unreachable — fail closed)", key))
		}
		if !argvEncoded(s.Kind()) {
			panic(fmt.Sprintf("opschema: compiled builder %q backs a %q op-class, which is not argv-encoded (contradiction — fail closed)", key, s.Kind()))
		}
	}
	return m
}

// templateSlotRE matches a WHOLE-ELEMENT substitution slot, e.g. "${unit}". Deliberately anchored: a slot
// must be the ENTIRE argv element, never a substring of one.
var templateSlotRE = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)

// renderTemplate turns an OPERATOR-AUTHORED template into a fixed argv vector by substituting declared,
// validated params. It is the DATA equivalent of a compiled ArgvBuilder and it does NOT weaken INV-02:
//
//   - the template is authored by an OPERATOR and shipped embedded + lockstep-hashed, exactly like the rest
//     of opschema.json — no model token contributes to it;
//   - substitution is WHOLE-ELEMENT ONLY. A slot must BE an argv element, never part of one. That is what
//     makes injection structurally impossible: a param value becomes exactly one argv element, so it cannot
//     add an element, a flag, a separator or a redirection whatever it contains. There is no shell anywhere
//     on this path (INV-02 forbids sh -c), so a value like "; rm -rf /" is an inert filename, not a command;
//   - the rendered result is a fixed argv vector, identical in kind to what a builder produces.
//
// Substring interpolation ("--size=${n}") is deliberately NOT supported. It would still be injection-safe
// for the same reason, but it makes the safety argument depend on reasoning about the surrounding literal
// rather than on the shape of the template. A stricter rule that is obviously correct beats a looser one
// that needs an argument. Split it into two elements instead.
//
// Validator tolerance == renderer tolerance: this rejects exactly what ValidateArgs rejects (missing or
// blank when Required), so a params set that validates always renders and one that fails never does.
// RollbackArgv renders the class's compensating argv. With no declared rollback template it is a fresh copy
// of the forward argv — INV-07 requires a BOUND rollback, not a perfect one, and re-running an idempotent verb
// reconverges to the known-good state.
func (s OpClassSpec) RollbackArgv(params map[string]string) ([]string, error) {
	if len(s.RollbackTemplate) == 0 {
		return s.Argv(params)
	}
	return s.renderFrom(s.RollbackTemplate, params)
}

func (s OpClassSpec) renderTemplate(params map[string]string) ([]string, error) {
	return s.renderFrom(s.ArgvTemplate, params)
}

func (s OpClassSpec) renderFrom(template []string, params map[string]string) ([]string, error) {
	out := make([]string, 0, len(template))
	for _, el := range template {
		m := templateSlotRE.FindStringSubmatch(el)
		if m == nil {
			out = append(out, el) // a literal
			continue
		}
		name := m[1]
		v, ok := params[name]
		if !ok || strings.TrimSpace(v) == "" {
			spec, _ := s.paramSpec(name)
			if spec.Example != "" {
				return nil, fmt.Errorf("op-class %q: missing required param %q (%s) — %s; e.g. %q",
					s.OpClass, name, spec.Type, spec.Description, spec.Example)
			}
			return nil, fmt.Errorf("op-class %q: missing required param %q", s.OpClass, name)
		}
		out = append(out, v)
	}
	return out, nil
}

// MatchTemplate is the INVERSE of renderTemplate: given an argv vector, decide whether this op-class could
// have produced it, and if so return the value that filled its slot. It lives beside the renderer on purpose —
// a reverse lookup that drifts from the thing it reverses is worse than no reverse lookup, because the effect
// leaf uses the pair as a round-trip integrity check (classify → re-derive → compare).
//
// Only TEMPLATED classes can be matched; a compiled builder is an opaque function and returns hit=false. That
// is the fail-closed direction: an unmatched argv gets no execution path.
//
// The match is STRUCTURAL — element count, then literal equality at every non-slot position. It never parses,
// never uses a regex against the value, and never accepts a prefix. A value is whatever occupied the slot.
func MatchTemplate(s OpClassSpec, argv []string) (value, param string, hit bool) {
	if len(s.ArgvTemplate) == 0 || len(argv) != len(s.ArgvTemplate) {
		return "", "", false
	}
	slots := 0
	for i, el := range s.ArgvTemplate {
		m := templateSlotRE.FindStringSubmatch(el)
		if m == nil {
			if argv[i] != el {
				return "", "", false // a literal that does not match
			}
			continue
		}
		slots++
		if slots > 1 {
			// Multi-slot templates are not reversible to a SINGLE (class, value) pair, which is the shape the
			// effect leaf's allowlist speaks in. Refuse rather than return one of them and let the caller
			// believe it has the whole story.
			return "", "", false
		}
		if strings.TrimSpace(argv[i]) == "" {
			return "", "", false // an empty slot value would render differently than it reads
		}
		value, param = argv[i], m[1]
	}
	if slots != 1 {
		// ZERO slots means an all-literal template (or a compiled class, whose template is empty and which an
		// empty argv would otherwise match with an empty value). Refusing here is what keeps a non-templated
		// class unclassifiable, and it is the reason the caller can treat a hit as "this class built this".
		return "", "", false
	}
	return value, param, true
}

func (s OpClassSpec) paramSpec(name string) (ParamSpec, bool) {
	for _, p := range s.Params {
		if p.Name == name {
			return p, true
		}
	}
	return ParamSpec{}, false
}

// normalize trims and lowercases an op_class slug before a lookup — mirroring safety.IsNeverAuto — so a case
// or whitespace variant can never dodge (or false-miss) the registry.
func normalize(opClass string) string { return strings.ToLower(strings.TrimSpace(opClass)) }

// Lookup returns the schema for opClass by an EXACT normalized-slug match. ok=false ⇒ the op_class is not a
// registered actuatable class; callers MUST fail closed (an unregistered op_class has no argv builder, so it
// can never produce an execution argv). This is a data lookup, not a model-token-driven branch (INV-08).
func Lookup(opClass string) (OpClassSpec, bool) {
	key := normalize(opClass)
	// EMBEDDED FIRST, ALWAYS. The overlay is consulted only for slugs the reviewed registry does not define,
	// so a runtime-ratified row can never shadow, weaken, or redefine a code-released capability (ADR-0016;
	// SetOverlay also refuses such a row at load, making this the second of two independent guards).
	if s, ok := registry[key]; ok {
		return s, true
	}
	return overlayLookup(key)
}

// Specs returns the registered schemas sorted by op_class slug (for docs, the prompt catalog, and tests). A
// COPY of the slice — the caller cannot mutate the registry.
func Specs() []OpClassSpec {
	out := make([]OpClassSpec, 0, len(registry))
	for _, s := range registry {
		out = append(out, s)
	}
	// Overlay classes join the same sorted list, so every consumer of the registry — the agent prompt
	// catalog (through Catalog), guardallow, the docs generator — sees ratified capabilities through the ONE
	// render rather than each growing its own second source (spec/028 REQ-2815).
	out = append(out, overlaySpecs()...)
	sort.Slice(out, func(i, j int) bool { return out[i].OpClass < out[j].OpClass })
	return out
}

// Argv builds the FIXED argv for a registered op_class from its structured params. An unregistered op_class,
// or a params set missing a required param, returns an error and a nil argv — the caller fails closed. This
// is the ONE place an actuatable op-class's argv is constructed; the runner (sealedArgv) and the ssh effect
// leaf both route through it, so the params → argv translation cannot drift.
func Argv(opClass string, params map[string]string) ([]string, error) {
	spec, ok := Lookup(opClass)
	if !ok {
		return nil, fmt.Errorf("op-class %q is not a registered actuatable class", opClass)
	}
	return spec.Argv(params)
}

// Argv builds the FIXED argv for this spec from params (INV-02: a fixed vector, never a shell/string-built
// command). It delegates to the spec's declared builder; a nil builder (a schema with no execution path) is
// a fail-closed error.
func (s OpClassSpec) Argv(params map[string]string) ([]string, error) {
	if !argvEncoded(s.Kind()) {
		// A launch-encoded class has no fixed argv — its effect is encoded by the runner for its channel (e.g. an
		// AWX LaunchSpec). Calling Argv on it is a caller bug; fail closed rather than return a misleading empty argv.
		return nil, fmt.Errorf("op-class %q has effect_kind %q — it is not argv-encoded (its effect is encoded for its own channel); Argv is argv-encoded kinds only", s.OpClass, s.Kind())
	}
	if s.build == nil {
		if len(s.ArgvTemplate) > 0 {
			return s.renderTemplate(params)
		}
		return nil, fmt.Errorf("op-class %q: no argv builder and no argv template declared", s.OpClass)
	}
	return s.build(params)
}

// ValidateArgs screens params against spec's declared ParamSpec schema (poka-yoke), EXACTLY as tolerant as
// the argv builder it guards: a Required param that is absent or blank, or a value outside a non-empty Enum,
// is refused with a SINGLE actionable message naming the param (Writing Effective Tools). It MIRRORS
// agent.ValidateArgs so the propose-time actuation screen and the investigation-tool screen behave
// identically. A value the builder accepts must pass here, and one it rejects must fail here (proven by the
// validator-tolerance == builder-tolerance test) — the ACI-validation lesson: a validator that is stricter
// than its reader silently regresses valid calls. No token becomes control flow (INV-08).
func ValidateArgs(spec OpClassSpec, params map[string]string) error {
	for _, p := range spec.Params {
		v, present := params[p.Name]
		if p.Required && (!present || strings.TrimSpace(v) == "") {
			if p.Example != "" {
				return fmt.Errorf("missing required param %q (%s) — %s; e.g. %q", p.Name, p.Type, p.Description, p.Example)
			}
			return fmt.Errorf("missing required param %q (%s) — %s", p.Name, p.Type, p.Description)
		}
		if present && len(p.Enum) > 0 && !containsStr(p.Enum, v) {
			return fmt.Errorf("param %q=%q is not one of the allowed values [%s]", p.Name, v, strings.Join(p.Enum, ", "))
		}
	}
	return nil
}

// Catalog renders the registered actuatable op-classes as a STRUCTURED block for the agent preamble — each
// op_class, its op hint, and its typed params (name/type/required/enum/example) — so the model emits a
// COMPLETE proposal (e.g. restart-service WITH params.unit) "from its description and parameters alone"
// (Writing Effective Tools, ACI). This MIRRORS agent.ToolSet.Catalog's rendering of read-only tools. Pure
// DATA: nothing rendered becomes control flow (INV-08) — the op_class is validated by an exact Lookup, never
// executed as text. Deterministic (op-classes sorted). An empty registry renders "".
func Catalog() string {
	specs := Specs()
	if len(specs) == 0 {
		return ""
	}
	// INVERSE-ONLY classes never render (spec/029 T-029-3, gate-caught): a class that exists as
	// another class's declared rollback_op_class (stop-guest) is the ARMED REVERT's compensating
	// action — the model must never be OFFERED it as a proposal (the opcover exemption's own
	// rationale: a fault→stop pairing would teach the agent to down guests). Registering it made
	// it actuatable; rendering it made it proposable — and the change gate measured the judged
	// dimensions drop the moment the stop verb + its registry meta-prose entered every preamble.
	// Derived from the registry itself (the set of declared inverse targets), so every future
	// class inverse is excluded by construction, not by remembering a flag.
	inverseOnly := map[string]bool{}
	for _, s := range specs {
		if rc := strings.TrimSpace(s.RollbackOpClass); rc != "" {
			inverseOnly[normalize(rc)] = true
		}
	}
	var b strings.Builder
	for _, s := range specs {
		if inverseOnly[normalize(s.OpClass)] {
			continue
		}
		b.WriteString("- ")
		b.WriteString(s.OpClass)
		if s.Op != "" {
			b.WriteString(" (op: ")
			b.WriteString(s.Op)
			b.WriteByte(')')
		}
		b.WriteByte('\n')
		for _, p := range s.Params {
			b.WriteString("    - ")
			b.WriteString(p.Name)
			b.WriteString(" (")
			b.WriteString(p.Type)
			if p.Required {
				b.WriteString(", required")
			} else {
				b.WriteString(", optional")
			}
			b.WriteByte(')')
			if d := strings.TrimSpace(p.Description); d != "" {
				b.WriteString(" — ")
				b.WriteString(d)
			}
			if len(p.Enum) > 0 {
				b.WriteString(" [one of: ")
				b.WriteString(strings.Join(p.Enum, ", "))
				b.WriteByte(']')
			}
			if p.Example != "" {
				b.WriteString(" e.g. ")
				b.WriteString(p.Example)
			}
			b.WriteByte('\n')
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
