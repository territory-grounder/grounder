package acceptance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/cucumber/godog"

	actuation "github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/regime"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
	"github.com/territory-grounder/grounder/modules/actuation/awxjob"
	"github.com/territory-grounder/grounder/modules/knowledge/awxplaybooks"
)

// T-017-3 — the AWX-job actuator (REQ-1704..1708): the FIRST non-SSH mutating lane, driven through the SAME
// regime.LaneEffect → actuate.Interceptor.Do seam production uses. Registered from this file's init() append
// so the shared acceptance harness is never edited by parallel task work.
func init() {
	stepRegistrars = append(stepRegistrars, registerAWXJobSteps)
}

const (
	awxJobLaunchTokenEnv = "TG_AWXJOB_ACC_LAUNCH_TOKEN"
	awxJobLaunchTokenRef = "env:" + awxJobLaunchTokenEnv
	// the read-only sensor token the knowledge lane (T-017-5) uses — declared DISTINCTLY from the launch token.
	awxSensorTokenRef = "env:TG_AWXPB_ACCEPT_TOKEN"
)

// accAWXJobFake is a canned AWX that records launches and whether a Bearer token was presented, so the oracle
// can prove a refusal launched nothing and a launch carried a resolved token.
type accAWXJobFake struct {
	mu       sync.Mutex
	launches int
	lastBody string
	sawAuth  bool
}

func (f *accAWXJobFake) Do(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.HasPrefix(req.Header.Get("Authorization"), "Bearer ") {
		f.sawAuth = true
	}
	if strings.HasSuffix(req.URL.Path, "/launch/") && req.Method == http.MethodPost {
		f.launches++
		if req.Body != nil {
			b, _ := io.ReadAll(req.Body)
			f.lastBody = string(b)
		}
		body := `{"id":501,"job":501,"status":"pending","job_template":7,"url":"/api/v2/jobs/501/"}`
		return &http.Response{StatusCode: 201, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header)}, nil
}

func (f *accAWXJobFake) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.launches }

// accAWXJobDecider is a scripted policy authorizer for the interceptor's policy layer (spec/015).
type accAWXJobDecider struct {
	verdict policy.Verdict
	calls   int
}

func (d *accAWXJobDecider) Decide(_ context.Context, in policy.EvalInput) (policy.PolicyDecision, error) {
	d.calls++
	return policy.NewPolicyDecision(d.verdict, "rule-awxjob", in.Band, nil, in.Mode, "acc", policy.DecisionAudit{}), nil
}

func accAWXJobAllowlist() awxjob.TemplateAllowlist {
	return awxjob.TemplateAllowlist{
		7: {OpClass: "restart-service", ExtraVarsSchema: awxjob.ExtraVarsSchema{"service": awxjob.VarString}},
	}
}

// awxJobDriveResult is one governed-launch attempt's observable outcome.
type awxJobDriveResult struct {
	out       actuate.Outcome
	applyErr  error
	launches  int
	sawAuth   bool
	lastBody  string
	readOnly  bool
	deciderHi int
}

// driveAWXJob builds the REAL actuator + awx-job lane + interceptor seam over the given chokepoint (and
// optional policy decider) and drives ONE launch through actuate.Interceptor.Do — exactly the production path.
func driveAWXJob(cp *safety.Chokepoint, dec *accAWXJobDecider, spec awxjob.LaunchSpec, reversible bool) (awxJobDriveResult, error) {
	_ = os.Setenv(awxJobLaunchTokenEnv, "acc-launch-token-distinct")
	fake := &accAWXJobFake{}
	client, err := awxjob.NewClient(awxjob.ClientConfig{BaseURL: "https://awx.test", TokenRef: config.SecretRef(awxJobLaunchTokenRef), HTTPClient: fake})
	if err != nil {
		return awxJobDriveResult{}, err
	}
	act, err := awxjob.New(awxjob.Config{Client: client, Allowlist: accAWXJobAllowlist(), ModeGate: cp})
	if err != nil {
		return awxJobDriveResult{}, err
	}
	lane := regime.NewAWXJobLane(regime.WithAWXActuator(act))
	seam := regime.NewLaneEffect(func(l actuation.Actuator) *actuate.Interceptor {
		i := actuate.NewInterceptor(cp, l, audit.NewLedger())
		if dec != nil {
			i = i.WithPolicyDecider(dec, func() policy.Mode { return policy.ModeFullAuto })
		}
		return i
	})
	argv, stdin, err := awxjob.EncodeLaunch(spec)
	if err != nil {
		return awxJobDriveResult{}, err
	}
	m, err := manifest.New(
		manifest.Action{Target: "web01", OpClass: spec.OpClass, Op: "restart", Reversible: reversible},
		safety.BandAuto, "plan#acc-awxjob", "pred#acc-awxjob",
	)
	if err != nil {
		return awxJobDriveResult{}, err
	}
	req := actuate.Request{
		Manifest: m,
		Gated:    true,
		Argv:     argv,
		Stdin:    stdin,
		Evidence: []actuate.Evidence{{ToolResultID: "tr", Captured: true, Successful: true, Recent: true, Relevant: true}},
		Observe:  func(context.Context) []verify.ObservedAlert { return []verify.ObservedAlert{} },
		Band:     safety.BandAuto, // fresh per-incident band (TG-126): AUTO admits at 1b; the lane/policy/mode gate the launch
	}
	out, aerr := seam.Apply(context.Background(), lane, req)
	r := awxJobDriveResult{out: out, applyErr: aerr, launches: fake.count(), sawAuth: fake.sawAuth, lastBody: fake.lastBody, readOnly: act.ReadOnly()}
	if dec != nil {
		r.deciderHi = dec.calls
	}
	return r, nil
}

func validAWXJobSpec() awxjob.LaunchSpec {
	return awxjob.LaunchSpec{TemplateID: 7, OpClass: "restart-service", ExtraVars: map[string]any{"service": "nginx"}, Limit: "web01"}
}

func registerAWXJobSteps(sc *godog.ScenarioContext) {
	w := &awxJobStepsWorld{}

	// ---- REQ-1704: launches only an allowlisted, policy-authorized template ----
	sc.Step(`^an operator template-allowlist binding job templates to op-classes and a policy engine verdict$`, func() error {
		return nil // the allowlist + decider are constructed per sub-case in the When step
	})
	sc.Step(`^the AWX-job actuator is asked to launch a template$`, func() error {
		var err error
		// allowlisted template + policy AUTO → launches.
		w.r1704Allowed, err = driveAWXJob(safety.NewActuatingChokepoint(), &accAWXJobDecider{verdict: policy.VerdictAuto}, validAWXJobSpec(), true)
		if err != nil {
			return err
		}
		// NON-allowlisted template + policy AUTO → the actuator refuses (allowlist), 0 launches.
		w.r1704NotListed, err = driveAWXJob(safety.NewActuatingChokepoint(), &accAWXJobDecider{verdict: policy.VerdictAuto},
			awxjob.LaunchSpec{TemplateID: 999, OpClass: "restart-service", ExtraVars: map[string]any{"service": "nginx"}}, true)
		if err != nil {
			return err
		}
		// allowlisted template + policy DENY → the interceptor refuses (policy), 0 launches.
		w.r1704Denied, err = driveAWXJob(safety.NewActuatingChokepoint(), &accAWXJobDecider{verdict: policy.VerdictDeny}, validAWXJobSpec(), true)
		return err
	})
	sc.Step(`^it launches only an allowlisted template whose op-class the policy engine did not deny and refuses a non-allowlisted or policy-denied template$`, func() error {
		if !w.r1704Allowed.out.Executed || w.r1704Allowed.launches != 1 {
			return errf("an allowlisted, policy-auto template must launch exactly once: %+v launches=%d", w.r1704Allowed.out, w.r1704Allowed.launches)
		}
		if w.r1704NotListed.out.Executed || w.r1704NotListed.launches != 0 {
			return errf("a non-allowlisted template must refuse and NOT launch: %+v launches=%d", w.r1704NotListed.out, w.r1704NotListed.launches)
		}
		if w.r1704Denied.out.Executed || w.r1704Denied.launches != 0 {
			return errf("a policy-denied template must refuse and NOT launch: %+v launches=%d", w.r1704Denied.out, w.r1704Denied.launches)
		}
		return nil
	})

	// ---- REQ-1705: the effect is a template id + typed extra_vars; unknown keys rejected; no command ----
	sc.Step(`^a job template with an operator-declared extra_vars schema$`, func() error { return nil })
	sc.Step(`^the actuator builds the launch body$`, func() error {
		var err error
		w.r1705Valid, err = driveAWXJob(safety.NewActuatingChokepoint(), nil, validAWXJobSpec(), true)
		if err != nil {
			return err
		}
		// a smuggled, undeclared extra_var is rejected before any launch.
		w.r1705Unknown, err = driveAWXJob(safety.NewActuatingChokepoint(), nil,
			awxjob.LaunchSpec{TemplateID: 7, OpClass: "restart-service", ExtraVars: map[string]any{"service": "nginx", "cmd": "rm -rf /"}}, true)
		// argv encoding is the fixed verb only (no command string).
		w.r1705Argv, _, _ = awxjob.EncodeLaunch(validAWXJobSpec())
		return err
	})
	sc.Step(`^the effect is the template id plus typed extra_vars validated against the schema an unknown extra_vars key is rejected and no free-form command string is passed$`, func() error {
		if !w.r1705Valid.out.Executed || w.r1705Valid.launches != 1 {
			return errf("a typed, schema-valid launch must execute: %+v", w.r1705Valid.out)
		}
		// the launch body carries ONLY typed extra_vars + limit — no command / argv / cmd field.
		var body map[string]any
		if err := json.Unmarshal([]byte(w.r1705Valid.lastBody), &body); err != nil {
			return errf("launch body must be JSON: %v (%s)", err, w.r1705Valid.lastBody)
		}
		for k := range body {
			if k != "extra_vars" && k != "limit" {
				return errf("launch body must carry only typed extra_vars + limit, saw %q", k)
			}
		}
		if ev, _ := body["extra_vars"].(map[string]any); ev["service"] != "nginx" {
			return errf("the typed extra_var must be sent, got %v", body["extra_vars"])
		}
		// an unknown key is rejected (no launch).
		if w.r1705Unknown.out.Executed || w.r1705Unknown.launches != 0 {
			return errf("an unknown extra_vars key must be rejected: %+v", w.r1705Unknown.out)
		}
		// no free-form command string: the argv is the single fixed verb.
		if len(w.r1705Argv) != 1 || w.r1705Argv[0] != awxjob.LaunchVerb {
			return errf("argv must be the fixed launch verb only (no command string), got %v", w.r1705Argv)
		}
		return nil
	})

	// ---- REQ-1706: token resolved after policy authorization, before launch; launch only while mode permits ----
	sc.Step(`^a classified AWX-job action the policy engine authorized with a non-deny verdict$`, func() error { return nil })
	sc.Step(`^the actuation chokepoint reaches the target$`, func() error {
		var err error
		// mode permits (actuating) + policy AUTO → the launch fires and carries a resolved Bearer token.
		w.r1706On, err = driveAWXJob(safety.NewActuatingChokepoint(), &accAWXJobDecider{verdict: policy.VerdictAuto}, validAWXJobSpec(), true)
		if err != nil {
			return err
		}
		// mode Shadow → the interceptor refuses at the mode chokepoint; NO launch, the token is never resolved.
		w.r1706Off, err = driveAWXJob(safety.NewReadOnlyChokepoint(), &accAWXJobDecider{verdict: policy.VerdictAuto}, validAWXJobSpec(), true)
		return err
	})
	sc.Step(`^the AWX API token is resolved through the credential engine after authorization and before launch and the job launches only while the mode chokepoint permits actuation$`, func() error {
		// AFTER authorization: the policy decider was consulted before the launch.
		if w.r1706On.deciderHi != 1 {
			return errf("the policy engine must be consulted before launch, calls=%d", w.r1706On.deciderHi)
		}
		// token resolved + presented at launch time (BEFORE the effect completes; the launch carried the Bearer).
		if !w.r1706On.out.Executed || w.r1706On.launches != 1 || !w.r1706On.sawAuth {
			return errf("a mode-permitted, authorized launch must resolve the token and launch: %+v launches=%d auth=%v", w.r1706On.out, w.r1706On.launches, w.r1706On.sawAuth)
		}
		// launches ONLY while the mode permits: under Shadow, no launch and the token was never resolved/sent.
		if w.r1706Off.out.Executed || w.r1706Off.launches != 0 || w.r1706Off.sawAuth {
			return errf("under Shadow the job must NOT launch and the token must not be resolved: %+v launches=%d auth=%v", w.r1706Off.out, w.r1706Off.launches, w.r1706Off.sawAuth)
		}
		return nil
	})

	// ---- REQ-1707: read-only setup = sensor; mutating template stays OFF (mode + never-auto floor) ----
	sc.Step(`^a read-only AWX setup fact-gathering job and a mutating job template$`, func() error { return nil })
	sc.Step(`^the engine runs each$`, func() error {
		var err error
		// mutating template under Shadow → refused, 0 launches; the actuator is a read-only (sensor) posture.
		w.r1707Shadow, err = driveAWXJob(safety.NewReadOnlyChokepoint(), nil, validAWXJobSpec(), true)
		if err != nil {
			return err
		}
		// the never-auto floor: an IRREVERSIBLE mutating op is refused at the floor even under an actuating mode.
		w.r1707Floor, err = driveAWXJob(safety.NewActuatingChokepoint(), nil,
			awxjob.LaunchSpec{TemplateID: 7, OpClass: "restart-service", ExtraVars: map[string]any{"service": "nginx"}}, false)
		if err != nil {
			return err
		}
		// under a (simulated) owner-present flip, the reversible mutating template routes through and launches.
		w.r1707Flip, err = driveAWXJob(safety.NewActuatingChokepoint(), nil, validAWXJobSpec(), true)
		return err
	})
	sc.Step(`^the setup job is a Phase-1-safe sensor and the mutating template routes through the mode chokepoint and the never-auto floor and stays off while the mode is Shadow until the owner-present flip$`, func() error {
		// Phase-1-safe sensor: at Shadow the actuator is READ-ONLY (no mutation) — the sensor-safe posture.
		if !w.r1707Shadow.readOnly {
			return errf("at Shadow the awx-job actuator must be read-only (Phase-1-safe sensor posture)")
		}
		// mutating template STAYS OFF at Shadow: refused, 0 launches.
		if w.r1707Shadow.out.Executed || w.r1707Shadow.launches != 0 {
			return errf("a mutating template must stay OFF at Shadow: %+v launches=%d", w.r1707Shadow.out, w.r1707Shadow.launches)
		}
		// routes through the never-auto floor: an irreversible op is refused even under an actuating mode.
		if w.r1707Floor.out.Executed || w.r1707Floor.launches != 0 {
			return errf("an irreversible op must be refused at the never-auto floor: %+v launches=%d", w.r1707Floor.out, w.r1707Floor.launches)
		}
		// routes through the mode chokepoint: at the flip a reversible mutating template launches.
		if !w.r1707Flip.out.Executed || w.r1707Flip.launches != 1 {
			return errf("a reversible mutating template must launch once the mode permits: %+v launches=%d", w.r1707Flip.out, w.r1707Flip.launches)
		}
		return nil
	})

	// ---- REQ-1708: launch token is a sealed SecretRef, distinct from the read-only sensor token ----
	sc.Step(`^the AWX-job lane and the read-only sensor and knowledge lane$`, func() error {
		_ = os.Setenv(awxJobLaunchTokenEnv, "acc-launch-token")
		_ = os.Setenv("TG_AWXPB_ACCEPT_TOKEN", "acc-sensor-token")
		return nil
	})
	sc.Step(`^each authenticates to AWX$`, func() error {
		// an empty launch token fails closed (a token is required — no anonymous launch).
		_, w.r1708EmptyErr = awxjob.NewClient(awxjob.ClientConfig{BaseURL: "https://awx.test", TokenRef: ""})
		// the launch client uses the LAUNCH token ref; the sensor lane uses a DISTINCT read-only token ref.
		_, w.r1708LaunchErr = awxjob.NewClient(awxjob.ClientConfig{BaseURL: "https://awx.test", TokenRef: config.SecretRef(awxJobLaunchTokenRef), HTTPClient: &accAWXJobFake{}})
		_, w.r1708SensorErr = awxplaybooks.NewClient(awxplaybooks.ClientConfig{BaseURL: "https://awx.test", TokenRef: config.SecretRef(awxSensorTokenRef), HTTPClient: &acceptFakeAWX{}})
		return nil
	})
	sc.Step(`^every token is a sealed SecretRef with no plaintext in config the ledger or any exportable artifact and the sensor token is a read-only token declared distinctly from any launch-capable token$`, func() error {
		if w.r1708EmptyErr == nil {
			return errf("an empty launch token reference must fail closed")
		}
		if w.r1708LaunchErr != nil || w.r1708SensorErr != nil {
			return errf("both the launch and sensor clients must construct with their sealed token refs: launch=%v sensor=%v", w.r1708LaunchErr, w.r1708SensorErr)
		}
		// distinct: the launch token ref and the read-only sensor token ref are different declarations.
		if awxJobLaunchTokenRef == awxSensorTokenRef {
			return errf("the launch token must be declared distinctly from the read-only sensor token")
		}
		// sealed SecretRef, not plaintext (INV-13): each token is a scheme-prefixed INDIRECTION, and the ref
		// string never contains the secret value it points to — it is safe to log, commit, and export.
		for _, ref := range []string{awxJobLaunchTokenRef, awxSensorTokenRef} {
			if !strings.HasPrefix(ref, "env:") && !strings.HasPrefix(ref, "file:") && !strings.HasPrefix(ref, "store:") {
				return errf("token %q must be a sealed SecretRef (env:/file:/store:), never a literal", ref)
			}
			val, rerr := config.SecretRef(ref).Resolve()
			if rerr != nil {
				return errf("the sealed ref %q must resolve at runtime: %v", ref, rerr)
			}
			if val == "" || strings.Contains(ref, val) {
				return errf("the SecretRef %q must be an indirection, never the plaintext secret", ref)
			}
		}
		return nil
	})
}

// awxJobStepsWorld holds the per-scenario observable outcomes for the T-017-3 scenarios.
type awxJobStepsWorld struct {
	r1704Allowed, r1704NotListed, r1704Denied awxJobDriveResult
	r1705Valid, r1705Unknown                  awxJobDriveResult
	r1705Argv                                 []string
	r1706On, r1706Off                         awxJobDriveResult
	r1707Shadow, r1707Floor, r1707Flip        awxJobDriveResult
	r1708EmptyErr, r1708LaunchErr             error
	r1708SensorErr                            error
}

// errf is a tiny local error helper so the step bodies stay readable.
func errf(format string, args ...any) error { return fmt.Errorf(format, args...) }
