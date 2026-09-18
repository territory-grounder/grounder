package deploy

// LOAD SHEDDING MUST BE MEASURABLE, NOT JUST LOGGED (2026-08-06).
//
// The sidecar shed 334 requests in 24 h and nothing reported it. A shed request never becomes a
// completion, so `sidecar_up` stayed 1 and `sidecar_completions_total{outcome="error"}` stayed 0; the only
// trace was a warn! line in the container log. Meanwhile the eval change gate lost half its sessions on
// each arm, the model tier breaker tripped, and the judge-death dead-man latched OPEN against a brain that
// was answering fine — it was just refusing to wait (the queue patience was 5 s against a 9.0 s mean
// completion).
//
// So the counter and its rule ship together, and this guard pins the pairing the same way
// TestTheSidecarIsScrapedAndItsRuleHasATarget does: a rule over a series nothing publishes is permanently
// silent and reads on a dashboard as coverage.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func sidecarSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("claude-proxy", "src", "main.rs"))
	if err != nil {
		t.Fatalf("read claude-proxy/src/main.rs: %v", err)
	}
	return string(b)
}

// KILLING MUTATION: delete the sidecar_shed_total block from Metrics::render (the Rust side), or delete
// the SidecarShedding rule (the Prometheus side). RED either way — the pair must move together.
func TestTheShedCounterIsPublishedAndAlerted(t *testing.T) {
	rules := stripYAMLCommentLines(monitoringFile(t, "alert.rules.yml"))
	src := sidecarSource(t)

	if !strings.Contains(src, "sidecar_shed_total") {
		t.Error("the sidecar publishes no sidecar_shed_total series — a shed request never becomes a " +
			"completion, so with no counter the shedding is invisible to every metric TG has")
	}
	// ANCHORED on the newline. `strings.Contains(rules, "alert: SidecarShedding")` is satisfied by
	// `alert: SidecarSheddingRENAMED` — that exact mutation SURVIVED the first version of this guard, the
	// same superstring trap that once let a renamed `..._DISABLED` metric pass a check in this repo.
	if !strings.Contains(rules, "alert: SidecarShedding\n") {
		t.Error("no SidecarShedding rule — 334 sheds in 24 h reached nobody, and the eval gate, the model " +
			"breaker and the judge dead-man all failed downstream of it")
	}
	if !strings.Contains(rules, "sidecar_shed_total") {
		t.Error("the SidecarShedding rule does not read sidecar_shed_total, so it is measuring something else")
	}
	// The vacuity floor for the rule itself: the counter only exists from the release that added it, and an
	// absent series makes SidecarShedding permanently silent — which is the exact state that hid the sheds.
	if !strings.Contains(rules, "alert: SidecarSheddingUnmeasured\n") || !strings.Contains(rules, "absent(sidecar_shed_total)") {
		t.Error("nothing alerts on the shed counter being ABSENT. SidecarShedding cannot fire without the " +
			"series, so a deploy that predates the counter silently restores the blindness")
	}
}

// The increment must sit on the SHED path, not somewhere a served request also reaches — a counter wired
// to the wrong branch reports load shedding that is not happening, or none that is.
//
// KILLING MUTATION: move the fetch_add above the `if let Ok(p) = try_acquire_owned()` fast path so every
// call counts. RED.
func TestTheShedCounterIncrementsOnlyOnTheShedBranch(t *testing.T) {
	src := sidecarSource(t)
	const marker = "async fn acquire_slot"
	i := strings.Index(src, marker)
	if i < 0 {
		t.Fatal("acquire_slot is gone from the sidecar — this guard has no subject")
	}
	body := src[i:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}
	inc := strings.Index(body, "shed.fetch_add")
	if inc < 0 {
		t.Fatal("acquire_slot does not increment the shed counter, so the 503 it returns is uncounted")
	}
	// The fast path returns BEFORE the timeout arm. The increment must come after that return, or a
	// served-immediately request is counted as shed.
	fast := strings.Index(body, "try_acquire_owned")
	if fast < 0 {
		t.Fatal("acquire_slot no longer has a try-first fast path — this guard's ordering assumption is stale")
	}
	if inc < fast {
		t.Errorf("the shed counter is incremented BEFORE the try-first fast path, so requests that got a "+
			"slot immediately are counted as shed:\n%s", body)
	}
	// NEGATIVE CONTROL: the scoper must actually be scoped. If it returned the whole file, the ordering
	// check above would compare positions in unrelated code.
	if strings.Contains(body, "fn main(") {
		t.Error("the acquire_slot scoper ran past the function, so the ordering assertion is file-wide")
	}
}

// The default patience must stay above the measured mean completion. The Rust suite asserts this too; here
// it is pinned from the deploy side, because the value that actually runs comes from compose.yml.
//
// KILLING MUTATION: set SIDECAR_QUEUE_WAIT_MS back to a value below one completion. RED.
func TestComposeGivesTheSidecarMorePatienceThanOneCompletion(t *testing.T) {
	compose := stripYAMLCommentLines(monitoringComposeFile(t))
	if !strings.Contains(compose, "SIDECAR_QUEUE_WAIT_MS") {
		t.Fatal("compose.yml sets no SIDECAR_QUEUE_WAIT_MS — the deployed patience is whatever the binary " +
			"defaults to, which is the thing that was wrong")
	}
	const measuredMeanMs = 9030 // 21_415_904 ms / 2371 completions, live, 2026-08-06
	got := numericEnv(t, compose, "SIDECAR_QUEUE_WAIT_MS")
	if got <= measuredMeanMs {
		t.Errorf("SIDECAR_QUEUE_WAIT_MS=%d is not longer than one average completion (%d ms) — a caller that "+
			"queues times out before the slot ahead of it finishes, which is a load-shed gate wearing a "+
			"queue's name", got, measuredMeanMs)
	}
}

// monitoringComposeFile reads the sidecar's compose file — the source of the env the deployed container
// actually gets, as distinct from the binary's compiled-in default.
func monitoringComposeFile(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("claude-proxy", "compose.yml"))
	if err != nil {
		t.Fatalf("read claude-proxy/compose.yml: %v", err)
	}
	return string(b)
}

// numericEnv pulls a `KEY: "1234"` value out of a compose environment block.
func numericEnv(t *testing.T, compose, key string) int {
	t.Helper()
	for _, line := range strings.Split(compose, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, key+":") {
			continue
		}
		v := strings.TrimSpace(strings.TrimPrefix(trimmed, key+":"))
		v = strings.Trim(v, `"'`)
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("%s is %q, which is not a number — the deployed patience is unreadable", key, v)
		}
		return n
	}
	t.Fatalf("%s not found in compose.yml", key)
	return 0
}
