package main

import (
	"os"
	"strings"
	"testing"
)

// ★ THE ARM GUARD MUST CHECK IDENTITY, NOT SPELLING (TG-356).
//
// The frontier cross-check refuses to arm when the frontier tier "equals" the local judge tier — by string.
// A litellm model_name is an ALIAS, and on this estate `judge` and `fallback-deepseek` both resolve to
// deepseek/deepseek-v4-pro. That pair passes a name check and arms the judge as its own anchor.
//
// adapters/model proves SameUpstreamModel behaves. Only this proves main() ASKS it — and a mutation that
// replaced the call with a hardcoded "different models" verdict passed the entire adapters suite while the
// guard did nothing. Fifth time this shape has come up: the resolver guarded, the call site not.
func TestTheArmGuardResolvesUpstreamModels(t *testing.T) {
	src := stripFrontierComments(readFrontierMain(t))

	if !strings.Contains(src, "gw.SameUpstreamModel(") {
		t.Fatal("main.go does not call SameUpstreamModel when arming the frontier cross-check. The remaining " +
			"name comparison passes any two DIFFERENT aliases of one model, so the anchor can be the judge " +
			"itself — the exact blind spot the cross-check exists to close.")
	}
	// The name check must SURVIVE as well: it is the cheap path that still catches the literal same-tier
	// case when the gateway cannot be reached.
	if !strings.Contains(src, "frontierTier == localTier") {
		t.Error("the literal tier-equality case was removed. It is the fallback when resolution fails; " +
			"without it an unreachable gateway means no independence check at all.")
	}
}

// An unverified claim must be SAID, not implied by silence. "the guard passed" and "the guard could not
// check" are different facts and only one is reassuring.
func TestUnverifiedIndependenceIsAnnounced(t *testing.T) {
	src := stripFrontierComments(readFrontierMain(t))
	if !strings.Contains(src, "UNVERIFIED") {
		t.Error("when the gateway cannot resolve the tiers, main.go arms without announcing that " +
			"independence is UNVERIFIED. An operator then reads a successful arm as a proven-independent " +
			"anchor, which is the claim this whole check exists to make honestly.")
	}
}

func TestTheFrontierWiringGuardIgnoresProse(t *testing.T) {
	prose := "// gw.SameUpstreamModel(ctx, a, b)\nfunc main() {}\n"
	if got := stripFrontierComments(prose); strings.Contains(got, "gw.SameUpstreamModel(") {
		t.Fatalf("stripFrontierComments left commented-out code in place; got %q", got)
	}
}

func readFrontierMain(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("main.go is empty — the assertions would be vacuous")
	}
	return string(b)
}

func stripFrontierComments(src string) string {
	var b strings.Builder
	for _, l := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "//") {
			continue
		}
		b.WriteString(l)
		b.WriteString("\n")
	}
	return b.String()
}
