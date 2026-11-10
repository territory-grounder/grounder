package runner

import (
	"context"
	"testing"

	"github.com/territory-grounder/grounder/agent"
)

type hrgTool struct{ name string }

func (t hrgTool) Name() string   { return t.name }
func (t hrgTool) ReadOnly() bool { return true }
func (t hrgTool) Invoke(context.Context, map[string]string) (agent.ToolResult, error) {
	return agent.ToolResult{}, nil
}

// TG-638: the gate is armed only for a SERVICE alert with a host and the host-services read available.
func TestHostReadGateFor(t *testing.T) {
	with := agent.NewReadOnlyToolSet()
	_ = with.Register(hrgTool{name: hostServicesTool})
	without := agent.NewReadOnlyToolSet()
	_ = without.Register(hrgTool{name: "get-device-status"})

	if g := hostReadGateFor("Service-up/down", "site01app01", with); g == nil || g.Tool != hostServicesTool || g.Host != "site01app01" {
		t.Fatalf("a service alert must arm the gate on the incident host, got %+v", g)
	}
	for _, c := range []struct {
		rule, host string
		tools      *agent.ToolSet
	}{
		{"Device-Down", "site01app01", with},
		{"Disk-Space-90", "site01app01", with},
		{"Service-up/down", "", with},
		{"Service-up/down", "site01app01", without},
		{"Service-up/down", "site01app01", nil},
	} {
		if g := hostReadGateFor(c.rule, c.host, c.tools); g != nil {
			t.Errorf("rule=%q host=%q: the gate must stay off, got %+v", c.rule, c.host, g)
		}
	}
}
