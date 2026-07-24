// Package actuation defines the actuation adapter contract for Territory Grounder.
//
// Provenance: [O] INV-02 (no shell, ever; actuation is a fixed argv vector), C-02/C-03/C-04/H-06
// (the predecessor's command-injection class), P0-5. Threat: "OS command injection executes attacker
// shell before any model/gate".
//
// The class of OS-command-injection is made *unrepresentable* here:
//   - Actuation is Exec(ctx, argv []string, stdin []byte) — a fixed program + explicit argument
//     vector. There is no command STRING to interpolate into.
//   - There is NO sh -c, NO fmt.Sprintf into a command, NO manual quote-escaping helper.
//   - Caller-supplied scalars are already typed/validated Go values by the time they reach here and
//     are passed as literal argv elements — shell metacharacters in an argument cannot alter the
//     executed program.
//
// In Phase 0/1 only READ-ONLY adapters are registered (get/describe/logs). Mutating actuation is a
// Phase-2 concern gated by safety.MutationGate; the interface exists, the mutation does not.
package actuation

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
)

// Result is the typed outcome of an actuation call.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Actuator is a capability-scoped, per-agent actuation adapter. Implementations MUST NOT spawn a
// shell or build a command by string concatenation.
type Actuator interface {
	// Capability is the declared capability slug this adapter provides (e.g. "k8s.readonly").
	Capability() string
	// ReadOnly reports whether this adapter can only observe (true) or may mutate (false). In
	// Phase 0/1 every registered adapter returns true.
	ReadOnly() bool
	// Exec runs a FIXED program with an explicit argument vector. argv[0] is the program; argv[1:]
	// are literal arguments. No shell is involved.
	Exec(ctx context.Context, argv []string, stdin []byte) (Result, error)
}

var (
	// ErrEmptyArgv is returned when no program is supplied.
	ErrEmptyArgv = errors.New("actuation: empty argv (no program)")
	// ErrMutatingBlocked is returned when a non-read-only actuation is attempted in a read-only build.
	ErrMutatingBlocked = errors.New("actuation: mutating actuation is disabled (Phase 0/1 is read-only)")
)

// runArgv executes a fixed argv vector via exec.CommandContext WITHOUT a shell. This is the single
// choke point through which all local actuation flows; note there is no "sh", "-c", or string
// interpolation anywhere in this function.
func runArgv(ctx context.Context, argv []string, stdin []byte) (Result, error) {
	if len(argv) == 0 {
		return Result{}, ErrEmptyArgv
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) // program + literal args; NOT sh -c
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	res := Result{Stdout: out.Bytes(), Stderr: errb.Bytes()}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	// A non-zero exit is a Result, not a Go error; only spawn/IO failures are errors.
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		return res, err
	}
	return res, nil
}

// LocalReadOnly is the reference read-only local actuation adapter AND the fail-closed DEFAULT effect leaf
// (bootstrap.BuildEffectActuator returns it when no SSH host+identity is configured). It declares ReadOnly()
// true, and — matching that contract — its Exec REFUSES to run any argv: reaching Exec means the interceptor's
// mode chokepoint already admitted a mutation (mode ∈ {Semi-auto, Full-auto}), so a read-only leaf being asked
// to actuate is a misconfiguration (an actuating mode with no real effect leaf wired), NOT something to run in
// the worker process. Refusing here couples "mode actuating" with "effect leaf can actually actuate" as a
// DESIGNED floor, instead of relying incidentally on the distroless image lacking systemctl/docker (the
// integration-audit A1 finding). It stays argv-only either way — it never builds a shell.
type LocalReadOnly struct {
	Cap string
}

func (a LocalReadOnly) Capability() string { return a.Cap }
func (a LocalReadOnly) ReadOnly() bool      { return true }

func (a LocalReadOnly) Exec(_ context.Context, argv []string, _ []byte) (Result, error) {
	if len(argv) == 0 {
		return Result{}, ErrEmptyArgv
	}
	// A read-only adapter never executes a mutation locally — fail closed (an actuating mode reached the
	// execute chokepoint with no mutating effect leaf configured).
	return Result{}, ErrMutatingBlocked
}

// LocalRunner runs a fully-formed fixed argv vector as a LOCAL process via exec.CommandContext — argv-only,
// NEVER sh -c. It exists to satisfy the ssh module's Runner seam (Run(ctx, argv, stdin) (Result, error)): the
// SSH mutating actuator constructs its complete `ssh -o … identity@host <remote-command>` invocation and
// hands THAT argv here to be spawned as the local ssh client. LocalRunner carries NO gate of its own — the
// process mutation gate and the reversible-op/unit allowlist inside the ssh.Module (and the interceptor
// upstream) are the sole authority; this only runs a command those controls already admitted. It is the
// production runner the Phase-2 effect-leaf seam wires in, and it stays inert while mutation is off (the ssh
// module refuses before it is ever reached).
type LocalRunner struct{}

// Run executes the fixed argv vector with no shell (the same single choke point as LocalReadOnly).
func (LocalRunner) Run(ctx context.Context, argv []string, stdin []byte) (Result, error) {
	return runArgv(ctx, argv, stdin)
}
