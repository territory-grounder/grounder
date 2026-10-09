package main

import (
	"os"
	"strings"
	"testing"
)

// TG-112. `mutation_enabled` was the retired vocabulary; `tg_may_actuate` is the name. During the
// deprecation window the grounder emitted BOTH from one read, and the alert rule carried
// `or mutation_enabled == 1` because dropping the alias while any process published only the old name
// would have blinded that process (measured live 2026-08-06: tg_may_actuate had exactly two publishers,
// worker and worker-actuate, and no grounder series).
//
// That window is CLOSED: every consumer — alert.rules.yml, safety.json, the console, shadowbench — joins
// on tg_may_actuate / tg_policy_mode, so these tests now pin the END state: the current name is published
// and the alias is GONE. Reintroducing the alias (or dropping the current name) is the killing mutation.
//
// These are source assertions rather than metric-value assertions because the grounder's /metrics closure
// is built inline in main() and has no seam to call. The vacuity floor + stripper self-test keep that
// honest.

func grounderMain(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	src := stripGrounderComments(string(b))
	if len(src) < 10_000 {
		t.Fatalf("VACUITY FLOOR: main.go stripped to %d bytes — every assertion below would pass on a stub",
			len(src))
	}
	return src
}

func stripGrounderComments(src string) string {
	var out []string
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func TestStripGrounderCommentsActuallyStrips(t *testing.T) {
	got := stripGrounderComments("// tg_may_actuate\nreal()\n")
	if strings.Contains(got, "tg_may_actuate") {
		t.Fatalf("the stripper left a comment in place, so every assertion below can be satisfied by "+
			"prose — and the block under test is heavily commented. got %q", got)
	}
	if !strings.Contains(got, "real()") {
		t.Fatalf("the stripper removed real code: %q", got)
	}
}

// TestTheGrounderPublishesTheCurrentPostureName pins both halves of the retirement's end state: the
// current name is emitted, and the deprecated alias is not.
func TestTheGrounderPublishesTheCurrentPostureName(t *testing.T) {
	src := grounderMain(t)

	if !strings.Contains(src, `Name: "tg_may_actuate"`) {
		t.Error("the grounder does not publish tg_may_actuate — the posture alert cannot see this process " +
			"and the console's posture chip reads a gap as unknown.")
	}
	if strings.Contains(src, `Name: "mutation`+`_enabled"`) {
		t.Error("the grounder reintroduced the RETIRED mutation-binary alias (TG-112). Every consumer " +
			"joins on tg_may_actuate / tg_policy_mode now; a second name for the same read is how the two " +
			"drift, and the alert rule no longer carries the old-name OR-leg to catch it.")
	}
}

// TestTheGaugeUsesTheSharedRead pins tg_may_actuate to the one `enabled` expression derived from the
// single gate.MayActuate() read in the metrics closure — a second read could observe a different value
// than the one the log line and admin surface report.
func TestTheGaugeUsesTheSharedRead(t *testing.T) {
	src := grounderMain(t)

	k := strings.Index(src, `Name: "tg_may_actuate"`)
	if k < 0 {
		t.Skip("tg_may_actuate absent; the test above reports that")
	}
	window := src[k:min(k+220, len(src))]
	if !strings.Contains(window, "Value: enabled") {
		t.Errorf("tg_may_actuate does not use the shared `enabled` value — it is computing its own read, "+
			"so the gauge, the boot log and /admin/status can disagree.\nwindow:\n%s", window)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
