package deploy

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// TestModelConcurrencyBoundsShipArmed pins the TG-384 fix at the point it actually protects prod: the deployed
// compose must forward a POSITIVE concurrency bound BY DEFAULT, not a blank that leaves the model path
// unbounded until an operator remembers to set it.
//
// The mechanism (the gateway semaphore in adapters/model and the runner worker's MaxConcurrentActivityExecution
// Size in cmd/worker) was built and unit-tested, but both read `${VAR:-}` — an empty default — so a fresh
// deploy shipped the mechanism INERT and a 157-alert burst still became 157 simultaneous investigations that
// tripped the brain in six seconds (the pve03 self-DoS, 133 unrecoverable empty diagnoses). "Built but inert"
// reads as done to every gate that only compiles code; only a test that inspects the SHIPPED DEFAULT can tell
// armed from merely-present.
//
// Killing mutation (the empty-input case, TG-365): revert either default to `${VAR:-}` and this goes RED —
// the empty string is not a positive int. A value outside the sane band (a typo like 9000 that effectively
// re-disables the bound, or 0) also goes RED. Deleting the compose line entirely goes RED (no match).
func TestModelConcurrencyBoundsShipArmed(t *testing.T) {
	compose := mustReadFile(t, filepath.Join(repoRoot(t), "deploy", "docker-compose.yml"))

	cases := []struct {
		key      string // the env key whose compose default must ship armed
		min, max int    // the sane band: below min is pointless, above max effectively disables the bound
		why      string
	}{
		{"TG_MODEL_MAX_CONCURRENCY", 1, 64, "the gateway's in-flight completion cap — the brain-load bound that parks (never drops) excess"},
		{"TG_MAX_CONCURRENT_INVESTIGATIONS", 1, 128, "the runner's activity cap — the durable half; excess waits in Temporal's queue rather than storming the sidecar"},
	}

	for _, c := range cases {
		def, ok := composeDefaultOf(compose, c.key)
		if !ok {
			t.Errorf("%s: no `%s: ${%s:-DEFAULT}` line in deploy/docker-compose.yml — the bound cannot ship if the key is not forwarded (%s)", c.key, c.key, c.key, c.why)
			continue
		}
		n, err := strconv.Atoi(def)
		if err != nil {
			t.Errorf("%s: compose default is %q, not an integer — a blank/inert default leaves the model path unbounded on a fresh deploy, which is exactly the TG-384 self-DoS (%s)", c.key, def, c.why)
			continue
		}
		if n < c.min || n > c.max {
			t.Errorf("%s: compose default is %d, outside the sane band [%d,%d] — a bound this size is not a bound (%s)", c.key, n, c.min, c.max, c.why)
		}
	}
}

// composeDefaultOf returns the DEFAULT in a compose `KEY: ${KEY:-DEFAULT}` interpolation, and whether the line
// was found. An empty default (`${KEY:-}`) returns ("", true) — found but inert, which the caller rejects.
func composeDefaultOf(compose, key string) (string, bool) {
	re := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(key) + `:\s*\$\{` + regexp.QuoteMeta(key) + `:-([^}]*)\}`)
	m := re.FindStringSubmatch(compose)
	if m == nil {
		return "", false
	}
	return m[1], true
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
