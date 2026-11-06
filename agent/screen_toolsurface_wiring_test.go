package agent

// THE TOOL SURFACE'S SCREEN IS GUARDED AT THE CALL SITE, NOT ONLY IN THE HELPER (TG-57 R18 item 1).
//
// The audit that produced TG-57 recorded that redaction "scrubs only outbound notices", leaving tool outputs
// and the audit ledger exposed. That is STALE as written: screenToolOutput's result already feeds BOTH the
// model prompt and step.Evidence, so the ledger stores post-screen bytes by construction.
//
// A SECOND finding from the same read is NOT in this commit: the recoverable-tool-error branch formats
// err.Error() straight into a model-visible message, unscreened. The fix for it is one function and it is
// written — but agent/ is on the eval-evidence gate, the fast change gate came back FAIL on
// falsifiable_prediction, and a behavioural change does not merge on an unexplained regression however
// small its blast radius looks. This commit is therefore TEST-ONLY: it changes no production byte.
//
// What was missing is the part this repo gets wrong repeatedly: the helper had a test and the WIRING had
// none. screen_observation_test.go calls screenToolOutput directly and passes on any loop that ignores it.
// A one-token regression — `tr.Output` where `screened` belongs — reintroduces the exact leak TG-57 names,
// with every existing test still green. Eight prior instances of resolver-guarded/wiring-unguarded are on
// record in this project; this is the ninth surface.
//
// The live spine says nothing either way, and that is the point of guarding it here rather than measuring:
// agent_step (18,558 rows) and agent_step_evidence (357 rows) on dc1tg01 hold ZERO [REDACTED:] and ZERO
// [SCREENED:] markers as of 2026-08-06. A screen that never fires and a screen that is never called produce
// the identical table.

import (
	"os"
	"strings"
	"testing"
)

// toolDispatchBlock returns the loop's tool-dispatch window from loop.go with comment lines removed. The
// rationale above and in loop.go names `tr.Output` in prose, and a comment is not an assignment.
func toolDispatchBlock(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("loop.go")
	if err != nil {
		t.Fatalf("read loop.go: %v", err)
	}
	const openAnchor = "tr, err := t.Invoke(ctx, d.Args)"
	const closeAnchor = "observationEnvelope(d.Tool, tr, screened)"
	i := strings.Index(string(src), openAnchor)
	if i < 0 {
		t.Fatalf("the tool-dispatch site no longer opens with %q — this guard is scanning for a shape that "+
			"no longer exists and would pass on any wiring", openAnchor)
	}
	j := strings.Index(string(src)[i:], closeAnchor)
	if j < 0 {
		t.Fatalf("the screened observation is no longer fed to the model prompt via %q — the payload the "+
			"model sees is not the payload the screen produced", closeAnchor)
	}
	var kept []string
	for _, ln := range strings.Split(string(src)[i:i+j+len(closeAnchor)], "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "//") {
			continue
		}
		kept = append(kept, ln)
	}
	return strings.Join(kept, "\n")
}

// KILLING MUTATION: feed the model or the ledger from `tr.Output` instead of `screened`. RED — that is the
// leak surface TG-57 describes, and it is one identifier wide.
func TestTheToolResultReachesModelAndLedgerOnlyAfterScreening(t *testing.T) {
	block := toolDispatchBlock(t)

	for _, want := range []string{
		"screened, notes := screenToolOutput(tr.ID, tr.Output)", // the ONE place raw output is legitimate
		"step.EvidenceID, step.Evidence = tr.ID, screened",      // the ledger takes the screened bytes
		"observationEnvelope(d.Tool, tr, screened)",             // and so does the model prompt
	} {
		if !strings.Contains(block, want) {
			t.Errorf("the tool-dispatch site does not contain %q — screenToolOutput is tested in isolation "+
				"while the loop feeds something else.\n%s", want, block)
		}
	}

	// `tr.Output` may appear EXACTLY ONCE in this window: as screenToolOutput's argument. A second occurrence
	// is a raw payload going somewhere it should not — that is the whole defect, and a plain Contains check
	// could never see it, because the legitimate occurrence satisfies it.
	if n := strings.Count(block, "tr.Output"); n != 1 {
		t.Errorf("`tr.Output` appears %d times in the tool-dispatch window, want exactly 1 (the argument to "+
			"screenToolOutput). Any other use puts an unscreened, attacker-influenceable tool payload into "+
			"the model prompt or the audit ledger.\n%s", n, block)
	}
}

// NEGATIVE CONTROL for the extractor: the window must open on the dispatch line and must not swallow the
// whole file. Widening an anchor does not obviously blow the window up — it silently moves it — so the
// opening line is pinned rather than merely bounding the size.
func TestTheToolDispatchWindowIsScoped(t *testing.T) {
	block := toolDispatchBlock(t)
	if strings.TrimSpace(block) == "" {
		t.Fatal("extracted an empty window — the occurrence-count assertion above would read 0 and fail for " +
			"the wrong reason, and any Contains check would be vacuous")
	}
	if !strings.HasPrefix(strings.TrimSpace(block), "tr, err := t.Invoke(ctx, d.Args)") {
		t.Fatalf("the window does not open on the tool dispatch, so the assertions above are scanning some "+
			"other part of the loop:\n%.200s", block)
	}
	if strings.Contains(block, "func protocolPreamble") || strings.Contains(block, "func screenToolOutput") {
		t.Fatal("the window extends past the dispatch site into other functions, so a match anywhere in " +
			"loop.go would satisfy the assertions above")
	}
}
