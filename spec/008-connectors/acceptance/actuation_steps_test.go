package acceptance

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cucumber/godog"

	actuation "github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/modules"
	k8s "github.com/territory-grounder/grounder/modules/actuation/kubernetes"
	"github.com/territory-grounder/grounder/modules/actuation/mcp"
	"github.com/territory-grounder/grounder/modules/actuation/proxmox"
	sshmod "github.com/territory-grounder/grounder/modules/actuation/ssh"
)

// The four actuation modules (REQ-811..814) bind their scenarios here. Their step texts are all distinct,
// so one registrar carries all four. Each drives the real module + the mechanical never-auto floor.
func init() {
	moduleStepRegistrars = append(moduleStepRegistrars, registerActuationSteps)
}

// recRunner records the argv it was asked to run (for ssh/k8s/proxmox, whose Runner is (ctx, argv, stdin)).
type recRunner struct{ argv []string }

func (r *recRunner) Run(_ context.Context, argv []string, _ []byte) (actuation.Result, error) {
	r.argv = argv
	return actuation.Result{}, nil
}

// mcpRunner records the tool it was asked to run (mcp's Runner is (ctx, tool, argv, stdin)).
type mcpRunner struct{ ran string }

func (r *mcpRunner) Run(_ context.Context, tool string, _ []string, _ []byte) (actuation.Result, error) {
	r.ran = tool
	return actuation.Result{}, nil
}

func registerActuationSteps(sc *godog.ScenarioContext) {
	reg := modules.NewRegistry()
	var (
		runner    = &recRunner{}
		mcpR      = &mcpRunner{}
		sshMod    *sshmod.Module
		k8sMod    *k8s.Module
		pveMod    *proxmox.Module
		mcpMod    *mcp.Module
		execLog   sshmod.ExecutionLog
		lastErr   error
		sshMutMod *sshmod.Module
		mutRunner = &recRunner{}
		mutErr    error
	)

	// ---- REQ-811 SSH ----
	sc.Step(`^the SSH actuation module is registered and enabled with a per-agent scoped identity$`, func() error {
		sshMod = sshmod.New("web01", "svc-agent", runner)
		return reg.Register(modules.Registration{Surface: modules.SurfaceActuation, SourceType: sshMod.Capability(), Capability: "ssh", Enabled: true, Adapter: sshMod})
	})
	sc.Step(`^a mutating command is executed as a fixed argv array through the Execute chokepoint$`, func() error {
		if _, err := sshMod.Exec(context.Background(), []string{"systemctl", "restart", "nginx"}, nil); err != nil {
			lastErr = err
			return nil
		}
		execLog, lastErr = sshMod.RecordExec("act-42", []string{"systemctl", "stop", "nginx"}, []string{"systemctl", "start", "nginx"})
		return nil
	})
	sc.Step(`^one execution_log row with its rollback command is recorded and no interactive shell or host-key-bypass option is expressible$`, func() error {
		if lastErr != nil {
			return fmt.Errorf("ssh actuation must succeed: %w", lastErr)
		}
		got := strings.Join(runner.argv, " ")
		if runner.argv[0] != "ssh" || !strings.Contains(got, "StrictHostKeyChecking=yes") {
			return fmt.Errorf("must be a host-key-verified ssh invocation, got %q", got)
		}
		for _, bad := range []string{"StrictHostKeyChecking=no", "-t", "sh -c"} {
			if strings.Contains(got, bad) {
				return fmt.Errorf("must not express %q, got %q", bad, got)
			}
		}
		if execLog.ActionID != "act-42" || len(execLog.Rollback) == 0 {
			return fmt.Errorf("an execution_log row with a bound rollback must be recorded: %+v", execLog)
		}
		return nil
	})

	// ---- REQ-812 Kubernetes ----
	sc.Step(`^the Kubernetes actuation module is registered and enabled with a configured cluster context$`, func() error {
		k8sMod = k8s.New("prod", runner)
		return reg.Register(modules.Registration{Surface: modules.SurfaceActuation, SourceType: k8sMod.Capability(), Capability: "kubernetes", Enabled: true, Adapter: k8sMod})
	})
	sc.Step(`^a kubectl delete operation is proposed$`, func() error {
		_, lastErr = k8sMod.Operation("delete", "deployment", "web")
		return nil
	})
	sc.Step(`^it is clamped to the non-configurable never-auto floor regardless of confidence or policy$`, func() error {
		if lastErr == nil || !strings.Contains(lastErr.Error(), "never-auto floor") {
			return fmt.Errorf("kubectl delete must be clamped to the floor, got %v", lastErr)
		}
		if !safety.IsNeverAuto("kubectl-delete") {
			return fmt.Errorf("kubectl-delete must be on the mechanical never-auto floor")
		}
		return nil
	})

	// ---- REQ-813 Proxmox ----
	sc.Step(`^the Proxmox actuation module is registered with the lifecycle enable flag unset$`, func() error {
		pveMod = proxmox.New("https://pve01:8006", config.SecretRef("env:X")) // native HTTP actuator, read-only (no gate)
		return reg.Register(modules.Registration{Surface: modules.SurfaceActuation, SourceType: pveMod.Capability(), Capability: "proxmox", Enabled: true, Adapter: pveMod})
	})
	sc.Step(`^a guest reboot is proposed$`, func() error {
		lastErr = pveMod.Lifecycle("reboot")
		return nil
	})
	sc.Step(`^the lifecycle operation is refused and reboot is clamped to the non-configurable never-auto floor$`, func() error {
		if lastErr == nil {
			return fmt.Errorf("a guest reboot with the enable flag unset must be refused")
		}
		if !safety.IsNeverAuto("reboot") {
			return fmt.Errorf("reboot must be on the mechanical never-auto floor")
		}
		return nil
	})

	// ---- REQ-814 MCP ----
	sc.Step(`^the MCP tool actuation surface is registered with a set of capability-scoped tools$`, func() error {
		mcpMod = mcp.New(mcpR)
		mcpMod.RegisterTool("k8s.get", false)
		mcpMod.RegisterTool("k8s.rollout", true) // mutating, behind the enable flag
		return reg.Register(modules.Registration{Surface: modules.SurfaceActuation, SourceType: mcpMod.Capability(), Capability: "mcp", Enabled: true, Adapter: mcpMod})
	})
	sc.Step(`^an unregistered tool is invoked$`, func() error {
		_, lastErr = mcpMod.Exec(context.Background(), []string{"unregistered.tool"}, nil)
		return nil
	})
	sc.Step(`^it has no execution path and lifecycle-mutating tools remain behind an explicit enable flag$`, func() error {
		if lastErr == nil {
			return fmt.Errorf("an unregistered tool must have no execution path")
		}
		if mcpR.ran != "" {
			return fmt.Errorf("an unregistered tool must never reach the runner, ran %q", mcpR.ran)
		}
		// a registered mutating tool is still withheld behind the disabled enable flag.
		if _, err := mcpMod.Exec(context.Background(), []string{"k8s.rollout"}, nil); err == nil {
			return fmt.Errorf("a mutating tool must remain behind the explicit enable flag")
		}
		return nil
	})

	// ---- REQ-822 SSH mutating path is inert while mutation is off (task #21) ----
	sc.Step(`^the SSH actuation module is configured with a reversible-op allowlist and the mutation gate off$`, func() error {
		// the gate is off by default (Phase 0/1); the module carries the canary reversible allowlist + one unit.
		sshMutMod = sshmod.New("web01", "svc-agent", mutRunner, sshmod.WithMutation(safety.NewReadOnlyChokepoint(), []string{"nginx"}, nil))
		return nil
	})
	sc.Step(`^a reversible restart of an allowlisted unit is attempted through the mutating path$`, func() error {
		_, _, mutErr = sshMutMod.Mutate(context.Background(), "act-822", sshmod.OpClassRestartService, "nginx")
		return nil
	})
	sc.Step(`^the module reports read-only, the restart does not execute, and no command reaches the runner$`, func() error {
		if !sshMutMod.ReadOnly() {
			return fmt.Errorf("gate OFF: the module must report read-only")
		}
		if !errors.Is(mutErr, safety.ErrMutationDisabled) {
			return fmt.Errorf("gate OFF: the mutating path must refuse with ErrMutationDisabled, got %v", mutErr)
		}
		if mutRunner.argv != nil {
			return fmt.Errorf("gate OFF: no command may reach the runner, got %v", mutRunner.argv)
		}
		return nil
	})
}
