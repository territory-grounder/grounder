package bootstrap

import (
	"context"
	"os"
	"strings"
	"testing"
)

// ★ THE MCP SURFACE IS DECLARED AND HAS NO EXECUTION PATH (TG-56).
//
// `actuation/mcp` appears among the 19 capabilities the worker prints at boot, which reads as a live
// capability. It is not one, and this pins BOTH of the independent reasons — because a future change that
// removes one of them would leave the other doing all the work silently.
//
//  1. No tool is ever registered, so Exec returns ErrNoExecutionPath before reaching any runner (INV-17).
//  2. bootstrap wires deniedMCPRunner, which refuses read-only and mutating tools alike.
//
// The package doc used to say "Read-only tools run through Phase 0/1" without qualification. That is true
// of the chokepoint's logic and false of this wiring, and the gap is exactly the class TG keeps finding:
// a capability described in one file and foreclosed in another.
func TestTheWiredMCPRunnerDeniesEverything(t *testing.T) {
	// The runner bootstrap actually wires — not a stand-in.
	_, err := deniedMCPRunner{}.Run(context.Background(), "netbox.get_device", []string{"--name", "x"}, nil)
	if err == nil {
		t.Fatal("the wired MCP runner executed a tool. modules/bootstrap declares this family DISABLED, so " +
			"every tool — read-only included — must be refused; a runner that returns nil here gives the " +
			"declared-but-dead capability a real execution path")
	}
	if !strings.Contains(err.Error(), "no execution path") {
		t.Errorf("refusal was %q; it must say the surface has no execution path, so an operator reading a "+
			"failed tool call learns the capability is disabled rather than that the tool is broken", err)
	}
}

// A read-only tool is refused too. This is the assertion the old package comment would have failed.
func TestEvenAReadOnlyToolIsRefusedByTheWiredRunner(t *testing.T) {
	if _, err := (deniedMCPRunner{}).Run(context.Background(), "read.only.probe", nil, nil); err == nil {
		t.Fatal("a read-only tool ran. The chokepoint PERMITS read-only tools in Phase 0/1, but this " +
			"deployment's runner must still refuse them — otherwise the package doc's old wording becomes " +
			"true by accident and MCP is live without anyone deciding it should be")
	}
}

// And the doc must not re-acquire the unqualified claim. Comment-stripped so it cannot pass on its own
// explanatory prose.
func TestTheMCPDocDoesNotClaimReadOnlyToolsRun(t *testing.T) {
	b, err := readMCPDoc()
	if err != nil {
		t.Fatalf("read mcp.go: %v", err)
	}
	if strings.Contains(b, "Read-only tools run\n// through Phase 0/1; mutating tools cannot run until the flag is set.") {
		t.Error("mcp.go again states that read-only tools RUN in Phase 0/1, unqualified. They are permitted " +
			"by the chokepoint and unreachable in this deployment (no tool is registered, and the wired " +
			"runner denies). Say both, or the file advertises a capability that does not exist.")
	}
	if !strings.Contains(b, "TG-56") {
		t.Error("the qualification citing TG-56 was removed from mcp.go's package doc")
	}
}

func readMCPDoc() (string, error) {
	b, err := os.ReadFile("../actuation/mcp/mcp.go")
	return string(b), err
}
