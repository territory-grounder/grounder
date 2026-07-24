// Package ssh is the loadable SSH actuation module (spec/008 REQ-811, T-008-15).
//
// It implements adapters/actuation.Actuator: it executes a FIXED argv vector against a configured host
// under a per-agent scoped identity, wrapped in a non-interactive, host-key-verified ssh invocation. There
// is NO parameter that can express an interactive shell or disable host-key verification (INV-02/INV-13) —
// the safety is structural, not a runtime check. Because the remote sshd runs the command through the login
// shell, each remote argument is POSIX-shell-quoted before transport (sshArgv/remoteCommand) so a
// metacharacter or space in an argument can never inject or word-split on the far host. In Phase 0/1 the module is read-only; a mutating command
// records one execution_log row whose rollback is bound to the ActionManifest action id (INV-07,
// rollback.go). Every command still traverses the single pre-execution guard chokepoint (core/actuate) —
// this module is the effect leaf, not the gate.
//
// Provenance: [O] INV-02/INV-07/INV-13/INV-21, spec/008.
package ssh

import (
	"context"
	"strings"

	actuation "github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/safety"
)

// Capability is the declared capability slug this adapter provides.
const Capability = "ssh"

// Runner runs a fixed argv (no shell). *exec.Cmd-backed runners satisfy it in production; the oracle
// injects a recording fake so the argv-construction path runs without a live SSH connection.
type Runner interface {
	Run(ctx context.Context, argv []string, stdin []byte) (actuation.Result, error)
}

// Module is the SSH actuation adapter. Construct with New (read-only) or New(..., WithMutation(gate,
// allowedUnits)) for the Phase-2 mutating configuration. The mutating fields are nil/empty on a read-only
// module, so it can never resolve a mutating command.
type Module struct {
	host     string
	identity string // the per-agent scoped identity (a username/principal), never a password
	run      Runner

	// Phase-2 mutating configuration (nil/empty ⇒ a read-only module). gate is the PROCESS mutation gate
	// this module reads to decide ReadOnly()/GuardMutation; reversible is the capability-declared
	// reversible-op allowlist; allowedUnits is the operator-declared allowed-units allowlist
	// (config-not-code). Set only via WithMutation. Populating them does NOT turn mutation on — the gate
	// stays the sole key.
	gate              *safety.Chokepoint
	reversible        map[string]bool
	allowedUnits      map[string]bool
	allowedContainers map[string]bool // operator-declared allowed docker containers for restart-container (config-not-code)
}

// New builds an SSH module for a host, a per-agent scoped identity, and a runner. With no options it is
// read-only (ReadOnly() is always true); WithMutation adds the gated, allowlisted mutating path.
func New(host, identity string, run Runner, opts ...Option) *Module {
	m := &Module{host: host, identity: identity, run: run}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Capability implements adapters/actuation.Actuator.
func (m *Module) Capability() string { return Capability }

// ActuationHost implements the interceptor's HostBound capability: every mutation this leaf executes lands on
// this single configured host (sshArgv wraps the argv as identity@host), because the leaf does NOT receive the
// action's target. Exposing it lets the interceptor's host-match gate refuse a target≠host mismatch so an
// action admitted for another host can never mis-execute here (fail-closed). Per-target routing would retire
// this single-host binding.
func (m *Module) ActuationHost() string { return m.host }

// Compile-time guard: the interceptor's host-match gate (spec/013 REQ-1219) reads this single-host mutating
// leaf via a structural HostBound{ ActuationHost() string } assertion. Keep the method so the fail-closed
// safety gate can never silently self-disable for THIS leaf by an accidental signature change.
var _ interface{ ActuationHost() string } = (*Module)(nil)

// ReadOnly reports whether the module can only observe. It returns true UNLESS the process mutation gate is
// enabled AND this module carries a genuine reversible-op execution path (a capability-declared reversible
// op_class that is not on the never-auto floor). Mutation ships OFF — the gate is disabled through all of
// Phase 0/1 — so this returns true in every production and test path, except one that constructs a
// test-only enabled gate. That is the structural proof the module cannot mutate until the key is turned:
// the mutating CODE PATH exists and is exercised, but it is unreachable while the gate is off.
func (m *Module) ReadOnly() bool {
	return !(m.gate != nil && m.gate.MayActuate() && m.hasReversiblePath())
}

// compile-time proof the module satisfies the stable actuation interface.
var _ actuation.Actuator = (*Module)(nil)

// sshArgv wraps a fixed command argv in a non-interactive, host-key-verified ssh invocation. The options
// are hard-coded: StrictHostKeyChecking=yes (never a bypass), BatchMode=yes and PasswordAuthentication=no
// (no prompt, key-only), and no "-t" (no PTY / interactive shell). The LOCAL ssh client is invoked argv-only
// (no local shell). The remote sshd, however, runs whatever follows the destination through the login shell —
// the ssh client joins those words with spaces and the remote shell re-parses them — so a bare argv would be
// word-split and its metacharacters (`;`, `|`, `$(...)`, spaces) interpreted on the far side. To preserve
// argv semantics across that remote shell (INV-02: a metacharacter in an argument can never alter the executed
// program), each element is POSIX single-quoted and joined into ONE remote command word, so the remote shell
// parses it back into EXACTLY these arguments. This mirrors the predecessor's per-argument shell-quoting.
func (m *Module) sshArgv(argv []string) []string {
	out := make([]string, 0, len(sshCanonicalOpts)+3)
	out = append(out, "ssh")
	out = append(out, sshCanonicalOpts...)
	out = append(out, m.identity+"@"+m.host, remoteCommand(argv))
	return out
}

// sshCanonicalOpts is the FIXED, non-interactive, host-key-verifying option prologue every ssh actuation
// carries. It is the single source of truth BOTH sshArgv (which emits it) and the native runner's
// parseSSHArgv (which REQUIRES it verbatim) read, so a Runner — a public interface anyone could call
// directly — can never be handed a non-canonical invocation that routes to a different host or weakens
// (e.g. StrictHostKeyChecking=no) host-key verification. Defense in depth beyond Module.Exec, which always
// produces exactly this prologue.
var sshCanonicalOpts = []string{
	"-o", "StrictHostKeyChecking=yes",
	"-o", "BatchMode=yes",
	"-o", "PasswordAuthentication=no",
}

// remoteCommand renders a command argv as a single POSIX-shell-safe string for the REMOTE login shell. Each
// element is wrapped in single quotes (an embedded single quote is escaped as the classic `'\”`), so no
// element's spaces or shell metacharacters can inject or split on the remote host.
func remoteCommand(argv []string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}

// Exec runs a fixed argv over ssh — the Actuator chokepoint the interceptor calls. An empty argv is
// rejected; the fixed ssh invocation can never be an interactive shell, and each remote argument is
// shell-quoted so a metacharacter cannot alter the program. On a mutation-configured module (a gate is
// wired) Exec re-enforces the mutation gate AND the reversible-op allowlist HERE, at the effect leaf, as
// defense in depth: even reached directly, an ungated command — or one that is not the exact canonical argv
// of an allowlisted reversible op on an allowlisted unit — never runs (INV-09). A read-only module (no
// gate) is the argv transport whose execution path is withheld by the registry being disabled and by the
// interceptor upstream.
func (m *Module) Exec(ctx context.Context, argv []string, stdin []byte) (actuation.Result, error) {
	if len(argv) == 0 {
		return actuation.Result{}, actuation.ErrEmptyArgv
	}
	if m.gate != nil {
		if err := m.guardMutatingArgv(argv); err != nil {
			return actuation.Result{}, err
		}
	}
	return m.run.Run(ctx, m.sshArgv(argv), stdin)
}
