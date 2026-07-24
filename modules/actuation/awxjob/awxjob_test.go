package awxjob

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	actuation "github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/regime"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// ---------------------------------------------------------------------------------------------------------
// fakeAWX is a canned AWX that records every request (method + path + body) and COUNTS launches, so a refusal
// can be proven by "the launch endpoint was never hit". A test may script a specific launch/job response.
// ---------------------------------------------------------------------------------------------------------

type fakeAWX struct {
	mu       sync.Mutex
	methods  []string
	paths    []string
	bodies   []string
	launches int // POST .../launch/ count — the load-bearing "did it actuate?" counter

	launchResp string // JSON returned by a launch POST (default: job 123 pending)
	launchCode int    // status code for a launch POST (default 201)
	jobResp    string // JSON returned by GET /jobs/{id}/ (default: job 123 successful)
}

func (f *fakeAWX) Do(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body string
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		body = string(b)
	}
	f.methods = append(f.methods, req.Method)
	f.paths = append(f.paths, req.URL.Path)
	f.bodies = append(f.bodies, body)

	code := 200
	resp := "{}"
	switch {
	case strings.HasSuffix(req.URL.Path, "/launch/") && req.Method == http.MethodPost:
		f.launches++
		code = f.launchCode
		if code == 0 {
			code = 201
		}
		resp = f.launchResp
		if resp == "" {
			resp = `{"id":123,"job":123,"status":"pending","job_template":7,"url":"/api/v2/jobs/123/"}`
		}
	case strings.HasPrefix(req.URL.Path, "/api/v2/jobs/") && req.Method == http.MethodGet:
		resp = f.jobResp
		if resp == "" {
			resp = `{"id":123,"status":"successful","failed":false,"started":"t0","finished":"t1"}`
		}
	}
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(resp)), Header: make(http.Header)}, nil
}

func (f *fakeAWX) launchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.launches
}

// ---------------------------------------------------------------------------------------------------------
// Fixtures: a launch-capable actuator over a fake AWX, and an admissible interceptor request for the awx-job
// lane (mirrors core/actuate's admissible mutating request so it drives the REAL chain).
// ---------------------------------------------------------------------------------------------------------

const launchTokenEnv = "TG_AWXJOB_TEST_LAUNCH_TOKEN"

// canonicalAllowlist sanctions template 7 (restart-service) with a typed one-var schema.
func canonicalAllowlist() TemplateAllowlist {
	return TemplateAllowlist{
		7: {OpClass: "restart-service", ExtraVarsSchema: ExtraVarsSchema{"service": VarString}},
	}
}

// newActuator builds a launch-capable actuator over the fake AWX with the given mode chokepoint.
func newActuator(t *testing.T, awx *fakeAWX, gate *safety.Chokepoint) *Actuator {
	t.Helper()
	t.Setenv(launchTokenEnv, "launch-token-distinct-from-sensor")
	c, err := NewClient(ClientConfig{
		BaseURL:    "https://awx.test",
		TokenRef:   "env:" + launchTokenEnv, // the LAUNCH token, a sealed SecretRef distinct from the sensor token (REQ-1708)
		HTTPClient: awx,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	a, err := New(Config{Client: c, Allowlist: canonicalAllowlist(), ModeGate: gate})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func noObserved(context.Context) []verify.ObservedAlert { return []verify.ObservedAlert{} }

// launchRequest builds an admissible interceptor request for a template-7 launch with a valid typed var.
func launchRequest(t *testing.T, spec LaunchSpec) actuate.Request {
	t.Helper()
	m, err := manifest.New(
		manifest.Action{Target: "web01", OpClass: "restart-service", Op: "restart", Reversible: true},
		safety.BandAuto, "plan#awxjob", "pred#awxjob",
	)
	if err != nil {
		t.Fatalf("build manifest: %v", err)
	}
	argv, stdin, err := EncodeLaunch(spec)
	if err != nil {
		t.Fatalf("EncodeLaunch: %v", err)
	}
	return actuate.Request{
		Manifest: m,
		Gated:    true,
		Argv:     argv,
		Stdin:    stdin,
		Evidence: []actuate.Evidence{{ToolResultID: "tr-1", Captured: true, Successful: true, Recent: true, Relevant: true}},
		Observe:  noObserved,
		Band:     safety.BandAuto, // fresh per-incident band (TG-126): AUTO admits at 1b
	}
}

func validSpec() LaunchSpec {
	return LaunchSpec{TemplateID: 7, OpClass: "restart-service", ExtraVars: map[string]any{"service": "nginx"}, Limit: "web01"}
}

// ---------------------------------------------------------------------------------------------------------
// THE CORE SAFETY PROOF (REQ-1702/1707): the awx-job actuator is INERT at Shadow, and it reaches the AWX
// launch exactly once ONLY through actuate.Interceptor.Do under an actuating chokepoint.
// ---------------------------------------------------------------------------------------------------------

// TestAWXJobLane_InertAtShadow_LaunchesOnlyViaInterceptor is the load-bearing test the task requires: driving
// the awx-job lane through the SAME regime.LaneEffect seam production uses, a proposed launch is REFUSED with
// ZERO launches under a Shadow chokepoint (the default/only reachable mode), and reaches the AWX launch
// endpoint EXACTLY ONCE under a test-only actuating chokepoint — always via Interceptor.Do, never around it.
func TestAWXJobLane_InertAtShadow_LaunchesOnlyViaInterceptor(t *testing.T) {
	// --- Shadow: the production posture. The interceptor's mode chokepoint refuses BEFORE Exec; 0 launches. ---
	awxOff := &fakeAWX{}
	shadow := safety.NewReadOnlyChokepoint() // mode Shadow — MayActuate is always false
	laneOff := regime.NewAWXJobLane(regime.WithAWXActuator(newActuator(t, awxOff, shadow)))
	seamOff := regime.NewLaneEffect(func(l actuation.Actuator) *actuate.Interceptor {
		return actuate.NewInterceptor(shadow, l, audit.NewLedger())
	})
	out, err := seamOff.Apply(context.Background(), laneOff, launchRequest(t, validSpec()))
	if err != nil {
		t.Fatalf("Apply must not error on a wired seam under Shadow: %v", err)
	}
	if !out.Refused || out.Executed {
		t.Fatalf("under Shadow the awx-job launch must be REFUSED before execute, got %+v", out)
	}
	if awxOff.launchCount() != 0 {
		t.Fatalf("under Shadow NO AWX job may be launched, got %d launches", awxOff.launchCount())
	}

	// --- Actuating (test-only): the SAME lane now reaches the AWX launch exactly once, only via Do. ---
	awxOn := &fakeAWX{}
	actuating := safety.NewActuatingChokepoint()
	laneOn := regime.NewAWXJobLane(regime.WithAWXActuator(newActuator(t, awxOn, actuating)))
	seamOn := regime.NewLaneEffect(func(l actuation.Actuator) *actuate.Interceptor {
		return actuate.NewInterceptor(actuating, l, audit.NewLedger())
	})
	out2, err := seamOn.Apply(context.Background(), laneOn, launchRequest(t, validSpec()))
	if err != nil {
		t.Fatalf("Apply must not error on a wired seam: %v", err)
	}
	if !out2.Executed {
		t.Fatalf("under an actuating chokepoint the launch must execute via Do, got %+v", out2)
	}
	if awxOn.launchCount() != 1 {
		t.Fatalf("the AWX launch endpoint must be hit EXACTLY once via Interceptor.Do, got %d", awxOn.launchCount())
	}
	if !strings.HasSuffix(awxOn.paths[len(awxOn.paths)-1], "/api/v2/job_templates/7/launch/") {
		t.Fatalf("the launch must POST the allowlisted template's launch endpoint, got %v", awxOn.paths)
	}
}

// TestActuator_Exec_InertAtShadow_DirectCall proves the effect-leaf defense in depth: even called DIRECTLY
// (not through the interceptor), the actuator refuses to launch while the mode chokepoint is Shadow (or nil)
// — two independent gates keep mutation OFF.
func TestActuator_Exec_InertAtShadow_DirectCall(t *testing.T) {
	awx := &fakeAWX{}
	argv, stdin, _ := EncodeLaunch(validSpec())

	// nil gate ⇒ read-only actuator, no launch path.
	aNil := newActuator(t, awx, nil)
	if !aNil.ReadOnly() {
		t.Fatal("a nil-gate actuator must be ReadOnly()")
	}
	if _, err := aNil.Exec(context.Background(), argv, stdin); err == nil {
		t.Fatal("a nil-gate actuator must refuse Exec (fail closed)")
	}

	// Shadow gate ⇒ still read-only, Exec refuses.
	aShadow := newActuator(t, awx, safety.NewReadOnlyChokepoint())
	if !aShadow.ReadOnly() {
		t.Fatal("a Shadow-gate actuator must be ReadOnly()")
	}
	if _, err := aShadow.Exec(context.Background(), argv, stdin); err == nil {
		t.Fatal("a Shadow-gate actuator must refuse Exec (fail closed)")
	}
	if awx.launchCount() != 0 {
		t.Fatalf("no launch may fire while the mode is Shadow, got %d", awx.launchCount())
	}

	// Actuating gate ⇒ NOT read-only, Exec launches exactly once.
	aOn := newActuator(t, awx, safety.NewActuatingChokepoint())
	if aOn.ReadOnly() {
		t.Fatal("an actuating-gate actuator with an allowlist must NOT be ReadOnly()")
	}
	res, err := aOn.Exec(context.Background(), argv, stdin)
	if err != nil {
		t.Fatalf("an actuating actuator must launch: %v", err)
	}
	if awx.launchCount() != 1 {
		t.Fatalf("exactly one launch expected, got %d", awx.launchCount())
	}
	launched, err := DecodeLaunched(res.Stdout)
	if err != nil || launched.JobID != 123 {
		t.Fatalf("Exec must return the async job handle (job 123), got %+v err=%v", launched, err)
	}
}

// ---------------------------------------------------------------------------------------------------------
// REQ-1704: allowlist + op-class binding. A non-allowlisted template or an op-class/template mismatch
// refuses with ZERO launches (proven under an actuating gate so ONLY the allowlist can refuse).
// ---------------------------------------------------------------------------------------------------------

func TestExec_AllowlistAndOpClassBinding(t *testing.T) {
	cases := []struct {
		name    string
		spec    LaunchSpec
		wantErr error
	}{
		{"non-allowlisted template refuses", LaunchSpec{TemplateID: 999, OpClass: "restart-service", ExtraVars: map[string]any{"service": "nginx"}}, ErrTemplateNotAllowlisted},
		{"op-class mismatch refuses", LaunchSpec{TemplateID: 7, OpClass: "reboot-host", ExtraVars: map[string]any{"service": "nginx"}}, ErrOpClassMismatch},
		{"allowlisted + matched op-class launches", validSpec(), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			awx := &fakeAWX{}
			a := newActuator(t, awx, safety.NewActuatingChokepoint())
			argv, stdin, _ := EncodeLaunch(tc.spec)
			_, err := a.Exec(context.Background(), argv, stdin)
			if tc.wantErr == nil {
				if err != nil || awx.launchCount() != 1 {
					t.Fatalf("expected a launch, err=%v launches=%d", err, awx.launchCount())
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), strings.SplitN(tc.wantErr.Error(), ":", 2)[0]) {
				t.Fatalf("expected refusal %v, got %v", tc.wantErr, err)
			}
			if awx.launchCount() != 0 {
				t.Fatalf("a refused template must NOT launch, got %d", awx.launchCount())
			}
		})
	}
}

// ---------------------------------------------------------------------------------------------------------
// REQ-1705: typed extra_vars. An undeclared key or a type mismatch refuses with ZERO launches; there is no
// free-form command string in the launch body (the body carries only the typed extra_vars + limit).
// ---------------------------------------------------------------------------------------------------------

func TestExec_TypedExtraVarsRejectsUnknownKey(t *testing.T) {
	awx := &fakeAWX{}
	a := newActuator(t, awx, safety.NewActuatingChokepoint())
	// "shell" is not in template 7's schema {service:string} — a smuggled var is rejected.
	spec := LaunchSpec{TemplateID: 7, OpClass: "restart-service", ExtraVars: map[string]any{"service": "nginx", "shell": "rm -rf /"}}
	argv, stdin, _ := EncodeLaunch(spec)
	_, err := a.Exec(context.Background(), argv, stdin)
	if err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("an undeclared extra_var must be rejected, got %v", err)
	}
	if awx.launchCount() != 0 {
		t.Fatalf("a rejected extra_var must NOT launch, got %d", awx.launchCount())
	}
}

func TestExec_LaunchBodyIsTypedVarsOnly_NoCommandString(t *testing.T) {
	awx := &fakeAWX{}
	a := newActuator(t, awx, safety.NewActuatingChokepoint())
	argv, stdin, _ := EncodeLaunch(validSpec())
	if len(argv) != 1 || argv[0] != LaunchVerb {
		t.Fatalf("argv must be the fixed launch verb only (no command string), got %v", argv)
	}
	if _, err := a.Exec(context.Background(), argv, stdin); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	body := awx.bodies[len(awx.bodies)-1]
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("launch body must be JSON: %v (%s)", err, body)
	}
	// Only extra_vars + limit may appear — no command / argv / cmd field.
	for k := range got {
		if k != "extra_vars" && k != "limit" {
			t.Fatalf("launch body must carry only typed extra_vars + limit, saw key %q", k)
		}
	}
	ev, _ := got["extra_vars"].(map[string]any)
	if ev["service"] != "nginx" {
		t.Fatalf("the typed extra_var must be sent, got %v", got["extra_vars"])
	}
}

// ---------------------------------------------------------------------------------------------------------
// REQ-1705 (async-gap): AWX reporting a SENT field under ignored_fields is a launch refusal, never a no-op.
// ---------------------------------------------------------------------------------------------------------

func TestLaunch_IgnoredFieldIsRefusal(t *testing.T) {
	awx := &fakeAWX{launchResp: `{"id":124,"status":"pending","ignored_fields":{"extra_vars":{"service":"nginx"}}}`}
	a := newActuator(t, awx, safety.NewActuatingChokepoint())
	argv, stdin, _ := EncodeLaunch(validSpec())
	_, err := a.Exec(context.Background(), argv, stdin)
	if err == nil || !strings.Contains(err.Error(), "ignored") {
		t.Fatalf("an ignored extra_var must be a refusal, got %v", err)
	}
}

// ---------------------------------------------------------------------------------------------------------
// REQ-1708: the launch token is a sealed SecretRef, distinct from a read-only sensor token, and never a
// literal. An empty token reference fails closed at construction.
// ---------------------------------------------------------------------------------------------------------

func TestClient_TokenIsSealedRef_FailsClosedEmpty(t *testing.T) {
	if _, err := NewClient(ClientConfig{BaseURL: "https://awx.test", TokenRef: ""}); err == nil {
		t.Fatal("an empty launch token reference must fail closed")
	}
	// The launch token env is distinct from any sensor token env, and is a reference not a literal.
	os.Unsetenv(launchTokenEnv)
	c, err := NewClient(ClientConfig{BaseURL: "https://awx.test", TokenRef: "env:" + launchTokenEnv, HTTPClient: &fakeAWX{}})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	// With the env unset the token cannot resolve — a launch fails closed (never launches with a blank token).
	if _, err := c.Launch(context.Background(), 7, map[string]any{"service": "nginx"}, "web01"); err == nil {
		t.Fatal("a launch with an unresolvable token must fail closed")
	}
}

// ---------------------------------------------------------------------------------------------------------
// REQ-1709: GetJob polls a job to a terminal status; the terminal set is the four AWX finished states.
// ---------------------------------------------------------------------------------------------------------

func TestGetJob_And_TerminalStatuses(t *testing.T) {
	awx := &fakeAWX{jobResp: `{"id":123,"status":"successful","failed":false,"started":"t0","finished":"t1"}`}
	t.Setenv(launchTokenEnv, "launch-token")
	c, _ := NewClient(ClientConfig{BaseURL: "https://awx.test", TokenRef: "env:" + launchTokenEnv, HTTPClient: awx})
	j, err := c.GetJob(context.Background(), 123)
	if err != nil || j.Status != "successful" {
		t.Fatalf("GetJob: %+v err=%v", j, err)
	}
	for _, s := range []string{"successful", "failed", "error", "canceled"} {
		if !IsTerminalStatus(s) {
			t.Fatalf("%q must be terminal", s)
		}
	}
	for _, s := range []string{"pending", "waiting", "running", "new", ""} {
		if IsTerminalStatus(s) {
			t.Fatalf("%q must NOT be terminal", s)
		}
	}
}

// ---------------------------------------------------------------------------------------------------------
// extra_vars schema validation units + encode/decode round-trip.
// ---------------------------------------------------------------------------------------------------------

func TestExtraVarsSchema_Validate(t *testing.T) {
	s := ExtraVarsSchema{"service": VarString, "replicas": VarNumber, "force": VarBool}
	// Valid typed subset (numbers arrive as float64 after a JSON round-trip).
	if err := s.Validate(map[string]any{"service": "nginx", "replicas": float64(3), "force": true}); err != nil {
		t.Fatalf("valid vars rejected: %v", err)
	}
	// A subset (omitting keys) is fine.
	if err := s.Validate(map[string]any{"service": "nginx"}); err != nil {
		t.Fatalf("a subset must be accepted: %v", err)
	}
	// Unknown key rejected.
	if err := s.Validate(map[string]any{"cmd": "x"}); err == nil {
		t.Fatal("unknown key must be rejected")
	}
	// Type mismatch rejected.
	if err := s.Validate(map[string]any{"service": 3}); err == nil {
		t.Fatal("type mismatch must be rejected")
	}
	// An illegal schema declaration fails closed.
	if err := (ExtraVarsSchema{"x": VarType("array")}).Validate(map[string]any{"x": "y"}); err == nil {
		t.Fatal("an illegal schema type must fail closed")
	}
}

func TestEncodeLaunch_RoundTrip(t *testing.T) {
	spec := validSpec()
	argv, stdin, err := EncodeLaunch(spec)
	if err != nil {
		t.Fatalf("EncodeLaunch: %v", err)
	}
	if len(argv) != 1 || argv[0] != LaunchVerb {
		t.Fatalf("argv must be [LaunchVerb], got %v", argv)
	}
	var back LaunchSpec
	if err := json.Unmarshal(stdin, &back); err != nil {
		t.Fatalf("stdin must decode to a LaunchSpec: %v", err)
	}
	if back.TemplateID != 7 || back.OpClass != "restart-service" || back.Limit != "web01" {
		t.Fatalf("round-trip lost fields: %+v", back)
	}
	// A non-positive template id fails closed.
	if _, _, err := EncodeLaunch(LaunchSpec{TemplateID: 0}); err == nil {
		t.Fatal("a non-positive template id must fail closed")
	}
}

// TestExec_RejectsNonLaunchArgv proves the structural argv guard: anything but the fixed verb refuses.
func TestExec_RejectsNonLaunchArgv(t *testing.T) {
	awx := &fakeAWX{}
	a := newActuator(t, awx, safety.NewActuatingChokepoint())
	_, stdin, _ := EncodeLaunch(validSpec())
	for _, argv := range [][]string{{"systemctl", "restart", "nginx"}, {}, {"awx-job-template-launch", "extra"}} {
		if _, err := a.Exec(context.Background(), argv, stdin); err == nil {
			t.Fatalf("a non-launch argv %v must be refused", argv)
		}
	}
	if awx.launchCount() != 0 {
		t.Fatalf("no launch may fire for a non-launch argv, got %d", awx.launchCount())
	}
}
