package awxjob

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// These tests drive SelfTest over a REAL *http.Client against a REAL httptest server — ClientConfig.HTTPClient
// is deliberately left nil so NewClient builds its production transport. The Doer fake the launch tests use
// (fakeAWX in awxjob_test.go) is the right seam for asserting launch-body shapes; it is the wrong seam here,
// because half of what this probe claims to establish IS the transport: a closed port, a TLS failure and a
// timeout are outcomes a Doer fake can only simulate by returning an error someone typed.

const selfTestTokenEnv = "TG_AWXJOB_SELFTEST_TOKEN"
const selfTestTokenValue = "launch-token-value-that-must-never-be-echoed"

// awxAPI is a stand-in AWX that serves the two GETs the probe makes and RECORDS every request it receives.
//
// The recording is not diagnostics. It is the test's only way to assert the safety property that matters most
// for this module — that pressing TEST launched nothing — and it is asserted in EVERY case, including the
// failing ones, because a probe that starts a job on its way to reporting an error has still started a job.
type awxAPI struct {
	mu   sync.Mutex
	hits []string // "GET /api/v2/me/"

	meStatus    int  // non-zero: force this status on /api/v2/me/
	tmplStatus  int  // non-zero: force this status on every /api/v2/job_templates/{id}/
	superuser   bool // what /me/ reports for is_superuser
	username    string
	nonAWXBody  bool           // serve a 200 that is not an AWX /me/ payload
	templates   map[int]string // id -> job-template JSON
	tmplRawBody string         // non-empty: serve this exact 200 body for EVERY template read, whatever the id
	sawMutation bool           // set if anything ever hit /launch/ or arrived non-GET
}

func (s *awxAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.hits = append(s.hits, r.Method+" "+r.URL.Path)
	if r.Method != http.MethodGet || strings.Contains(r.URL.Path, "/launch/") {
		s.sawMutation = true
	}
	s.mu.Unlock()

	// The credential is checked for real: a probe that never presented the token would pass against a revoked
	// one, which is one of the three things TEST exists to rule out.
	if r.Header.Get("Authorization") != "Bearer "+selfTestTokenValue {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"Authentication credentials were not provided."}`))
		return
	}

	switch {
	case r.URL.Path == "/api/v2/me/":
		if s.meStatus != 0 {
			w.WriteHeader(s.meStatus)
			_, _ = w.Write([]byte(`{"detail":"forced"}`))
			return
		}
		if s.nonAWXBody {
			_, _ = w.Write([]byte(`{"count":0,"results":[]}`))
			return
		}
		name := s.username
		if name == "" {
			name = "tg-launcher"
		}
		_, _ = fmt.Fprintf(w, `{"count":1,"results":[{"id":42,"username":%q,"is_superuser":%t}]}`, name, s.superuser)
	case strings.HasPrefix(r.URL.Path, "/api/v2/job_templates/"):
		if s.tmplStatus != 0 {
			w.WriteHeader(s.tmplStatus)
			_, _ = w.Write([]byte(`{"detail":"forced"}`))
			return
		}
		if s.tmplRawBody != "" {
			// The proxy/wrong-product case: a 200 carrying JSON that is not the template that was asked for.
			_, _ = w.Write([]byte(s.tmplRawBody))
			return
		}
		id, _ := strconv.Atoi(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v2/job_templates/"), "/"))
		body, ok := s.templates[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":"Not found."}`))
			return
		}
		_, _ = w.Write([]byte(body))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"Not found."}`))
	}
}

func (s *awxAPI) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.hits...)
}

func (s *awxAPI) mutated() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sawMutation
}

// template renders an AWX job-template payload with the two prompt-on-launch flags the probe reads.
func template(id int, name string, askVars, askLimit bool) string {
	return fmt.Sprintf(`{"id":%d,"name":%q,"job_type":"run","ask_variables_on_launch":%t,"ask_limit_on_launch":%t}`,
		id, name, askVars, askLimit)
}

// selfTestActuator builds an actuator over the served AWX with a REAL HTTP transport (HTTPClient nil).
func selfTestActuator(t *testing.T, baseURL string, allowlist TemplateAllowlist) *Actuator {
	t.Helper()
	t.Setenv(selfTestTokenEnv, selfTestTokenValue)
	c, err := NewClient(ClientConfig{BaseURL: baseURL, TokenRef: "env:" + selfTestTokenEnv})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	a, err := New(Config{Client: c, Allowlist: allowlist})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// oneTemplateAllowlist sanctions template 7 with a typed one-var schema (the canonical shape).
func oneTemplateAllowlist() TemplateAllowlist {
	return TemplateAllowlist{7: {OpClass: "restart-service", ExtraVarsSchema: ExtraVarsSchema{"service": VarString}}}
}

// ---------------------------------------------------------------------------------------------------------
// The table: what the operator is told, for each shape of outcome.
// ---------------------------------------------------------------------------------------------------------

func TestSelfTest(t *testing.T) {
	cases := []struct {
		name      string
		api       *awxAPI
		allowlist TemplateAllowlist
		wantErr   bool
		// wantSummary / wantDetail are substrings that must all appear.
		wantSummary []string
		wantDetail  []string
		notDetail   []string
	}{
		{
			// SUCCESS. Every load-bearing fragment of the Summary comes from the SERVED payload, not from the
			// configuration: the username and the template name are only knowable by having read them back.
			name: "success reports the account and the template names it read back",
			api: &awxAPI{templates: map[int]string{
				7: template(7, "Restart a service", true, true),
			}},
			allowlist:   oneTemplateAllowlist(),
			wantSummary: []string{"tg-launcher", "user id 42", `7="Restart a service"`, "nothing was launched"},
			wantDetail:  []string{"does not prove it may EXECUTE"},
		},
		{
			// A WRONG-INSTANCE probe must be legible. The served name differs from what the operator thinks
			// template 7 is; the probe cannot know that, but by printing the name it lets the operator see it.
			name: "success prints the served template name so a wrong instance is visible",
			api: &awxAPI{templates: map[int]string{
				7: template(7, "Delete the customer database", true, true),
			}},
			allowlist:   oneTemplateAllowlist(),
			wantSummary: []string{`7="Delete the customer database"`},
		},
		{
			name: "several templates are listed in a stable id order",
			api: &awxAPI{templates: map[int]string{
				3: template(3, "Alpha", true, true),
				7: template(7, "Bravo", true, true),
			}},
			allowlist: TemplateAllowlist{
				7: {OpClass: "b", ExtraVarsSchema: ExtraVarsSchema{"x": VarString}},
				3: {OpClass: "a", ExtraVarsSchema: ExtraVarsSchema{"x": VarString}},
			},
			wantSummary: []string{`3="Alpha", 7="Bravo"`, "re-read 2 of 2"},
		},
		{
			// 401 on the identity read: the credential class, named specifically, plus the cache caveat that
			// makes the advice actionable (saving a new token is not enough while the worker runs).
			name:        "401 names the credential as the problem",
			api:         &awxAPI{meStatus: http.StatusUnauthorized},
			allowlist:   oneTemplateAllowlist(),
			wantErr:     true,
			wantSummary: []string{"could not authenticate", "nothing was launched"},
			wantDetail:  []string{"401", "revoked", "RESTART the worker"},
		},
		{
			// 403 on the TEMPLATE read is a different diagnosis from 403 on identity: the token is valid, the
			// account simply cannot see that object. Telling an operator to replace a working token here would
			// send them to fix the wrong thing.
			name: "403 on a template names the permission, not the token",
			api: &awxAPI{tmplStatus: http.StatusForbidden, templates: map[int]string{
				7: template(7, "Restart a service", true, true),
			}},
			allowlist:   oneTemplateAllowlist(),
			wantErr:     true,
			wantSummary: []string{"tg-launcher", "1 of 1 checked sanctioned template(s) could not be read"},
			wantDetail:  []string{"403", "lacks permission", "at least Read"},
			notDetail:   []string{"revoked"},
		},
		{
			// THE FAILURE THIS MODULE EXISTS TO CATCH: an allowlist id that is not on this AWX. A bare auth
			// check passes here, and the lane then refuses at the flip.
			name:        "a sanctioned template AWX does not have is an error naming the wrong-instance case",
			api:         &awxAPI{templates: map[int]string{}},
			allowlist:   oneTemplateAllowlist(),
			wantErr:     true,
			wantSummary: []string{"could not be read"},
			wantDetail:  []string{"404", "DIFFERENT AWX", "job template 7"},
		},
		{
			// THE SAME FALSE GREEN AS "a 200 with no user", ON THE OTHER READ. A reverse proxy, a captive
			// portal or a different product answering on this base URL serves a 200 whose body unmarshals
			// cleanly into a struct of zero values. Unguarded, the probe reported `0=""` as a successfully
			// re-read sanctioned template and returned a PASS — a green tick certifying an estate nobody
			// reached, on the one module where the thing being certified is a launch allowlist.
			name:        "a 200 that is not a job template is not a re-read",
			api:         &awxAPI{tmplRawBody: `{}`},
			allowlist:   oneTemplateAllowlist(),
			wantErr:     true,
			wantSummary: []string{"could not be read", "nothing was launched"},
			wantDetail:  []string{"not as AWX"},
			notDetail:   []string{`0=""`},
		},
		{
			// AWX answering template 99 has not answered the question asked. Reporting the SERVED id back
			// would let an endpoint that returns the same object for every template read look like a clean
			// sweep of the whole allowlist — which is exactly the wrong-instance failure this second read
			// exists to expose, dressed as a pass.
			name:        "AWX answering with a different template id is refused, not reported as the sanctioned one",
			api:         &awxAPI{tmplRawBody: `{"id":99,"name":"Something else","ask_variables_on_launch":true,"ask_limit_on_launch":true}`},
			allowlist:   oneTemplateAllowlist(),
			wantErr:     true,
			wantSummary: []string{"could not be read"},
			wantDetail:  []string{"asked AWX for job template 7", "answered with template 99"},
			notDetail:   []string{`99="Something else"`},
		},
		{
			// A 404 on the IDENTITY read means the base URL is not an AWX API. Sending the operator to audit
			// template ids for that is the confident-wrong-diagnosis failure: they go and fix the thing we
			// named, find nothing wrong with it, and learn to distrust the button.
			name:        "404 on the identity read blames the base URL, not the allowlist",
			api:         &awxAPI{meStatus: http.StatusNotFound},
			allowlist:   oneTemplateAllowlist(),
			wantErr:     true,
			wantSummary: []string{"could not authenticate"},
			wantDetail:  []string{"no AWX API at this base URL", "NOT implicated"},
			notDetail:   []string{"the id in the sanctioned-templates list is wrong"},
		},
		{
			// A 403 on the identity read is a token SCOPE / account-policy problem. "Grant it Read on the job
			// template" names an object the probe never got as far as asking for.
			name:        "403 on the identity read names the token scope, not a job-template grant",
			api:         &awxAPI{meStatus: http.StatusForbidden},
			allowlist:   oneTemplateAllowlist(),
			wantErr:     true,
			wantSummary: []string{"could not authenticate"},
			wantDetail:  []string{"403", "SCOPE"},
			notDetail:   []string{"Read on the job template"},
		},
		{
			// An empty allowlist is an honest pass with a caveat: the credential and endpoint really are
			// proven, and the lane really can only refuse.
			name:        "an empty allowlist passes but says the lane can only refuse",
			api:         &awxAPI{},
			allowlist:   TemplateAllowlist{},
			wantSummary: []string{"no sanctioned templates to check"},
			wantDetail:  []string{"EMPTY", "can only refuse"},
		},
		{
			// A 200 that is not AWX (a proxy error page, a captive portal) must not read as authenticated.
			name:        "a 200 with no user is not a pass",
			api:         &awxAPI{nonAWXBody: true},
			allowlist:   oneTemplateAllowlist(),
			wantErr:     true,
			wantSummary: []string{"could not authenticate"},
			wantDetail:  []string{"not as AWX"},
		},
		{
			// The prompt-on-launch reads: a template that would refuse every launch carrying extra_vars is a
			// note now rather than a surprise after the owner-present flip.
			name: "a template that will not accept the declared variables is reported as a note",
			api: &awxAPI{templates: map[int]string{
				7: template(7, "Restart a service", false, true),
			}},
			allowlist:  oneTemplateAllowlist(),
			wantDetail: []string{"ask_variables_on_launch is off", "Prompt on launch"},
		},
		{
			name: "a template that will not accept a host limit is reported as a note",
			api: &awxAPI{templates: map[int]string{
				7: template(7, "Restart a service", true, false),
			}},
			allowlist:  oneTemplateAllowlist(),
			wantDetail: []string{"ask_limit_on_launch is off", "target host"},
		},
		{
			// A least-privilege finding only visible from here.
			name: "a superuser launch token is reported",
			api: &awxAPI{superuser: true, templates: map[int]string{
				7: template(7, "Restart a service", true, true),
			}},
			allowlist:  oneTemplateAllowlist(),
			wantDetail: []string{"SUPERUSER"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.api)
			defer srv.Close()

			a := selfTestActuator(t, srv.URL, tc.allowlist)
			res, err := a.SelfTest(context.Background(), "operator@example")

			if tc.wantErr && err == nil {
				t.Fatalf("SelfTest: want error, got nil (summary %q)", res.Summary)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("SelfTest: unexpected error: %v (detail %q)", err, res.Detail)
			}
			for _, want := range tc.wantSummary {
				if !strings.Contains(res.Summary, want) {
					t.Errorf("Summary %q does not contain %q", res.Summary, want)
				}
			}
			for _, want := range tc.wantDetail {
				if !strings.Contains(res.Detail, want) {
					t.Errorf("Detail %q does not contain %q", res.Detail, want)
				}
			}
			for _, unwanted := range tc.notDetail {
				if strings.Contains(res.Detail, unwanted) {
					t.Errorf("Detail %q must not contain %q", res.Detail, unwanted)
				}
			}
			// THE STANDING SAFETY ASSERTION, made in every case including the failing ones: the probe must
			// never have launched anything, and must never have issued a non-GET.
			if tc.api.mutated() {
				t.Fatalf("SelfTest issued a mutating request: %v", tc.api.requests())
			}
			// The token is never echoed back to the operator. Result and error text both get pasted into
			// tickets.
			if strings.Contains(res.Summary+res.Detail, selfTestTokenValue) {
				t.Fatal("SelfTest leaked the launch token into the Result")
			}
			if err != nil && strings.Contains(err.Error(), selfTestTokenValue) {
				t.Fatal("SelfTest leaked the launch token into the error")
			}
		})
	}
}

// ---------------------------------------------------------------------------------------------------------
// THE KILLING ORACLE.
// ---------------------------------------------------------------------------------------------------------

// TestSelfTestIsNotAConfiguredValuesCheck is the test that FAILS if SelfTest is ever replaced by a
// "the configured values are non-empty" check.
//
// Every input here is present and well-formed: a base URL, a resolvable launch-token reference, a non-empty
// token value, and a sanctioned template with a typed schema. A configuration validator sees a perfect module
// and returns a pass. The estate says otherwise — AWX rejects the credential — and this test asserts an ERROR.
//
// This is what makes the probe more than a mock. The three things an operator presses TEST to rule out are a
// revoked credential, a permission never granted, and a host that has been down for a week; a non-empty-values
// check passes all three, and would pass this test's setup too.
func TestSelfTestIsNotAConfiguredValuesCheck(t *testing.T) {
	api := &awxAPI{meStatus: http.StatusUnauthorized}
	srv := httptest.NewServer(api)
	defer srv.Close()

	a := selfTestActuator(t, srv.URL, oneTemplateAllowlist())

	// Everything a config check could look at is populated and non-empty.
	if a.client.baseURL == "" || strings.TrimSpace(string(a.client.tokenRef)) == "" || len(a.allowlist) == 0 {
		t.Fatal("fixture is wrong: the killing oracle requires COMPLETE configuration")
	}
	if tok, err := a.client.token(); err != nil || tok == "" {
		t.Fatalf("fixture is wrong: the launch token must resolve to a non-empty value (%v)", err)
	}

	res, err := a.SelfTest(context.Background(), "operator@example")
	if err == nil {
		t.Fatalf("SelfTest returned nil error against an AWX that rejects the credential — this is a "+
			"configured-values check wearing a test's name (summary %q)", res.Summary)
	}
	if !strings.Contains(res.Detail, "401") {
		t.Errorf("Detail %q does not name the credential failure", res.Detail)
	}
	if len(api.requests()) == 0 {
		t.Fatal("SelfTest never issued a request — it cannot have exercised the real network path")
	}
}

// TestSelfTestUnreachableIsAnErrorNotAPass closes the server before probing, so the port is dead: the same
// shape as an AWX that has been down for a week. A configured-values check cannot tell this from a healthy
// deployment.
func TestSelfTestUnreachableIsAnErrorNotAPass(t *testing.T) {
	srv := httptest.NewServer(&awxAPI{})
	base := srv.URL
	srv.Close() // the port is now closed — every dial is refused

	a := selfTestActuator(t, base, oneTemplateAllowlist())

	res, err := a.SelfTest(context.Background(), "operator@example")
	if err == nil {
		t.Fatalf("SelfTest passed against a closed port (summary %q)", res.Summary)
	}
	if !strings.Contains(res.Detail, "could not be reached") {
		t.Errorf("Detail %q does not say AWX is unreachable", res.Detail)
	}
	// The diagnosis must not blame the credential: nothing ever read it.
	if strings.Contains(res.Detail, "revoked") {
		t.Errorf("Detail %q blames the token for a transport failure", res.Detail)
	}
}

// TestSelfTestNeverTouchesLaunch is the read-only guarantee stated as an assertion rather than as a comment:
// across a full successful probe, EVERY request AWX saw was a GET, and none of them was a launch.
//
// The Actuator's only surface method is Exec, which POSTs to /launch/. Nothing but this test stands between a
// future refactor that "reuses the existing client method" and an unreviewed actuation fired from a settings
// dialog.
func TestSelfTestNeverTouchesLaunch(t *testing.T) {
	api := &awxAPI{templates: map[int]string{
		3: template(3, "Alpha", true, true),
		7: template(7, "Bravo", true, true),
	}}
	srv := httptest.NewServer(api)
	defer srv.Close()

	a := selfTestActuator(t, srv.URL, TemplateAllowlist{
		3: {OpClass: "a", ExtraVarsSchema: ExtraVarsSchema{"x": VarString}},
		7: {OpClass: "b", ExtraVarsSchema: ExtraVarsSchema{"x": VarString}},
	})
	if _, err := a.SelfTest(context.Background(), "operator@example"); err != nil {
		t.Fatalf("SelfTest: %v", err)
	}

	got := api.requests()
	want := []string{"GET /api/v2/me/", "GET /api/v2/job_templates/3/", "GET /api/v2/job_templates/7/"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("requests = %v, want exactly %v", got, want)
	}
}

// TestSelfTestBoundsTheNumberOfTemplateReads proves the probe stays inside the console's 30s budget by
// construction: an operator with a long allowlist gets a bounded, deterministic set of reads and a Summary
// that says how many of how many were checked, rather than a timeout that reads as "AWX is down".
func TestSelfTestBoundsTheNumberOfTemplateReads(t *testing.T) {
	api := &awxAPI{templates: map[int]string{}}
	allowlist := TemplateAllowlist{}
	for id := 1; id <= maxProbedTemplates+3; id++ {
		api.templates[id] = template(id, "T"+strconv.Itoa(id), true, true)
		allowlist[id] = TemplatePolicy{OpClass: "c" + strconv.Itoa(id), ExtraVarsSchema: ExtraVarsSchema{}}
	}
	srv := httptest.NewServer(api)
	defer srv.Close()

	a := selfTestActuator(t, srv.URL, allowlist)
	res, err := a.SelfTest(context.Background(), "operator@example")
	if err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	// 1 identity read + exactly maxProbedTemplates template reads.
	if n := len(api.requests()); n != maxProbedTemplates+1 {
		t.Fatalf("issued %d requests, want %d: %v", n, maxProbedTemplates+1, api.requests())
	}
	if !strings.Contains(res.Summary, fmt.Sprintf("re-read %d of %d", maxProbedTemplates, len(allowlist))) {
		t.Errorf("Summary %q does not disclose the partial read", res.Summary)
	}
	if !strings.Contains(res.Detail, "were NOT checked") {
		t.Errorf("Detail %q does not disclose the unchecked templates", res.Detail)
	}
}

// TestSelfTestCancelledContextStops proves the probe honours ctx: moduletest bounds the activity and a probe
// that ignored cancellation would keep spending the operator's spinner after the answer stopped mattering.
func TestSelfTestCancelledContextStops(t *testing.T) {
	api := &awxAPI{templates: map[int]string{7: template(7, "Restart a service", true, true)}}
	srv := httptest.NewServer(api)
	defer srv.Close()

	a := selfTestActuator(t, srv.URL, oneTemplateAllowlist())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := a.SelfTest(ctx, "operator@example"); err == nil {
		t.Fatal("SelfTest ignored a cancelled context")
	}
	if api.mutated() {
		t.Fatalf("SelfTest issued a mutating request: %v", api.requests())
	}
}

// TestSelfTestSecretFailuresAreNamedAsTGSide keeps the two secret-lane faults distinct from an AWX rejection.
// An operator told "AWX rejected the credential" for an unresolvable SecretRef goes and mints a new AWX token
// for a problem that never left this process.
func TestSelfTestSecretFailuresAreNamedAsTGSide(t *testing.T) {
	api := &awxAPI{templates: map[int]string{7: template(7, "Restart a service", true, true)}}
	srv := httptest.NewServer(api)
	defer srv.Close()

	t.Run("empty value at the reference", func(t *testing.T) {
		t.Setenv(selfTestTokenEnv, "") // the reference resolves; the value stored there is blank
		c, err := NewClient(ClientConfig{BaseURL: srv.URL, TokenRef: "env:" + selfTestTokenEnv})
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		a, err := New(Config{Client: c, Allowlist: oneTemplateAllowlist()})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		res, err := a.SelfTest(context.Background(), "operator@example")
		if err == nil {
			t.Fatal("SelfTest passed with an empty launch token")
		}
		if !strings.Contains(res.Detail, "EMPTY") {
			t.Errorf("Detail %q does not name the empty secret", res.Detail)
		}
		if strings.Contains(res.Detail, "AWX rejected") {
			t.Errorf("Detail %q blames AWX for a TG-side secret fault", res.Detail)
		}
	})
}

// TestSelfTestUnconfiguredActuator covers the zero-value receiver: an honest "not configured" rather than a
// nil-pointer panic the console would render as an infrastructure fault.
func TestSelfTestUnconfiguredActuator(t *testing.T) {
	var a Actuator
	res, err := a.SelfTest(context.Background(), "operator@example")
	if err == nil {
		t.Fatal("an actuator with no client must not report a pass")
	}
	if !strings.Contains(res.Summary, "no launch client") {
		t.Errorf("Summary %q does not say the lane is unconfigured", res.Summary)
	}
}

// TestDescriptorTestVerbPromisesOnlyWhatSelfTestDoes ties the consent contract to the code. The verb is shown
// BEFORE the press, on the one module whose surface method starts real jobs, so it must say GET and it must
// say that nothing is launched — and it must not promise the job-status read this probe cannot perform (that
// read needs a job id, and a lane that has never launched has none).
func TestDescriptorTestVerbPromisesOnlyWhatSelfTestDoes(t *testing.T) {
	d := Descriptor()
	if d.Test.Mutating {
		t.Fatal("the AWX-job Test must be declared non-mutating")
	}
	verb := strings.ToLower(d.Test.Verb)
	for _, must := range []string{"get only", "launches nothing", "sanctioned job template"} {
		if !strings.Contains(verb, must) {
			t.Errorf("Test.Verb %q does not contain %q", d.Test.Verb, must)
		}
	}
	if strings.Contains(verb, "job's status") {
		t.Errorf("Test.Verb %q still promises a job-status read SelfTest does not perform", d.Test.Verb)
	}
}

// TestWhoAmIAndGetJobTemplateAreGETOnly pins the two new client reads at the client level, independent of the
// probe that composes them: neither may ever become a write.
func TestWhoAmIAndGetJobTemplateAreGETOnly(t *testing.T) {
	api := &awxAPI{templates: map[int]string{7: template(7, "Restart a service", true, false)}}
	srv := httptest.NewServer(api)
	defer srv.Close()

	t.Setenv(selfTestTokenEnv, selfTestTokenValue)
	c, err := NewClient(ClientConfig{BaseURL: srv.URL, TokenRef: "env:" + selfTestTokenEnv})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	who, err := c.WhoAmI(context.Background())
	if err != nil {
		t.Fatalf("WhoAmI: %v", err)
	}
	if who.Username != "tg-launcher" || who.ID != 42 {
		t.Fatalf("WhoAmI = %+v, want the served account", who)
	}
	jt, err := c.GetJobTemplate(context.Background(), 7)
	if err != nil {
		t.Fatalf("GetJobTemplate: %v", err)
	}
	if jt.Name != "Restart a service" || !jt.AskVariablesOnLaunch || jt.AskLimitOnLaunch {
		t.Fatalf("GetJobTemplate = %+v, want the served template with its prompt flags", jt)
	}
	if _, err := c.GetJobTemplate(context.Background(), 0); err == nil {
		t.Fatal("GetJobTemplate must refuse a non-positive id")
	}
	if api.mutated() {
		t.Fatalf("a read method issued a mutating request: %v", api.requests())
	}
}

// TestGetJobTemplateRefusesAnAnswerThatIsNotTheTemplateAsked pins the guard at the client level, where it
// belongs: GetJobTemplate(7) means "the job template numbered 7 on this AWX", and a 200 carrying anything else
// has not answered that. Without this, valid-but-empty JSON from a proxy decodes into a zero-valued struct and
// every caller downstream — the self-test included — treats it as a successful read.
func TestGetJobTemplateRefusesAnAnswerThatIsNotTheTemplateAsked(t *testing.T) {
	for _, tc := range []struct {
		name, body, wantErr string
	}{
		{"valid JSON that is not a template", `{}`, "not as AWX"},
		{"a different template than the one asked for", `{"id":99,"name":"Something else"}`, "answered with template 99"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &awxAPI{tmplRawBody: tc.body}
			srv := httptest.NewServer(api)
			defer srv.Close()

			t.Setenv(selfTestTokenEnv, selfTestTokenValue)
			c, err := NewClient(ClientConfig{BaseURL: srv.URL, TokenRef: "env:" + selfTestTokenEnv})
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			jt, err := c.GetJobTemplate(context.Background(), 7)
			if err == nil {
				t.Fatalf("GetJobTemplate accepted %s as job template 7: %+v", tc.body, jt)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestStatusErrorCarriesTheCode proves failures are classifiable by SHAPE. Vendor prose changes between AWX
// releases and a proxy in front of AWX substitutes its own error page entirely; the status code does not.
func TestStatusErrorCarriesTheCode(t *testing.T) {
	api := &awxAPI{meStatus: http.StatusForbidden}
	srv := httptest.NewServer(api)
	defer srv.Close()

	t.Setenv(selfTestTokenEnv, selfTestTokenValue)
	c, err := NewClient(ClientConfig{BaseURL: srv.URL, TokenRef: "env:" + selfTestTokenEnv})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = c.WhoAmI(context.Background())
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("WhoAmI error %v is not a *StatusError", err)
	}
	if se.Status != http.StatusForbidden {
		t.Fatalf("StatusError.Status = %d, want 403", se.Status)
	}
	// The historical message shape is preserved, so existing callers reading the text are unaffected.
	if !strings.Contains(se.Error(), "status 403") {
		t.Fatalf("StatusError.Error() = %q, want the historical \"status 403\" shape", se.Error())
	}
	if strings.Contains(se.Error(), selfTestTokenValue) {
		t.Fatal("StatusError leaked the token")
	}
}
