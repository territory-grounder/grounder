package runner

import (
	"os"
	"strings"
	"testing"
)

// TG-80 P1-4: the CANCELLED terminal must reach the session record. The interceptor's Outcome.Cancelled
// rides ExecuteResult into the workflow, which must turn it into a `cancelled:` outcome AND record the
// triage row before returning — otherwise a cancelled effect lands as "proposed" (the verify path) or as
// nothing at all. Source guard in the house pattern (pins anchor on code, comments stripped).
//
// KILLING MUTATION: delete the exec.Cancelled branch in workflow.go — the outcome pin and the record pin
// both fail, naming the regression.
func TestTG80CancelTerminalIsWiredToTheRecord(t *testing.T) {
	b, err := os.ReadFile("workflow.go")
	if err != nil {
		t.Fatalf("read workflow.go: %v", err)
	}
	var kept []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		kept = append(kept, line)
	}
	src := strings.Join(kept, "\n")
	if len(src) < 10_000 {
		t.Fatal("workflow.go read came back implausibly small — the guard would be vacuous")
	}
	idx := strings.Index(src, "if exec.Cancelled")
	if idx < 0 {
		t.Fatal("the workflow no longer branches on exec.Cancelled — a cancelled effect would be verified and recorded as proposed (TG-80 P1-4)")
	}
	tail := src[idx:]
	if !strings.Contains(tail[:min(len(tail), 600)], `res.Outcome = "cancelled:remote-killed`) {
		t.Error("the cancelled branch no longer sets the cancelled: outcome — the record would carry the wrong terminal (TG-80 P1-4)")
	}
	if !strings.Contains(tail[:min(len(tail), 600)], "recordTriage(ctx, a, env, res,") {
		t.Error("the cancelled branch no longer records the triage row — a cancelled session would leave no durable terminal (TG-80 P1-4)")
	}
	a, err := os.ReadFile("activities.go")
	if err != nil {
		t.Fatalf("read activities.go: %v", err)
	}
	if !strings.Contains(string(a), "Cancelled: out.Cancelled") {
		t.Error("ExecuteActivity no longer carries Outcome.Cancelled into ExecuteResult — the workflow branch is dead code (TG-80 P1-4)")
	}
}
