package acceptance

import (
	"fmt"
	"regexp"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/core/httpapi"
	"github.com/territory-grounder/grounder/core/trace"
)

// T-020-2 binds REQ-2015: the detail endpoint projects credential + secret-bearing fields as REFERENCES and
// SCHEMES only, never a secret value (INV-13). The tracer walk now carries a credential-resolve boundary
// (credential_resolution, spec/016) whose non-secret identity is the resolved user + connection scheme + the
// key-reference SCHEME (env/file/…), NEVER key material or a full ref value. This oracle assembles a session
// whose walk includes that boundary, then asserts (a) the assembled step's credential fields are schemes only
// and (b) the REAL httpapi.ProjectSessionDetail projection carries NO field — on ANY node — matching a
// value-shaped-secret denylist.
func init() {
	stepRegistrars = append(stepRegistrars, registerCredentialProjectionSteps)
}

// credSecretShaped is the value-shaped-secret denylist (mirrors the REQ-2000 detail oracle): a projected field
// must never look like a password/secret/token or a PEM key block.
var credSecretShaped = regexp.MustCompile(`(?i)(password|secret|token|-----BEGIN)`)

// credSchemeShaped matches a bare scheme/reference token (letters, digits, dot, dash, colon-scheme) — what a
// credential field is allowed to be. A full ref value or key material would not match this narrow shape.
var credSchemeShaped = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]*$`)

type credProjWorld struct {
	tr  trace.SessionTrace
	dto httpapi.SessionDetailDTO
}

func registerCredentialProjectionSteps(sc *godog.ScenarioContext) {
	w := &credProjWorld{}

	sc.Step(`^a session whose steps reference credentials and secret-bearing fields$`, func() error {
		// A credential-resolve boundary with a scheme-only identity. If the write path leaked, KeyRefScheme would
		// be a full "env:SUPER_SECRET" ref; the tracer must carry only the scheme token "env".
		w.tr = trace.Assemble("ext-req2015", trace.SpineRecords{
			Classification: trace.ClassificationRecord{Present: true, Band: "AUTO", RiskLevel: "low", ActionID: "act-1"},
			Credentials: []trace.CredentialRecord{
				{Target: "librespeed01", Outcome: "resolved", User: "ops", Scheme: "ssh", KeyRefScheme: "env", Source: "native-hostdiag"},
			},
			Triage:  trace.TriageRecord{Present: true, Host: "librespeed01", Proposed: true, Op: "restart-service", Conclusion: "restart nginx"},
			Verdict: trace.VerdictRecord{Present: true, Verdict: "match"},
		})
		w.dto = httpapi.ProjectSessionDetail(w.tr)
		return nil
	})

	sc.Step(`^the detail endpoint projects the session$`, func() error {
		if len(w.dto.Nodes) == 0 {
			return fmt.Errorf("the projection has no nodes to inspect")
		}
		// A credential-resolve step must actually be in the walk, else the oracle proves nothing.
		hasCred := false
		for _, s := range w.tr.Steps {
			if s.Kind == trace.StepCredential {
				hasCred = true
			}
		}
		if !hasCred {
			return fmt.Errorf("no credential-resolve step in the assembled walk — nothing to assert on")
		}
		return nil
	})

	sc.Step(`^every credential and secret-bearing field is a SecretRef reference or a scheme and no field matches a value-shaped secret denylist$`, func() error {
		// (a) the assembled credential step carries SCHEME-shaped credential fields only (INV-13).
		for _, s := range w.tr.Steps {
			if s.Kind != trace.StepCredential {
				continue
			}
			for name, v := range map[string]string{"credential_scheme": s.CredentialScheme, "credential_ref": s.CredentialRef} {
				if v == "" {
					continue
				}
				if credSecretShaped.MatchString(v) {
					return fmt.Errorf("%s = %q matches the value-shaped-secret denylist", name, v)
				}
				if !credSchemeShaped.MatchString(v) {
					return fmt.Errorf("%s = %q is not a bare scheme/reference token (INV-13 — schemes only)", name, v)
				}
			}
		}
		// (b) the REAL endpoint projection carries no value-shaped secret in ANY field of ANY node.
		for _, n := range w.dto.Nodes {
			fields := append([]string{n.T, n.Lb, n.St, n.Src, n.Pay, n.Gate, n.Band, n.Hash, n.Verdict, n.Ts}, n.Plan...)
			for _, f := range fields {
				if f != "" && credSecretShaped.MatchString(f) {
					return fmt.Errorf("projected field %q on a %q node matches the value-shaped-secret denylist", f, n.T)
				}
			}
		}
		return nil
	})
}
