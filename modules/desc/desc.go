// Package desc is the DECLARATIVE configuration schema a module publishes about itself.
//
// WHY IT EXISTS. TG has 42 module packages across 7 surfaces. The operator-facing requirement is a
// configuration dialog per module — set it up, Save, Test. Hand-writing 42 dialogs guarantees drift the
// first time a module gains a field: the form and the binary disagree, and the disagreement is invisible
// because nothing compares them.
//
// So a module DECLARES its fields and the console GENERATES the dialog. The descriptor is the single
// place that knows a field exists, which env key it maps to, whether it is a secret, and whether changing
// it takes effect without a restart.
//
// WHAT THIS PACKAGE MAY NOT DO. It is a stdlib-only leaf: it imports nothing from TG. A module already
// imports its own adapters; making the descriptor pull in config, wiring or httpapi would create import
// cycles the moment a core package wanted to read a descriptor. Values live nowhere near here — a
// descriptor describes SHAPE, never content, and in particular never a secret's value.
package desc

import (
	"fmt"
	"regexp"
	"strings"
)

// FieldType is how the console renders and validates one field.
type FieldType string

const (
	TypeText FieldType = "text"
	TypeURL  FieldType = "url"
	// TypeKVMap is "name=value, name=value" (e.g. routed rooms).
	TypeKVMap FieldType = "kvmap"
	// TypeIDList is a comma-separated identity list (e.g. matrix approver mxids).
	TypeIDList FieldType = "idlist"
	// TypeSecretRef is the POINTER to a secret (bao:...#key). Rendered read-only with provenance: the
	// operator sets the secret VALUE, never the pointer. Letting a UI rewrite the pointer would move the
	// secret out from under the boot secret-policy gate, which is the control that refuses plaintext.
	TypeSecretRef FieldType = "secret_ref"
	// TypeSecretValue is the secret ITSELF. Write-only by construction: it has its own lane to the secret
	// backend, is never returned by any read, and never appears in a descriptor's default.
	TypeSecretValue FieldType = "secret_value"
	TypeBool        FieldType = "bool"
	TypeDuration    FieldType = "duration"
)

// SecClass marks how much damage a wrong value does, so the console can treat fields differently.
type SecClass string

const (
	// SecOrdinary — a wrong value degrades the module.
	SecOrdinary SecClass = "ordinary"
	// SecAuthority — the field decides WHO or WHAT is trusted. The matrix approver set is the example:
	// it is the list of identities whose vote can release a governed action (INV-12). A change here is a
	// change to the trust boundary and must be ledgered and visibly marked, never rendered as an ordinary
	// text box.
	SecAuthority SecClass = "authority"
	// SecSecret — the value is a credential.
	SecSecret SecClass = "secret"
)

// Effect says whether a saved change is observable without a restart. A dialog that cannot say this
// truthfully produces a Save button that silently does nothing until the next deploy.
type Effect string

const (
	// EffectLive — the module re-reads this per use, so a save takes effect on the next call.
	EffectLive Effect = "live"
	// EffectRestart — read once at boot; the save is durable but inert until the worker restarts. The
	// console MUST say so rather than implying success.
	EffectRestart Effect = "restart"
	// EffectReadOnly — displayed for provenance, not settable here.
	EffectReadOnly Effect = "readonly"
)

// Field is one configurable input.
type Field struct {
	Name     string // stable identifier within the module
	EnvKey   string // the TG_* env key the binary reads; "" for a field with no env form
	Label    string // human-facing
	Help     string // what it does
	Type     FieldType
	Security SecClass
	Effect   Effect
	Required bool
	// Pattern optionally constrains each value (per ENTRY for list/map types, not the whole string).
	Pattern string
	// MaxItems bounds a list/map; MaxLen bounds one entry.
	MaxItems, MaxLen int
}

// ModuleSecretPrefix is the ONE namespace every module's secret lives under.
//
// It is a dedicated prefix, separate from TG's own operational secrets, and that separation is what lets
// the console's writer be scoped ONCE rather than per module. A first version of this enumerated each
// module's path in the OpenBao policy, which meant the configuration dialog worked for exactly the
// modules somebody had remembered to add to an HCL file and returned 403 for every other one. A Save
// button that fails because an operator has not hand-edited a policy is the defect this whole surface
// exists to remove.
const ModuleSecretPrefix = "secret/data/tg/modules/"

// SecretLane is where a module's ONE secret is written.
//
// KVPath is DERIVED from the module identity (see ModuleSecretPath), never hand-written, so a new module
// cannot land outside the writer's reach or inside TG's own secret namespace. Validate refuses any lane
// that is not under ModuleSecretPrefix.
//
// The module's *_TOKEN_REF must point HERE. That pointer is read from the environment at boot and nothing
// can rewrite it at runtime, so adopting this prefix is a one-time per-module change to the reference —
// after which every rotation is a Save, with no OpenBao or .env work at all.
type SecretLane struct {
	KVPath string // e.g. "secret/data/tg/modules/notifier/matrix"
	Field  string // e.g. "token"
}

// ModuleSecretPath derives the lane path for a module. Deriving rather than declaring is the point: a
// descriptor cannot name a path, so it cannot reach outside the prefix the writer is scoped to.
func ModuleSecretPath(surface, sourceType string) string {
	return ModuleSecretPrefix + strings.ToLower(strings.TrimSpace(surface)) + "/" +
		strings.ToLower(strings.TrimSpace(sourceType))
}

// TestSpec describes what pressing Test does.
type TestSpec struct {
	// Verb is what the operator is told will happen ("post a test message to the approvals room").
	//
	// EMPTY MEANS NO PROBE EXISTS. The console then disables the button and says so, and a guard enforces
	// the biconditional: a module declares a verb if and only if it implements core/selftest.Tester.
	Verb string
	// Mutating must be false. A Test that changes the estate is not a test; it is an unreviewed action
	// triggered from a settings dialog. Kept as an explicit field so the guard can assert it.
	Mutating bool
	// Emits marks a probe that OTHER PEOPLE CAN SEE — one whose evidence is an outward artefact rather
	// than a read. The notifiers are the whole category: delivery is the only proof that delivery works,
	// so their probe posts a real (marked, attributed, non-ballot) message into an operations room.
	//
	// IT EXISTS SO AN AUTOMATED SWEEP CANNOT BECOME A PAGER. A scheduled run of every probe is the point
	// of having probes — the 2026-08-02 syslog-ng breakage silently cost two sites their device logs and
	// surfaced only because somebody pressed a button by hand. But a sweep that ran the notifier probe on
	// a timer would post into the approvals room on every tick, which is not monitoring, it is noise
	// aimed at the exact room that must stay readable during an incident.
	//
	// So a sweep skips emitters and says how many it skipped; a human pressing TEST runs everything. The
	// distinction is DECLARED rather than inferred from the surface name, because "notifier" is a
	// property of today's fleet and "emits when probed" is the property that actually matters.
	Emits bool
}

// Descriptor is one module's published configuration schema.
type Descriptor struct {
	Surface    string // "notifier", "tracker", ...
	SourceType string // "matrix", "youtrack", ...
	Title      string
	Summary    string
	Fields     []Field
	Secret     SecretLane
	Test       TestSpec
}

var (
	envKeyRe = regexp.MustCompile(`^TG_[A-Z0-9_]+$`)
	// kvPathRe pins the secret path shape so a descriptor cannot point the writer at an arbitrary
	// location — including one outside the module prefix the console's credential is scoped to.
	kvPathRe = regexp.MustCompile(`^[a-z0-9_-]+/data/[a-z0-9._/-]+$`)
)

// reservedPrefixes are control-plane config namespaces a MODULE descriptor may never claim. A module
// declaring a field under safety. or operator. would put a governance control behind a connector's
// settings dialog.
var reservedPrefixes = []string{"safety.", "gateway.", "session.", "operator.", "net.", "ingest."}

// Validate refuses a descriptor that would produce a dishonest or dangerous dialog. It is called by the
// catalog at init, so a malformed descriptor fails the build's tests rather than shipping a broken form.
func (d Descriptor) Validate() error {
	if strings.TrimSpace(d.Surface) == "" || strings.TrimSpace(d.SourceType) == "" {
		return fmt.Errorf("desc: surface and source_type are required")
	}
	if strings.TrimSpace(d.Title) == "" {
		return fmt.Errorf("desc: %s/%s has no title", d.Surface, d.SourceType)
	}
	if len(d.Fields) == 0 {
		return fmt.Errorf("desc: %s/%s declares no fields — a module with no configurable input needs no descriptor", d.Surface, d.SourceType)
	}
	if d.Test.Emits && strings.TrimSpace(d.Test.Verb) == "" {
		return fmt.Errorf("desc: %s/%s declares an EMITTING test with no verb — the operator would cause "+
			"something other people can see without being told what", d.Surface, d.SourceType)
	}
	if d.Test.Mutating {
		return fmt.Errorf("desc: %s/%s declares a MUTATING test — a Test that changes the estate is an "+
			"unreviewed action triggered from a settings dialog", d.Surface, d.SourceType)
	}
	secrets := 0
	seen := map[string]bool{}
	for _, f := range d.Fields {
		if strings.TrimSpace(f.Name) == "" {
			return fmt.Errorf("desc: %s/%s has an unnamed field", d.Surface, d.SourceType)
		}
		if seen[f.Name] {
			return fmt.Errorf("desc: %s/%s declares field %q twice", d.Surface, d.SourceType, f.Name)
		}
		seen[f.Name] = true
		if f.EnvKey != "" && !envKeyRe.MatchString(f.EnvKey) {
			return fmt.Errorf("desc: %s/%s field %q has a malformed env key %q", d.Surface, d.SourceType, f.Name, f.EnvKey)
		}
		for _, p := range reservedPrefixes {
			if strings.HasPrefix(strings.ToLower(f.Name), p) {
				return fmt.Errorf("desc: %s/%s field %q claims the reserved control-plane namespace %q",
					d.Surface, d.SourceType, f.Name, p)
			}
		}
		switch f.Type {
		case TypeSecretValue:
			secrets++
			if f.Effect == EffectReadOnly {
				return fmt.Errorf("desc: %s/%s secret field %q is read-only — it could never be set", d.Surface, d.SourceType, f.Name)
			}
			if f.Security != SecSecret {
				return fmt.Errorf("desc: %s/%s field %q is a secret VALUE but is not classified secret", d.Surface, d.SourceType, f.Name)
			}
		case TypeSecretRef:
			// The pointer is display-only. A settable pointer would let a dialog move the secret out from
			// under the boot secret-policy gate.
			if f.Effect != EffectReadOnly {
				return fmt.Errorf("desc: %s/%s secret REFERENCE %q must be read-only: the operator sets the "+
					"secret, not the pointer", d.Surface, d.SourceType, f.Name)
			}
		}
	}
	if secrets > 1 {
		return fmt.Errorf("desc: %s/%s declares %d secret values; the secret lane holds one", d.Surface, d.SourceType, secrets)
	}
	if secrets == 1 {
		if d.Secret.KVPath == "" || d.Secret.Field == "" {
			return fmt.Errorf("desc: %s/%s has a secret field but no secret lane (KVPath/Field)", d.Surface, d.SourceType)
		}
		if !kvPathRe.MatchString(d.Secret.KVPath) {
			return fmt.Errorf("desc: %s/%s secret lane path %q is malformed", d.Surface, d.SourceType, d.Secret.KVPath)
		}
		// THE LANE IS DERIVED, NOT DECLARED. A descriptor that could name its own path could point the
		// console's writer at another module's secret — or at TG's own — and the writer is necessarily
		// allowed to write somewhere. Refusing anything but the derived path makes that unreachable.
		if want := ModuleSecretPath(d.Surface, d.SourceType); d.Secret.KVPath != want {
			return fmt.Errorf("desc: %s/%s declares secret lane %q but the derived lane is %q — a module "+
				"may not name its own secret path", d.Surface, d.SourceType, d.Secret.KVPath, want)
		}
	}
	return nil
}

// EnvKeys returns every env key the descriptor claims, for the guard that proves each one is really read
// by the binary. A field wired to a key nothing reads is a control that does nothing.
func (d Descriptor) EnvKeys() []string {
	out := make([]string, 0, len(d.Fields))
	for _, f := range d.Fields {
		if f.EnvKey != "" {
			out = append(out, f.EnvKey)
		}
	}
	return out
}
