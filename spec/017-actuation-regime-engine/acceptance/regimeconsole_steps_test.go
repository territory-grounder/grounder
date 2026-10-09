package acceptance

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/httpapi"
)

// T-017-7 (REQ-1716), read-only slice — the console regime UX. The console itself is a React app (its own
// vitest oracles live in frontend/src/panels/regime/regime.test.tsx); the Go acceptance harness binds the
// scenario at the CONTRACT level: it drives the REAL httpapi regime read DTOs over the in-memory reader twin
// (the same twin the httpapi handler oracles use; CI has no Postgres) and asserts the exact non-secret wire
// shape the panel binds to — the per-target regime→lane resolutions (incl. a fail-closed refusal), the
// actuation tail with each template's authorized op-class, the deferred verdicts that resolve the
// pending-verification queue, and the per-lane coverage roll-up. Registered via a step-registrar slice so the
// shared harness (acceptance_test.go) is never edited by parallel task work (the same pattern the other
// slices use). The ledger-audited allowlist EDITOR (the write path) stays @pending as declared debt.
func init() {
	stepRegistrars = append(stepRegistrars, registerRegimeConsoleSteps)
}

// consoleSecretValue is a plaintext AWX token the oracle proves NEVER appears in the read view — only its
// SecretRef reference (token_ref) does (INV-13). The console renders token_ref, never a token value.
const consoleSecretValue = "s3cr3t-awx-oauth2-token-value"

// regimeConsoleWorld drives the real httpapi regime read DTOs and captures the JSON wire body the panel reads.
type regimeConsoleWorld struct {
	reader *httpapi.MemRegimeReader
	body   []byte
	err    error
}

func registerRegimeConsoleSteps(sc *godog.ScenarioContext) {
	w := &regimeConsoleWorld{}

	// ---- Given: a populated, NON-SECRET regime spine behind the read API ----
	sc.Step(`^the console regime surface reads the regime read API$`, func() error {
		// Two resolved targets + one fail-closed refusal; a verified launch and a launched-but-unverified
		// launch; a per-lane coverage roll-up. The AWX token appears ONLY as its SecretRef reference.
		w.reader = &httpapi.MemRegimeReader{
			Resolutions: []httpapi.RegimeResolution{
				{Target: "librespeed01", Regime: "native-ssh", Lane: "native-ssh", Outcome: "resolved", CreatedAt: "2026-07-20T10:10:00Z"},
				{Target: "awx-edge", Regime: "awx-job", Lane: "awx-job", RuleID: "awx-edge", Outcome: "resolved", CreatedAt: "2026-07-20T10:09:00Z"},
				{Target: "mystery01", Outcome: "refused", CreatedAt: "2026-07-20T10:08:00Z"},
			},
			Actuations: []httpapi.RegimeActuation{
				{ActionID: "act-1", Lane: "awx-job", JobTemplateID: "42", OpClass: "restart-service", JobID: "9001", TokenRef: "store:awx.launch", CreatedAt: "2026-07-20T10:07:00Z"},
				{ActionID: "act-2", Lane: "awx-job", JobTemplateID: "43", OpClass: "gather-facts", JobID: "9002", TokenRef: "store:awx.sensor", CreatedAt: "2026-07-20T10:05:00Z"},
			},
			DeferredVerdicts: []httpapi.RegimeDeferredVerdict{
				{ActionID: "act-1", JobID: "9001", Status: "successful", Verdict: "match", Graduation: "verified_clean", CreatedAt: "2026-07-20T10:06:00Z"},
			},
			Coverage: []httpapi.RegimeLaneCoverage{
				{Lane: "awx-job", Resolutions: 1, Actuations: 2},
				{Lane: "native-ssh", Resolutions: 1, Actuations: 0},
			},
		}
		return nil
	})

	// ---- When: the operator views the regime state (the aggregated GET /v1/regime read) ----
	sc.Step(`^the operator views the regime state$`, func() error {
		// Assemble the aggregated page EXACTLY as httpapi.regimeHandler does (the four reader projections), then
		// marshal it to the identical wire form the handler encodes — this IS the /v1/regime body the panel reads.
		ctx := context.Background()
		p := auth.Principal{}
		res, err := w.reader.RegimeResolutions(ctx, p, 50)
		if err != nil {
			w.err = err
			return nil
		}
		act, err := w.reader.RegimeActuations(ctx, p, 50)
		if err != nil {
			w.err = err
			return nil
		}
		ver, err := w.reader.RegimeDeferredVerdicts(ctx, p, 50)
		if err != nil {
			w.err = err
			return nil
		}
		cov, err := w.reader.RegimeLaneCoverage(ctx, p)
		if err != nil {
			w.err = err
			return nil
		}
		page := httpapi.RegimePage{Resolutions: res, Actuations: act, DeferredVerdicts: ver, LaneCoverage: cov}
		b, err := json.Marshal(page)
		if err != nil {
			w.err = err
			return nil
		}
		w.body = b
		return nil
	})

	// ---- Then: the wire shape the panel binds to is present, non-secret, and fails closed on a refusal ----
	sc.Step(`^the read view carries the per-target regime and lane resolutions the actuation tail with each template's authorized op-class the deferred verdicts and the per-lane coverage roll-up as real non-secret engine state$`, func() error {
		if w.err != nil {
			return w.err
		}
		// Decode into a MIRROR of the frontend TS contract (api/generated-client.ts) — every json tag the panel
		// binds to must be present and correctly typed. If the server DTO renamed/dropped a field the panel
		// depends on, this decode-and-assert fails.
		var page struct {
			Resolutions []struct {
				Target    string `json:"target"`
				Regime    string `json:"regime"`
				Lane      string `json:"lane"`
				RuleID    string `json:"rule_id"`
				Outcome   string `json:"outcome"`
				CreatedAt string `json:"created_at"`
			} `json:"resolutions"`
			Actuations []struct {
				ActionID      string `json:"action_id"`
				Lane          string `json:"lane"`
				JobTemplateID string `json:"job_template_id"`
				OpClass       string `json:"op_class"`
				JobID         string `json:"job_id"`
				TokenRef      string `json:"token_ref"`
				CreatedAt     string `json:"created_at"`
			} `json:"actuations"`
			DeferredVerdicts []struct {
				ActionID   string `json:"action_id"`
				JobID      string `json:"job_id"`
				Status     string `json:"status"`
				Verdict    string `json:"verdict"`
				Graduation string `json:"graduation"`
			} `json:"deferred_verdicts"`
			LaneCoverage []struct {
				Lane        string `json:"lane"`
				Resolutions int    `json:"resolutions"`
				Actuations  int    `json:"actuations"`
			} `json:"lane_coverage"`
		}
		if err := json.Unmarshal(w.body, &page); err != nil {
			return fmt.Errorf("the /v1/regime body must decode into the panel's contract shape: %w", err)
		}

		// The per-target regime→lane map: three resolutions, and the refusal is FAIL-CLOSED (empty lane).
		if len(page.Resolutions) != 3 {
			return fmt.Errorf("expected 3 resolutions for the regime→lane map, got %d", len(page.Resolutions))
		}
		var refused, resolved bool
		for _, r := range page.Resolutions {
			if r.Outcome == "refused" {
				refused = true
				if r.Lane != "" || r.Regime != "" {
					return fmt.Errorf("a refusal must carry no regime/lane (fail closed): %+v", r)
				}
			}
			if r.Target == "awx-edge" && r.Regime == "awx-job" && r.Lane == "awx-job" {
				resolved = true
			}
		}
		if !refused || !resolved {
			return fmt.Errorf("the map must carry a resolved awx-job row and a fail-closed refusal: %+v", page.Resolutions)
		}

		// The actuation tail: each launch names its template AND the template's authorized op-class, and the
		// token is a SecretRef REFERENCE, never a plaintext value.
		if len(page.Actuations) != 2 {
			return fmt.Errorf("expected 2 actuations on the tail, got %d", len(page.Actuations))
		}
		for _, a := range page.Actuations {
			if a.JobTemplateID == "" || a.OpClass == "" {
				return fmt.Errorf("each actuation must carry a template id and its authorized op-class: %+v", a)
			}
			if !strings.HasPrefix(a.TokenRef, "store:") && !strings.HasPrefix(a.TokenRef, "env:") && !strings.HasPrefix(a.TokenRef, "file:") {
				return fmt.Errorf("token_ref must be a SecretRef reference, never a value: %q", a.TokenRef)
			}
		}

		// The pending-verification queue the panel derives: a launch whose action_id has NO terminal deferred
		// verdict is pending. Exactly one launch (act-2) is unverified here.
		verified := map[string]bool{}
		for _, v := range page.DeferredVerdicts {
			verified[v.ActionID] = true
		}
		if len(page.DeferredVerdicts) != 1 || page.DeferredVerdicts[0].Graduation != "verified_clean" {
			return fmt.Errorf("expected one terminal deferred verdict (verified_clean): %+v", page.DeferredVerdicts)
		}
		pending := 0
		for _, a := range page.Actuations {
			if !verified[a.ActionID] {
				pending++
			}
		}
		if pending != 1 {
			return fmt.Errorf("the pending-verification queue must derive to exactly one launch, got %d", pending)
		}

		// The per-lane coverage roll-up: the awx-job lane carries the real traffic counts.
		var awxCov bool
		for _, c := range page.LaneCoverage {
			if c.Lane == "awx-job" && c.Actuations == 2 {
				awxCov = true
			}
		}
		if !awxCov {
			return fmt.Errorf("the lane-coverage roll-up must report awx-job traffic: %+v", page.LaneCoverage)
		}

		// INV-13: no plaintext secret ever appears anywhere in the read view the console renders.
		if strings.Contains(string(w.body), consoleSecretValue) {
			return fmt.Errorf("INV-13 VIOLATION: a plaintext secret leaked into the /v1/regime read view")
		}
		return nil
	})
}
