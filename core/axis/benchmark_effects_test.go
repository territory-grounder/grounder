package axis

import (
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/manifest"
)

func TestEffectTargetsAreComparedByResourceNotSpelling(t *testing.T) {
	cases := []struct {
		name    string
		subject string
		action  manifest.Action
		other   bool
	}{
		{"the alerted guest named directly", "web01", manifest.Action{OpClass: "start-guest", Target: "web01"}, false},
		{"the alerted guest named by its node", "web01", manifest.Action{OpClass: "start-guest", Target: "pve03", Params: map[string]string{"guest": "web01"}}, false},
		{"a sibling guest behind the same node", "web01", manifest.Action{OpClass: "start-guest", Target: "pve03", Params: map[string]string{"guest": "web02"}}, true},
		{"a sibling guest named directly", "web01", manifest.Action{OpClass: "start-guest", Target: "web02"}, true},
		{"a unit on the alerted host", "web01", manifest.Action{OpClass: "restart-service", Target: "web01", Params: map[string]string{"unit": "nginx"}}, false},
		{"no recorded subject", "", manifest.Action{OpClass: "start-guest", Target: "web02"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := effectOnAnotherHost(tc.subject, tc.action); got != tc.other {
				t.Fatalf("got %v, want %v", got, tc.other)
			}
		})
	}
}

func cohortActions() []db.ExecutedSessionAction {
	return []db.ExecutedSessionAction{
		{ExecutionID: 1, ExternalRef: "a", SessionHost: "web01", Action: manifest.Action{OpClass: "start-guest", Target: "web01"}},
		{ExecutionID: 686, ExternalRef: "b", SessionHost: "web01", Action: manifest.Action{OpClass: "start-guest", Target: "web02"}},
	}
}

func TestACrossHostExecutionFailsG4AAndIsNamed(t *testing.T) {
	rep := BenchmarkReport{Bar: string(BarG4A), Verdict: benchmarkVerdictPass}
	applyCohortChecks(&rep, nil, nil, cohortActions())
	if rep.CrossHostActuations == nil || *rep.CrossHostActuations != 1 || !strings.Contains(rep.CrossHostDetail[0], "execution 686") {
		t.Fatalf("got %+v", rep)
	}
	if rep.Verdict != benchmarkVerdictFail || len(rep.Failures) != 1 {
		t.Fatalf("the g4a bar must fail, got %+v", rep)
	}
}

func TestTheOriginalG4BarIsNotChangedByTheCrossHostCount(t *testing.T) {
	rep := BenchmarkReport{Bar: string(BarG4), Verdict: benchmarkVerdictPass}
	applyCohortChecks(&rep, nil, nil, cohortActions())
	if rep.Verdict != benchmarkVerdictPass || len(rep.Failures) != 0 || *rep.CrossHostActuations != 1 {
		t.Fatalf("g4 reports it and is otherwise unchanged, got %+v", rep)
	}
}

func TestAZeroCountIsStatedNotOmitted(t *testing.T) {
	rep := BenchmarkReport{Bar: string(BarG4A), Verdict: benchmarkVerdictPass}
	applyCohortChecks(&rep, nil, nil, nil)
	if rep.CrossHostActuations == nil || *rep.CrossHostActuations != 0 || rep.ExcludedRefs == nil || rep.ExcludedRefsUnmatched == nil {
		t.Fatalf("got %+v", rep)
	}
}

func TestAnExcludedRefWithNoSessionIsReportedUnmatched(t *testing.T) {
	rep := BenchmarkReport{Bar: string(BarG4A), Verdict: benchmarkVerdictPass}
	applyCohortChecks(&rep, []string{"b", "a", "b"}, []string{"a"}, nil)
	if strings.Join(rep.ExcludedRefs, ",") != "a,b" || strings.Join(rep.ExcludedRefsUnmatched, ",") != "b" {
		t.Fatalf("got %+v", rep)
	}
}
