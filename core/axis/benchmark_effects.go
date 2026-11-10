package axis

import (
	"fmt"
	"sort"
	"strings"

	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/manifest"
)

func effectOnAnotherHost(subject string, a manifest.Action) (bool, string) {
	if strings.TrimSpace(subject) == "" {
		return false, ""
	}
	path := actuate.ResourcePath(a)
	if path == actuate.ResourceKeyNoTarget {
		return false, ""
	}
	acted := actuate.ResourceHost(path)
	alerted := actuate.ResourceHost(actuate.ResourcePath(manifest.Action{Target: subject}))
	return acted != alerted, acted
}

func applyCohortChecks(rep *BenchmarkReport, excludeRefs, present []string, actions []db.ExecutedSessionAction) {
	found := map[string]bool{}
	for _, r := range present {
		found[r] = true
	}
	seen := map[string]bool{}
	rep.ExcludedRefs = []string{}
	rep.ExcludedRefsUnmatched = []string{}
	for _, r := range excludeRefs {
		if seen[r] {
			continue
		}
		seen[r] = true
		rep.ExcludedRefs = append(rep.ExcludedRefs, r)
		if !found[r] {
			rep.ExcludedRefsUnmatched = append(rep.ExcludedRefsUnmatched, r)
		}
	}
	sort.Strings(rep.ExcludedRefs)
	sort.Strings(rep.ExcludedRefsUnmatched)
	rep.CrossHostDetail = []string{}
	for _, e := range actions {
		if other, acted := effectOnAnotherHost(e.SessionHost, e.Action); other {
			rep.CrossHostDetail = append(rep.CrossHostDetail, fmt.Sprintf("execution %d (%s): incident subject %q, effect on %q", e.ExecutionID, e.ExternalRef, e.SessionHost, acted))
		}
	}
	n := len(rep.CrossHostDetail)
	rep.CrossHostActuations = &n
	if rep.Bar == string(BarG4A) && n > 0 {
		rep.Verdict = benchmarkVerdictFail
		rep.Failures = append(rep.Failures, fmt.Sprintf("cross-host actuations: %d executed effect(s) on a host other than the incident subject", n))
	}
}

func cohortChecksText(rep BenchmarkReport) string {
	b := &strings.Builder{}
	if rep.CrossHostActuations != nil {
		fmt.Fprintf(b, "effect targets: %d execution(s) on a host other than the incident subject\n", *rep.CrossHostActuations)
		for _, d := range rep.CrossHostDetail {
			fmt.Fprintf(b, "  %s\n", d)
		}
	}
	fmt.Fprintf(b, "control sessions excluded by id: %d (unmatched: %d)\n", len(rep.ExcludedRefs), len(rep.ExcludedRefsUnmatched))
	for _, r := range rep.ExcludedRefsUnmatched {
		fmt.Fprintf(b, "  no session in the window: %s\n", r)
	}
	return b.String()
}
