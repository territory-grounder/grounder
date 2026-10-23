package deploy

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TG-174. `hermetic-build-probe` proves TG's image builds with no Docker daemon — the prerequisite for
// dropping the host `docker.sock` from the shared runners, where it is mounted GLOBALLY and is
// root-equivalent for every job of every project.
//
// It shipped gated on merge requests that change one of four files. Measured 2026-08-06: it had run
// ZERO times — searched every page of the project's job history, not a head-limited window — while
// roughly fifty merge requests went through that day without touching any of those four files.
//
// The plan it belongs to says .image-build switches over "when this has been green across a few real
// changes". A job that cannot run accumulates no evidence, so the switch was waiting on a condition that
// could not arrive. That is this repo's signature defect wearing CI clothes: present, correct, and never
// reaching.
//
// The property below is deliberately about CADENCE, not about this one job's filters: a probe whose only
// trigger is a narrow `changes:` filter is indistinguishable from a probe that is switched off.

func ciConfig(t *testing.T) map[string]any {
	t.Helper()
	b, err := os.ReadFile("../.gitlab-ci.yml")
	if err != nil {
		t.Fatalf("read .gitlab-ci.yml: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse .gitlab-ci.yml: %v", err)
	}
	if len(doc) < 10 {
		t.Fatalf("VACUITY FLOOR: parsed only %d top-level keys from .gitlab-ci.yml — every assertion "+
			"below would pass on a stub", len(doc))
	}
	return doc
}

func rulesOf(t *testing.T, doc map[string]any, job string) []map[string]any {
	t.Helper()
	j, ok := doc[job].(map[string]any)
	if !ok {
		t.Fatalf("job %q is absent from .gitlab-ci.yml — this guard is anchored on a job that no longer "+
			"exists and would otherwise pass while checking nothing", job)
	}
	raw, ok := j["rules"].([]any)
	if !ok {
		t.Fatalf("job %q declares no rules", job)
	}
	var out []map[string]any
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// TestTheHermeticProbeHasATriggerThatActuallyFires is the finding.
func TestTheHermeticProbeHasATriggerThatActuallyFires(t *testing.T) {
	rules := rulesOf(t, ciConfig(t), "hermetic-build-probe")
	if len(rules) == 0 {
		t.Fatal("hermetic-build-probe has no rules at all")
	}

	var unconditional int
	for _, r := range rules {
		if _, narrowed := r["changes"]; narrowed {
			continue // a changes-filtered arm may never fire; it cannot be the only trigger
		}
		if _, ok := r["if"]; ok {
			unconditional++
		}
	}
	if unconditional == 0 {
		t.Error("every trigger for hermetic-build-probe is narrowed by a `changes:` filter. It ran ZERO " +
			"times in its first day for exactly this reason, while ~50 merge requests went past. A probe " +
			"that only fires when one of a handful of files moves cannot accumulate the evidence its own " +
			"comment says the .image-build switch is waiting for — it is indistinguishable from disabled.")
	}
}

// TestTheProbeCannotRedMain pins the other half. The cheap way to satisfy the test above is an
// unconditional main arm that FAILS the pipeline — which turns an evidence-gathering probe into something
// that blocks every deploy, and a control that breaks deploys gets deleted rather than fixed.
func TestTheProbeCannotRedMain(t *testing.T) {
	rules := rulesOf(t, ciConfig(t), "hermetic-build-probe")

	var checkedMain bool
	for _, r := range rules {
		cond, _ := r["if"].(string)
		if !strings.Contains(cond, `$CI_COMMIT_BRANCH == "main"`) {
			continue
		}
		checkedMain = true
		af, ok := r["allow_failure"].(bool)
		if !ok || !af {
			t.Errorf("the main arm of hermetic-build-probe does not set allow_failure: true (got %v). "+
				"This probe exists to gather evidence for a switch that has NOT happened; the real image "+
				"build still ships via .image-build. Letting it red main makes a kaniko hiccup block every "+
				"deploy, and this project's rule is that main is never left red.", r["allow_failure"])
		}
	}
	if !checkedMain {
		t.Fatal("VACUITY FLOOR: no main arm was examined, so this test asserted nothing. Either the main " +
			"arm is gone — in which case the probe is back to never firing — or its condition changed shape.")
	}
}

// TestTheMergeRequestArmStillFailsLoudly guards the inverse over-correction: making EVERY arm
// allow_failure would silence the one place the probe is cheap and actionable.
func TestTheMergeRequestArmStillFailsLoudly(t *testing.T) {
	rules := rulesOf(t, ciConfig(t), "hermetic-build-probe")

	var checkedMR bool
	for _, r := range rules {
		cond, _ := r["if"].(string)
		if !strings.Contains(cond, "merge_request_event") {
			continue
		}
		checkedMR = true
		if af, ok := r["allow_failure"].(bool); ok && af {
			t.Error("the merge-request arm allows failure. On an MR this probe is cheap and the author is " +
				"right there — that is where a broken hermetic build should stop the pipeline. Allowing " +
				"failure everywhere leaves a job that runs, goes red, and nobody is required to look.")
		}
	}
	if !checkedMR {
		t.Fatal("VACUITY FLOOR: no merge-request arm was examined — this test asserted nothing")
	}
}
