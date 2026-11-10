package deploy_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func loadCIJobs(t *testing.T) map[string]map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", ".gitlab-ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	jobs := map[string]map[string]any{}
	for name, v := range doc {
		if m, ok := v.(map[string]any); ok && !strings.HasPrefix(name, ".") {
			jobs[name] = m
		}
	}
	return jobs
}

func runsOnAMainPush(job map[string]any) bool {
	rules, ok := job["rules"].([]any)
	if !ok || len(rules) == 0 {
		return true
	}
	for _, r := range rules {
		m, _ := r.(map[string]any)
		cond, _ := m["if"].(string)
		if !strings.Contains(cond, `$CI_PIPELINE_SOURCE == "schedule"`) {
			return true
		}
	}
	return false
}

func neededJobs(t *testing.T, job map[string]any) map[string]bool {
	t.Helper()
	got := map[string]bool{}
	list, ok := job["needs"].([]any)
	if !ok {
		t.Fatalf("needs must be a list, got %T", job["needs"])
	}
	for _, n := range list {
		switch v := n.(type) {
		case string:
			got[v] = true
		case map[string]any:
			name, _ := v["job"].(string)
			got[name] = true
		}
	}
	return got
}

func TestADeployWaitsForEveryBlockingVerifyJob(t *testing.T) {
	jobs := loadCIJobs(t)
	var gates []string
	for name, job := range jobs {
		if job["stage"] != "verify" || job["allow_failure"] == true || !runsOnAMainPush(job) {
			continue
		}
		gates = append(gates, name)
	}
	if len(gates) < 15 {
		t.Fatalf("expected the verify stage's gates, found %d: %v", len(gates), gates)
	}
	for _, deploy := range []string{"deploy", "deploy-sidecar"} {
		job, ok := jobs[deploy]
		if !ok {
			t.Fatalf("job %s is missing", deploy)
		}
		needs := neededJobs(t, job)
		for _, gate := range gates {
			if !needs[gate] {
				t.Errorf("%s does not wait for the blocking verify job %q: a red gate could not stop the deploy", deploy, gate)
			}
		}
		for need := range needs {
			if _, ok := jobs[need]; !ok {
				t.Errorf("%s needs %q, which is not a job", deploy, need)
			}
		}
	}
}
