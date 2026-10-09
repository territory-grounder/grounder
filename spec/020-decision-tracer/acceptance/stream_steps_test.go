package acceptance

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/httpapi"
	"github.com/territory-grounder/grounder/core/trace"
)

// T-020-13 binds REQ-2013 + REQ-2010: the per-session STEP CHANNEL streams the walk's REAL boundary steps
// (assembled from the durable spine — never a client-side simulation clock) over the SAME authenticated,
// elevated trace-read route as the detail endpoint, so a queued or live-running session animates and no session
// state is invisible. The executed case is served by the detail endpoint; the queued/running cases stream their
// HONEST partial prefix (INV-15, no fabricated tail). The SSE wire mechanics (event framing, done-on-terminal,
// 404/503 fail-closed) are proven by core/httpapi TestSessionStream*; the console animation is a JS surface
// (like REQ-2012, verified by make console-verify). This oracle binds the server step-channel contract.
func init() {
	stepRegistrars = append(stepRegistrars, registerStreamSteps)
}

type streamWorld struct {
	queued, running, executed trace.SessionTrace
	streamGated               bool
}

func registerStreamSteps(sc *godog.ScenarioContext) {
	w := &streamWorld{}

	assemble := func() {
		// queued: classified only (no proposal yet). running: classified + investigating + proposed, no verdict.
		// executed: proposed + a mechanical verdict.
		w.queued = trace.Assemble("ext-queued", trace.SpineRecords{
			Classification: trace.ClassificationRecord{Present: true, Band: "AUTO", RiskLevel: "low", ActionID: "act-q"},
		})
		w.running = trace.Assemble("ext-running", trace.SpineRecords{
			Classification: trace.ClassificationRecord{Present: true, Band: "AUTO", RiskLevel: "low", ActionID: "act-r"},
			AgentCycles:    []trace.AgentCycleRecord{{Cycle: 1, Tool: "get-device-status", Thought: "investigate"}},
			Triage:         trace.TriageRecord{Present: true, Host: "web01", AlertRule: "svc-down", Proposed: true, Op: "restart-service"},
		})
		w.executed = trace.Assemble("ext-executed", trace.SpineRecords{
			Classification: trace.ClassificationRecord{Present: true, Band: "AUTO", RiskLevel: "low", ActionID: "act-e"},
			Triage:         trace.TriageRecord{Present: true, Host: "web01", Proposed: true, Op: "restart-service"},
			Verdict:        trace.VerdictRecord{Present: true, Verdict: "match"},
		})
	}
	proveGated := func(ref string, tr trace.SessionTrace) {
		// Prove the stream route is REGISTERED and gated: an UNAUTHENTICATED GET must be refused with an AUTH
		// status (401/403), not a 404 — a bare "!= 200" would pass even if the route were absent (chi 404s an
		// unknown path). Asserting the auth status proves the route exists AND is behind AuthTraceRead.
		rt := auth.NewRouter(&auth.Verifier{})
		httpapi.Register(rt, httpapi.Deps{SessionDetailRead: fixedDetailReader{tr: tr}})
		srv := httptest.NewServer(rt.Mux())
		defer srv.Close()
		if resp, err := http.Get(srv.URL + "/v1/sessions/" + ref + "/stream"); err == nil {
			w.streamGated = resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden
			_ = resp.Body.Close()
		}
	}

	// --- REQ-2013 ---
	sc.Step(`^a queued or running session and the per-session step channel$`, func() error {
		assemble()
		proveGated("ext-running", w.running)
		return nil
	})
	sc.Step(`^the session crosses each decision boundary$`, func() error {
		if len(w.running.Steps) == 0 {
			return fmt.Errorf("the running session has no boundary steps to stream")
		}
		return nil
	})
	sc.Step(`^the console animates the session from real boundary step events over an authenticated channel rather than a client-side simulation clock$`, func() error {
		if !w.streamGated {
			return fmt.Errorf("the step channel is NOT gated — an unauthenticated GET reached the stream")
		}
		// The streamed events ARE the real assembled boundary steps (the same projection the detail endpoint
		// serves) — not a fabricated/simulated sequence: every streamed node maps to a durable spine boundary.
		dto := httpapi.ProjectSessionDetail(w.running)
		if len(dto.Nodes) != len(w.running.Steps) {
			return fmt.Errorf("streamed nodes (%d) != real assembled steps (%d) — the channel would fabricate", len(dto.Nodes), len(w.running.Steps))
		}
		for i, n := range dto.Nodes {
			if n.T != string(w.running.Steps[i].Kind) {
				return fmt.Errorf("streamed node %d = %q, not the real boundary %q", i, n.T, w.running.Steps[i].Kind)
			}
		}
		return nil
	})

	// --- REQ-2010 ---
	sc.Step(`^a queued session a live-running session and an executed session$`, func() error {
		assemble()
		return nil
	})
	sc.Step(`^the operator inspects each$`, func() error { return nil })
	sc.Step(`^the executed session is served by the detail read endpoint and the queued and running sessions animate from real boundary events over the step channel so no session state is invisible$`, func() error {
		if httpapi.ProjectSessionDetail(w.executed).Status != string(trace.StatusExecuted) {
			return fmt.Errorf("executed session not served as executed by the detail projection")
		}
		// queued + running each have a non-empty HONEST partial prefix the step channel streams (no fabricated
		// tail): none is invisible, and neither carries a verify step it has not reached.
		for name, tr := range map[string]trace.SessionTrace{"queued": w.queued, "running": w.running} {
			nodes := httpapi.ProjectSessionDetail(tr).Nodes
			if len(nodes) == 0 {
				return fmt.Errorf("%s session is INVISIBLE (no streamable boundary steps)", name)
			}
			for _, n := range nodes {
				if n.T == string(trace.StepVerify) {
					return fmt.Errorf("%s session fabricated a verify step it has not reached", name)
				}
			}
		}
		return nil
	})
}
