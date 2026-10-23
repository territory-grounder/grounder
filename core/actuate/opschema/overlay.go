package opschema

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/territory-grounder/grounder/core/safety"
)

// THE COMPOSED REGISTRY — two tamper domains, explicitly unequal (spec/028 REQ-2814/REQ-2815, ADR-0016).
//
// The EMBEDDED registry (opschema.json, go:embed + lockstep-hashed) is the strong domain: admitting a
// capability there is a code release, reviewed and hash-bound. The OVERLAY is the runtime-admitted domain:
// op-classes an operator ratified from an evidence dossier, anchored in the ONE hash-chained ledger.
//
// They compose at EXACTLY ONE seam — Lookup, Specs, Catalog consult embedded THEN overlay — and the
// embedded base ALWAYS WINS a slug collision. That single rule is what keeps spec/007's scope statement
// honest: the lockstep hash governs the embedded registry, the ledger hash governs the overlay, and no
// runtime write can shadow, weaken, or redefine a reviewed capability.
//
// WHY THE FAILURE DIRECTION IS ALWAYS "FEWER CAPABILITIES". Every overlay row carries entry_hash =
// SHA-256(canonical spec), mirrored into its opclass:ratify GovDecision so ROW CONTENT is chain-covered.
// The snapshot re-verifies each row at load and DROPS a mismatching row loudly. A tampered overlay row
// therefore REMOVES a capability instead of granting a forged one — the only direction a security control
// is allowed to fail in. A dropped class falls back to rung 0 (registry absence) where it seals to nil argv
// and every effect leaf refuses it.

// OverlayEntry is one ratified op-class as the composed registry consumes it: the operator-authored spec
// plus the hash the ledger attests. The loader recomputes the hash over Spec and drops the entry when it
// disagrees with Hash.
type OverlayEntry struct {
	Spec OpClassSpec
	Hash string
}

// overlaySnapshot is the atomically-swapped composed view. A nil snapshot means "embedded only", which is
// the correct day-zero state and the correct state after any load failure.
type overlaySnapshot struct {
	byKey map[string]OpClassSpec
	order []string
}

// overlayPtr holds the current snapshot. atomic so a refresh never tears a concurrent Lookup: readers take
// the whole snapshot by pointer, so a swap is invisible mid-read and no lock sits on the actuation path.
var overlayPtr atomic.Pointer[overlaySnapshot]

// SetOverlay atomically installs a new overlay snapshot, DROPPING every entry that fails validation or whose
// content hash disagrees with the hash the ledger attests. It returns the accepted count and the per-entry
// rejections so the caller can page on a mismatch — a silently shrinking capability set is still a change an
// operator must hear about, even though the failure direction is safe.
//
// Entries are rejected, never repaired. A row whose hash does not match is not "slightly wrong"; it is a row
// whose provenance no longer holds, and the only safe reading of unprovable provenance is absence.
func SetOverlay(entries []OverlayEntry) (accepted int, rejected []string) {
	snap := &overlaySnapshot{byKey: make(map[string]OpClassSpec, len(entries))}
	for _, e := range entries {
		key := normalize(e.Spec.OpClass)
		if key == "" {
			rejected = append(rejected, "overlay entry with a blank op_class — dropped")
			continue
		}
		// EMBEDDED ALWAYS WINS. An overlay row that shadows a reviewed class is dropped rather than merged:
		// a runtime write must never redefine what a code release established.
		if _, embedded := registry[key]; embedded {
			rejected = append(rejected, fmt.Sprintf("overlay op-class %q shadows an EMBEDDED class — dropped (embedded always wins)", e.Spec.OpClass))
			continue
		}
		// The hash is verified BEFORE the spec is trusted for anything else, including validation: validating
		// first would run admission logic over content whose provenance is already in doubt.
		want, err := CanonicalHash(e.Spec)
		if err != nil {
			rejected = append(rejected, fmt.Sprintf("overlay op-class %q: cannot canonicalize for hashing (%v) — dropped", e.Spec.OpClass, err))
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(e.Hash), want) {
			rejected = append(rejected, fmt.Sprintf("overlay op-class %q: entry_hash MISMATCH (ledger %q, computed %q) — dropped, capability withheld", e.Spec.OpClass, e.Hash, want))
			continue
		}
		// An overlay class is template-encoded by construction (no compiled builder can exist for a class the
		// binary has never heard of), so validation runs with hasCompiledBuilder=false.
		spec, err := ValidateSpec(e.Spec, false)
		if err != nil {
			rejected = append(rejected, fmt.Sprintf("overlay op-class %q: %v — dropped", e.Spec.OpClass, err))
			continue
		}
		if _, dup := snap.byKey[key]; dup {
			rejected = append(rejected, fmt.Sprintf("overlay op-class %q appears twice in one snapshot — dropped", e.Spec.OpClass))
			continue
		}
		snap.byKey[key] = spec
		snap.order = append(snap.order, key)
	}
	sort.Strings(snap.order)
	overlayPtr.Store(snap)
	return len(snap.byKey), rejected
}

// ClearOverlay drops every overlay class, returning the registry to embedded-only. Used at shutdown and by
// oracles; a revoke reaches the same end state through SetOverlay with the row absent.
func ClearOverlay() { overlayPtr.Store(&overlaySnapshot{byKey: map[string]OpClassSpec{}}) }

// overlayLookup resolves a normalized key against the current snapshot.
func overlayLookup(key string) (OpClassSpec, bool) {
	snap := overlayPtr.Load()
	if snap == nil {
		return OpClassSpec{}, false
	}
	s, ok := snap.byKey[key]
	return s, ok
}

// overlaySpecs returns the overlay classes in slug order (a copy), SKIPPING any slug the embedded registry
// defines — the same embedded-first rule Lookup applies, at the enumeration seam.
//
// SetOverlay already refuses a shadowing row, so this filter is the second of two independent guards, and it
// is here rather than only in Lookup because Specs feeds the agent's prompt Catalog: a shadowing row that
// slipped past admission would otherwise ADVERTISE a capability the reviewed registry never granted, even
// while Lookup correctly refused to resolve it. A guard that covers resolution but not enumeration leaves the
// model reasoning about tools it cannot actually be given.
func overlaySpecs() []OpClassSpec {
	snap := overlayPtr.Load()
	if snap == nil {
		return nil
	}
	out := make([]OpClassSpec, 0, len(snap.order))
	for _, k := range snap.order {
		if _, embedded := registry[k]; embedded {
			continue
		}
		out = append(out, snap.byKey[k])
	}
	return out
}

// IsEmbedded reports whether opClass is present in the EMBEDDED, lockstep-hashed registry — as distinct from
// the composed registry, which also contains runtime-ratified overlay classes.
//
// THIS IS THE AUTO CEILING (ADR-0016 decision 2). The graduation ladder consults this — not Lookup — before
// promoting a class to the SILENT rung. An overlay-only class caps at auto_notice ("acts and pages"); reaching
// auto requires the class to exist here, i.e. a code release via embed-export. The rung where no human watches
// always lives in the domain with the strongest guarantees, and this predicate is how that is enforced
// structurally rather than by convention.
func IsEmbedded(opClass string) bool {
	_, ok := registry[normalize(opClass)]
	return ok
}

// CanonicalHash returns SHA-256 over the canonical JSON encoding of a spec — the value stored as entry_hash
// and mirrored into the opclass:ratify GovDecision action_id, so the LEDGER attests row CONTENT and not
// merely the fact that a ratification occurred.
//
// Canonical means: Go's encoding/json over the exported spec fields, whose struct-tag order is fixed at
// compile time and whose map-free shape has no iteration-order ambiguity. Two processes hashing the same
// spec therefore agree without a canonicalization library.
func CanonicalHash(s OpClassSpec) (string, error) {
	s.OpClass = normalize(s.OpClass)
	s.Family = strings.ToLower(strings.TrimSpace(s.Family))
	s.SafetyTier = strings.ToLower(strings.TrimSpace(s.SafetyTier))
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// ValidateSpec is the ERROR-RETURNING form of the registry's own admission checks (REQ-2814).
//
// WHY THIS EXISTS. These checks were written as init-time panics: the embedded registry is compiled in, so a
// malformed entry is a build defect and dying at boot is right. Ratification changes the threat model — the
// spec now arrives at RUNTIME from an operator form, and a live worker that panics on operator input is a
// denial-of-service with an audit trail. Same rules, same messages, returned instead of thrown.
// mustBuildRegistry calls this and panics on error, so embedded behavior is preserved exactly.
//
// It returns the NORMALIZED spec (family and tier lowercased, slug normalized) so callers store the same
// shape the registry stores and a case variant can never dodge a later exact-match lookup.
func ValidateSpec(s OpClassSpec, hasCompiledBuilder bool) (OpClassSpec, error) {
	if normalize(s.OpClass) == "" {
		return s, fmt.Errorf("an op-class schema has a blank op_class (fail closed)")
	}
	s.OpClass = normalize(s.OpClass)
	// FAMILY + SAFETY TIER are CLOSED enumerations. An unrecognised family would silently open a NEW
	// graduation ladder nobody is watching; an unrecognised tier would leave a class with no band floor.
	s.Family = strings.ToLower(strings.TrimSpace(s.Family))
	s.SafetyTier = strings.ToLower(strings.TrimSpace(s.SafetyTier))
	if !knownFamilies[s.Family] {
		return s, fmt.Errorf("op-class %q declares family %q, which is not in the closed set — "+
			"an unknown family would create an unwatched graduation ladder (fail closed)", s.OpClass, s.Family)
	}
	if !knownTiers[s.SafetyTier] {
		return s, fmt.Errorf("op-class %q declares safety_tier %q, which is not in the closed set "+
			"[low-reversible medium irreversible vendor-critical] — a class with no tier has no band floor (fail closed)",
			s.OpClass, s.SafetyTier)
	}
	// STATE PRECONDITION is a CLOSED enumeration (TG-378). An unrecognised value would read as "declares
	// nothing" to a gate matching known values — a precondition that silently never fires.
	s.RequiresTargetState = strings.ToLower(strings.TrimSpace(s.RequiresTargetState))
	if s.RequiresTargetState != "" && s.RequiresTargetState != RequiresNotRunning && s.RequiresTargetState != RequiresRunning {
		return s, fmt.Errorf("op-class %q declares requires_target_state %q, which is not in the closed set "+
			"[\"\" %q %q] — an unknown precondition would silently never be enforced (fail closed)",
			s.OpClass, s.RequiresTargetState, RequiresNotRunning, RequiresRunning)
	}

	// COMMIT-CONFIRMED eligibility (spec/029 REQ-2904; sign-off TG-488 B5): declared eligibility
	// without a declared inverse — or with a window the verifier can outrun — is refused at load.
	// A class with NO CommitConfirmed declaration is simply not eligible; nothing arms (that is the
	// conservative floor, not an error).
	if s.CommitConfirmed != nil {
		hasRB, hasRBClass := len(s.RollbackTemplate) > 0, strings.TrimSpace(s.RollbackOpClass) != ""
		if hasRB == hasRBClass { // both, or neither
			return s, fmt.Errorf("op-class %q declares commit_confirmed but not EXACTLY ONE inverse "+
				"(rollback_template xor rollback_op_class) — an eligible class without a single "+
				"registry-defined inverse cannot self-revert (fail closed; REQ-2904)", s.OpClass)
		}
		w := s.CommitConfirmed.WindowSeconds
		if w < commitConfirmWindowFloorSeconds {
			return s, fmt.Errorf("op-class %q commit_confirmed window %ds is under the %ds floor — a "+
				"window shorter than two poll cycles reverts healthy changes the monitor simply had not "+
				"seen yet (fail closed; REQ-2904)", s.OpClass, w, commitConfirmWindowFloorSeconds)
		}
		if s.Kind() == EffectAWXLaunch && w <= AwxWindowFloorSeconds {
			return s, fmt.Errorf("op-class %q is awx-launched with a %ds commit_confirmed window — the "+
				"deferred verify may legitimately arrive as late as %ds, so this window would fire the "+
				"inverse against a change whose verdict is still in flight (fail closed; TG-488 B5)",
				s.OpClass, w, AwxWindowFloorSeconds)
		}
	} else if strings.TrimSpace(s.RollbackOpClass) != "" {
		return s, fmt.Errorf("op-class %q declares rollback_op_class without commit_confirmed — a "+
			"dangling inverse declaration is a shape error (fail closed)", s.OpClass)
	}

	switch s.Kind() {
	case EffectSSHArgv, EffectProxmoxLifecycle:
		hasTemplate := len(s.ArgvTemplate) > 0
		// EXACTLY ONE of {compiled builder, argv template}.
		if hasCompiledBuilder && hasTemplate {
			return s, fmt.Errorf("op-class %q declares BOTH a compiled argv builder and an argv_template "+
				"— two contradictory definitions of what it actuates (fail closed)", s.OpClass)
		}
		if !hasCompiledBuilder && !hasTemplate {
			return s, fmt.Errorf("%s op-class %q has a schema but neither a compiled argv builder nor an "+
				"argv_template (fail closed — unactuatable)", s.Kind(), s.OpClass)
		}
		// THE PROGRAM IS NEVER A SLOT (INV-02). argv[0] is the executable; a substituted value there is
		// INV-02's central prohibition wearing a data costume.
		for _, t := range [][]string{s.ArgvTemplate, s.RollbackTemplate} {
			if len(t) > 0 && templateSlotRE.FindStringSubmatch(t[0]) != nil {
				return s, fmt.Errorf("op-class %q template starts with the slot %q — argv[0] is the "+
					"PROGRAM and must be an operator-authored literal, never a substituted value (fail "+
					"closed; INV-02)", s.OpClass, t[0])
			}
		}
		for _, el := range append(append([]string{}, s.ArgvTemplate...), s.RollbackTemplate...) {
			m := templateSlotRE.FindStringSubmatch(el)
			if m == nil {
				// An element is EITHER a literal OR a whole-element slot — never a mix. "--unit=${unit}"
				// would pass through as a literal and put the characters "${unit}" on the wire: a silent,
				// permissive failure that reaches the estate as a wrong command.
				if strings.Contains(el, "${") {
					return s, fmt.Errorf("op-class %q argv_template element %q embeds a slot inside a "+
						"larger string — an element is either a literal or a WHOLE-element %s slot, never a "+
						"mix (fail closed; write it as two elements, e.g. [\"--unit\", \"${unit}\"])",
						s.OpClass, el, "${param}")
				}
				continue
			}
			ps, found := s.paramSpec(m[1])
			if !found {
				return s, fmt.Errorf("op-class %q argv_template references undeclared param %q "+
					"(fail closed — the slot could never be filled)", s.OpClass, m[1])
			}
			if !ps.Required {
				return s, fmt.Errorf("op-class %q argv_template slot %q is not a REQUIRED param — an "+
					"absent value would render a DIFFERENT command than the template reads as (fail closed)",
					s.OpClass, m[1])
			}
		}
	case EffectAWXLaunch:
		if hasCompiledBuilder {
			return s, fmt.Errorf("awx-launch op-class %q must NOT have a compiled argv builder — the runner encodes its LaunchSpec (fail closed)", s.OpClass)
		}
	case EffectK8sDeclarative:
		// Launch-encoded like awx-launch: the runner translates the op-class into a gitops-mr ProposeSpec, so a
		// compiled argv builder here would be a second, contradictory definition of what it actuates.
		if hasCompiledBuilder {
			return s, fmt.Errorf("k8s-declarative op-class %q must NOT have a compiled argv builder — the runner encodes its gitops-mr ProposeSpec (fail closed)", s.OpClass)
		}
	default:
		return s, fmt.Errorf("op-class %q has an unknown effect_kind %q (want %q | %q | %q | %q) — fail closed",
			s.OpClass, s.EffectKind, EffectSSHArgv, EffectAWXLaunch, EffectProxmoxLifecycle, EffectK8sDeclarative)
	}
	return s, nil
}

// ValidateRatification is the FULL admission gate for an operator-authored template (REQ-2814): every
// embedded-registry check, PLUS the three that exist only because this spec arrived at runtime.
//
// modelText is every screened model string associated with the candidate's occurrences — the verbs,
// rationales and undo sketches the AGENT wrote. It is passed in so the tripwire can refuse them.
//
// THE LAUNDERING TRIPWIRE is the one worth explaining. TG's constitutional line is that a model never writes
// its own tools (ARCHITECTURE §T3): the executed vector is always operator-typed. But the operator ratifies
// while LOOKING at the model's suggested text, and a copy-paste would launder model output into an executed
// argv while every other check still passed — the form was filled by a human, so authorship looks satisfied.
// Refusing any element that BYTE-MATCHES model text closes that path structurally. The operator may express
// the same intent; they may not transcribe the model's string.
func ValidateRatification(s OpClassSpec, modelText []string) (OpClassSpec, error) {
	key := normalize(s.OpClass)
	if key == "" {
		return s, fmt.Errorf("an op-class schema has a blank op_class (fail closed)")
	}
	// OVERLAY NEVER SHADOWS EMBEDDED — refused at admission as well as at load, so a shadowing row can never
	// be written in the first place (the load-time drop is the backstop, not the only guard).
	if IsEmbedded(key) {
		return s, fmt.Errorf("op-class %q is already EMBEDDED in the reviewed registry — an overlay row may "+
			"never shadow or redefine a code-released capability (fail closed; ADR-0016)", s.OpClass)
	}
	spec, err := ValidateSpec(s, false)
	if err != nil {
		return s, err
	}
	// TIER VS DESTRUCTIVENESS. The tier is a CLAIM about the op; IsDestructiveOp/IsNeverAuto are the
	// server's own reading of it. A claimed low-reversible tier on a destructive verb would buy a faster
	// ladder climb for the most dangerous class of action — so a contradiction is refused rather than
	// resolved in the claim's favour.
	if AutoEligible(spec.SafetyTier) && destructiveOpClass(spec.OpClass) {
		return s, fmt.Errorf("op-class %q claims the auto-eligible tier %q but the server reads it as "+
			"destructive/never-auto — a tier claim may never soften what the op actually does (fail closed)",
			spec.OpClass, spec.SafetyTier)
	}
	// THE LAUNDERING TRIPWIRE.
	for _, el := range append(append([]string{}, spec.ArgvTemplate...), spec.RollbackTemplate...) {
		trimmed := strings.TrimSpace(el)
		if trimmed == "" {
			continue
		}
		for _, mt := range modelText {
			if strings.TrimSpace(mt) == "" {
				continue
			}
			if trimmed == strings.TrimSpace(mt) {
				return s, fmt.Errorf("template element %q byte-matches model-suggested text — the executed "+
					"vector must be operator-AUTHORED, never transcribed from the model (fail closed; the "+
					"laundering tripwire, ADR-0016 decision 3)", el)
			}
		}
	}
	return spec, nil
}

// destructiveOpClass is the server's own reading of whether a slug names a destructive/never-auto operation,
// independent of the tier the ratification CLAIMS. A package var so an oracle can prove the contradiction
// refusal without shipping a destructive class in the test registry.
var destructiveOpClass = func(opClass string) bool {
	return safety.IsNeverAuto(opClass) || safety.IsDestructiveOp(opClass)
}
