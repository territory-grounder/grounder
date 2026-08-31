package egress

// Oracles for the outbound meter (TG-160).
//
// THE VACUITY FLOOR (house rule 3). Every test here that exercises a matcher/scanner asserts BOTH
// directions: that it matches what it must AND that it refuses what it must. A filter test that only
// checks "no complaints" passes just as happily when the filter matches nothing at all, and this
// repository has shipped exactly that failure before — a control that was green because it was inert.
// TestDeclaredDestinationsScanIsNotVacuous and TestAllowlistIsNotVacuous exist specifically to fail if
// the scan or the matcher ever degrades to matching zero things.

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// composeShapedEnv mirrors the endpoint keys deploy/docker-compose.yml actually passes to the worker —
// including the credential-shaped keys that MUST NOT be scanned.
func composeShapedEnv() []string {
	return []string{
		"TG_LITELLM_URL=http://litellm:4000",
		"TG_OPENBAO_ADDR=https://bao.estate.example:8200",
		"TG_TEMPORAL_HOSTPORT=temporal:7233",
		"TG_NETBOX_URL=https://netbox.estate.example/",
		"TG_PVE_URL=https://pve1.estate.example:8006/api2/json/",
		"TG_YOUTRACK_URL=https://youtrack.estate.example",
		"TG_MATRIX_HOMESERVER=https://matrix.estate.example",
		"TG_LDAP_URLS=ldaps://ipa1.estate.example:636,ldaps://ipa2.estate.example:636",
		"TG_LIBRENMS_DEPLOYMENTS=nl=https://librenms.estate.example,gr=https://librenms2.estate.example",
		"TG_HOSTDIAG_DEPLOYMENTS=host1.estate.example,host2.estate.example",
		"TG_DISCOVERY_SYSTEMD_HOSTS=host3.estate.example host4.estate.example",
		// Not endpoints. None of these may contribute a destination.
		"TG_YOUTRACK_TOKEN_REF=bao:secret/data/youtrack#token",
		"TG_ADMIN_TOKEN=hunter2",
		"TG_SEAL_KEY=0123456789abcdef0123456789abcdef",
		"TG_KNOWLEDGE_FILE=/knowledge/corpus.maintained.json",
		"TG_ESTATE_REFRESH_INTERVAL=5m",
		"TG_LDAP_CA=file:/secrets/freeipa-ca.crt",
	}
}

func TestDeclaredDestinationsScanIsNotVacuous(t *testing.T) {
	got := DeclaredDestinations(composeShapedEnv())
	// VACUITY FLOOR: a scan that matches nothing is the failure this test exists to catch.
	if len(got) == 0 {
		t.Fatal("VACUOUS: the endpoint scan matched ZERO destinations over a compose-shaped environment. " +
			"Every outbound destination would then read as off-allowlist and the meter would be noise, " +
			"or (under enforce) the process would lose the network entirely.")
	}
	idx := map[string]bool{}
	for _, h := range got {
		idx[h] = true
	}
	for _, want := range []string{
		"litellm", "bao.estate.example", "temporal", "netbox.estate.example", "pve1.estate.example",
		"youtrack.estate.example", "matrix.estate.example", "ipa1.estate.example", "ipa2.estate.example",
		"librenms.estate.example", "librenms2.estate.example",
		"host1.estate.example", "host3.estate.example", "host4.estate.example",
	} {
		if !idx[want] {
			t.Errorf("declared destination %q was not derived from the deployment configuration; the "+
				"connector is configured but the meter does not know about it, so its legitimate traffic "+
				"will be counted as exfil. got=%v", want, got)
		}
	}
}

func TestDeclaredDestinationsNeverReadsCredentialOrPathValues(t *testing.T) {
	got := DeclaredDestinations(composeShapedEnv())
	for _, h := range got {
		switch {
		case strings.Contains(h, "hunter2"), strings.Contains(h, "secret"),
			strings.Contains(h, "corpus"), strings.Contains(h, "freeipa-ca"):
			t.Fatalf("the endpoint scan read a NON-endpoint value and derived %q from it — a scan that "+
				"reads *_TOKEN/*_KEY/*_REF/file paths can put credential material into a metric label", h)
		}
	}
	// A credential value that LOOKS like a URL must still be ignored: the key, not the value, decides.
	if hosts := DeclaredDestinations([]string{"TG_EVIL_TOKEN_REF=https://attacker.example/x"}); len(hosts) != 0 {
		t.Fatalf("a *_TOKEN_REF value was scanned as an endpoint: %v", hosts)
	}
}

func TestAllowlistIsNotVacuous(t *testing.T) {
	a := NewAllowlist(DeclaredDestinations(composeShapedEnv()))
	if a.Size() == 0 {
		t.Fatal("VACUOUS: the compiled allowlist holds ZERO rules")
	}
	// It must MATCH a declared host…
	if rule, ok := a.Permits("netbox.estate.example"); !ok {
		t.Fatalf("declared host netbox.estate.example is not permitted (rule=%q) — the matcher matches nothing", rule)
	}
	// …and it must REFUSE an undeclared one. Without this half the test passes for a match-everything rule.
	if rule, ok := a.Permits("pastebin.example"); ok {
		t.Fatalf("undeclared host pastebin.example was PERMITTED under rule %q — the allowlist admits "+
			"everything, so an exfil destination would never be flagged", rule)
	}
}

func TestEmptyAllowlistPermitsNothing(t *testing.T) {
	a := NewAllowlist(nil)
	if a.Size() != 0 {
		t.Fatalf("empty allowlist reports %d rules", a.Size())
	}
	if _, ok := a.Permits("anything.example"); ok {
		t.Fatal("an EMPTY allowlist permitted a host. Fail-open here means a deployment that declared no " +
			"destinations reports every outbound as fine — a control that succeeds by doing nothing.")
	}
	// Loopback is the one always-permitted case: it never leaves the netns and is not egress.
	if rule, ok := a.Permits("127.0.0.1"); !ok || rule != "loopback" {
		t.Fatalf("loopback should always be permitted under rule loopback, got (%q,%v)", rule, ok)
	}
}

func TestAllowlistWildcardAndNormalisation(t *testing.T) {
	a := NewAllowlist([]string{"*.estate.example", "https://Portal.Example.Org/api", "temporal:7233"})
	for _, h := range []string{"a.estate.example", "estate.example", "portal.example.org", "PORTAL.EXAMPLE.ORG.", "temporal"} {
		if _, ok := a.Permits(h); !ok {
			t.Errorf("%q should be permitted", h)
		}
	}
	for _, h := range []string{"estate.example.evil.test", "notportal.example.org", "temporal.evil.test", ""} {
		if _, ok := a.Permits(h); ok {
			t.Errorf("%q must NOT be permitted — a suffix rule that matches a lookalike domain is worse "+
				"than no rule, because it launders the exact destination an attacker registers", h)
		}
	}
	// A bare "*" or "*.tld" is too broad to be a declaration and must be dropped, not compiled.
	if wide := NewAllowlist([]string{"*", "*.local"}); wide.Size() != 0 {
		t.Fatalf("an over-broad wildcard compiled into %d rules: %v", wide.Size(), wide.Rules())
	}
}

// newTestServers returns one "declared" and one "undeclared" server plus an allowlist naming only the
// first, using the real host:port each httptest server binds.
func newTestServers(t *testing.T, body string) (declared, undeclared *httptest.Server, a *Allowlist) {
	t.Helper()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, body)
	})
	declared = httptest.NewServer(h)
	undeclared = httptest.NewServer(h)
	t.Cleanup(declared.Close)
	t.Cleanup(undeclared.Close)
	// httptest binds 127.0.0.1, which the allowlist always permits as loopback — so rewrite the host to a
	// name and point the transport at the real address via a custom dialer would be heavier than needed.
	// Instead the requests below are issued with an explicit Host in the URL and a transport that dials
	// the server; see meterClient.
	return declared, undeclared, NewAllowlist([]string{"declared.test"})
}

// dialRewriter sends every request to addr regardless of the URL host, so a test can exercise the meter
// against real hostnames without DNS.
type dialRewriter struct {
	addrFor map[string]string
	next    http.RoundTripper
}

func (d dialRewriter) RoundTrip(r *http.Request) (*http.Response, error) {
	if a, ok := d.addrFor[r.URL.Hostname()]; ok {
		r = r.Clone(r.Context())
		r.URL.Host = a
	}
	return d.next.RoundTrip(r)
}

func TestMeterCountsVolumeAndSeparatesOffAllowlistDestinations(t *testing.T) {
	body := strings.Repeat("y", 500)
	declared, undeclared, allow := newTestServers(t, body)
	base := dialRewriter{addrFor: map[string]string{
		"declared.test":   strings.TrimPrefix(declared.URL, "http://"),
		"undeclared.test": strings.TrimPrefix(undeclared.URL, "http://"),
	}, next: http.DefaultTransport}

	var logs []string
	m := NewMeter(base, allow, WithLogger(func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }))
	c := &http.Client{Transport: m}

	post := func(host string, payload string) {
		t.Helper()
		resp, err := c.Post("http://"+host+"/v1/x", "application/json", strings.NewReader(payload))
		if err != nil {
			t.Fatalf("post %s: %v", host, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	post("declared.test", strings.Repeat("a", 100))
	post("undeclared.test", strings.Repeat("b", 300))
	post("undeclared.test", strings.Repeat("c", 300))

	s := m.Snapshot()
	if s.Requests != 3 {
		t.Fatalf("total requests = %d, want 3", s.Requests)
	}
	if s.OffRequests != 2 {
		t.Fatalf("off-allowlist requests = %d, want 2 — the meter is not separating undeclared "+
			"destinations, which is the entire signal", s.OffRequests)
	}
	if s.OffBytesOut != 600 {
		t.Fatalf("off-allowlist bytes out = %d, want 600 — VOLUME is the exfil dimension; a destination "+
			"count alone cannot tell one health probe from a corpus upload", s.OffBytesOut)
	}
	if s.BytesIn < 1500 {
		t.Fatalf("bytes in = %d, want >= 1500 (3 x %d)", s.BytesIn, len(body))
	}
	// The undeclared host must be NAMED, once, and only once.
	var named int
	for _, l := range logs {
		if strings.Contains(l, "undeclared.test") && strings.Contains(l, "OFF-ALLOWLIST") {
			named++
		}
	}
	if named != 1 {
		t.Fatalf("off-allowlist host named in %d log lines, want exactly 1 (first sighting only); logs=%v", named, logs)
	}
	var found bool
	for _, d := range s.OffAllowlist {
		if d.Rule == "undeclared.test" && d.Requests == 2 && d.BytesOut == 600 {
			found = true
		}
	}
	if !found {
		t.Fatalf("per-destination detail missing undeclared.test: %+v", s.OffAllowlist)
	}
}

func TestMeterModeNeverBlocks(t *testing.T) {
	_, undeclared, allow := newTestServers(t, "ok")
	base := dialRewriter{addrFor: map[string]string{"undeclared.test": strings.TrimPrefix(undeclared.URL, "http://")}, next: http.DefaultTransport}
	c := &http.Client{Transport: NewMeter(base, allow)}
	resp, err := c.Get("http://undeclared.test/")
	if err != nil {
		t.Fatalf("METER MODE BLOCKED A REQUEST: %v. The default posture must be observe-only — an "+
			"allowlist nobody has audited must not be able to take production off the network.", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestEnforceModeRefusesOffAllowlistWithANamedError(t *testing.T) {
	declared, undeclared, allow := newTestServers(t, "ok")
	base := dialRewriter{addrFor: map[string]string{
		"declared.test":   strings.TrimPrefix(declared.URL, "http://"),
		"undeclared.test": strings.TrimPrefix(undeclared.URL, "http://"),
	}, next: http.DefaultTransport}
	m := NewMeter(base, allow, WithMode(ModeEnforce))
	c := &http.Client{Transport: m}

	if _, err := c.Get("http://undeclared.test/"); err == nil {
		t.Fatal("enforce mode did not refuse an off-allowlist destination")
	} else {
		var re *RefusalError
		if !errors.As(err, &re) {
			t.Fatalf("refusal is not a typed *RefusalError: %v", err)
		}
		if re.Host != "undeclared.test" || !strings.Contains(re.Error(), "not on the declared outbound allowlist") {
			t.Fatalf("refusal does not name the destination and the reason: %v", re)
		}
	}
	// A DECLARED destination must still go through — enforcement that blocks everything is an outage,
	// not a control, and this half is what distinguishes the two.
	resp, err := c.Get("http://declared.test/")
	if err != nil {
		t.Fatalf("enforce mode blocked a DECLARED destination: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if s := m.Snapshot(); s.Refusals != 1 || !s.Enforcing {
		t.Fatalf("snapshot refusals=%d enforcing=%v, want 1/true", s.Refusals, s.Enforcing)
	}
}

func TestOffAllowlistDestinationCardinalityIsBounded(t *testing.T) {
	m := NewMeter(http.DefaultTransport, NewAllowlist(nil))
	for i := 0; i < destinationCap*4; i++ {
		m.recordOff(fmt.Sprintf("h%03d.beacon.test", i), 10, 0)
	}
	s := m.Snapshot()
	if len(s.OffAllowlist) > destinationCap+1 {
		t.Fatalf("off-allowlist destination family holds %d series, cap is %d(+overflow). Unbounded "+
			"labels let a beaconing process amplify itself into the TSDB.", len(s.OffAllowlist), destinationCap)
	}
	var overflow bool
	for _, d := range s.OffAllowlist {
		if d.Rule == overflowHost {
			overflow = true
		}
	}
	if !overflow {
		t.Fatal("past the cap, hosts must fold into the \"other\" bucket so the COUNT is still complete")
	}
	if s.OffRequests != uint64(destinationCap*4) {
		t.Fatalf("aggregate off-allowlist count = %d, want %d — the cap must bound LABELS, never the total",
			s.OffRequests, destinationCap*4)
	}
}

// TestARequestWhoseBodyIsNeverClosedIsStillCounted pins the ordering fix. The natural implementation
// records the whole call from the response body's Close, and then any caller that leaks a body — the
// common shape in error paths — removes that outbound call from the meter ENTIRELY, request count
// included. That would let the exfil signal be suppressed by ordinary sloppiness, so the request and its
// OUTBOUND bytes are recorded when the call is made; only response bytes wait for Close.
func TestARequestWhoseBodyIsNeverClosedIsStillCounted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	allow := NewAllowlist([]string{"declared.test"})
	base := dialRewriter{addrFor: map[string]string{"beacon.test": strings.TrimPrefix(srv.URL, "http://")}, next: http.DefaultTransport}
	m := NewMeter(base, allow)

	resp, err := (&http.Client{Transport: m}).Post("http://beacon.test/x", "application/json", strings.NewReader(strings.Repeat("e", 2048)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_ = resp // deliberately NOT drained and NOT closed
	s := m.Snapshot()
	if s.OffRequests != 1 {
		t.Fatalf("off-allowlist requests = %d, want 1. A leaked response body erased the whole outbound "+
			"call from the meter — the exfil signal would then be suppressed by an unclosed body.", s.OffRequests)
	}
	if s.OffBytesOut != 2048 {
		t.Fatalf("off-allowlist bytes out = %d, want 2048 — OUTBOUND volume is the exfil dimension and it "+
			"must not depend on the caller draining the response", s.OffBytesOut)
	}
	resp.Body.Close()
}

func TestLoopbackIsNotCountedAsEgress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer srv.Close()
	m := NewMeter(http.DefaultTransport, NewAllowlist(nil))
	resp, err := (&http.Client{Transport: m}).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if s := m.Snapshot(); s.OffRequests != 0 {
		t.Fatalf("a loopback call was counted as off-allowlist egress (%d). Self-calls would then bury "+
			"the real signal under the process's own health checks.", s.OffRequests)
	}
}
