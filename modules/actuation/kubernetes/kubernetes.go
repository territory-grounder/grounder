// Package kubernetes is the loadable Kubernetes actuation module (spec/008 REQ-812, T-008-16).
//
// It implements adapters/actuation.Actuator and exposes typed, individually-permissioned kubectl/helm
// operations against a configured cluster context. delete and drain are clamped to the non-configurable
// never-auto floor (core/safety.IsNeverAuto) regardless of confidence, band, or policy — no flag lifts
// them (INV-09). Every operation still traverses the single pre-execution guard chokepoint (INV-21); this
// module is the effect leaf. Read-only through Phase 0/1.
//
// Provenance: [O] INV-09/INV-21, spec/008.
package kubernetes

import (
	"context"
	"fmt"
	"strings"

	actuation "github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/safety"
)

// Capability is the declared capability slug this adapter provides.
const Capability = "kubernetes"

// Runner runs a fixed argv (no shell); the oracle injects a recording fake.
type Runner interface {
	Run(ctx context.Context, argv []string, stdin []byte) (actuation.Result, error)
}

// Module is the Kubernetes actuation adapter. Construct with New.
type Module struct {
	context string // the configured cluster context
	run     Runner
}

// New builds a Kubernetes module for a cluster context and a runner.
func New(clusterContext string, run Runner) *Module {
	return &Module{context: clusterContext, run: run}
}

// Capability implements adapters/actuation.Actuator.
func (m *Module) Capability() string { return Capability }

// ReadOnly reports true through Phase 0/1.
func (m *Module) ReadOnly() bool { return true }

// compile-time proof the module satisfies the stable actuation interface.
var _ actuation.Actuator = (*Module)(nil)

// opClass maps a kubectl operation to its never-auto floor op-class slug (kubectl-delete / kubectl-drain).
func opClass(op string) string { return "kubectl-" + op }

// Operation builds the typed kubectl/helm argv for a permitted operation. delete and drain are clamped to
// the non-configurable never-auto floor and refused unconditionally; an unknown operation is refused. The
// clamp does not depend on any flag, confidence, or policy — it is a mechanical property of the op-class.
func (m *Module) Operation(op string, args ...string) ([]string, error) {
	op = strings.ToLower(strings.TrimSpace(op)) // normalize so a case/whitespace variant cannot dodge the floor
	if safety.IsNeverAuto(opClass(op)) {
		return nil, fmt.Errorf("kubernetes: %q is clamped to the non-configurable never-auto floor and can never auto-execute", op)
	}
	switch op {
	case "get", "describe", "patch", "rollout", "scale":
		return append([]string{"kubectl", "--context", m.context, op}, args...), nil
	case "apply":
		// `apply --prune` deletes any live resource absent from the applied manifest (delete-equivalent), so it
		// is clamped to the never-auto floor like `delete`; a plain apply is permitted.
		for _, a := range args {
			if a == "--prune" || strings.HasPrefix(a, "--prune=") {
				return nil, fmt.Errorf("kubernetes: %q is clamped to the non-configurable never-auto floor (it deletes resources absent from the manifest) and can never auto-execute", "apply --prune")
			}
		}
		return append([]string{"kubectl", "--context", m.context, op}, args...), nil
	case "helm":
		// helm's teardown subcommands (uninstall/delete a release + its PVCs, or rollback to a prior revision)
		// are destruction-equivalent and clamped to the never-auto floor; install/upgrade/list/etc. are permitted.
		if len(args) > 0 && isDestructiveHelm(args[0]) {
			return nil, fmt.Errorf("kubernetes: %q is clamped to the non-configurable never-auto floor and can never auto-execute", "helm "+strings.ToLower(strings.TrimSpace(args[0])))
		}
		return append([]string{"helm", "--kube-context", m.context}, args...), nil
	default:
		return nil, fmt.Errorf("kubernetes: unsupported operation %q", op)
	}
}

// isDestructiveHelm reports whether a helm subcommand tears a release down (deleting its resources / PVCs) or
// rolls it back — the destruction-equivalent class the never-auto floor withholds from auto-execution.
func isDestructiveHelm(sub string) bool {
	switch strings.ToLower(strings.TrimSpace(sub)) {
	case "uninstall", "delete", "del", "rollback", "reset":
		return true
	}
	return false
}

// Exec runs a typed operation: argv[0] is the operation name, argv[1:] its arguments. It routes through
// Operation, so delete/drain are clamped to the floor here too (defense in depth). No shell is involved.
func (m *Module) Exec(ctx context.Context, argv []string, stdin []byte) (actuation.Result, error) {
	if len(argv) == 0 {
		return actuation.Result{}, actuation.ErrEmptyArgv
	}
	cmd, err := m.Operation(argv[0], argv[1:]...)
	if err != nil {
		return actuation.Result{}, err
	}
	return m.run.Run(ctx, cmd, stdin)
}
