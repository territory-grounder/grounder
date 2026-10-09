package observe

import (
	"log"
	"sync"
	"time"

	"github.com/territory-grounder/grounder/adapters/model"
)

// GatewayObserver implements adapters/model.CallObserver: it records every model-gateway call into the
// metrics Registry AND logs the notable ones (any non-"ok" outcome — rate_limit/timeout/empty/… — or a
// success slower than SlowThreshold) as a structured line. It is the composition-root glue between the
// gateway boundary and TG's observability, so a slow reasoning model or a provider error is visible in
// BOTH /metrics and the worker log for EVERY caller (agent loop, offline gate, judge, generator,
// calibrator), not just the ones behind a decorator.
//
// OBSERVE-ONLY: it never blocks, never fails a completion, and never touches a decision path — a
// CallObserver must not break inference. The logged detail is bounded provider error prose, never a secret.
type GatewayObserver struct {
	reg  *Registry
	slow time.Duration
	logf func(string, ...any)
	// missingSeen dedupes the "this tier reports no usage" warning to once per tier. Guarded by its own
	// mutex because the gateway is shared by every caller and observers run on their goroutines.
	missingMu   sync.Mutex
	missingSeen map[string]bool
}

// NewGatewayObserver wires an observer that records into reg and logs notable calls via logf. A slow of 0
// defaults to 60s (above a reasoning model's normal judge-sized latency, so the log flags the concerning
// tail, not every call); a nil logf defaults to log.Printf. A nil reg still logs (the metric record no-ops).
func NewGatewayObserver(reg *Registry, slow time.Duration, logf func(string, ...any)) *GatewayObserver {
	if slow <= 0 {
		slow = 60 * time.Second
	}
	if logf == nil {
		logf = log.Printf
	}
	return &GatewayObserver{reg: reg, slow: slow, logf: logf, missingSeen: map[string]bool{}}
}

// ObserveCall records the call in the metrics registry and logs it when it is notable (failed/empty/slow).
// The common healthy fast path is recorded but not logged, to keep the log signal-dense.
func (o *GatewayObserver) ObserveCall(tier, caller, outcome string, statusCode int, seconds float64, detail string) {
	// caller is UNBOUNDED ("runner:<external-ref>", …) so it goes to the LOG only — RecordModelCall stays
	// on tier, or the metric's label cardinality would grow without bound. The log is where it earns its
	// keep: TG-530's ~600 doomed empty-model calls/day sat anonymous for days because this line named only
	// the (empty) tier.
	RecordModelCall(o.reg, tier, outcome, seconds)
	if outcome == "ok" && seconds < o.slow.Seconds() {
		return
	}
	if len(detail) > 200 {
		detail = detail[:200]
	}
	if len(caller) > 120 {
		caller = caller[:120]
	}
	o.logf("modelcall: tier=%s caller=%q outcome=%s status=%d %.1fs detail=%q", tier, caller, outcome, statusCode, seconds, detail)
}

// ObserveUsage records the provider's REPORTED token accounting for one billable completion (TG-44), and
// LOGS the first time a tier comes back with no usage block. The log matters as much as the counter: an
// unmeasured tier means the cost breaker is billing that tier from a chars/4 estimate that measured 1.9x-
// 13.8x LOW against the live gateway, so the budget it enforces is not the budget the operator set. Once
// per tier, not per call — a per-call line on a broken provider would bury the log it belongs in.
//
// OBSERVE-ONLY, like ObserveCall: it counts and logs; it never blocks and never fails a completion.
func (o *GatewayObserver) ObserveUsage(tier string, u model.Usage) {
	o.reg.Usage(tier, u.PromptTokens, u.CompletionTokens, u.Measured)
	if u.Measured {
		return
	}
	o.missingMu.Lock()
	first := !o.missingSeen[tier]
	if first {
		if o.missingSeen == nil {
			o.missingSeen = map[string]bool{}
		}
		o.missingSeen[tier] = true
	}
	o.missingMu.Unlock()
	if first {
		o.logf("modelusage: tier=%s returned NO usage block — token/cost accounting for this tier falls back "+
			"to the chars/4 ESTIMATE, which measured 1.9x-13.8x low against the live gateway; the cost "+
			"breaker's budget is therefore approximate for it (see tg_model_usage_missing_total)", tier)
	}
}

var (
	_ model.CallObserver  = (*GatewayObserver)(nil)
	_ model.UsageObserver = (*GatewayObserver)(nil)
)
