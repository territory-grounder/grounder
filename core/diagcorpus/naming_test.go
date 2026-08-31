package diagcorpus

import "testing"

// The diagnosis-NAMING ground truth (TG-542): the commensurable oracle for the correct_diagnosis QUALITY
// judge, as opposed to the action-POLICY oracle Score applies. These tests drive the LOGIC over synthetic
// diagnostic text — the mechanismTerms MAP is provisional and validated against the campaign-#3 fresh
// population, but the scoring logic and the outcome plumbing must be correct now.

func namingItem(fault, conclusion, diagnosis string) Item {
	return Item{ExternalRef: "x", Host: "h", AlertRule: "r", FaultType: fault, Conclusion: conclusion, Diagnosis: diagnosis}
}

func TestScoreDiagnosisNaming(t *testing.T) {
	cases := []struct {
		name string
		it   Item
		want DiagNaming
	}{
		{"device-down named", namingItem("device-down", "Device is DOWN (SNMP unreachable), guest not responding", ""), Named},
		{"device-down recovered stand-down is Named", namingItem("device-down", "The guest is currently UP, the condition self-resolved, no action warranted", ""), Named},
		{"disk-fill named", namingItem("disk-fill", "root filesystem on / is >= 90% in use, disk space low", ""), Named},
		{"container-down named", namingItem("container-down", "the container has exited and is not running", ""), Named},
		{"service-down named via typed diagnosis", namingItem("service-down", "", "the systemd unit is inactive; service down"), Named},
		{"wrong/unrelated diagnosis is Unnamed", namingItem("disk-fill", "the network path to the upstream router is degraded and BGP flapped", ""), Unnamed},
		{"empty text is NoDiagText", namingItem("device-down", "", ""), NoDiagText},
		{"whitespace-only text is NoDiagText", namingItem("device-down", "   ", ""), NoDiagText},
	}
	for _, c := range cases {
		if got := ScoreDiagnosisNaming(c.it); got != c.want {
			t.Errorf("%s: ScoreDiagnosisNaming = %q, want %q", c.name, got, c.want)
		}
	}
}

// The outcome plumbing: Named→Truth=true, Unnamed→false, and the exclusions (unhealable class, no diagnostic
// text, unjudged session) DROP the item exactly as JudgeOutcomes drops its own — so the two calibrations run
// over the same eligibility, differing only in the ground-truth source.
func TestJudgeDiagnosisOutcomes(t *testing.T) {
	rs := rules()
	items := []Item{
		{ExternalRef: "named-hi", FaultType: "device-down", Conclusion: "guest is down, unreachable"},    // Named, judged high
		{ExternalRef: "unnamed-hi", FaultType: "disk-fill", Conclusion: "the switch uplink is flapping"}, // Unnamed, judged high → the FP a diagnosis judge must catch
		{ExternalRef: "no-text", FaultType: "device-down", Conclusion: ""},                               // excluded: nothing to name
		{ExternalRef: "unhealable", FaultType: "mem-pressure", Conclusion: "memory pressure high"},       // excluded: Score==Excluded
		{ExternalRef: "unjudged", FaultType: "device-down", Conclusion: "guest is down"},                 // excluded: no judge call
	}
	judged := map[string]bool{"named-hi": true, "unnamed-hi": true, "no-text": true, "unhealable": true}
	out := JudgeDiagnosisOutcomes(items, rs, judged)
	if len(out) != 2 {
		t.Fatalf("expected exactly 2 scored outcomes (named-hi, unnamed-hi); the rest are excluded — got %d: %+v", len(out), out)
	}
	var named, unnamed *bool
	for i := range out {
		if out[i].Truth {
			named = &out[i].Judge
		} else {
			unnamed = &out[i].Judge
		}
	}
	if named == nil || !*named {
		t.Error("the Named session judged-high must be Truth=true, Judge=true (a true positive)")
	}
	if unnamed == nil || !*unnamed {
		t.Error("the Unnamed session judged-high must be Truth=false, Judge=true (the FALSE POSITIVE a diagnosis-quality calibration exists to expose)")
	}
}
