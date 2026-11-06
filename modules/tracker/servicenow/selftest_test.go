package servicenow

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

// Oracles for the TEST button (core/selftest.Tester).
//
// They drive a real httptest server through the module's real `do`, so the parts a stub cannot judge —
// query encoding, the row cap, and the HTTP BASIC scheme the Table API requires — are the parts under
// test. The probe's whole claim is that the network path works with the real credential; an oracle that
// faked the network path would be asserting nothing.

// probeSrv answers per TABLE (the path) and records every request.
type probeSrv struct {
	byTable map[string]probeReply
	seen    []probeSeen
}

type probeReply struct {
	status int
	body   string
}

type probeSeen struct{ method, path, query, auth string }

func (p *probeSrv) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.seen = append(p.seen, probeSeen{
		method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, auth: r.Header.Get("Authorization"),
	})
	rep, ok := p.byTable[r.URL.Path]
	if !ok {
		http.Error(w, `{"error":{"message":"No such table"}}`, http.StatusBadRequest)
		return
	}
	if rep.status != 0 && rep.status != http.StatusOK {
		http.Error(w, rep.body, rep.status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, rep.body)
}

const (
	probeIncidentPath = "/api/now/table/incident"
	probeJournalPath  = "/api/now/table/sys_journal_field"

	// state "6" is the out-of-box RESOLVED code, so the fixture also exercises the state fold.
	probeIncidentJSON = `{"result":[{"number":"INC0010023","state":"6","opened_at":"2026-07-12 08:14:02"}]}`
	probeJournalJSON  = `{"result":[{"element":"work_notes","sys_created_on":"2026-07-12 09:02:00"}]}`
)

const probePasswordEnv = "TG_TEST_SN_PROBE_PASSWORD"

// newProbeFixture wires the module exactly as production does: instance URL, instance user, and a
// password RESOLVED FROM ITS REFERENCE at call time (INV-13) rather than injected pre-resolved.
func newProbeFixture(t *testing.T, srvURL string, opts ...Option) *Module {
	t.Helper()
	t.Setenv(probePasswordEnv, "pw-abc123")
	return New(srvURL, testUsername, config.SecretRef("env:"+probePasswordEnv), opts...)
}

func probeOKRoutes() map[string]probeReply {
	return map[string]probeReply{
		probeIncidentPath: {body: probeIncidentJSON},
		probeJournalPath:  {body: probeJournalJSON},
	}
}

// A green TEST must name what it OBSERVED, not merely that it worked: "ok" cannot distinguish a correctly
// configured module from one pointed at a clone of the same instance. Every observation asserted here
// comes from the SERVED payload, so a probe reporting hardcoded prose would fail.
func TestSelfTestReportsTheIncidentItObserved(t *testing.T) {
	h := &probeSrv{byTable: probeOKRoutes()}
	srv := httptest.NewServer(h)
	defer srv.Close()

	res, err := newProbeFixture(t, srv.URL).SelfTest(context.Background(), "alice@example")
	if err != nil {
		t.Fatalf("SelfTest must pass against a healthy instance: %v", err)
	}
	for _, want := range []string{
		srv.Listener.Addr().String(), // WHICH instance — the wrong-instance tell
		testUsername,                 // WHO the instance authenticated
		"INC0010023",                 // the readable number an engineer here recognises, from the payload
		"state 6",                    // what the instance said
		"resolved",                   // what THIS deployment's mapping turns it into
	} {
		if !strings.Contains(res.Summary, want) {
			t.Errorf("Summary must report %q, got %q", want, res.Summary)
		}
	}
	if strings.Contains(res.Summary+res.Detail, "pw-abc123") {
		t.Fatal("the password leaked into the Result")
	}

	// REGRESSION SEAM: Table API auth is HTTP Basic base64(username:password). A probe sending a bare
	// Bearer would 401 against every real instance while passing against a lenient fake.
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(testUsername+":pw-abc123"))
	if len(h.seen) != 2 {
		t.Fatalf("want the two documented GETs, got %d: %+v", len(h.seen), h.seen)
	}
	for _, got := range h.seen {
		if got.method != http.MethodGet {
			t.Errorf("the probe must be read-only; it issued %s %s", got.method, got.path)
		}
		if got.auth != wantAuth {
			t.Errorf("request to %s did not carry Basic base64(username:password): %q", got.path, got.auth)
		}
		// BOUNDED: the incident table at a real site holds hundreds of thousands of rows and the dialog
		// has 30 seconds with no retry.
		if !strings.Contains(got.query, "sysparm_limit=1") {
			t.Errorf("read of %s is not bounded: %q", got.path, got.query)
		}
	}
	// The probe must never ask for incident text: it needs to know the table is readable, and an incident
	// title is exactly what should not be dragged into a settings dialog.
	if strings.Contains(h.seen[0].query, "short_description") {
		t.Errorf("the probe requested incident text it does not need: %q", h.seen[0].query)
	}
	// ...and never asks for the journal VALUE either, for the same reason.
	if strings.Contains(h.seen[1].query, "value") {
		t.Errorf("the probe requested journal text it does not need: %q", h.seen[1].query)
	}
}

// The descriptor's verb is the consent contract and it now names these two tables. If the probe stops
// reading them the dialog is lying again, so the paths are pinned.
func TestSelfTestCallsTheTablesTheDescriptorPromises(t *testing.T) {
	h := &probeSrv{byTable: probeOKRoutes()}
	srv := httptest.NewServer(h)
	defer srv.Close()
	if _, err := newProbeFixture(t, srv.URL).SelfTest(context.Background(), ""); err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	if h.seen[0].path != probeIncidentPath || h.seen[1].path != probeJournalPath {
		t.Fatalf("probe read %q then %q, want %q then %q",
			h.seen[0].path, h.seen[1].path, probeIncidentPath, probeJournalPath)
	}
}

// THE KILLING ORACLE. Every configured value is present and non-empty — instance URL, instance user,
// password reference, and a password that resolves — and the instance rejects the credential. A probe
// implemented as a "configured-values-are-non-empty" check passes this case; this one must fail it.
//
// That is what makes the probe more than a mock: a revoked password, an ACL never granted, and an
// instance that has been hibernating for a week all have complete, non-empty configuration.
func TestSelfTestFailsWithFullConfigWhenTheInstanceRejectsTheCredential(t *testing.T) {
	h := &probeSrv{byTable: map[string]probeReply{
		probeIncidentPath: {status: http.StatusUnauthorized, body: `{"error":{"message":"User Not Authenticated"}}`},
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()
	m := newProbeFixture(t, srv.URL)

	// Config is complete: this is not a "missing value" case.
	if m.baseURL == "" || m.username == "" || m.tokenRef == "" {
		t.Fatal("fixture is wrong: the killing oracle requires COMPLETE configuration")
	}
	if pw, err := m.tokenRef.Resolve(); err != nil || pw == "" {
		t.Fatalf("fixture is wrong: the password must resolve to a real value (%q, %v)", pw, err)
	}

	res, err := m.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatal("a rejected credential must FAIL the test; a pass here would certify a dead account")
	}
	// The 401 is ambiguous between the two halves of the Basic credential, and saying so is the
	// actionable part: an operator who only resets the password can chase a wrong username for hours.
	if !strings.Contains(res.Detail, "Instance user") || !strings.Contains(res.Detail, "password") {
		t.Errorf("Detail must name BOTH halves of the credential, got %q", res.Detail)
	}
}

// Failure classification, on the SHAPE of the failure rather than vendor prose. Each case is a distinct
// thing an operator has to do something different about.
func TestSelfTestClassifiesFailuresActionably(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reply  probeReply
		want   []string
		reject []string
	}{
		{
			name:  "401 names both halves of the Basic credential",
			reply: probeReply{status: http.StatusUnauthorized, body: `{"error":{"message":"User Not Authenticated"}}`},
			want:  []string{"Instance user", "password"},
		},
		{
			name:  "403 names the ACL, not the password",
			reply: probeReply{status: http.StatusForbidden, body: `{"error":{"message":"Insufficient rights"}}`},
			want:  []string{"authenticated", "ACLs", "role"},
			// A 403 is not a bad password; sending an operator to reset a working credential sends them
			// away from the actual fix.
			reject: []string{"expired"},
		},
		{
			name:  "400 points at the URL and the table",
			reply: probeReply{status: http.StatusBadRequest, body: `{"error":{"message":"Invalid table"}}`},
			want:  []string{"table", "Instance URL"},
		},
		{
			name:  "5xx blames the instance, not the configuration",
			reply: probeReply{status: http.StatusBadGateway, body: `<html>bad gateway</html>`},
			want:  []string{"unhealthy", "not a credential problem"},
		},
		{
			// The single most common ServiceNow developer-instance failure: a 200 carrying an HTML
			// wake-up page. A probe that only checked the status code would call this a PASS.
			name:  "a 200 that is not Table API JSON reads as a hibernating instance",
			reply: probeReply{body: `<html><body>Your instance is hibernating</body></html>`},
			want:  []string{"HIBERNATED", "not with the Table API's JSON"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &probeSrv{byTable: map[string]probeReply{probeIncidentPath: tc.reply}}
			srv := httptest.NewServer(h)
			defer srv.Close()
			res, err := newProbeFixture(t, srv.URL).SelfTest(context.Background(), "")
			if err == nil {
				t.Fatal("want an error; a failed probe that returns nil certifies a module nobody checked")
			}
			if !strings.Contains(res.Summary, srv.Listener.Addr().String()) {
				t.Errorf("even a failure must say WHERE it went, got %q", res.Summary)
			}
			for _, w := range tc.want {
				if !strings.Contains(res.Detail, w) {
					t.Errorf("Detail must contain %q, got %q", w, res.Detail)
				}
			}
			for _, r := range tc.reject {
				if strings.Contains(res.Detail, r) {
					t.Errorf("Detail must NOT contain %q (wrong diagnosis), got %q", r, res.Detail)
				}
			}
		})
	}
}

// An instance that has been down for a week is one of the three things TEST exists to rule out. It must
// be an error with an unreachable Detail — never a pass, and never a bare "error".
func TestSelfTestReportsAnUnreachableInstance(t *testing.T) {
	srv := httptest.NewServer(&probeSrv{byTable: probeOKRoutes()})
	addr := srv.Listener.Addr().String()
	srv.Close() // the port is now closed: the transport class this must classify

	res, err := newProbeFixture(t, srv.URL).SelfTest(context.Background(), "")
	if err == nil {
		t.Fatal("an unreachable instance must FAIL the test")
	}
	if !strings.Contains(res.Detail, "could not be reached") || !strings.Contains(res.Detail, addr) {
		t.Errorf("Detail must say the instance is unreachable and name it, got %q", res.Detail)
	}
}

// An account that reads incidents but NOT the journal returns history with the resolution missing, which
// reads as an estate where nobody ever wrote anything down. The probe passes — the tracker works — but it
// must say what will be missing, because nothing else in the console ever would.
func TestSelfTestReportsAnUnreadableJournal(t *testing.T) {
	h := &probeSrv{byTable: map[string]probeReply{
		probeIncidentPath: {body: probeIncidentJSON},
		probeJournalPath:  {status: http.StatusForbidden, body: `{"error":{"message":"Insufficient rights"}}`},
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()
	res, err := newProbeFixture(t, srv.URL).SelfTest(context.Background(), "")
	if err != nil {
		t.Fatalf("an unreadable journal does not break the tracker: %v", err)
	}
	if !strings.Contains(res.Detail, "sys_journal_field") || !strings.Contains(res.Detail, "work notes") {
		t.Errorf("Detail must name the journal gap and its consequence, got %q", res.Detail)
	}
}

// An empty incident table is INCONCLUSIVE, not a failure: the query was accepted, and the Table API
// answers "no incidents here" and "your ACLs hid them all" identically. The pass must say so rather than
// present a green tick that implies more than was proven.
func TestSelfTestPassesButSaysWhatAnEmptyTableCannotProve(t *testing.T) {
	h := &probeSrv{byTable: map[string]probeReply{
		probeIncidentPath: {body: `{"result":[]}`},
		probeJournalPath:  {body: probeJournalJSON},
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()
	res, err := newProbeFixture(t, srv.URL).SelfTest(context.Background(), "")
	if err != nil {
		t.Fatalf("an accepted query with no rows is not a failed credential: %v", err)
	}
	if !strings.Contains(res.Detail, "read ACL") {
		t.Errorf("the pass must state the ambiguity it cannot resolve, got %q", res.Detail)
	}
}

// A deployment that customized its choice list and left the state codes at the out-of-box values reads
// resolved incidents as OPEN — a silent, config-only fault. Rendering the raw code beside this
// deployment's fold is what makes it visible on an otherwise green test.
func TestSelfTestShowsAStateCodeThisDeploymentDoesNotMap(t *testing.T) {
	h := &probeSrv{byTable: map[string]probeReply{
		probeIncidentPath: {body: `{"result":[{"number":"INC0042","state":"18","opened_at":"2026-07-12 08:14:02"}]}`},
		probeJournalPath:  {body: probeJournalJSON},
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()
	res, err := newProbeFixture(t, srv.URL).SelfTest(context.Background(), "")
	if err != nil {
		t.Fatalf("an unmapped state code is a warning on a working tracker, not a failure: %v", err)
	}
	if !strings.Contains(res.Summary, "does not map") {
		t.Errorf("Summary must flag the unmapped code, got %q", res.Summary)
	}
}

// The console holds an operator on a spinner and moduletest bounds the activity at 30s with ONE attempt,
// so a probe that ignored ctx would hang the dialog rather than fail it.
func TestSelfTestRespectsContext(t *testing.T) {
	srv := httptest.NewServer(&probeSrv{byTable: probeOKRoutes()})
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newProbeFixture(t, srv.URL).SelfTest(ctx, ""); err == nil {
		t.Fatal("a cancelled context must abort the probe")
	}
}

// A base URL that carries userinfo must never render it: a Result is the most-pasted text in an incident.
func TestInstanceHostNeverRendersEmbeddedCredentials(t *testing.T) {
	m := New("https://svc:hunter2@dev12345.service-now.com", "svc", config.SecretRef("env:NOPE"))
	if got := m.instanceHost(); strings.Contains(got, "hunter2") || strings.Contains(got, "svc") {
		t.Fatalf("instanceHost leaked userinfo: %q", got)
	} else if got != "dev12345.service-now.com" {
		t.Fatalf("instanceHost = %q, want dev12345.service-now.com", got)
	}
}

// A probe CUT SHORT is not a best-effort miss. The incident read succeeds, the operator's context is then
// cancelled — the console's 30-second single attempt expiring looks the same from here — and the journal
// read never returns. Reporting that as a pass would show a green tick above a Detail whose own words are
// "the test was cancelled".
func TestSelfTestFailsWhenCancelledBetweenItsTwoReads(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == probeJournalPath {
			cancel()
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, probeIncidentJSON)
	}))
	defer srv.Close()

	res, err := newProbeFixture(t, srv.URL).SelfTest(ctx, "")
	if err == nil {
		t.Fatalf("a cancelled probe must FAIL, not pass with a journal note: %+v", res)
	}
	if !strings.Contains(res.Detail, "not a pass") {
		t.Errorf("Detail must say the test did not finish, got %q", res.Detail)
	}
}
