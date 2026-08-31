// This file adds the SSH module's Phase-2 MUTATING path (spec/008 REQ-822, task #21): a genuine,
// argv-only, registry-gated mutating execution path that is STRUCTURALLY UNREACHABLE while the process
// mutation gate is off. The reversible-op allowlist resolves only a capability-declared reversible op_class
// (the canary `restart-service`) on an operator-declared allowed unit; the non-configurable never-auto
// floor is wired so a stateful/irreversible class can never resolve even if it was allowlisted by mistake
// (INV-09). Mutation ships OFF: with the gate disabled — the whole of Phase 0/1 — ReadOnly() is true and
// every mutating entry refuses before touching the runner. Turning the gate on is task #23's key; this file
// only builds the wiring.
//
// Provenance: [O] INV-02 (no shell; fixed argv, POSIX-quoted), INV-07 (rollback bound to action_id),
// INV-09 (never-auto floor + fail-closed gate), INV-17/INV-21, spec/008.
package ssh

import (
	"context"
	"errors"
	"regexp"
	"strings"

	actuation "github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/safety"
)

// OpClassRestartService is the canary reversible op_class this module can mutate with once the mutation gate
// is turned on (#23): a `systemctl restart <unit>` of an allowlisted unit. It is deliberately the one
// conservative-remediation class the predecessor auto-granted — reversible, non-destructive, non-stateful —
// and it is NOT on the never-auto floor.
const OpClassRestartService = "restart-service"

// OpClassRestartContainer is the second reversible op_class this module can mutate with: a
// `docker restart <container>` of an allowlisted container. Like restart-service it is a conservative,
// reversible, non-destructive, non-stateful remediation the predecessor auto-granted (the plurality of its
// real remediations), and it is NOT on the never-auto floor. A stateful container (a DB/queue/store) is still
// floored by safety.IsStatefulWorkload, and only an operator-allowlisted container name can resolve.
const OpClassRestartContainer = "restart-container"

// OpClassReloadService is the third reversible op_class: a `systemctl reload <unit>` — the MOST conservative
// service remediation (re-reads config without dropping connections or restarting the process). It shares the
// systemd-unit vocabulary with restart-service (same validUnit + allowed-units allowlist), is non-disruptive
// and reversible, and is NOT on the never-auto floor.
const OpClassReloadService = "reload-service"

// OpClassStartService is the fourth reversible op_class: a `systemctl start <unit>` — the conservative
// DOWN-service remediation (bring a stopped service that should be running back up), the natural pair to
// restart-service (which covers a hung RUNNING service). It shares the systemd-unit vocabulary with
// restart/reload-service (same validUnit + allowed-units allowlist), is reversible (start↔stop) and
// non-stateful, and is NOT on the never-auto floor. Its INV-07 compensating inverse is a `systemctl stop`
// (NOT a re-run of the forward start argv — see resolveOp).
const OpClassStartService = "start-service"

// OpClassStartContainer is the fifth reversible op_class: a `docker start <container>` — the conservative
// DOWN-container remediation, and the natural pair to restart-container exactly as start-service pairs with
// restart-service: restart covers a running-but-wedged container, start covers a STOPPED one. Reversible
// (start↔stop), so the registry declares its compensating action explicitly as `docker stop` rather than
// letting it default to a re-run.
//
// It was the first verb added as PURE REGISTRY DATA — no compiled builder, no case in resolveOp — which is
// also how it exposed that the leaf's resolvable set was a hand-maintained literal that a data-only verb
// could never join.
const OpClassStartContainer = "start-container"

var (
	// ErrNoExecutionPath is returned for an op_class not on this module's reversible allowlist, or an argv
	// that is not a recognized canonical mutating command (INV-17 at the effect leaf).
	ErrNoExecutionPath = errors.New("ssh: op_class has no reversible execution path (not allowlisted)")
	// ErrNeverAutoFloor is returned for an op_class on the non-configurable never-auto floor — it can never
	// resolve to a mutating command even if it was mistakenly allowlisted (INV-09).
	ErrNeverAutoFloor = errors.New("ssh: op_class is on the non-configurable never-auto floor and can never auto-execute")
	// ErrStatefulWorkload is returned for a unit naming a stateful workload (DB/queue/store) whose restart
	// risks data or quorum loss — a hard floor here regardless of the allowlist.
	ErrStatefulWorkload = errors.New("ssh: target names a stateful workload — a mutating restart is refused (data/quorum loss risk)")
	// ErrUnitNotAllowed is returned for a systemd unit not on the operator-declared allowed-units allowlist
	// (config-not-code) — a mutating restart is NEVER of an arbitrary unit.
	ErrUnitNotAllowed = errors.New("ssh: target unit is not on the allowed-units allowlist")
	// ErrInvalidUnit is returned for a unit whose token carries a space, newline, slash, or shell
	// metacharacter — it can never be a real systemd unit and must never reach transport.
	ErrInvalidUnit = errors.New("ssh: target unit is not a valid systemd unit token")
	// ErrContainerNotAllowed is returned for a docker container not on the operator-declared
	// allowed-containers allowlist (config-not-code) — a mutating restart is NEVER of an arbitrary container.
	ErrContainerNotAllowed = errors.New("ssh: target container is not on the allowed-containers allowlist")
	// ErrInvalidContainer is returned for a container name carrying a space, newline, slash, or shell
	// metacharacter — it can never be a real docker container name and must never reach transport.
	ErrInvalidContainer = errors.New("ssh: target container is not a valid docker container name")
)

// Option configures a Module.
type Option func(*Module)

// WithMutation gives the module its Phase-2 mutating configuration: the process mutation gate it reads to
// decide ReadOnly()/GuardMutation, and the operator-declared allowed-units + allowed-containers allowlists
// (config-not-code). The reversible op_class set is DERIVED FROM THE REGISTRY (see reversibleFromRegistry),
// not listed here; a stateful or irreversible class can never join it because resolveOp floors it via
// safety.NeverAutoFloor and safety.IsStatefulWorkload, and an EMPTY allowlist refuses every target. Passing
// this does NOT turn mutation on — the gate stays the sole key.
func WithMutation(gate *safety.Chokepoint, allowedUnits, allowedContainers []string) Option {
	return func(m *Module) {
		m.gate = gate
		m.reversible = reversibleFromRegistry()
		m.allowedUnits = map[string]bool{}
		for _, u := range allowedUnits {
			if u = strings.TrimSpace(u); u != "" {
				m.allowedUnits[u] = true
			}
		}
		m.allowedContainers = map[string]bool{}
		for _, c := range allowedContainers {
			if c = strings.TrimSpace(c); c != "" {
				m.allowedContainers[c] = true
			}
		}
	}
}

// hasReversiblePath reports whether the module carries at least one reversible op_class that is NOT on the
// never-auto floor — a genuine mutating execution path. Used by ReadOnly().
func (m *Module) hasReversiblePath() bool {
	for oc := range m.reversible {
		if !safety.IsNeverAuto(oc) {
			return true
		}
	}
	return false
}

// unitRe is the strict systemd-unit token shape: alphanumerics plus the systemd-legal `@:._-` and nothing
// else. NO space, slash, newline, or shell metacharacter can match, so an injected unit is rejected before
// it ever reaches transport (belt to sshArgv's POSIX-quoting suspenders, INV-02).
var unitRe = regexp.MustCompile(`^[A-Za-z0-9@:._-]{1,128}$`)

// validUnit reports whether unit is a syntactically valid systemd unit token (no injection surface).
func validUnit(unit string) bool { return unitRe.MatchString(unit) }

// containerRe is the strict docker container-name shape: a leading alphanumeric then alphanumerics plus the
// docker-legal `_.-`, and nothing else. NO space, slash, colon, newline, or shell metacharacter can match, so
// an injected name is rejected before it ever reaches transport (belt to sshArgv's POSIX-quoting suspenders).
var containerRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// validContainer reports whether name is a syntactically valid docker container name (no injection surface).
func validContainer(name string) bool { return containerRe.MatchString(name) }

// resolveOp resolves a reversible op_class + target unit into the fixed COMMAND argv and its rollback argv,
// enforcing the reversible-op allowlist in fail-closed order:
//  1. the never-auto floor — an irreversible/floor class never resolves, even if mis-allowlisted (INV-09);
//  2. a stateful workload — a DB/queue/store restart risks data/quorum loss, a hard floor here;
//  3. the op_class must be on this module's capability-declared reversible allowlist;
//  4. the unit must be a valid token AND on the operator-declared allowed-units allowlist (never arbitrary).
//
// The COMMAND argv is a fixed vector (no shell, no string building). A restart/reload's compensating action is
// a re-run (idempotent reconvergence to the known-good running state); a start's compensating action is the
// inverse verb `systemctl stop` (a start is NOT its own inverse). INV-07 requires a bound rollback, not a
// perfect one.
// reversibleFromRegistry derives the leaf's resolvable set from the OP-CLASS REGISTRY rather than from a
// literal map here. The literal map was the "implemented but not reachable" trap in its purest form: a verb
// could be registered, validated, classifiable, prompt-rendered and holding a graduation row, and still refuse
// at the leaf with "not allowlisted" — measured the day this changed, on a verb added as a pure JSON block.
// Nothing failed; the verb was simply inert.
//
// Membership is NOT a widening of authority. A class here still traverses, unchanged: the never-auto floor,
// the stateful-workload clamp, strict unit/container token validation, the operator's allowed-units /
// allowed-containers allowlist (an EMPTY allowlist refuses every target), the mode chokepoint, and the target
// host's own argv allowlist. What it stops doing is refusing for the one reason that carried no information —
// that a second list had not been updated.
//
// A class whose FAMILY this leaf has no vocabulary for is deliberately excluded: there is no way to know
// whether its slot is a unit, a container or something else, so it must not resolve. That exclusion is
// asserted, not assumed — see TestEveryRegisteredSSHClassIsReachableOrDeclared.
func reversibleFromRegistry() map[string]bool {
	out := map[string]bool{}
	for _, s := range opschema.Specs() {
		if s.Kind() != opschema.EffectSSHArgv || len(s.ArgvTemplate) == 0 {
			continue
		}
		switch s.Family {
		case opschema.FamilyServiceLifecycle, opschema.FamilyContainerLifecycle:
			out[s.OpClass] = true
		}
	}
	return out
}

func (m *Module) resolveOp(opClass, unit string) (cmd, rollback []string, err error) {
	oc := strings.ToLower(strings.TrimSpace(opClass)) // normalize so a case/whitespace variant cannot dodge a check
	if safety.IsNeverAuto(oc) {
		return nil, nil, ErrNeverAutoFloor
	}
	if safety.IsStatefulWorkload(unit, oc) {
		return nil, nil, ErrStatefulWorkload
	}
	if !m.reversible[oc] {
		return nil, nil, ErrNoExecutionPath
	}
	spec, known := opschema.Lookup(oc)
	if !known {
		return nil, nil, ErrNoExecutionPath
	}

	// The leaf's runtime controls are keyed on the class's FAMILY, not on its name. That is what makes a new
	// verb pure data: `start-container` needs no case here because it is container-lifecycle, and this leaf
	// already knows that a container-lifecycle slot is a container name gated by the allowed-containers
	// allowlist. A family this leaf has NO vocabulary for must never resolve — there would be no way to know
	// what its slot even is, let alone which allowlist gates it.
	var param string
	switch spec.Family {
	case opschema.FamilyServiceLifecycle:
		// Strict unit-token validation THEN the operator allowed-units allowlist — the leaf's stricter runtime
		// controls, applied BEFORE the shared shape the registry builds.
		if !validUnit(unit) {
			return nil, nil, ErrInvalidUnit
		}
		if !m.allowedUnits[unit] {
			return nil, nil, ErrUnitNotAllowed
		}
		param = opschema.ParamUnit
	case opschema.FamilyContainerLifecycle:
		// The same fail-closed controls in the container vocabulary: strict name shape THEN the operator
		// allowed-containers allowlist (never an arbitrary container). The never-auto and stateful floors were
		// already enforced above, keyed on this same name.
		if !validContainer(unit) {
			return nil, nil, ErrInvalidContainer
		}
		if !m.allowedContainers[unit] {
			return nil, nil, ErrContainerNotAllowed
		}
		param = opschema.ParamContainer
	default:
		return nil, nil, ErrNoExecutionPath
	}

	// Build the FORWARD argv from the shared op-class SCHEMA REGISTRY — the ONE place each shape is declared,
	// shared with the runner's sealedArgv and the interceptor's structure gate, so this leaf can never define a
	// second, drifting argv. The value has already passed validation + the allowlist above, so the build cannot
	// fail here; a defensive error still maps fail closed.
	args := map[string]string{param: unit}
	fwd, berr := spec.Argv(args)
	if berr != nil {
		return nil, nil, invalidFor(spec.Family)
	}
	// INV-07 requires a BOUND rollback, not a perfect one. The registry declares it: absent means a re-run
	// (idempotent reconvergence, correct for restart/reload), and a class whose forward is NOT its own inverse
	// declares the inverse explicitly — `start-service` -> `systemctl stop`, `start-container` -> `docker stop`.
	// Keeping the pair in the registry is what stops a rollback drifting from the forward it undoes.
	back, berr := spec.RollbackArgv(args)
	if berr != nil {
		return nil, nil, invalidFor(spec.Family)
	}
	return fwd, back, nil
}

// invalidFor maps a defensive build failure to the family's own invalid-target error, so a caller sees a
// vocabulary-appropriate refusal rather than a generic one.
func invalidFor(family string) error {
	if family == opschema.FamilyContainerLifecycle {
		return ErrInvalidContainer
	}
	return ErrInvalidUnit
}

// argvSide names WHICH declared template of an op-class an argv matched: the FORWARD ArgvTemplate, or the
// class's declared compensating RollbackTemplate. The classifier reports it so guardMutatingArgv can compare
// the argv against the canonical argv OF THE MATCHED SIDE — a compensating `systemctl stop nginx` must equal
// resolveOp's rollback argv, never be forced through the forward comparison it can only fail.
type argvSide int

const (
	sideForward  argvSide = iota // the argv is the class's canonical FORWARD shape (spec.Argv)
	sideRollback                 // the argv is the class's declared COMPENSATING shape (spec.RollbackArgv)
)

// classifyArgv maps a fixed command argv back to the (op_class, param, side) the reversible allowlist speaks
// in. Only the exact shapes resolveOp can BUILD are recognized — the forward argv AND, for a class declaring a
// distinct rollback_template, its compensating argv (TG-464 gap A: before this, a sealed inverse's
// `systemctl stop`/`docker stop` had NO execution path on this leaf and "zero inverses have ever run").
// Anything else has no execution path. This is a STRUCTURAL match on the argv vector, never a parse of a
// command string.
//
// It is driven by the REGISTRY, not by a list here. The previous form was a linear if-chain naming four verbs,
// which meant every new verb had to be added in two places that could silently disagree — and a disagreement
// is not cosmetic: guardMutatingArgv classifies an argv, re-derives the canonical argv from the class it got
// back, and refuses on mismatch. A verb missing from the chain is unexecutable; a verb classified as the WRONG
// class is refused too, but only by luck of the re-derivation failing. Reading the shapes out of the same
// registry that BUILDS them makes both cases unrepresentable.
//
// Only templated classes can be reversed — a compiled builder is an opaque function. That is not a gap: a
// compiled class is simply not classifiable here and gets no execution path on this leaf, which is the
// fail-closed direction.
func classifyArgv(argv []string) (opClass, unit string, side argvSide, ok bool) {
	return classifyAgainst(opschema.Specs(), argv)
}

// classifyAgainst is classifyArgv over an EXPLICIT spec set. The seam exists so the ambiguity refusal can be
// proven: the live registry is injective (asserted separately), which means the double-match branch is
// unreachable from any test driven by the real registry — and a branch no test can reach is a branch no
// mutation control can falsify. Constructing an ambiguous pair here is the only way to show the refusal is
// real rather than decorative.
//
// Each spec contributes up to TWO matchable shapes: its forward ArgvTemplate and — when it declares a
// DISTINCT non-empty RollbackTemplate — a synthetic single-template spec over that rollback shape, matched by
// the same opschema.MatchTemplate primitive (never a second matcher). The rollback side is skipped when:
//   - the forward is not templated (a compiled/launch class has no shape on this leaf; granting only its
//     inverse a path here would classify an argv for a class this leaf can never resolve), or
//   - the declared rollback equals the forward element-for-element (a declared re-run IS the forward shape;
//     matching it twice would make the class self-ambiguous and silently unexecutable).
//
// The ambiguity refusal extends over the WHOLE forward∪rollback union: ANY two hits — two classes' forwards,
// two rollbacks, or one class's forward colliding with another's rollback — refuse BOTH rather than pick one,
// for the same reason as ever: guardMutatingArgv re-derives whichever we happened to return, so an ambiguous
// classification silently actuates one verb under another verb's governance record.
func classifyAgainst(specs []opschema.OpClassSpec, argv []string) (opClass, unit string, side argvSide, ok bool) {
	var matched, param string
	var matchedSide argvSide
	hits := 0
	consider := func(s opschema.OpClassSpec, sd argvSide) bool {
		got, name, hit := opschema.MatchTemplate(s, argv)
		if !hit {
			return true
		}
		if hits++; hits > 1 {
			return false // ambiguous across the union — refuse both (see the doc comment)
		}
		matched, param, matchedSide = s.OpClass, got, sd
		_ = name
		return true
	}
	for _, s := range specs {
		if !consider(s, sideForward) {
			return "", "", 0, false
		}
		if len(s.ArgvTemplate) == 0 || len(s.RollbackTemplate) == 0 || argvEqual(s.RollbackTemplate, s.ArgvTemplate) {
			continue
		}
		if !consider(opschema.OpClassSpec{OpClass: s.OpClass, ArgvTemplate: s.RollbackTemplate}, sideRollback) {
			return "", "", 0, false
		}
	}
	if hits != 1 {
		return "", "", 0, false
	}
	return matched, param, matchedSide, true
}

// guardMutatingArgv is the adapter-level defense in depth the mutating Exec runs: the gate must be ON, the
// argv must be a recognized reversible op on an allowlisted unit, and it must equal EXACTLY the canonical
// argv resolveOp would build FOR THE MATCHED SIDE — the forward argv for a forward classification, the
// compensating rollback argv (spec.RollbackArgv, the SAME rendering resolveOp already returns) for a
// rollback-side classification — so a caller can neither run while mutation is off nor smuggle an extra
// argument past the allowlist. The floors and allowlists are side-blind on purpose: resolveOp enforces the
// never-auto floor, the stateful clamp, and the operator unit/container allowlist on the (class, unit) pair
// BEFORE either canonical argv is compared, so an inverse traverses exactly the controls its forward did.
func (m *Module) guardMutatingArgv(argv []string) error {
	if err := m.gate.GuardMutation(); err != nil {
		return err // gate OFF ⇒ safety.ErrMutationDisabled — the effect leaf never mutates while the key is out
	}
	opClass, unit, side, ok := classifyArgv(argv)
	if !ok {
		return ErrNoExecutionPath
	}
	cmd, rollback, err := m.resolveOp(opClass, unit)
	if err != nil {
		return err
	}
	canonical := cmd
	if side == sideRollback {
		canonical = rollback
	}
	if !argvEqual(canonical, argv) {
		return ErrNoExecutionPath
	}
	return nil
}

func argvEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Mutate is the module's genuine mutating path — the effect leaf's own entry the runner activity calls AFTER
// the interceptor admits a gated, sealed, evidence-bound action. It:
//   - refuses unless the process mutation gate is ON (mutation ships OFF ⇒ this refuses through all of Phase
//     0/1, never touching the runner) — INV-09;
//   - resolves the reversible op_class + unit against the allowlist + never-auto floor (fail closed);
//   - records ONE execution_log row whose rollback is bound to the ActionManifest action id (INV-07);
//   - runs the FIXED argv over the host-key-verified, non-interactive ssh invocation — no shell, each remote
//     argument POSIX-quoted (INV-02) — and returns the Result with its execution_log.
//
// It NEVER builds a command by string concatenation and NEVER spawns a shell.
func (m *Module) Mutate(ctx context.Context, actionID, opClass, unit string) (actuation.Result, ExecutionLog, error) {
	if m.gate == nil {
		return actuation.Result{}, ExecutionLog{}, safety.ErrMutationDisabled // a read-only module has no mutating path
	}
	if err := m.gate.GuardMutation(); err != nil {
		return actuation.Result{}, ExecutionLog{}, err // gate OFF ⇒ refuse before any resolve/record/run
	}
	cmd, rollback, err := m.resolveOp(opClass, unit)
	if err != nil {
		return actuation.Result{}, ExecutionLog{}, err
	}
	log, err := m.RecordExec(actionID, cmd, rollback) // action_id-bound rollback (INV-07)
	if err != nil {
		return actuation.Result{}, ExecutionLog{}, err
	}
	res, err := m.run.Run(ctx, m.sshArgv(cmd), nil)
	return res, log, err
}

// ExecLog derives the execution_log for an argv the interceptor has already EXECUTED through this effect leaf,
// bound to the authorizing action id (INV-07). It is the recorder hook the interceptor's Do calls AFTER a
// (gated) execute so a mutation is attributable and undoable — the effect leaf owns the derivation of the
// compensating inverse, the interceptor owns the durable write. It re-derives the (op_class, unit) from the
// FIXED argv (classifyArgv) and its inverse via resolveOp — the SAME canonical shapes the mutating path
// builds — so the recorded rollback is exactly the compensating action, never a guessed one, and reuses
// RecordExec so the identical construction/validation runs. A read-only module (no gate) or a non-mutating /
// unrecognized argv yields no log (there is nothing to record); anything the module could execute is a
// recognized reversible restart on an allowlisted unit. WHILE the gate is off Do refuses before execute, so
// ExecLog is never reached — nothing executes, nothing is recorded.
func (m *Module) ExecLog(actionID string, command []string) (forward, rollback []string, err error) {
	if m.gate == nil {
		return nil, nil, nil // read-only module — there is no mutating command to record
	}
	opClass, unit, side, ok := classifyArgv(command)
	if !ok {
		return nil, nil, nil // not a recognized mutating command (e.g. a read-only get) — nothing to record
	}
	cmd, rb, rerr := m.resolveOp(opClass, unit)
	if rerr != nil {
		return nil, nil, rerr
	}
	if side == sideRollback {
		// The EXECUTED command was the class's compensating argv (the manual-rollback lane, TG-464): the
		// record's forward is what actually ran (the inverse), and its INV-07-bound rollback is the class's
		// forward argv — undoing an undo re-runs the forward. Same derivation the proxmox leaf's ExecLog
		// makes for an executed `stop` (start↔stop).
		cmd, rb = rb, cmd
	}
	log, rerr := m.RecordExec(actionID, cmd, rb) // reuse the action_id-bound construction/validation (INV-07)
	if rerr != nil {
		return nil, nil, rerr
	}
	return log.Command, log.Rollback, nil
}
