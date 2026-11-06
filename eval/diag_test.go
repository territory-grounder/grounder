package eval

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/agent/skills"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/execclass"
)

type logModel struct{ inner agent.Completer }

func (l logModel) Complete(ctx context.Context, u, m string, msgs []model.Message) (string, error) {
	out, err := l.inner.Complete(ctx, u, m, msgs)
	fmt.Printf("\n[MODEL cycle out] err=%v raw=%q\n", err, out)
	return out, err
}

// TestAgentDiag runs ONE agent session with the REAL seed (skills + protocol) against the live gateway and
// prints every model output + the terminal Result (outcome + REASON) — to diagnose why triage stops.
func TestAgentDiag(t *testing.T) {
	gw := os.Getenv("TG_EVAL_GATEWAY")
	if gw == "" {
		t.Skip("set TG_EVAL_GATEWAY + LITELLM_MASTER_KEY")
	}
	m := logModel{inner: model.NewGateway(gw, config.SecretRef("env:LITELLM_MASTER_KEY"))}
	tools := evalTools(Incident{ExternalRef: "eval-01", AlertRule: "Devices up/down", Host: "dc1bookwyrm01", Severity: "critical", Summary: "the device is ICMP-unreachable; it is a Proxmox guest VM."}, loadEstateGraph(t, "estate_fixture.json"))
	guidance, loaded := skills.Default().Compose(skills.Context{Phase: skills.PhaseInvestigate, ExecClass: execclass.DeepInvestigation})
	fmt.Printf("=== skills loaded: %v (guidance %d chars) ===\n", loaded, len(guidance))
	seed := []model.Message{{Role: "user", Content: "Incident eval-01 (Devices up/down on dc1bookwyrm01): investigate read-only and propose.\n\n" + guidance}}
	ag := &agent.Agent{Model: m, Tools: tools, Limits: agent.DefaultLimits(), ModelName: "fast", User: "diag"}
	res, err := ag.Run(context.Background(), seed)
	fmt.Printf("\n=== RESULT: outcome=%v reason=%q cycles=%d confidence=%.2f proposedOp=%q toolResults=%d err=%v ===\n",
		res.Outcome, res.Reason, res.Cycles, res.Confidence, res.Proposal.Action.Op, len(res.ToolResults), err)
}
