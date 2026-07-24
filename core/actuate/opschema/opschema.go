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
type OpClassSpec struct {
	OpClass    string      `json:"op_class"`               // the canonical op_class slug (e.g. "restart-service")
	Op         string      `json:"op"`                     // a human op hint rendered to the model (e.g. "restart")
	EffectKind string      `json:"effect_kind,omitempty"`  // the effect CHANNEL (see Kind); "" ⇒ EffectSSHArgv (behavior-preserving)
	Params     []ParamSpec `json:"params"`                 // the structured params this op-class requires/accepts
	build      ArgvBuilder `json:"-"`                      // the deterministic params → fixed argv translation (COMPILED, ssh-argv only)
}

// Kind returns the op-class's effect kind, defaulting an absent/blank value to EffectSSHArgv — so the loadable
// schema need not stamp the SSH default on every existing class, and an older schema (no effect_kind field)
// keeps its ssh-argv meaning. Normalized (trim + lowercase) so a case/whitespace variant cannot dodge the
// exact-match dispatch the runner does on it.
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
	// restart-service — reversible, non-destructive, non-stateful, NOT on the never-auto floor. The unit is
	// config-not-code, supplied structurally; the argv is [systemctl, restart, <unit>], built here and NOWHERE
	// else. Strict unit-token validation + the operator allowed-units allowlist are the effect leaf's runtime
	// controls (ssh/mutate.go); this builder screens only presence.
	"restart-service": func(params map[string]string) ([]string, error) {
		unit := strings.TrimSpace(params[ParamUnit])
		if unit == "" {
			return nil, fmt.Errorf("op-class %q: missing required param %q (%s) — %s; e.g. %q",
				"restart-service", ParamUnit, "string", "the systemd unit to restart", "nginx")
		}
		return []string{"systemctl", "restart", unit}, nil
	},
	// restart-container — reversible (the container reconverges to its running state), non-destructive,
	// non-stateful, NOT on the floor; the predecessor's plurality remediation. Argv [docker, restart,
	// <container>]. Strict name-shape + the operator allowed-containers allowlist are the effect leaf's runtime
	// controls; this builder screens only presence.
	"restart-container": func(params map[string]string) ([]string, error) {
		container := strings.TrimSpace(params[ParamContainer])
		if container == "" {
			return nil, fmt.Errorf("op-class %q: missing required param %q (%s) — %s; e.g. %q",
				"restart-container", ParamContainer, "string", "the docker container to restart", "librespeed")
		}
		return []string{"docker", "restart", container}, nil
	},
	// reload-service — a `systemctl reload <unit>`: the MOST conservative service remediation (re-reads config
	// WITHOUT dropping connections/restarting), reversible, non-stateful, NOT on the floor. Shares the unit
	// param + argv shape family with restart-service; the effect leaf applies the SAME unit validation + allowlist.
	"reload-service": func(params map[string]string) ([]string, error) {
		unit := strings.TrimSpace(params[ParamUnit])
		if unit == "" {
			return nil, fmt.Errorf("op-class %q: missing required param %q (%s) — %s; e.g. %q",
				"reload-service", ParamUnit, "string", "the systemd unit to reload", "nginx")
		}
		return []string{"systemctl", "reload", unit}, nil
	},
	// start-service — a `systemctl start <unit>`: the conservative DOWN-service remediation (bring a stopped
	// service that should be running back up). Reversible (start↔stop), non-destructive, non-stateful, NOT on
	// the floor. The natural pair to restart-service: restart covers a hung/degraded running service, start
	// covers a stopped one. Shares the unit param + argv shape family; the ssh effect leaf (modules/actuation/
	// ssh/mutate.go) resolves it through the SAME validUnit + operator allowed-units allowlist as restart/reload
	// and pairs it with the `systemctl stop` rollback inverse; this builder screens only presence.
	"start-service": func(params map[string]string) ([]string, error) {
		unit := strings.TrimSpace(params[ParamUnit])
		if unit == "" {
			return nil, fmt.Errorf("op-class %q: missing required param %q (%s) — %s; e.g. %q",
				"start-service", ParamUnit, "string", "the systemd unit to start", "nginx")
		}
		return []string{"systemctl", "start", unit}, nil
	},
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
}

// registry is the immutable set of actuatable op-class schemas, keyed by NORMALIZED op_class slug: the LOADED
// schema (opschema.json) with its COMPILED builder attached. It is UNEXPORTED and reachable only through
// Lookup/Specs/Catalog (mirroring safety.neverAutoFloor) so no package can mutate a live schema during a
// canary — an exported map could be reassigned/deleted, silently redefining what an op-class actuates.
var registry = mustBuildRegistry(opschemaJSON, builders)

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
		if key == "" {
			panic("opschema: an op-class schema has a blank op_class (fail closed)")
		}
		if _, dup := m[key]; dup {
			panic(fmt.Sprintf("opschema: duplicate op-class %q in opschema.json (fail closed)", s.OpClass))
		}
		switch s.Kind() {
		case EffectSSHArgv, EffectProxmoxLifecycle:
			// An ARGV-encoded class REQUIRES a compiled builder — argv construction TOUCHES the estate (INV-02),
			// so it is code, never loaded. No builder ⇒ unactuatable ⇒ fail closed at init. (ssh-argv routes by
			// target; proxmox-lifecycle routes by kind to the proxmox lane — both build a fixed argv here.)
			b, ok := builders[key]
			if !ok || b == nil {
				panic(fmt.Sprintf("opschema: %s op-class %q has a schema but no compiled argv builder (fail closed — unactuatable)", s.Kind(), s.OpClass))
			}
			s.build = b
		case EffectAWXLaunch:
			// A launch-encoded class must have NO compiled argv builder — its effect is an AWX launch the runner
			// encodes (a LaunchSpec), NOT an argv. A builder here would be a second, contradictory effect path.
			if _, ok := builders[key]; ok {
				panic(fmt.Sprintf("opschema: awx-launch op-class %q must NOT have a compiled argv builder — the runner encodes its LaunchSpec (fail closed)", s.OpClass))
			}
			// s.build stays nil; the runner encodes this class's effect from its params (extra_vars).
		default:
			panic(fmt.Sprintf("opschema: op-class %q has an unknown effect_kind %q (want %q | %q | %q) — fail closed", s.OpClass, s.EffectKind, EffectSSHArgv, EffectAWXLaunch, EffectProxmoxLifecycle))
		}
		m[key] = s
	}
	// Every compiled builder must back an ARGV-encoded schema: a builder with no schema is unreachable, and a
	// builder keyed to a launch-encoded class is a contradiction (that class encodes its effect elsewhere). Fail closed.
	for key := range builders {
		s, ok := m[key]
		if !ok {
			panic(fmt.Sprintf("opschema: compiled builder %q has no schema in opschema.json (unreachable — fail closed)", key))
		}
		if !argvEncoded(s.Kind()) {
			panic(fmt.Sprintf("opschema: compiled builder %q backs a %q op-class, which is not argv-encoded (contradiction — fail closed)", key, s.Kind()))
		}
	}
	return m
}

// normalize trims and lowercases an op_class slug before a lookup — mirroring safety.IsNeverAuto — so a case
// or whitespace variant can never dodge (or false-miss) the registry.
func normalize(opClass string) string { return strings.ToLower(strings.TrimSpace(opClass)) }

// Lookup returns the schema for opClass by an EXACT normalized-slug match. ok=false ⇒ the op_class is not a
// registered actuatable class; callers MUST fail closed (an unregistered op_class has no argv builder, so it
// can never produce an execution argv). This is a data lookup, not a model-token-driven branch (INV-08).
func Lookup(opClass string) (OpClassSpec, bool) {
	s, ok := registry[normalize(opClass)]
	return s, ok
}

// Specs returns the registered schemas sorted by op_class slug (for docs, the prompt catalog, and tests). A
// COPY of the slice — the caller cannot mutate the registry.
func Specs() []OpClassSpec {
	out := make([]OpClassSpec, 0, len(registry))
	for _, s := range registry {
		out = append(out, s)
	}
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
		return nil, fmt.Errorf("op-class %q: no argv builder declared", s.OpClass)
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
	var b strings.Builder
	for _, s := range specs {
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
