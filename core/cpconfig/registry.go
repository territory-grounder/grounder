// Package cpconfig is the control-plane configuration registry + layered resolver (task #27, Phase A).
//
// It answers "what is TG's configuration, and where does each knob come from?" as a READ surface. The
// registry is the compiled single source of truth for which knobs exist, which are LAW (pinned, never
// overridable) and which are legitimately operator-tunable (console_writable). The resolver layers the
// sources — LAW (compiled) over env (boot) over console overrides — and clamps: a console/env override can
// NEVER reach a LAW key. Phase A is read-only: no console overrides are wired (the Console store is nil),
// there is no write path, and NO secret VALUE is ever emitted (secrets keep their own /v1/secrets surface).
//
// Provenance: task #27 design brief — control-plane self-config kept strictly disjoint from estate mutation;
// [O] INV-01 (every route authenticated), INV-15 (the surface reflects the components' authoritative state,
// never a fabricated value), single-org (ADR-0010, one config namespace).
package cpconfig

// Source is where a resolved value came from — rendered as a badge in the console.
type Source string

const (
	SourceLaw     Source = "law"     // compiled, pinned by law — never overridable
	SourceEnv     Source = "env"     // supplied at boot via env/file
	SourceConsole Source = "console" // an operator override written through the console (Phase B; none in Phase A)
	SourceDefault Source = "default" // the compiled default (no env/console value supplied)
)

// Key is one registered configuration knob. Law keys are pinned by the mechanical safety core and can
// never be overridden by env or console; ConsoleWritable marks a non-law key an operator may tune through
// the console (Phase B enables the write path — Phase A only reports the flag).
type Key struct {
	Name            string
	Description     string
	Law             bool
	ConsoleWritable bool

	// SHAPE — the module descriptor's own declared constraints, pushed in with the key (TG-262).
	//
	// They exist here so the WRITE path can refuse a value the field forbids. Until TG-260 nothing read
	// desc.Field's Pattern/MaxLen/MaxItems at all; TG-260 made the worker enforce them at RESOLVE time,
	// which left the console returning 200 for a value the worker would later refuse — the operator saw a
	// saved setting that was not in effect, and only the boot log said so. Checking here puts the refusal
	// where the operator is, in the dialog, with the field's own words.
	//
	// Empty/zero means unconstrained, which is what every compiled control-plane key is: only module keys
	// carry a descriptor, and they arrive through SetModuleKeys.
	Type     string // desc.FieldType: url | duration | bool | idlist | kvmap | text
	Pattern  string // per-ENTRY for list/map types, whole-value otherwise
	MaxLen   int
	MaxItems int
	// Help is the field's own explanation, echoed in a refusal so the operator is told what IS allowed
	// rather than only that they were wrong.
	Help string
}

// Registry is the compiled single source of truth for the configuration knobs. Order is stable (LAW first,
// then operational) so the console renders deterministically.
func Registry() []Key {
	return append(compiledRegistry(), ModuleKeys()...)
}

// compiledRegistry is the control-plane registry: the knobs the platform itself owns. Module keys are
// appended by Registry() from what the composition root installed (see modulekeys.go), so a connector's
// settings can never be confused with a law-pinned safety control.
func compiledRegistry() []Key {
	return []Key{
		// LAW — the mechanical safety core. Pinned; never console-writable, never env-overridable.
		{Name: "safety.never_auto_floor", Law: true, ConsoleWritable: false,
			Description: "irreversible / stateful / destructive / jailbreak ⇒ POLL_PAUSE — pinned by law"},
		{Name: "safety.may_actuate", Law: true, ConsoleWritable: false,
			Description: "derived actuation permission — mode ∈ {Semi-auto, Full-auto} AND preflight green AND breaker closed; reported, never a writable switch (TG-112)"},
		{Name: "safety.predict_then_verify", Law: true, ConsoleWritable: false,
			Description: "every action commits a prediction and is mechanically verified — required"},

		// Operational — env-supplied today; some legitimately operator-tunable (Phase B write path).
		{Name: "gateway.litellm_url", Law: false, ConsoleWritable: true,
			Description: "LiteLLM gateway base URL"},
		{Name: "session.ttl", Law: false, ConsoleWritable: true,
			Description: "browser operator session lifetime"},
		{Name: "session.admin_ttl", Law: false, ConsoleWritable: true,
			Description: "admin elevation lifetime (step-up TTL, task #27 Phase B)"},
		{Name: "operator.name", Law: false, ConsoleWritable: false,
			Description: "the Phase-1 operator account name (identity)"},
		{Name: "operator.admin_name", Law: false, ConsoleWritable: false,
			Description: "the admin step-up account name (identity; credential by reference only)"},
		{Name: "net.public_addr", Law: false, ConsoleWritable: false,
			Description: "public API bind address (boot only)"},
		{Name: "net.admin_addr", Law: false, ConsoleWritable: false,
			Description: "admin API bind address (boot only)"},
		{Name: "ingest.librenms_deployments", Law: false, ConsoleWritable: true,
			Description: "configured LibreNMS ingest deployments (site|url|tokenref)"},
	}
}
