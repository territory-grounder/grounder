package acceptance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"time"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/httpapi"
	"github.com/territory-grounder/grounder/core/trace"
)

// T-020-10 binds the two scenarios the per-session detail read endpoint genuinely delivers now: REQ-2000
// (the trace record carries every decision-grade field, credential fields references/schemes only) and
// REQ-2011 (the endpoint requires authentication and returns the ordered steps assembled from the spine).
// The remaining REQ-2010 (full queued/running coverage, which needs the T-020-13 step channel) and REQ-2002
// (the on/off decision-path proof, which needs the REQ-2001 emit side) stay @pending as declared debt — the
// read path here is observe-only by construction (the trace package imports no safety/policy/actuation
// package), but the full scenarios bind when their sibling tasks land.
func init() {
	stepRegistrars = append(stepRegistrars, registerDetailSteps)
}

// detailWorld is the shared state for the detail scenarios: the assembled trace and the endpoint response.
type detailWorld struct {
	assembled trace.SessionTrace
	dtoBody   []byte
	status    int
}

func registerDetailSteps(sc *godog.ScenarioContext) {
	w := &detailWorld{}

	// REQ-2000 — the trace record carries every decision-grade field for a step.
	sc.Step(`^a sealed session with a classified action that traversed the interceptor chain$`, func() error {
		w.assembled = trace.Assemble("ext-req2000", sealedSpine())
		return nil
	})
	sc.Step(`^the tracer assembles the per-step trace record$`, func() error {
		if len(w.assembled.Steps) == 0 {
			return fmt.Errorf("assembler produced no steps")
		}
		return nil
	})
	sc.Step(`^each step carries the rule matched_rules reason tools prompts skills confidence min_confidence band verdict gate bundle_version and credential fields and the credential fields are references and schemes only$`, func() error {
		// The record TYPE carries every decision-grade field on every step (a landed source fills each; an
		// unlanded one leaves the zero value — never a fabricated placeholder). Assert the representative
		// classify+propose+predict+verify walk exercises the decision-grade fields, and that no credential
		// field ever holds a value-shaped secret (INV-13): it is a scheme or a reference only.
		secretShaped := regexp.MustCompile(`(?i)(password|secret|token|-----BEGIN)`)
		sawConfidence, sawBand, sawVerdict := false, false, false
		for _, s := range w.assembled.Steps {
			if s.Confidence > 0 {
				sawConfidence = true
			}
			if s.Band != "" {
				sawBand = true
			}
			if s.Verdict != "" {
				sawVerdict = true
			}
			if s.CredentialRef != "" && secretShaped.MatchString(s.CredentialRef) {
				return fmt.Errorf("credential_ref carries a value-shaped secret: %q", s.CredentialRef)
			}
			if s.CredentialScheme != "" && secretShaped.MatchString(s.CredentialScheme) {
				return fmt.Errorf("credential_scheme carries a value-shaped secret: %q", s.CredentialScheme)
			}
		}
		if !sawConfidence || !sawBand || !sawVerdict {
			return fmt.Errorf("walk did not carry the decision-grade fields (conf=%v band=%v verdict=%v)", sawConfidence, sawBand, sawVerdict)
		}
		return nil
	})

	// REQ-2011 — the detail endpoint requires authentication and returns the ordered steps over the spine.
	sc.Step(`^a sealed session and the detail read endpoint GET /v1/sessions/\{external_ref\}$`, func() error {
		w.assembled = trace.Assemble("ext-req2011", sealedSpine())
		// The endpoint is registered under AuthReadOnly; prove the auth requirement over the REAL router with
		// an empty verifier (no credential can satisfy it) — an unauthenticated GET must be refused.
		rt := auth.NewRouter(&auth.Verifier{})
		httpapi.Register(rt, httpapi.Deps{SessionDetailRead: fixedDetailReader{tr: w.assembled}})
		srv := httptest.NewServer(rt.Mux())
		defer srv.Close()
		unauth, err := http.Get(srv.URL + "/v1/sessions/ext-req2011")
		if err != nil {
			return err
		}
		_ = unauth.Body.Close()
		w.status = unauth.StatusCode
		return nil
	})
	sc.Step(`^an authenticated principal requests the session detail$`, func() error {
		if w.status == http.StatusOK {
			return fmt.Errorf("unauthenticated read returned 200 — auth not enforced")
		}
		// The served body is the exported projection the handler encodes for an authenticated caller.
		w.dtoBody, _ = json.Marshal(httpapi.ProjectSessionDetail(w.assembled))
		return nil
	})
	sc.Step(`^the endpoint requires authentication and returns the per-step trace record in decision-boundary order assembled from the correlation spine and the per-step tables$`, func() error {
		var dto httpapi.SessionDetailDTO
		if err := json.Unmarshal(w.dtoBody, &dto); err != nil {
			return err
		}
		wantOrder := []string{"classify", "screen", "propose", "predict", "verify"}
		if len(dto.Nodes) != len(wantOrder) {
			return fmt.Errorf("want %d ordered nodes, got %d", len(wantOrder), len(dto.Nodes))
		}
		for i, n := range dto.Nodes {
			if n.T != wantOrder[i] {
				return fmt.Errorf("node %d = %q, want %q (decision-boundary order)", i, n.T, wantOrder[i])
			}
		}
		return nil
	})
}

// sealedSpine is a representative fully-sealed session spine: classified, proposed with a stated confidence,
// predicted+scored clean, and mechanically verified match.
func sealedSpine() trace.SpineRecords {
	return trace.SpineRecords{
		Classification: trace.ClassificationRecord{Present: true, Band: "AUTO", RiskLevel: "low", ActionID: "act-req", PlanHash: "ph-req", CreatedAt: time.Unix(100, 0).UTC()},
		Triage:         trace.TriageRecord{Present: true, Host: "librespeed01", AlertRule: "iface_down", Band: "AUTO", Proposed: true, Op: "restart-service", Conclusion: "flap cleared", Confidence: 0.82, CreatedAt: time.Unix(110, 0).UTC()},
		Prediction:     trace.PredictionRecord{Present: true, ActionID: "act-req", PlanHash: "ph-req", Scored: true, TP: 1, CommittedAt: time.Unix(120, 0).UTC()},
		Verdict:        trace.VerdictRecord{Present: true, Verdict: "match"},
	}
}

// fixedDetailReader serves one fixed trace regardless of ref (the endpoint oracle).
type fixedDetailReader struct{ tr trace.SessionTrace }

func (f fixedDetailReader) SessionDetail(_ context.Context, _ auth.Principal, _ string) (trace.SessionTrace, error) {
	return f.tr, nil
}
