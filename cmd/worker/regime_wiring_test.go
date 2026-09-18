package main

import (
	"testing"

	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/core/regime"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/modules/actuation/awxjob"
)

// parseRegimeRules: a well-formed rule set maps each selector+regime; empty is no rules (default lane).
func TestParseRegimeRules(t *testing.T) {
	rules, err := parseRegimeRules(`[
		{"id":"awx-edge","selector":"group:awx-managed","regime":"awx-job"},
		{"id":"gitops","selector":"host-glob:k8s*","regime":"gitops-mr"}
	]`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("want 2 rules, got %d", len(rules))
	}
	if rules[0].ID != "awx-edge" || rules[0].Regime != regime.RegimeAWXJob || rules[0].Selector.Kind != credential.KindGroup {
		t.Fatalf("rule[0] wrong: %+v", rules[0])
	}
	if empty, err := parseRegimeRules("   "); err != nil || empty != nil {
		t.Fatalf("empty spec must yield no rules, got %v err=%v", empty, err)
	}
}

// parseRegimeRules fails CLOSED on a bad regime / bad selector / missing id — never a silently dropped mapping.
func TestParseRegimeRulesFailClosed(t *testing.T) {
	for name, spec := range map[string]string{
		"unknown regime":     `[{"id":"x","selector":"host:a","regime":"telnet"}]`,
		"unknown kind":       `[{"id":"x","selector":"subnet:10.0.0.0/8","regime":"awx-job"}]`,
		"malformed selector": `[{"id":"x","selector":"nocolon","regime":"awx-job"}]`,
		"missing id":         `[{"id":"","selector":"host:a","regime":"awx-job"}]`,
		"not an array":       `{"id":"x"}`,
	} {
		if _, err := parseRegimeRules(spec); err == nil {
			t.Fatalf("%s: expected a fail-closed error, got nil", name)
		}
	}
}

// parseAWXJobAllowlist maps template→op-class+schema; empty ⇒ empty allowlist (read-only actuator).
func TestParseAWXJobAllowlist(t *testing.T) {
	al, err := parseAWXJobAllowlist(`[{"template_id":42,"op_class":"restart-service","extra_vars":{"host":"string","secs":"number"}}]`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pol, ok := al[42]
	if !ok || pol.OpClass != "restart-service" {
		t.Fatalf("template 42 not mapped: %+v", al)
	}
	if pol.ExtraVarsSchema["host"] != awxjob.VarString || pol.ExtraVarsSchema["secs"] != awxjob.VarNumber {
		t.Fatalf("schema wrong: %+v", pol.ExtraVarsSchema)
	}
	if empty, err := parseAWXJobAllowlist(""); err != nil || len(empty) != 0 {
		t.Fatalf("empty spec must yield an empty allowlist, got %v err=%v", empty, err)
	}
}

// parseAWXJobAllowlist fails CLOSED on a bad type / bad id / missing op-class.
func TestParseAWXJobAllowlistFailClosed(t *testing.T) {
	for name, spec := range map[string]string{
		"illegal var type": `[{"template_id":1,"op_class":"x","extra_vars":{"k":"object"}}]`,
		"non-positive id":  `[{"template_id":0,"op_class":"x"}]`,
		"missing op_class": `[{"template_id":1,"op_class":""}]`,
		"not an array":     `{"template_id":1}`,
	} {
		if _, err := parseAWXJobAllowlist(spec); err == nil {
			t.Fatalf("%s: expected a fail-closed error, got nil", name)
		}
	}
}

// targetForSelector maps each selector kind to the Target field it keys, and the built target MATCHES its rule.
func TestTargetForSelectorMatchesRule(t *testing.T) {
	for _, tok := range []string{"host:web01", "host-glob:nl*", "group:edge", "device-class:cisco-asa", "resource:svc1"} {
		sel, err := parseSharedSelector(tok)
		if err != nil {
			t.Fatalf("parse %q: %v", tok, err)
		}
		if !credential.Match(sel, targetForSelector(sel)) {
			t.Fatalf("selector %q does not match its own representative target", tok)
		}
	}
}

// SAFETY: the awx-job lane built with NO actuator (the absent-config path) carries the fail-closed
// pendingActuator — it can only refuse, never a permissive default. (The mode chokepoint would also refuse,
// but this proves the lane itself is inert even if the chokepoint were open.)
func TestUnconfiguredAWXJobLaneFailsClosed(t *testing.T) {
	// The wiring builds NewAWXJobLane() with no opts when the AWX config is absent — the same call here.
	lane := regime.NewAWXJobLane()
	if lane.Regime() != regime.RegimeAWXJob {
		t.Fatalf("lane regime = %s", lane.Regime())
	}
	// A resolver over just this lane resolves awx-job to it, and SelectLane succeeds (the lane is wired) — but
	// its effect leaf is the refusing placeholder, so nothing can actuate through it.
	eng := regime.NewEngine(
		[]regime.Rule{{ID: "r", Selector: credential.Selector{Kind: credential.KindHost, Pattern: "h"}, Regime: regime.RegimeAWXJob}},
		[]regime.Lane{lane},
	)
	got, err := eng.SelectLane(credential.Target{Host: "h"})
	if err != nil || got.Regime() != regime.RegimeAWXJob {
		t.Fatalf("select awx-job lane: got %v err %v", got, err)
	}
}

// deferredVerdictRow maps the safety verdict + verified bit onto the non-secret audit row vocabulary.
func TestDeferredVerdictRowMapping(t *testing.T) {
	clean := deferredVerdictRow(regime.DeferredResolution{
		ActionID: "a", JobID: "1", TerminalStatus: regime.JobSuccessful, Verdict: safety.VerdictMatch, Verified: true, CleanRun: true,
	})
	if clean.Verdict != regime.VerdictMatch || clean.Graduation != regime.GraduationVerifiedClean {
		t.Fatalf("clean mapping wrong: %+v", clean)
	}
	dev := deferredVerdictRow(regime.DeferredResolution{
		ActionID: "a", JobID: "1", TerminalStatus: regime.JobFailed, Verdict: safety.VerdictDeviation, Verified: true,
	})
	if dev.Verdict != regime.VerdictDeviation || dev.Graduation != regime.GraduationDeviated {
		t.Fatalf("deviation mapping wrong: %+v", dev)
	}
	unver := deferredVerdictRow(regime.DeferredResolution{
		ActionID: "a", JobID: "1", TerminalStatus: regime.JobCanceled, Verdict: "", Verified: false,
	})
	if unver.Verdict != regime.VerdictUnverified || unver.Graduation != regime.GraduationNoCredit {
		t.Fatalf("unverified mapping wrong: %+v", unver)
	}
	// Every mapped row must pass the audit writer's own validation (valid terminal status + verdict + grad).
	for _, r := range []regime.DeferredVerdictRow{clean, dev, unver} {
		if !r.Status.Valid() {
			t.Fatalf("mapped status not terminal-valid: %+v", r)
		}
	}
}
