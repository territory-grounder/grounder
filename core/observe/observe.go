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
	// ModelCall records one model-gateway completion at the gateway boundary: the model tier, the
	// classified outcome (ok/empty/rate_limit/timeout/…), and the wall-clock seconds it took.
	ModelCall(tier, outcome string, seconds float64)
	// Calibration records the CURRENT confidence-reliability curve. It is a SET, not an add: a reliability
	// score is the state of a curve, and each calibrator pass replaces the previous reading rather than
	// accumulating onto it.
	Calibration(CalibrationReading)
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

// CalibrationReading is one confidence-reliability curve as published. BaseRate is the observed clean rate
// (the outcome's climatology) and Skill is the Brier Skill Score against it — positive means the stated
// confidence carries information a constant does not, negative means it carries LESS. SkillDefined is false
// at a degenerate base rate, where the score is undefined and must be WITHHELD rather than rendered as 0
// (the same discipline as withholding the scores at N=0, REQ-2022).
type CalibrationReading = metrics.CalibrationReading

// RecordCalibration records the current confidence-reliability curve. A nil Emitter is a no-op, so the
// calibrator can forward unconditionally whether or not metrics are wired.
func RecordCalibration(e Emitter, c CalibrationReading) {
	if e != nil {
		e.Calibration(c)
	}
}

// RecordDecision records one governance classification decision. A nil Emitter is a no-op.
func RecordDecision(e Emitter, band string, withheld bool) {
	if e != nil {
		e.Decision(band, withheld)
	}
}

// RecordModelCall records one model-gateway completion (tier, classified outcome, seconds). A nil Emitter
// is a no-op, so the gateway's CallObserver adapter can forward here unconditionally.
func RecordModelCall(e Emitter, tier, outcome string, seconds float64) {
	if e != nil {
		e.ModelCall(tier, outcome, seconds)
	}
}

// Registry is the concrete Emitter: thread-safe monotonic counters plus a runtime sum, collected into a
// deterministic set of Prometheus samples. It is SHARED across the worker's concurrent Temporal activities,
// so every mutator holds the lock. Every method is nil-receiver safe (a nil *Registry is a no-op), so the
// Registry is safe to inject or omit. Construct one with NewRegistry — the zero value is not ready.
type Registry struct {
	mu           sync.Mutex
	runSeconds   float64
	runs         map[string]int64       // agent-loop count by clamped outcome
	toolCalls    int64                  // total tool calls
	toolErrors   int64                  // total tool errors
	approxTokens int64                  // total approximate tokens
	verdicts     map[string]int64       // verify-verdict count by clamped verdict
	decisions    map[decisionKey]int64  // governance-decision count by band + withheld
	modelCalls   map[modelCallKey]int64 // model-call count by clamped tier + outcome
	modelSeconds map[string]float64     // cumulative model-call seconds by clamped tier
	// modelTokens holds only PROVIDER-REPORTED tokens and usageMissing counts the billable calls that
	// reported none (TG-44). They are separate maps, not one map with an "estimated" label value, so there is
	// no way to accidentally sum a guess into the measured total: a series that can hold both is a series
	// somebody will add up.
	modelTokens  map[modelTokenKey]int64
	usageMissing map[string]int64
	// calib is the LATEST calibration reading. Unlike every other field here it is REPLACED, not incremented:
	// the calibrator recomputes the whole curve each pass, so accumulating would be meaningless.
	calib calibReading
}

type decisionKey struct {
	band     string
	withheld bool
}

type modelCallKey struct {
	tier    string
	outcome string
}

// modelTokenKey keys the MEASURED token counter by clamped tier + prompt/completion.
type modelTokenKey struct {
	tier string
	kind string
}

// NewRegistry returns an empty, ready Registry.
func NewRegistry() *Registry {
	return &Registry{
		runs:         map[string]int64{},
		verdicts:     map[string]int64{},
		decisions:    map[decisionKey]int64{},
		modelCalls:   map[modelCallKey]int64{},
		modelSeconds: map[string]float64{},
		modelTokens:  map[modelTokenKey]int64{},
		usageMissing: map[string]int64{},
	}
}

// Usage records the provider's REPORTED token accounting for ONE billable gateway completion (TG-44).
//
// It is deliberately BRANCHING, not additive-with-a-flag: a measured call adds to the token counters and an
// UNMEASURED one adds to tg_model_usage_missing_total INSTEAD. Nothing estimated ever lands in
// tg_model_tokens_total, so that series can be read as spend without a caveat — which is the only kind of
// cost number worth publishing. Labels are clamped enums; the value is a count. Nil-receiver safe.
func (r *Registry) Usage(tier string, promptTokens, completionTokens int, measured bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t := metrics.ClampModelTier(tier)
	if !measured {
		r.usageMissing[t]++
		return
	}
	if promptTokens > 0 {
		r.modelTokens[modelTokenKey{tier: t, kind: metrics.TokenKindPrompt}] += int64(promptTokens)
	}
	if completionTokens > 0 {
		r.modelTokens[modelTokenKey{tier: t, kind: metrics.TokenKindCompletion}] += int64(completionTokens)
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

// ModelCall records one model-gateway completion (tier, outcome, seconds). Both labels are clamped to their
// bounded enums on record. Nil-receiver safe.
// calibReading is the latest confidence-reliability curve. `set` distinguishes "never scored" from a genuine
// all-zero reading, so Collect can withhold the scores rather than publish a flawless-looking zero.
type calibReading struct {
	set     bool
	reading CalibrationReading
}

// Calibration replaces the stored reading. A nil Registry is a no-op.
func (r *Registry) Calibration(c CalibrationReading) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calib = calibReading{set: true, reading: c}
}

func (r *Registry) ModelCall(tier, outcome string, seconds float64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t := metrics.ClampModelTier(tier)
	r.modelCalls[modelCallKey{tier: t, outcome: metrics.ClampModelOutcome(outcome)}]++
	if seconds > 0 {
		r.modelSeconds[t] += seconds
	}
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

	mkeys := make([]modelCallKey, 0, len(r.modelCalls))
	for k := range r.modelCalls {
		mkeys = append(mkeys, k)
	}
	sort.Slice(mkeys, func(i, j int) bool {
		if mkeys[i].tier != mkeys[j].tier {
			return mkeys[i].tier < mkeys[j].tier
		}
		return mkeys[i].outcome < mkeys[j].outcome
	})
	for _, k := range mkeys {
		out = append(out, metrics.ModelCallsSample(k.tier, k.outcome, float64(r.modelCalls[k])))
	}
	tiers := make([]string, 0, len(r.modelSeconds))
	for t := range r.modelSeconds {
		tiers = append(tiers, t)
	}
	sort.Strings(tiers)
	for _, t := range tiers {
		out = append(out, metrics.ModelCallSecondsSample(t, r.modelSeconds[t]))
	}
	// The MEASURED token family and its honesty denominator (TG-44), both in stable sorted order.
	tkeys := make([]modelTokenKey, 0, len(r.modelTokens))
	for k := range r.modelTokens {
		tkeys = append(tkeys, k)
	}
	sort.Slice(tkeys, func(i, j int) bool {
		if tkeys[i].tier != tkeys[j].tier {
			return tkeys[i].tier < tkeys[j].tier
		}
		return tkeys[i].kind < tkeys[j].kind
	})
	for _, k := range tkeys {
		out = append(out, metrics.ModelTokensSample(k.tier, k.kind, float64(r.modelTokens[k])))
	}
	for _, t := range sortedKeys(r.usageMissing) {
		out = append(out, metrics.ModelUsageMissingSample(t, float64(r.usageMissing[t])))
	}
	// Withheld entirely until the calibrator has run once. Publishing a zeroed curve before any pass would
	// show a perfect calibration for a system that has not been measured at all.
	if r.calib.set {
		out = append(out, metrics.CalibrationSamples(r.calib.reading)...)
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
