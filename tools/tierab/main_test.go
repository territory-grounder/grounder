package main

import (
	"encoding/json"
	"testing"

	"github.com/territory-grounder/grounder/eval/tierab"
)

// ★ UNSET AND EXPLICITLY-EMPTY ARE DIFFERENT ANSWERS, and collapsing them breaks in opposite directions.
// A manifest that simply omits caller_prefix must get the SAFE default ("runner:"), so the eval judge's
// own primary-tier calls cannot be folded into the arm it is scoring. A manifest that explicitly sets ""
// is a deliberate "attribute everything in the window", which is what eval/tier-ab.sh asks for because it
// bounds the window to the agent phase instead (LiteLLM drops `user`, so the prefix would starve — TG-319).
//
// KILLING MUTATION (executed 2026-08-04): make CallerPrefix a plain `string` rather than a `*string`, so
// callerPrefix() cannot tell the two apart. RED — with the field defaulting to "" an omitted key silently
// attributes ALL in-window traffic; with it defaulting to the agent prefix an explicit "" is ignored and
// every arm starves to UNKNOWN. There is no single value that is correct for both, which is the point.
func TestAnOmittedCallerPrefixDefaultsSafeAndAnExplicitEmptyOneIsHonoured(t *testing.T) {
	var omitted armEntry
	if err := json.Unmarshal([]byte(`{"name":"ARM-CONTROL"}`), &omitted); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := omitted.callerPrefix(); got != tierab.AgentCallerPrefix {
		t.Fatalf("an omitted caller_prefix resolved to %q, want the SAFE default %q — a manifest that says "+
			"nothing must not silently attribute the eval judge's own primary-tier calls to the arm it scores",
			got, tierab.AgentCallerPrefix)
	}

	var explicit armEntry
	if err := json.Unmarshal([]byte(`{"name":"ARM-CONTROL","caller_prefix":""}`), &explicit); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := explicit.callerPrefix(); got != "" {
		t.Fatalf("an explicit caller_prefix:\"\" resolved to %q — the driver sets it deliberately because "+
			"LiteLLM drops `user` and the prefix would starve every arm to UNKNOWN (TG-319)", got)
	}

	var named armEntry
	if err := json.Unmarshal([]byte(`{"name":"ARM-CONTROL","caller_prefix":"runner:IFR-"}`), &named); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := named.callerPrefix(); got != "runner:IFR-" {
		t.Fatalf("an explicit caller_prefix was rewritten to %q", got)
	}
}

// ★ THE ARM SET IS PART OF AN ARCHIVED RECORD'S IDENTITY. Two runs on the same day at the same SHA with
// DIFFERENT arms are different experiments; keying the record on (date, kind, sha) alone overwrites one
// with the other. That happened on 2026-08-04 — the arm-haiku/arm-opus positive control landed on top of
// the collapsed default-arm verdict and the TG-204 finding was gone until the run was repeated.
//
// KILLING MUTATION (executed): drop armSlug from the archive path. RED — the collapsed default-arm run and
// the distinct haiku/opus run resolve to one directory and the second silently replaces the first.
func TestDifferentArmSetsArchiveToDifferentRecords(t *testing.T) {
	defaultArms := []armEntry{
		{Name: tierab.ArmControl, InvestigateTier: "fast", DecideTier: "primary"},
		{Name: tierab.ArmStrong, InvestigateTier: "primary", DecideTier: "primary"},
		{Name: tierab.ArmCheap, InvestigateTier: "fast", DecideTier: "fast"},
	}
	tierArms := []armEntry{
		{Name: tierab.ArmControl, InvestigateTier: "arm-haiku", DecideTier: "arm-opus"},
		{Name: tierab.ArmStrong, InvestigateTier: "arm-opus", DecideTier: "arm-opus"},
		{Name: tierab.ArmCheap, InvestigateTier: "arm-haiku", DecideTier: "arm-haiku"},
	}
	if armSlug(defaultArms) == armSlug(tierArms) {
		t.Fatal("the collapsed default-arm run and the distinct haiku/opus run resolve to ONE archive " +
			"directory — the second silently replaces the first, and the finding is lost")
	}
	// Stable across re-runs of the SAME arms: an MR re-running its own experiment must overwrite its own
	// record rather than accumulate near-duplicates.
	if armSlug(defaultArms) != armSlug(defaultArms) {
		t.Fatal("armSlug is not deterministic — a re-run would litter eval/history with near-duplicates")
	}
	if len(armSlug(defaultArms)) != 8 {
		t.Errorf("armSlug should be a short 8-hex-char digest, got %q", armSlug(defaultArms))
	}
}

// Every outcome that is not a measurement must exit NON-ZERO, and the two non-measurements must be
// distinguishable: COLLAPSED is a configuration fact (fix litellm), UNKNOWN is a telemetry fact (fix the
// log access). A caller that cannot tell them apart re-runs an expensive benchmark to fix a missing file.
func TestOnlyAMeasuredRunExitsZeroAndTheTwoNonMeasurementsDiffer(t *testing.T) {
	measured := exitFor(tierab.Verdict{Outcome: tierab.OutcomeMeasured})
	collapsed := exitFor(tierab.Verdict{Outcome: tierab.OutcomeCollapsed})
	unknown := exitFor(tierab.Verdict{Outcome: tierab.OutcomeUnknown})
	if measured != 0 {
		t.Errorf("a measured run exited %d, want 0", measured)
	}
	if collapsed == 0 || unknown == 0 {
		t.Fatalf("a non-measurement exited zero (collapsed=%d unknown=%d) — nothing about model tier was "+
			"measured in either case, and a zero status certifies it", collapsed, unknown)
	}
	if collapsed == unknown {
		t.Errorf("COLLAPSED and UNKNOWN share exit status %d — the first is fixed in litellm and the second "+
			"by pointing at the right proxy log; a caller that cannot tell them apart re-runs the benchmark", collapsed)
	}
}
