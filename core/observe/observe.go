// Package observe is Territory Grounder's nil-safe observability emitter: the ONE seam the read-only worker
// records agent-loop, verify, and governance-decision metrics through, without coupling the activities to
// the /metrics exposition. It is injected once at the composition root (cmd/worker/main.go → runner.Deps)
// and threaded into the activities.
//
// It is OBSERVE-ONLY. Recording a metric never gates, never changes control flow, and never touches the
// actuation / mutation-breaker / mode chokepoints — metrics observe; they never decide. Injecting this
// emitter is strictly additive: with it absent (a nil Emitter) every code path behaves exactly as before.
//
// NIL-SAFE by design: the package RecordX helpers no-op on a nil Emitter and every *Registry method no-ops
// on a nil receiver, so the no-DB path, the oracle, and tests keep working whether or not an emitter is
// wired — a nil emitter is a silent no-op that never panics.
//
// The exposition it renders is BOUNDED and SECRET-FREE by construction: it carries counts/seconds only,
// and every label value is a clamped enum (agent outcome, verify verdict, autonomy band, withheld) drawn
// from core/metrics — never a host, ref, op, arg, or credential. There is no path that puts caller free
// text on the wire.
package observe

import (
	"sort"
	"sync"
	"time"

	"github.com/territory-grounder/grounder/core/metrics"
)

// Emitter is the ONE observability seam the Runner's activities record through. Every method is
// side-effect-only — it counts; it never returns a decision. It is OPTIONAL: prefer the package RecordX
// helpers, which are nil-safe, so a call site never has to guard the injected (possibly nil) emitter.
type Emitter interface {
	// AgentLoop records the five-metric agent family for ONE investigate loop (OpenAI/SK observable-by-
	// default): runtime, tool-call count, tool errors, approximate tokens, and the terminal outcome.
	AgentLoop(AgentLoopStat)
	// Verdict records one mechanical post-execution verify verdict (match/partial/deviation/unset).
	Verdict(verdict string)
	// Decision records one governance classification decision — mirrors the classify:<band> row the
	// tamper-evident ledger appends — by autonomy band and whether autonomy was withheld.
	Decision(band string, withheld bool)
}

// AgentLoopStat is one agent-loop observation. Outcome is clamped to the bounded agent-outcome enum on
// record; the numeric fields are summed into monotonic counters. All fields are derived from the loop's
// own result (agent.Result) plus the wall-clock the activity measured — never from a secret.
type AgentLoopStat struct {
	Outcome      string        // agent.Outcome.String(): stop | escalate | proposed | hard-halt
	Duration     time.Duration // wall-clock time of the ReAct loop
	ToolCalls    int           // len(Result.ToolResults)
	ToolErrors   int           // tool results whose Success was false
	ApproxTokens int           // char/4 approximation of tokens processed (the gateway returns no usage)
}

// RecordAgentLoop records an agent-loop observation. A nil Emitter is a no-op (never panics).
func RecordAgentLoop(e Emitter, s AgentLoopStat) {
	if e != nil {
		e.AgentLoop(s)
	}
}

// RecordVerdict records one verify verdict. A nil Emitter is a no-op.
func RecordVerdict(e Emitter, verdict string) {
	if e != nil {
		e.Verdict(verdict)
	}
}

// RecordDecision records one governance classification decision. A nil Emitter is a no-op.
func RecordDecision(e Emitter, band string, withheld bool) {
	if e != nil {
		e.Decision(band, withheld)
	}
}

// Registry is the concrete Emitter: thread-safe monotonic counters plus a runtime sum, collected into a
// deterministic set of Prometheus samples. It is SHARED across the worker's concurrent Temporal activities,
// so every mutator holds the lock. Every method is nil-receiver safe (a nil *Registry is a no-op), so the
// Registry is safe to inject or omit. Construct one with NewRegistry — the zero value is not ready.
type Registry struct {
	mu           sync.Mutex
	runSeconds   float64
	runs         map[string]int64      // agent-loop count by clamped outcome
	toolCalls    int64                 // total tool calls
	toolErrors   int64                 // total tool errors
	approxTokens int64                 // total approximate tokens
	verdicts     map[string]int64      // verify-verdict count by clamped verdict
	decisions    map[decisionKey]int64 // governance-decision count by band + withheld
}

type decisionKey struct {
	band     string
	withheld bool
}

// NewRegistry returns an empty, ready Registry.
func NewRegistry() *Registry {
	return &Registry{
		runs:      map[string]int64{},
		verdicts:  map[string]int64{},
		decisions: map[decisionKey]int64{},
	}
}

// AgentLoop records the five-metric family for one loop. Nil-receiver safe.
func (r *Registry) AgentLoop(s AgentLoopStat) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if s.Duration > 0 {
		r.runSeconds += s.Duration.Seconds()
	}
	r.runs[metrics.ClampAgentOutcome(s.Outcome)]++
	if s.ToolCalls > 0 {
		r.toolCalls += int64(s.ToolCalls)
	}
	if s.ToolErrors > 0 {
		r.toolErrors += int64(s.ToolErrors)
	}
	if s.ApproxTokens > 0 {
		r.approxTokens += int64(s.ApproxTokens)
	}
}

// Verdict records one verify verdict. Nil-receiver safe.
func (r *Registry) Verdict(verdict string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.verdicts[metrics.ClampVerdict(verdict)]++
}

// Decision records one governance classification decision. Nil-receiver safe.
func (r *Registry) Decision(band string, withheld bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.decisions[decisionKey{band: metrics.ClampBand(band), withheld: withheld}]++
}

// Collect renders the current counters into Prometheus samples. A nil *Registry collects nothing. The
// output is DETERMINISTIC: the four base agent counters emit unconditionally, and the labelled families
// (runs, verdicts, decisions) are emitted in a stable sorted order so a scrape of an unchanged Registry is
// byte-identical every time (metrics.Render groups by name but preserves within-group order, so the
// stable order must be established HERE). Label values are bounded enums only — the output is secret-free.
func (r *Registry) Collect() []metrics.Sample {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	out := []metrics.Sample{
		metrics.AgentRunSecondsSample(r.runSeconds),
		metrics.AgentToolCallsSample(float64(r.toolCalls)),
		metrics.AgentToolErrorsSample(float64(r.toolErrors)),
		metrics.AgentTokensApproxSample(float64(r.approxTokens)),
	}

	for _, outcome := range sortedKeys(r.runs) {
		out = append(out, metrics.AgentRunsSample(outcome, float64(r.runs[outcome])))
	}
	for _, verdict := range sortedKeys(r.verdicts) {
		out = append(out, metrics.VerdictsSample(verdict, float64(r.verdicts[verdict])))
	}
	dkeys := make([]decisionKey, 0, len(r.decisions))
	for k := range r.decisions {
		dkeys = append(dkeys, k)
	}
	sort.Slice(dkeys, func(i, j int) bool {
		if dkeys[i].band != dkeys[j].band {
			return dkeys[i].band < dkeys[j].band
		}
		return !dkeys[i].withheld && dkeys[j].withheld
	})
	for _, k := range dkeys {
		out = append(out, metrics.DecisionsSample(k.band, k.withheld, float64(r.decisions[k])))
	}
	return out
}

func sortedKeys(m map[string]int64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

var _ Emitter = (*Registry)(nil)

// --- process-global default: the last-mile exposure seam ---
//
// The activities record through the INJECTED runner.Deps emitter (dependency injection). This global is a
// separate, narrow concern: it lets the worker's read-only /metrics handler collect the SAME registry the
// composition root already built, without threading it through the admin surface's constructor signature.
// It is written exactly once at boot (SetDefault) and read per scrape (Collect); both are nil-safe, so an
// unset default (every test that does not call SetDefault) collects nothing.

var (
	defaultMu  sync.RWMutex
	defaultReg *Registry
)

// SetDefault installs the process-global registry the /metrics handler collects. Call once at the
// composition root with the same registry injected into runner.Deps.
func SetDefault(r *Registry) {
	defaultMu.Lock()
	defaultReg = r
	defaultMu.Unlock()
}

// Collect returns the process-global registry's samples, or nil when no default has been installed
// (nil-safe — never panics).
func Collect() []metrics.Sample {
	defaultMu.RLock()
	r := defaultReg
	defaultMu.RUnlock()
	return r.Collect()
}
