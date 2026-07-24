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
// (config-not-code). The reversible op_class set is the curated conservative family — restart-service,
// reload-service, restart-container (see resolveOp); a stateful or irreversible class can never join the
// resolvable set because resolveOp floors it via safety.NeverAutoFloor and safety.IsStatefulWorkload, and an
// EMPTY allowlist refuses every target. Passing this does NOT turn mutation on — the gate stays the sole key.
func WithMutation(gate *safety.Chokepoint, allowedUnits, allowedContainers []string) Option {
	return func(m *Module) {
		m.gate = gate
		m.reversible = map[string]bool{OpClassRestartService: true, OpClassReloadService: true, OpClassRestartContainer: true, OpClassStartService: true}
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
	switch oc {
	case OpClassRestartService, OpClassReloadService, OpClassStartService:
		// The systemd-unit reversible classes share the same runtime controls in the unit vocabulary: strict
		// unit-token validation THEN the operator allowed-units allowlist. They differ ONLY in the fixed argv the
		// shared op-class SCHEMA REGISTRY builds (`restart-service` → [systemctl restart <unit>], `reload-service`
		// → [systemctl reload <unit>], `start-service` → [systemctl start <unit>]) — the ONE place each shape is
		// declared, shared with the runner's sealedArgv and the interceptor's structure gate, so the effect leaf
		// can never define a second, drifting argv.
		if !validUnit(unit) {
			return nil, nil, ErrInvalidUnit
		}
		if !m.allowedUnits[unit] {
			return nil, nil, ErrUnitNotAllowed
		}
		// The unit has already passed validUnit + the operator allowed-units allowlist above (the leaf's stricter
		// runtime controls, applied BEFORE the shared shape), so the registry build (keyed on the normalized
		// op-class `oc`) cannot fail here; a defensive error still maps to ErrInvalidUnit (fail closed).
		fwd, berr := opschema.Argv(oc, map[string]string{opschema.ParamUnit: unit})
		if berr != nil {
			return nil, nil, ErrInvalidUnit
		}
		if oc == OpClassStartService {
			// INV-07: a start's compensating action is the inverse verb `systemctl stop <unit>` (bring the service
			// back down), NOT a re-run of the forward start argv (a re-start is not an inverse). This is the ONE
			// rollback whose shape differs from its forward — declared here alongside the forward, fixed-vector.
			return fwd, []string{"systemctl", "stop", unit}, nil
		}
		// A restart/reload's compensating action is a re-run (idempotent reconvergence to the known-good state) —
		// INV-07 requires a bound rollback, not a perfect one — so the rollback is a fresh copy of the forward argv.
		return fwd, append([]string(nil), fwd...), nil
	case OpClassRestartContainer:
		// The 'unit' argument carries the docker container name for this class. Apply the same fail-closed
		// runtime controls restart-service does, in the container vocabulary: strict name shape THEN the
		// operator allowed-containers allowlist (never an arbitrary container). The never-auto floor and the
		// stateful-workload floor were already enforced above (steps 1–2), keyed on this same name.
		if !validContainer(unit) {
			return nil, nil, ErrInvalidContainer
		}
		if !m.allowedContainers[unit] {
			return nil, nil, ErrContainerNotAllowed
		}
		// Build the FORWARD argv from the shared op-class SCHEMA REGISTRY (the ONE place the restart-container →
		// [docker restart <container>] shape is declared). The name has already passed validContainer + the
		// allowlist above, so the registry build cannot fail; a defensive error still maps fail-closed. A
		// restart's compensating action is a re-restart (idempotent reconvergence) — INV-07 requires a bound
		// rollback, not a perfect inverse — so the rollback is a fresh copy of the same forward argv.
		fwd, berr := opschema.Argv(OpClassRestartContainer, map[string]string{opschema.ParamContainer: unit})
		if berr != nil {
			return nil, nil, ErrInvalidContainer
		}
		return fwd, append([]string(nil), fwd...), nil
	default:
		return nil, nil, ErrNoExecutionPath
	}
}

// classifyArgv maps a fixed command argv back to the (op_class, unit) the reversible allowlist speaks in.
// Only the exact shapes resolveOp can BUILD are recognized; anything else has no execution path. This is a
// STRUCTURAL match on the argv vector, never a parse of a command string.
func classifyArgv(argv []string) (opClass, unit string, ok bool) {
	if len(argv) == 3 && argv[0] == "systemctl" && argv[1] == "restart" {
		return OpClassRestartService, argv[2], true
	}
	if len(argv) == 3 && argv[0] == "systemctl" && argv[1] == "reload" {
		return OpClassReloadService, argv[2], true
	}
	if len(argv) == 3 && argv[0] == "systemctl" && argv[1] == "start" {
		return OpClassStartService, argv[2], true
	}
	if len(argv) == 3 && argv[0] == "docker" && argv[1] == "restart" {
		return OpClassRestartContainer, argv[2], true
	}
	return "", "", false
}

// guardMutatingArgv is the adapter-level defense in depth the mutating Exec runs: the gate must be ON, the
// argv must be a recognized reversible op on an allowlisted unit, and it must equal EXACTLY the canonical
// argv resolveOp would build — so a caller can neither run while mutation is off nor smuggle an extra
// argument past the allowlist.
func (m *Module) guardMutatingArgv(argv []string) error {
	if err := m.gate.GuardMutation(); err != nil {
		return err // gate OFF ⇒ safety.ErrMutationDisabled — the effect leaf never mutates while the key is out
	}
	opClass, unit, ok := classifyArgv(argv)
	if !ok {
		return ErrNoExecutionPath
	}
	cmd, _, err := m.resolveOp(opClass, unit)
	if err != nil {
		return err
	}
	if !argvEqual(cmd, argv) {
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
	opClass, unit, ok := classifyArgv(command)
	if !ok {
		return nil, nil, nil // not a recognized mutating command (e.g. a read-only get) — nothing to record
	}
	cmd, rb, rerr := m.resolveOp(opClass, unit)
	if rerr != nil {
		return nil, nil, rerr
	}
	log, rerr := m.RecordExec(actionID, cmd, rb) // reuse the action_id-bound construction/validation (INV-07)
	if rerr != nil {
		return nil, nil, rerr
	}
	return log.Command, log.Rollback, nil
}
