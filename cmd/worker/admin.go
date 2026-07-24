package main

// The worker's minimal internal admin surface (Phase-2 readiness review §4.B.2/§2): the worker OWNS the
// process-global actuation chokepoint (the mode-driven successor to the retired mutation gate), so the runtime
// kill-switch must reach THIS process. Today the worker has no HTTP server, so the only ON→OFF path was a
// restart. This adds a tiny listener serving exactly two routes: a HALT-ONLY kill-switch (POST /halt →
// chokepoint.ForceShadow, dropping the mode to read-only Shadow, REQ-1520) and a read-only /metrics exposition.
// It is SAFETY-ADDITIVE by construction: there is NO enable route here — /halt can only ever make the posture
// MORE restrictive, and /metrics is read-only. Bind it to the internal network (a distinct port, default :8444).

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/cost"
	"github.com/territory-grounder/grounder/core/metrics"
	"github.com/territory-grounder/grounder/core/observe"
	"github.com/territory-grounder/grounder/core/preflight"
	"github.com/territory-grounder/grounder/core/safety"
)

// workerAdmin is the worker's read-only-plus-halt admin surface. It holds only a DIGEST of the halt bearer
// token (never the token), so the process retains no reusable secret material.
type workerAdmin struct {
	gate       *safety.Chokepoint
	breaker    *safety.MutationBreaker
	cost       *cost.Accountant // the spend guard; nil ⇒ cost tracking disabled (no cost metrics emitted)
	ledger     *audit.Ledger
	haltDigest [32]byte // sha256 of the admin bearer token
	haltAuthed bool     // false ⇒ /halt is NOT registered (fail closed) — only /metrics is served
	halts      atomic.Int64
	// The boot credential-preflight (TG-113) result, surfaced on /metrics as tg_ssh_credential_ready so the
	// console shows a DEGRADED signal when a configured SSH key is missing/unreadable/unparseable by the
	// worker's real runtime user — instead of a false healthy. sshCredConfigured==0 ⇒ no SSH in use, no gauge.
	sshCredConfigured int
	sshCredReady      bool
}

// withSSHCredential records the boot credential-preflight result so /metrics can surface a DEGRADED-credential
// health signal (TG-113). Chainable. A zero configured count emits no gauge (native SSH is not in use).
func (a *workerAdmin) withSSHCredential(rep preflight.Report) *workerAdmin {
	a.sshCredConfigured = rep.Configured()
	a.sshCredReady = rep.Ready()
	return a
}

// newWorkerAdmin builds the admin surface. A non-empty bearerToken (resolved from TG_ADMIN_TOKEN_REF) arms
// the /halt route behind a constant-time bearer check; an empty token leaves /halt UNREGISTERED (fail
// closed) so only the read-only /metrics exists. A nil costAcct simply omits the cost gauges.
func newWorkerAdmin(gate *safety.Chokepoint, mb *safety.MutationBreaker, costAcct *cost.Accountant, ledger *audit.Ledger, bearerToken string) *workerAdmin {
	a := &workerAdmin{gate: gate, breaker: mb, cost: costAcct, ledger: ledger}
	if bearerToken != "" {
		a.haltDigest = sha256.Sum256([]byte(bearerToken))
		a.haltAuthed = true
	}
	return a
}

// mux builds the admin router: always /metrics (read-only), and /halt ONLY when a bearer token is
// configured. There is deliberately no /enable route — this surface can never turn mutation on.
func (a *workerAdmin) mux() *http.ServeMux {
	m := http.NewServeMux()
	m.Handle("/metrics", metrics.Handler(a.samples))
	if a.haltAuthed {
		m.HandleFunc("/halt", a.haltHandler)
	}
	return m
}

// authorized verifies the Authorization: Bearer <token> against the configured digest in constant time. A
// missing/malformed header, or an unconfigured halt token, is unauthorized (fail closed).
func (a *workerAdmin) authorized(r *http.Request) bool {
	if !a.haltAuthed {
		return false
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return false
	}
	got := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(got[:], a.haltDigest[:]) == 1
}

// haltHandler serves POST /halt: an authenticated operator disables the process-global mutation gate. It
// is idempotent and always safe (Disable can only turn mutation more off), records the halt to the
// tamper-evident governance ledger bound to a synthetic action_id, and returns 200. It NEVER enables.
func (a *workerAdmin) haltHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.authorized(r) {
		http.Error(w, "unauthenticated", http.StatusUnauthorized) // one indistinguishable 401
		return
	}
	// The kill: force the mode to Shadow (safe, idempotent) BEFORE recording, so the halt takes effect even if
	// the ledger write fails. This is the absorbed gate.Disable() — it drops the mode chokepoint to read-only
	// (REQ-1520). A synthetic action_id binds the halt in the audit chain (the ledger rejects an empty id).
	a.gate.ForceShadow("worker kill-switch: operator POST /halt")
	n := a.halts.Add(1)
	actionID := fmt.Sprintf("kill-switch-halt-%d", time.Now().UTC().UnixNano())
	if a.ledger != nil {
		if _, err := a.ledger.Append(audit.GovDecision{
			Decision: "safety:halt",
			Reason:   "worker kill-switch: operator POST /halt forced mode to Shadow (chokepoint.ForceShadow)",
			ActionID: actionID,
			Withheld: true, // autonomy withheld — mutation turned off
		}); err != nil {
			log.Printf("worker kill-switch: halt applied but ledger append failed: %v", err)
		}
	}
	log.Printf("worker kill-switch: HALT — mutation_enabled=%v (action_id=%s)", a.gate.MayActuate(), actionID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"halted":           true,
		"mutation_enabled": a.gate.MayActuate(),
		"action_id":        actionID,
		"halts":            n,
	})
}

// samples collects the worker's read-only metrics: the gate posture, the mutation breaker's three-state
// gauge + deviation count, and the halt total. All read-only; no secret is ever emitted.
func (a *workerAdmin) samples() []metrics.Sample {
	ctx := context.Background()
	enabled := 0.0
	if a.gate.MayActuate() {
		enabled = 1
	}
	out := []metrics.Sample{
		{Name: "mutation_enabled", Kind: metrics.Gauge, Help: "mutation gate: 1 on / 0 off (read-only foundation holds 0)", Value: enabled, Labels: map[string]string{"component": "worker"}},
		{Name: "tg_worker_halt_total", Kind: metrics.Counter, Help: "count of kill-switch halts applied via POST /halt", Value: float64(a.halts.Load())},
	}
	if a.breaker != nil {
		out = append(out,
			metrics.Sample{Name: "circuit_breaker_state", Kind: metrics.Gauge, Help: "mutation breaker: 0 closed / 1 half-open / 2 open", Value: a.breaker.StateValue(ctx), Labels: map[string]string{"name": "mutation"}},
			metrics.Sample{Name: "deviation_count", Kind: metrics.Counter, Help: "trip-worthy post-execution deviations/chain-gaps observed by the mutation breaker", Value: float64(a.breaker.Deviations())},
		)
	}
	// The COST/BUDGET spend guard gauges (spec/013 REQ-1211/1212): the durable UTC-day accrued spend and the
	// $-ceiling breaker's state. Read-only, fail-open (a store read error reports $0 / closed, logged in the
	// Accountant — a metrics read never halts). Emitted only when cost tracking is configured (a.cost != nil).
	if a.cost != nil {
		out = append(out,
			metrics.Sample{Name: "tg_cost_usd_today", Kind: metrics.Gauge, Help: "approximate USD spend accrued so far this UTC day (the cost breaker's daily accumulator)", Value: a.cost.TodayUSD(ctx)},
			metrics.Sample{Name: "tg_cost_breaker_state", Kind: metrics.Gauge, Help: "cost/budget breaker: 0 closed / 2 open (over budget)", Value: a.cost.StateValue(ctx), Labels: map[string]string{"name": "cost"}},
		)
	}
	// The CREDENTIAL health gauge (TG-113): the boot preflight proved (or failed to prove) that the worker's
	// REAL runtime user can resolve+read+parse every configured SSH private key. 0 = at least one is missing/
	// unreadable/unparseable ⇒ native SSH investigation + actuation is DEGRADED (the silent-kill made loud);
	// 1 = all configured refs are usable. Emitted only when at least one SSH key ref is configured — a
	// worker with no SSH surface has nothing to report and stays silent (no false-negative alert).
	if a.sshCredConfigured > 0 {
		ready := 0.0
		if a.sshCredReady {
			ready = 1
		}
		out = append(out, metrics.Sample{Name: "tg_ssh_credential_ready", Kind: metrics.Gauge, Help: "SSH credential preflight: 1 = every configured SSH key ref resolves+parses for the worker's runtime user; 0 = at least one is missing/unreadable/unparseable (native SSH investigation + actuation DEGRADED)", Value: ready, Labels: map[string]string{"component": "worker"}})
	}
	// The OBSERVE-ONLY agent-loop/verify/governance metrics recorded by the Runner's activities (spec/012),
	// collected from the process-global emitter the composition root installed. nil (no default set — e.g. a
	// unit test that never boots the worker) contributes nothing. Read-only; emits counts + bounded enum
	// labels only, never a secret.
	out = append(out, observe.Collect()...)
	return out
}

// startWorkerAdmin serves the admin surface on addr in a background goroutine (never blocks worker boot).
func startWorkerAdmin(addr string, a *workerAdmin) {
	srv := &http.Server{Addr: addr, Handler: a.mux(), ReadHeaderTimeout: 5 * time.Second}
	halt := "/halt DISABLED (no TG_ADMIN_TOKEN_REF resolved — fail closed)"
	if a.haltAuthed {
		halt = "/halt armed (bearer-authenticated, halt-only — no enable path)"
	}
	go func() {
		log.Printf("worker admin listener up on %s — /metrics (read-only); %s", addr, halt)
		if err := srv.ListenAndServe(); err != nil {
			log.Printf("worker admin listener exited: %v", err)
		}
	}()
}
