package wikicompile

import (
	"strings"
	"testing"
	"time"
)

// only returns the single article a fixture is expected to produce, so the tests read as one assertion
// rather than a tuple unpack each time.
func only(t *testing.T, in RuleInputs) Article {
	t.Helper()
	arts, _ := CompileRules(in)
	if len(arts) != 1 {
		t.Fatalf("want exactly 1 rule page, got %d", len(arts))
	}
	return arts[0]
}

func rs(ref, host, rule, raw, outcome, opClass string, mutated, clear bool, day int) RuleSession {
	return RuleSession{
		ExternalRef: ref, Host: host, Rule: rule, RawRule: raw, Outcome: outcome, OpClass: opClass,
		Mutated: mutated, ConfirmedClear: clear,
		CreatedAt: time.Date(2026, 7, day, 12, 0, 0, 0, time.UTC),
	}
}

// deviceDown is the real production family: ONE condition arriving under FOUR source names.
func deviceDown() RuleInputs {
	return RuleInputs{Sessions: []RuleSession{
		rs("a", "h1", "device-down", "Device-Down", "proposed", "start-guest", true, true, 30),
		rs("b", "h2", "device-down", "Devices-up/down", "proposed", "start-guest", true, false, 29),
		rs("c", "h1", "device-down", "Device-Down-SNMP-unreachable", "proposed", "", false, false, 28),
		rs("d", "h3", "device-down", "Device-Down-Due-to-no-ICMP-response.", "no-proposal:stop", "", false, false, 27),
	}}
}

// TestRulePagesFoldTheFamilyNotTheString — the reason this family exists at all.
//
// Production carries ONE fault class under FOUR source names (Device-Down, Devices-up/down,
// Device-Down-SNMP-unreachable, Device-Down-Due-to-no-ICMP-response.). Keying on the raw string would
// split the page four ways and hide exactly the recurrence that makes it worth reading — and it is the
// same non-folding that made a whole class of incident impossible to confirm as recovered
// (core/knowledge/rulefamily.go documents that bug).
//
// RED MUTATION CONTROL (executed 2026-08-01): grouping by RawRule instead of Rule yields 4 pages of 1
// session each and fails with the page count; restored green.
func TestRulePagesFoldTheFamilyNotTheString(t *testing.T) {
	arts, skips := CompileRules(deviceDown())
	if len(skips) != 0 {
		t.Fatalf("no refusals expected, got %+v", skips)
	}
	if len(arts) != 1 {
		t.Fatalf("four source names for ONE condition must fold into ONE page, got %d", len(arts))
	}
	if arts[0].Slug != "rule-device-down" {
		t.Errorf("slug = %q", arts[0].Slug)
	}
	body := arts[0].Body
	if !strings.Contains(body, "4 different source names denote this same condition") {
		t.Error("the page must state the vocabulary spread — that is the fact a per-host page cannot show")
	}
	// Every raw name must still be findable: an operator searches for the string the alert showed them.
	for _, raw := range []string{"Device-Down", "Devices-up/down", "Device-Down-SNMP-unreachable"} {
		if !strings.Contains(body, raw) {
			t.Errorf("raw source name %q must be rendered so the page is findable by what the alert said", raw)
		}
	}
	if !strings.Contains(body, "4 session(s) across 3 host(s)") {
		t.Errorf("the cross-host count is the headline; body:\n%s", body)
	}
}

// TestOnlyConfirmedCleanResolutionsAreListedAsHavingHeld — the section an operator acts on.
//
// A proposal never carried out is not evidence. A heal never confirmed clear is not evidence either —
// TG changed something and could not verify it held. Only mutated AND confirmed_clear earns the list.
//
// RED MUTATION CONTROL (executed 2026-08-01): counting `s.Mutated` alone as confirmed puts the
// unverified heal in the "what actually held" table; restored green.
func TestOnlyConfirmedCleanResolutionsAreListedAsHavingHeld(t *testing.T) {
	body := only(t, deviceDown()).Body
	if !strings.Contains(body, "1 confirmed-clean resolution(s)") {
		t.Errorf("exactly one session was mutated AND confirmed clear; body:\n%s", body)
	}
	// The unconfirmed heal must be counted, and counted SEPARATELY, with what it means spelled out.
	if !strings.Contains(body, "1** acted on, clear NOT confirmed") {
		t.Error("an acted-on-but-unverified session must be reported distinctly — 'TG changed something " +
			"and could not verify it held' is a different fact from a heal that worked")
	}
	// The unconfirmed one must NOT appear in the held table (ref "b").
	held := body[strings.Index(body, "## What actually held"):]
	if strings.Contains(held, "| b |") {
		t.Error("an unconfirmed heal must not be listed as having held")
	}
}

// TestRulePageWithNoConfirmedResolutionSaysSo — the honest-empty rule, applied to the section that
// matters most. "Nothing has been shown to work" and "we have not looked" must not render identically.
func TestRulePageWithNoConfirmedResolutionSaysSo(t *testing.T) {
	in := RuleInputs{Sessions: []RuleSession{
		rs("x", "h1", "disk-full", "DiskFull-90", "proposed", "prune-journal", false, false, 30),
	}}
	body := only(t, in).Body
	if !strings.Contains(body, "**Nothing yet.**") {
		t.Error("a rule with no confirmed-clean resolution must say so explicitly")
	}
	if !strings.Contains(body, "not the same as saying nothing works") {
		t.Error("it must distinguish 'the spine has not recorded one holding' from 'nothing works' — the " +
			"second is a claim about the estate this page has no basis for")
	}
}

// TestRuleCompileIsDeterministic — map iteration over hosts, raw names and outcomes is unordered in Go;
// any of them would churn every page on every compile.
func TestRuleCompileIsDeterministic(t *testing.T) {
	in := deviceDown()
	in.Sessions = append(in.Sessions,
		rs("e", "h9", "device-down", "Device-Down", "proposed", "start-guest", true, true, 30), // ties on the instant
		rs("f", "h4", "device-down", "Zebra-Rule", "proposed", "", false, false, 26),
	)
	first := only(t, in).Body
	for i := 0; i < 25; i++ {
		if only(t, in).Body != first {
			t.Fatal("rule page is not deterministic — unordered map iteration or an unstable sort would " +
				"make every compile differ even when nothing changed")
		}
	}
}

// TestHostileRuleNameIsRefusedNotRewritten — rule names arrive from inbound alert payloads, same as
// hostnames. Rewriting two rules onto one slug would merge unrelated fault classes.
func TestHostileRuleNameIsRefusedNotRewritten(t *testing.T) {
	in := RuleInputs{Sessions: []RuleSession{
		rs("x", "h1", "../../etc/passwd", "raw", "proposed", "", false, false, 30),
		rs("y", "h1", "ok-rule", "raw", "proposed", "", false, false, 30),
	}}
	arts, skips := CompileRules(in)
	if len(arts) != 1 || arts[0].Slug != "rule-ok-rule" {
		t.Errorf("only the safe rule may render, got %+v", arts)
	}
	if len(skips) != 1 || !strings.Contains(skips[0].Reason, "merge unrelated fault classes") {
		t.Errorf("the hostile rule must be refused with a reason, got %+v", skips)
	}
}

// TestBoundedReadIsReportedAsAFloor — the counts are the page's whole point, so a bounded read must not
// present them as totals.
func TestBoundedReadIsReportedAsAFloor(t *testing.T) {
	in := deviceDown()
	in.Truncated = true
	if !strings.Contains(only(t, in).Body, "every count here is a FLOOR") {
		t.Error("a truncated read must say its counts are a floor — recurrence is the decision input on " +
			"this page, and an undercount presented as a total argues the wrong way")
	}
}
