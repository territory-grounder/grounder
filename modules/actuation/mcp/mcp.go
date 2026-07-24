// Package mcp is the loadable MCP tool actuation surface (spec/008 REQ-814, T-008-18).
//
// It implements adapters/actuation.Actuator and registers each MCP tool as a typed, capability-scoped
// adapter reachable only through the single Exec chokepoint. An UNREGISTERED tool has NO execution path
// (INV-17) — the closure of the predecessor's "dead tool still callable" failure class — and every
// lifecycle-mutating tool is withheld behind an explicit enable flag (INV-08/INV-21). Read-only tools run
// through Phase 0/1; mutating tools cannot run until the flag is set.
//
// Provenance: [O] INV-08/INV-17/INV-21, spec/008.
package mcp

import (
	"context"
	"fmt"
	"sort"
	"sync"

	actuation "github.com/territory-grounder/grounder/adapters/actuation"
)

// Capability is the declared capability slug this adapter provides.
const Capability = "mcp"

// Runner invokes a registered tool with a fixed argument vector; the oracle injects a recording fake.
type Runner interface {
	Run(ctx context.Context, tool string, argv []string, stdin []byte) (actuation.Result, error)
}

// tool is a registered MCP tool's capability metadata.
type tool struct {
	mutating bool // a lifecycle-mutating tool is withheld behind the enable flag
}

// Module is the MCP tool actuation surface. Construct with New.
type Module struct {
	mu       sync.RWMutex
	tools    map[string]tool
	mutation bool // the explicit enable flag for lifecycle-mutating tools; OFF by default
	run      Runner
}

// Option configures a Module.
type Option func(*Module)

// WithMutationEnabled sets the explicit enable flag that permits mutating tools (floor rules still apply
// upstream). Off by default.
func WithMutationEnabled() Option { return func(m *Module) { m.mutation = true } }

// New builds an MCP surface with a runner.
func New(run Runner, opts ...Option) *Module {
	m := &Module{tools: map[string]tool{}, run: run}
	for _, o := range opts {
		o(m)
	}
	return m
}

// RegisterTool registers a capability-scoped tool. A tool that is not registered has no execution path.
func (m *Module) RegisterTool(name string, mutating bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tools[name] = tool{mutating: mutating}
}

// Registered returns the sorted set of registered tool names — the manifest a reconciler compares against.
func (m *Module) Registered() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.tools))
	for n := range m.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Capability implements adapters/actuation.Actuator.
func (m *Module) Capability() string { return Capability }

// ReadOnly reports true through Phase 0/1.
func (m *Module) ReadOnly() bool { return true }

// compile-time proof the module satisfies the stable actuation interface.
var _ actuation.Actuator = (*Module)(nil)

// ErrNoExecutionPath is returned when an unregistered tool is invoked — no registration, no path (INV-17).
var ErrNoExecutionPath = fmt.Errorf("mcp: tool is not registered — no execution path (INV-17)")

// Exec routes argv[0] as the tool name to a registered tool. An unregistered tool has no execution path; a
// mutating tool is refused while the enable flag is unset (INV-08). No shell is involved.
func (m *Module) Exec(ctx context.Context, argv []string, stdin []byte) (actuation.Result, error) {
	if len(argv) == 0 {
		return actuation.Result{}, actuation.ErrEmptyArgv
	}
	name := argv[0]
	m.mu.RLock()
	t, ok := m.tools[name]
	enabled := m.mutation
	m.mu.RUnlock()
	if !ok {
		return actuation.Result{}, fmt.Errorf("%w: %q", ErrNoExecutionPath, name)
	}
	if t.mutating && !enabled {
		return actuation.Result{}, fmt.Errorf("mcp: mutating tool %q is withheld behind the disabled enable flag", name)
	}
	return m.run.Run(ctx, name, argv[1:], stdin)
}
