package pveliveness

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

// The probe is exercised over a real TCP listener (httptest) rather than through the package's fakeDoer,
// because two of the things it must get right — a genuinely unreachable endpoint and the real
// PVEAPIToken authorization header on the wire — cannot be observed through a transport that returns
// canned structs.

// probeToken is the API token value the probe must actually present. It must never appear in a Result.
const probeToken = "PVEAPIToken-user@pve!tg=SECRET-VALUE-must-not-leak"

// probeCluster is a realistic /cluster/resources?type=vm answer: two managed guests (one running, one
// stopped), one unmanaged guest, and a storage row that is not a guest at all.
const probeCluster = `{"data":[
	{"type":"lxc","node":"dc1pve01","name":"dc1reactive01","vmid":101,"status":"running"},
	{"type":"qemu","node":"dc1pve01","name":"dc1mealie01","vmid":102,"status":"stopped"},
	{"type":"lxc","node":"dc1pve01","name":"dc1infra99","vmid":103,"status":"running"},
	{"type":"storage","node":"dc1pve01","name":"local-zfs","vmid":0,"status":"available"}
]}`

// probeSrv records what the probe really sent, so a test can prove the read is a GET to the cluster
// resource list carrying the resolved credential — not a check that configuration is non-empty.
type probeSrv struct {
	mu      sync.Mutex
	methods []string
	paths   []string
	queries []string
	auths   []string
	hits    int
}

func (r *probeSrv) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hits++
	r.methods = append(r.methods, req.Method)
	r.paths = append(r.paths, req.URL.Path)
	r.queries = append(r.queries, req.URL.RawQuery)
	r.auths = append(r.auths, req.Header.Get("Authorization"))
}

// newProbeServer starts a Proxmox stand-in that records every request and answers with h.
func newProbeServer(t *testing.T, rec *probeSrv, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if rec != nil {
			rec.record(req)
		}
		h(w, req)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func serveJSON(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

// newProbeSource builds the detector the console would probe, with every configured value present and
// non-empty: a base URL, a token reference that really resolves, a managed-guest allowlist and a site.
func newProbeSource(t *testing.T, base string, allowed []string) *Source {
	t.Helper()
	t.Setenv("TG_TEST_PROBE_PVE_TOKEN", probeToken)
	return New(base, "env:TG_TEST_PROBE_PVE_TOKEN", allowed, "nl", WithHTTPClient(http.DefaultClient))
}

func TestSelfTest(t *testing.T) {
	managed := []string{"dc1reactive01", "dc1mealie01"}

	cases := []struct {
		name        string
		handler     http.HandlerFunc
		allowed     []string
		closeFirst  bool // take the URL, then close the listener: a real unreachable endpoint
		wantErr     bool
		wantSummary []string
		wantDetail  []string
	}{
		{
			// The observation must come from the SERVED payload: the guest names, their statuses and the
			// count of visible guests appear nowhere in the module's configuration, so a Summary carrying
			// them can only have been read off the wire. The storage row must not be counted as a guest.
			name:        "success reports the estate and the managed-guest match",
			handler:     serveJSON(http.StatusOK, probeCluster),
			allowed:     managed,
			wantSummary: []string{"3 guest(s) visible", "2 of 2 managed guest(s) matched", "1 running, 1 stopped"},
		},
		{
			// The failure a probe that stopped at "authenticated" would miss: the token works, the estate
			// lists, and a guest TG manages is simply not in it.
			name:        "a managed guest that is not visible is named",
			handler:     serveJSON(http.StatusOK, probeCluster),
			allowed:     []string{"dc1reactive01", "dc1ghost01"},
			wantSummary: []string{"1 of 2 managed guest(s) matched"},
			wantDetail:  []string{"dc1ghost01", "TG_PROXMOX_ALLOWED_GUESTS"},
		},
		{
			name:        "no managed guest visible at all is a failure",
			handler:     serveJSON(http.StatusOK, probeCluster),
			allowed:     []string{"dc1ghost01"},
			wantErr:     true,
			wantSummary: []string{"0 of 1 managed guest(s) matched"},
			wantDetail:  []string{"can never fire", "pool"},
		},
		{
			name:       "401 names the credential",
			handler:    serveJSON(http.StatusUnauthorized, `{"data":null}`),
			allowed:    managed,
			wantErr:    true,
			wantDetail: []string{"401", "token", "env:TG_TEST_PROBE_PVE_TOKEN"},
		},
		{
			name:       "403 names the permission, not the token",
			handler:    serveJSON(http.StatusForbidden, `{"data":null}`),
			allowed:    managed,
			wantErr:    true,
			wantDetail: []string{"403", "permission", "privilege separation"},
		},
		{
			name:       "404 points at the base URL",
			handler:    serveJSON(http.StatusNotFound, `not found`),
			allowed:    managed,
			wantErr:    true,
			wantDetail: []string{"404", "base URL"},
		},
		{
			name:       "5xx says the node is unhealthy, not the credential",
			handler:    serveJSON(http.StatusInternalServerError, `{"data":null}`),
			allowed:    managed,
			wantErr:    true,
			wantDetail: []string{"500", "unhealthy"},
		},
		{
			name:       "a non-Proxmox answer is a failure and the body is not quoted",
			handler:    serveJSON(http.StatusOK, `<html><body>Please log in</body></html>`),
			allowed:    managed,
			wantErr:    true,
			wantDetail: []string{"not with a Proxmox API envelope", "API root"},
		},
		{
			name:       "a closed port is unreachable and an error, never a pass",
			handler:    serveJSON(http.StatusOK, probeCluster),
			allowed:    managed,
			closeFirst: true,
			wantErr:    true,
			wantDetail: []string{"could not be reached"},
		},
		{
			// The read succeeds, so the connection is provably sound — but the detector watches nothing,
			// and a green button beside a detector that can never fire certifies a check nobody made.
			name:        "an empty managed-guest list fails even though the API answered",
			handler:     serveJSON(http.StatusOK, probeCluster),
			allowed:     nil,
			wantErr:     true,
			wantSummary: []string{"3 guest(s) visible", "no managed guests are configured"},
			wantDetail:  []string{"TG_PROXMOX_ALLOWED_GUESTS", "watches NOTHING"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &probeSrv{}
			srv := newProbeServer(t, rec, c.handler)
			if c.closeFirst {
				srv.Close()
			}
			s := newProbeSource(t, srv.URL, c.allowed)

			res, err := s.SelfTest(context.Background(), "alice@example")
			if c.wantErr && err == nil {
				t.Fatalf("expected an error, got a pass: %+v", res)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("expected a pass, got error %v (%+v)", err, res)
			}
			if strings.TrimSpace(res.Summary) == "" {
				t.Error("Summary is empty — the operator is shown nothing")
			}
			for _, want := range c.wantSummary {
				if !strings.Contains(res.Summary, want) {
					t.Errorf("Summary %q does not name %q", res.Summary, want)
				}
			}
			if c.wantErr && strings.TrimSpace(res.Detail) == "" {
				t.Error("a failure must carry an actionable Detail")
			}
			for _, want := range c.wantDetail {
				if !strings.Contains(res.Detail, want) {
					t.Errorf("Detail %q does not name %q", res.Detail, want)
				}
			}
			// selftest contract clause 5 / INV-13: nothing an operator can paste into a ticket may carry the
			// credential — and this one is the Proxmox guest-lifecycle WRITE token the actuation lane shares.
			shown := res.Summary + " " + res.Detail
			if err != nil {
				shown += " " + err.Error()
			}
			if strings.Contains(shown, probeToken) {
				t.Fatalf("the Proxmox API token leaked into the operator-visible result: %q", shown)
			}
			// Even with an empty allowlist the probe must have made the real call — that is what
			// distinguishes "the connection is sound but nothing is watched" from an untested guess.
			rec.mu.Lock()
			hits := rec.hits
			rec.mu.Unlock()
			if !c.closeFirst && hits == 0 {
				t.Error("the probe never contacted the server — it cannot have tested anything")
			}
		})
	}
}

// The probe must be the REAL read the detector performs: one GET to /cluster/resources, carrying the token
// resolved from the module's own SecretRef, with no mutation of any kind.
func TestSelfTestIssuesTheRealReadOnlyRequest(t *testing.T) {
	rec := &probeSrv{}
	srv := newProbeServer(t, rec, serveJSON(http.StatusOK, probeCluster))
	s := newProbeSource(t, srv.URL, []string{"dc1reactive01"})

	if _, err := s.SelfTest(context.Background(), "alice@example"); err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.hits != 1 {
		t.Fatalf("expected exactly 1 request (one attempt, no retry), got %d: %v", rec.hits, rec.paths)
	}
	if rec.methods[0] != http.MethodGet {
		t.Errorf("probe issued %s — this module may only READ the estate; every guest mutation belongs "+
			"behind the mode chokepoint", rec.methods[0])
	}
	if rec.paths[0] != "/api2/json/cluster/resources" {
		t.Errorf("probe read %q, want the same endpoint FetchActive polls", rec.paths[0])
	}
	if rec.queries[0] != "type=vm" {
		t.Errorf("probe query %q, want type=vm (the same bounded read the poller performs)", rec.queries[0])
	}
	if rec.auths[0] != "PVEAPIToken="+probeToken {
		t.Errorf("probe presented %q, want the token resolved from the module's own SecretRef", rec.auths[0])
	}
}

// With no base URL there is nothing to read, and a pass would certify a detector that never polls.
func TestSelfTestNoBaseURLIsAnError(t *testing.T) {
	t.Setenv("TG_TEST_PROBE_PVE_TOKEN", probeToken)
	s := New("", "env:TG_TEST_PROBE_PVE_TOKEN", []string{"dc1reactive01"}, "nl")
	res, err := s.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatalf("an unconfigured endpoint must not pass: %+v", res)
	}
	if !strings.Contains(res.Detail, "TG_PROXMOX_BASE_URL") {
		t.Errorf("Detail %q must name the setting to fix", res.Detail)
	}
}

// A token reference that cannot be resolved is a TG-side fault, and the message must say so rather than
// blaming Proxmox — the operator would otherwise go and look at a healthy hypervisor.
func TestSelfTestUnresolvableTokenRefIsATGSideFault(t *testing.T) {
	srv := newProbeServer(t, nil, serveJSON(http.StatusOK, probeCluster))
	s := New(srv.URL, "env:TG_TEST_PROBE_PVE_TOKEN_THAT_IS_NOT_SET", []string{"dc1reactive01"}, "nl",
		WithHTTPClient(http.DefaultClient))
	res, err := s.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatalf("an unresolvable token reference must not pass: %+v", res)
	}
	if !strings.Contains(res.Detail, "TG-side") {
		t.Errorf("Detail %q must say the fault is TG's, not Proxmox's", res.Detail)
	}
}

// A CLUSTER TOO BIG FOR THE READ MUST NOT BE DIAGNOSED AS "NOT A PROXMOX API".
//
// /cluster/resources grows with the estate and both this probe and the poller cut the body at 4 MiB. A body
// cut off mid-JSON decodes exactly like a captive portal, so the naive classification tells an operator with
// a healthy hypervisor to go and fix a base URL that is correct — and hides the fault that actually matters,
// which is that the POLLER is reading under the same bound and therefore detecting nothing.
func TestSelfTestDoesNotMisdiagnoseAnOversizeInventory(t *testing.T) {
	var b strings.Builder
	b.Grow(5 << 20)
	b.WriteString(`{"data":[`)
	for i := 0; b.Len() < (4<<20)+4096; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"type":"lxc","node":"n","name":"guest` + strconv.Itoa(i) + `","vmid":` +
			strconv.Itoa(i) + `,"status":"running","notes":"` + strings.Repeat("y", 1200) + `"}`)
	}
	b.WriteString(`]}`)

	srv := newProbeServer(t, nil, serveJSON(http.StatusOK, b.String()))
	s := newProbeSource(t, srv.URL, []string{"guest1"})
	res, err := s.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatalf("an inventory the source cannot read whole must not pass: %+v", res)
	}
	for _, forbidden := range []string{"reverse proxy", "different application"} {
		if strings.Contains(res.Detail, forbidden) {
			t.Fatalf("a healthy Proxmox with a large cluster was diagnosed as %q: %s", forbidden, res.Detail)
		}
	}
	if !strings.Contains(res.Detail, "WORKED") || !strings.Contains(res.Detail, "NOT being detected") {
		t.Errorf("Detail %q must say the credential is fine AND that liveness is not being detected", res.Detail)
	}
}

// A base URL that cannot be parsed must not republish it. net/http strips the password from a TRANSPORT
// error; net/url does not strip anything from a PARSE error, and this credential is the one the ACTUATION
// lane shares.
func TestSelfTestNeverEchoesAnUnparseableBaseURL(t *testing.T) {
	const pw = "hunter2-must-not-leak"
	t.Setenv("TG_TEST_PROBE_PVE_TOKEN", probeToken)
	s := New("https://tg:"+pw+"@pve.example:8006\x7f", "env:TG_TEST_PROBE_PVE_TOKEN",
		[]string{"dc1reactive01"}, "nl", WithHTTPClient(http.DefaultClient))

	res, err := s.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatalf("an unusable base URL must not pass: %+v", res)
	}
	shown := res.Summary + " " + res.Detail + " " + err.Error()
	if strings.Contains(shown, pw) {
		t.Fatalf("the base URL's password leaked into the operator-visible result: %q", shown)
	}
	if !strings.Contains(res.Detail, "base URL") {
		t.Errorf("Detail %q must still name what the operator has to fix", res.Detail)
	}
}

// THE KILLING ORACLE.
//
// Every configured value is present and non-empty — base URL, a token reference that resolves to a real
// non-empty token, a managed-guest allowlist, a site label — and the endpoint answers 401. A SelfTest
// replaced by a configured-values-are-non-empty check passes here; this test fails it. That is what makes
// this probe more than a mock: a revoked token, a permission never granted and a node that has been down
// for a week are all invisible to a configuration check, and all three look exactly like this.
func TestSelfTestFailsWithCompleteConfigAgainstARejectingServer(t *testing.T) {
	srv := newProbeServer(t, nil, serveJSON(http.StatusUnauthorized, `{"data":null}`))
	t.Setenv("TG_TEST_PROBE_PVE_TOKEN", probeToken)

	const ref = "env:TG_TEST_PROBE_PVE_TOKEN"
	allowed := []string{"dc1reactive01"}
	// Guard the premise: if any of these were empty the test would prove nothing.
	if srv.URL == "" || ref == "" || len(allowed) == 0 {
		t.Fatal("the oracle requires a COMPLETE configuration")
	}
	if tok, err := config.SecretRef(ref).Resolve(); err != nil || tok == "" {
		t.Fatalf("the oracle requires a token reference that really resolves: %v", err)
	}

	s := New(srv.URL, ref, allowed, "nl", WithHTTPClient(http.DefaultClient))
	res, err := s.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatalf("a complete configuration pointed at a rejecting server MUST fail: %+v", res)
	}
	if !strings.Contains(res.Detail, "401") {
		t.Errorf("Detail %q must name the rejection so the operator knows the token is the problem", res.Detail)
	}

	// The same completeness against a dead port must also fail — the "node has been down for a week" case.
	dead := newProbeServer(t, nil, serveJSON(http.StatusOK, probeCluster))
	dead.Close()
	if res, err := New(dead.URL, ref, allowed, "nl", WithHTTPClient(http.DefaultClient)).
		SelfTest(context.Background(), "alice@example"); err == nil {
		t.Fatalf("a complete configuration pointed at a dead port MUST fail: %+v", res)
	}
}

// The probe must not disturb the poller's edge-detection state. FetchActive fires only on an observed
// running→stopped TRANSITION, so if SelfTest recorded what it saw into Source.prior, pressing TEST while a
// guest was running and then having it stop would either double-fire or, worse, swallow the transition —
// a settings dialog silently changing detection behaviour.
func TestSelfTestDoesNotDisturbEdgeDetection(t *testing.T) {
	srv := newProbeServer(t, nil, serveJSON(http.StatusOK, probeCluster))
	s := newProbeSource(t, srv.URL, []string{"dc1mealie01"}) // this guest is served as "stopped"

	if _, err := s.SelfTest(context.Background(), "alice@example"); err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	s.mu.Lock()
	n := len(s.prior)
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("the probe wrote %d entry(ies) into the poller's prior-state map; a self-test must not "+
			"change what the detector will fire on", n)
	}
}
