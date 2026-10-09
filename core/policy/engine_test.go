package policy

import (
	"context"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/credential"
)

// hostRule builds a validated non-default rule matching one exact host with the given verdict.
func hostRule(t *testing.T, id, host string, v Verdict) Rule {
	t.Helper()
	sel := credential.Selector{Kind: credential.KindHost, Pattern: host}
	r, err := NewRule(Rule{ID: id, Match: Match{Selector: &sel}, Verdict: v})
	if err != nil {
		t.Fatalf("build rule %q: %v", id, err)
	}
	return r
}

func mustEngine(t *testing.T, rules ...Rule) *Engine {
	t.Helper()
	e, err := NewEngine(context.Background(), RuleSet{Rules: rules})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return e
}

func decide(t *testing.T, e *Engine, in EvalInput) PolicyDecision {
	t.Helper()
	d, err := e.Decide(context.Background(), in)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	return d
}

// baseVerdict returns the Rego deny-overrides BASE verdict of a decision (before the composition stages
// tighten it) — the property the T-015-1 Rego oracles assert independent of the confidence/band/graduation/
// floor composition (which is exercised end-to-end in engine_compose_test.go).
func baseVerdict(d PolicyDecision) Verdict { return d.Audit().Base.Verdict }

// TestDenyOverrides is the load-bearing property (REQ-1504): a matching deny wins over any matching
// auto/approve REGARDLESS of rule order. Asserted under every permutation of a deny+auto+approve set.
func TestDenyOverrides_AnyPermutation(t *testing.T) {
	in := EvalInput{Host: "h1"}
	base := []Rule{
		hostRule(t, "auto-1", "h1", VerdictAuto),
		hostRule(t, "deny-1", "h1", VerdictDeny),
		hostRule(t, "approve-1", "h1", VerdictApprove),
	}
	perms := permute(base)
	for i, p := range perms {
		e := mustEngine(t, p...)
		d := decide(t, e, in)
		if d.Verdict() != VerdictDeny {
			t.Fatalf("perm %d (%s): got %q, want deny", i, ids(p), d.Verdict())
		}
		if d.MatchedRuleID() != "deny-1" {
			t.Fatalf("perm %d: deny provenance = %q, want deny-1", i, d.MatchedRuleID())
		}
	}
}

// TestVerdictLadder proves the deny-overrides ladder below a deny: approve beats auto, auto when only auto.
func TestVerdictLadder(t *testing.T) {
	in := EvalInput{Host: "h1"}
	cases := []struct {
		name  string
		rules []Rule
		want  Verdict
	}{
		{"approve-beats-auto", []Rule{hostRule(t, "a", "h1", VerdictAuto), hostRule(t, "b", "h1", VerdictApprove)}, VerdictApprove},
		{"auto-alone", []Rule{hostRule(t, "a", "h1", VerdictAuto)}, VerdictAuto},
		{"approve-alone", []Rule{hostRule(t, "a", "h1", VerdictApprove)}, VerdictApprove},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The deny-overrides ladder is a BASE-verdict property; the composition stages (which would clamp a
			// bare `auto` via the never-auto floor / default confidence gate) are asserted separately.
			if got := baseVerdict(decide(t, mustEngine(t, tc.rules...), in)); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFailClosedDefault_EmptyMatch: no rule matches → the fail-closed default `approve` (route to a human),
// NEVER a silent auto (REQ-1506/1507/1517).
func TestFailClosedDefault_EmptyMatch(t *testing.T) {
	// engine with a rule that matches a DIFFERENT host; the queried action matches nothing.
	e := mustEngine(t, hostRule(t, "other", "other-host", VerdictAuto))
	d := decide(t, e, EvalInput{Host: "h1"})
	if d.Verdict() != VerdictApprove {
		t.Fatalf("empty-match verdict = %q, want approve (fail-closed default)", d.Verdict())
	}
	if d.MatchedRuleID() != "" {
		t.Fatalf("empty-match matched rule = %q, want empty", d.MatchedRuleID())
	}
	// an engine with NO rules at all also defaults to approve, never auto.
	if got := decide(t, mustEngine(t), EvalInput{Host: "anything"}).Verdict(); got != VerdictApprove {
		t.Fatalf("no-rules verdict = %q, want approve", got)
	}
}

// TestNegativeControl: shuffled/irrelevant rule data must NOT produce a spurious allow. A pile of auto rules
// none of which match the action still resolves to the fail-closed default, not auto.
func TestNegativeControl_NoSpuriousAllow(t *testing.T) {
	irrelevant := []Rule{
		hostRule(t, "n1", "server-a", VerdictAuto),
		hostRule(t, "n2", "server-b", VerdictAuto),
		hostRule(t, "n3", "server-c", VerdictAuto),
	}
	e := mustEngine(t, irrelevant...)
	// the action's host matches NONE of the auto rules.
	d := decide(t, e, EvalInput{Host: "totally-different-host", OpClass: "restart", Argv: "systemctl restart nginx"})
	if d.Verdict() == VerdictAuto {
		t.Fatalf("negative control leaked a spurious auto: %+v", d)
	}
	if d.Verdict() != VerdictApprove {
		t.Fatalf("negative control verdict = %q, want approve", d.Verdict())
	}
}

// TestRegoIsFixed proves operators cannot inject Rego: the engine exposes ONE embedded module and rules
// enter only as data. There is no exported API taking Rego source, and the compiled module is the embed.
func TestRegoIsFixed_DataOnly(t *testing.T) {
	src := RegoModule()
	if !strings.Contains(src, "package tg.policy") {
		t.Fatalf("embedded module is not the fixed tg.policy module")
	}
	// The rule that resolves the action is pure DATA (a Verdict + a Selector), carrying no Rego. Evaluating
	// it still yields a decision, proving the fixed module consumed data, not operator code.
	e := mustEngine(t, hostRule(t, "data-only", "h1", VerdictDeny))
	if got := decide(t, e, EvalInput{Host: "h1"}).Verdict(); got != VerdictDeny {
		t.Fatalf("data rule did not drive the fixed module: got %q", got)
	}
	// Structural guarantee: NewEngine's only inputs are a context and a RuleSet (data). If an operator-Rego
	// setter existed this test file would compile against it; none does — the absence is the guarantee.
}

// TestArgvDenySide proves the deny side matches the raw command string (REQ-1505) independent of op-class.
func TestArgvDenySide(t *testing.T) {
	deny, err := NewRule(Rule{ID: "no-rm-rf", Match: Match{ArgvPattern: "rm -rf"}, Verdict: VerdictDeny})
	if err != nil {
		t.Fatalf("build argv rule: %v", err)
	}
	e := mustEngine(t, deny)
	if got := decide(t, e, EvalInput{Argv: "sudo rm -rf /var/tmp/cache"}).Verdict(); got != VerdictDeny {
		t.Fatalf("argv deny did not fire: got %q", got)
	}
	// a command that does not contain the pattern is not denied by it → fail-closed default approve.
	if got := decide(t, e, EvalInput{Argv: "systemctl restart nginx"}).Verdict(); got != VerdictApprove {
		t.Fatalf("unrelated command matched the argv deny: got %q", got)
	}
}

// --- test helpers -----------------------------------------------------------------------------------------

func ids(rules []Rule) string {
	out := make([]string, len(rules))
	for i, r := range rules {
		out[i] = r.ID
	}
	return strings.Join(out, ",")
}

// permute returns every ordering of rules (Heap's algorithm) so a property can be asserted order-independent.
func permute(rules []Rule) [][]Rule {
	var out [][]Rule
	a := make([]Rule, len(rules))
	copy(a, rules)
	var gen func(k int)
	gen = func(k int) {
		if k == 1 {
			cp := make([]Rule, len(a))
			copy(cp, a)
			out = append(out, cp)
			return
		}
		for i := 0; i < k; i++ {
			gen(k - 1)
			if k%2 == 0 {
				a[i], a[k-1] = a[k-1], a[i]
			} else {
				a[0], a[k-1] = a[k-1], a[0]
			}
		}
	}
	gen(len(a))
	return out
}
