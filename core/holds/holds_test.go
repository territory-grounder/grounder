package holds

import (
	"sort"
	"strings"
	"testing"
	"time"
)

// The sticky set is the whole policy: a reason that is about the ESTATE or the ACTOR survives re-detection; a
// reason about the PROPOSAL or about TG's own first-look inputs does not. Both halves are pinned, including the
// owner's carve-out (authorized-test is never a hold, so it is never sticky).
func TestStickyReasonsAreTheEstateAndActorFacts(t *testing.T) {
	for _, r := range []string{
		"actor-attributed-authorized", "actor-attribution-escalate", "actor-attributed-suspicious",
		"stateful-workload-mutation", "server-derived-destructive-op", "irreversible-or-never-auto-floor",
		"self-protected-control-plane-restart", "irreversible-op-class", ReasonHumanDeny,
	} {
		if !Sticky(r) {
			t.Errorf("%q must be sticky: it describes the estate or the actor, which a re-detection cannot refresh", r)
		}
		if Reading(r) == r {
			t.Errorf("%q has no plain-language reading", r)
		}
	}
	for _, r := range []string{
		"rationale-names-a-different-host", "no-committed-prediction", "ood-novel-incident",
		"op-class-not-graduated", "canary-policy-pinned", "authorized-test", "", "unrecorded",
		"auto-resolve-evidence-unbound", "verdict-deviation-or-invalid",
		ReasonPriorHold,
	} {
		if Sticky(r) {
			t.Errorf("%q must NOT be sticky: it is re-judged on fresh input every time (and a re-hold is never a hold of its own)", r)
		}
	}
	if Reading(ReasonPriorHold) == ReasonPriorHold {
		t.Error("the re-hold reason still needs a plain-language reading for the console")
	}
	keys := StickyReasons()
	if len(keys) != len(stickyReasons) || !sort.StringsAreSorted(keys) {
		t.Fatalf("StickyReasons must be the complete, sorted set: %v", keys)
	}
	for _, k := range keys {
		if !Sticky(k) {
			t.Errorf("StickyReasons lists %q but Sticky denies it", k)
		}
	}
}

func TestReasonFromDecisionOnlyNamesHolds(t *testing.T) {
	cases := []struct {
		decision string
		key      string
		hold     bool
	}{
		{"fallback:autonomous-hold:stateful-workload-mutation", "stateful-workload-mutation", true},
		{"fallback:autonomous-hold:", "unrecorded", true},
		{"human:deny", ReasonHumanDeny, true},
		{"fallback:autonomous-approve:ood-novel-incident", "", false},
		{"human:approve", "", false},
		{"human:timeout", "", false},
		{"human:vote-admitted-unconfigured", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		key, hold := ReasonFromDecision(c.decision)
		if key != c.key || hold != c.hold {
			t.Errorf("ReasonFromDecision(%q) = (%q, %v), want (%q, %v)", c.decision, key, hold, c.key, c.hold)
		}
	}
}

func TestOpenIsBoundedByExpiry(t *testing.T) {
	now := time.Date(2026, 11, 7, 20, 0, 0, 0, time.UTC)
	h := HoldDecision{DecidedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)}
	if !h.Open(now) {
		t.Fatal("a hold inside its window is open")
	}
	if h.Open(now.Add(2 * time.Hour)) {
		t.Fatal("a hold past its window is closed")
	}
	if h.Open(h.ExpiresAt) {
		t.Fatal("expiry is exclusive: at ExpiresAt the hold is closed")
	}
	if (HoldDecision{}).Open(now) {
		t.Fatal("a hold with no expiry is never open — an unbounded hold would be a permanent deny")
	}
}

func TestDetailNamesTheDecisionBeingHonoured(t *testing.T) {
	now := time.Date(2026, 11, 7, 20, 0, 0, 0, time.UTC)
	d := Detail(HoldDecision{ReasonKey: "actor-attributed-authorized", ExternalRef: "INC-1", DecidedBy: "tg:fallback-approver",
		DecidedAt: now, ExpiresAt: now.Add(DefaultRedetectWindow)})
	for _, want := range []string{"actor-attributed-authorized", "INC-1", "tg:fallback-approver", "2026-11-07T20:00:00Z", "2026-11-08T20:00:00Z"} {
		if !strings.Contains(d, want) {
			t.Errorf("detail %q lacks %q", d, want)
		}
	}
}
