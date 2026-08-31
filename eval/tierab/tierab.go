// Package tierab is the deterministic comparison core of TG-204's three-arm model-tier A/B: does the
// expensive reasoning tier actually buy diagnosis quality over the fast tier?
//
// THE DEFECT THIS PACKAGE EXISTS TO PREVENT. An A/B whose arms resolve to the SAME upstream model reports
// Δ = 0.00 on every axis and reads exactly like the most consequential possible finding — "the expensive
// tier buys nothing, retire it". TG-204 is one config read away from that mistake: in
// deploy/litellm-config.yaml the aliases `fast`, `primary` and `opus-cc` all name the SAME upstream model
// (`openai/opus-cc`), so TG-204's ARM-CONTROL (fast investigate / primary decide), ARM-STRONG (primary
// throughout) and ARM-CHEAP (fast throughout) are ONE ARM MEASURED THREE TIMES. The deltas would be pure
// judge noise and the decision they license — "retire the 53s tier" — would be made on a measurement that
// never varied its independent variable.
//
// The load-bearing fact is the shared MODEL, not the endpoint: TG-287 moved that channel to TLS on a new
// port while leaving all three aliases pointed at the same brain, so a note pinned to the api_base would
// have read stale within a day and the collapse would have looked like it had been fixed. Confirmed
// end-to-end against the DEPLOYED config on dc1tg01 and the proxy's own served_model on 2026-08-04:
// every arm was served claude-opus-5.
//
// ★ AND THE GATEWAY CANNOT TELL YOU. LiteLLM echoes back the REQUESTED ALIAS in the completion response's
// `model` field, not the model that actually served. Probed live through the box gateway on 2026-08-04:
//
//	alias "fast"      -> {"model":"fast",      "usage":{"prompt_tokens":155,...}}
//	alias "primary"   -> {"model":"primary",   "usage":{"prompt_tokens":155,...}}
//	alias "arm-haiku" -> {"model":"arm-haiku", "usage":{"prompt_tokens":144,...}}
//	alias "arm-opus"  -> {"model":"arm-opus",  "usage":{"prompt_tokens":155,...}}
//
// Every one of those responses is self-consistent and every one of them is useless as evidence of tier
// separation: a distinctness check reading the gateway's echo would pass VACUOUSLY on four aliases, two of
// which are the same brain. The ground truth lives one hop further upstream, in the tg-claude-proxy's own
// `served_model` telemetry (deploy/claude-proxy/src/main.rs logs it per completion, chosen from the CLI's
// modelUsage envelope rather than from what the caller asked for). So THIS package takes served-model
// evidence as an INPUT and refuses to compare arms without it — see Compare and ArmSignature.
//
// It performs NO SSH, NO model calls and NO I/O in Compare: the noisy on-box run happens in eval/tier-ab.sh,
// this is pure comparison, unit-tested in CI. Same split as eval/gate + tools/evalgate.
package tierab

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/eval/gate"
)

// The three arms TG-204 specifies. The names are the experiment's identity: they appear in TG_EVAL_ARM, in
// the printed report and in the archived verdict, so a reader can re-derive which tier pair produced which
// number without re-reading the driver script.
const (
	// ArmControl is production's CURRENT routing: the read-only investigate loop on the fast tier, the one
	// forced decision cycle on the reasoning tier (temporal/runner.investigateTierFor + decisionTierFor).
	ArmControl = "ARM-CONTROL"
	// ArmStrong is the reasoning tier throughout — the a-priori interesting arm for TG, whose loop is
	// policy-bound (HandoffPoll=5) rather than latency-bound, so a faster model buys no extra iterations.
	ArmStrong = "ARM-STRONG"
	// ArmCheap is the fast tier throughout — the arm the source paper predicts should win.
	ArmCheap = "ARM-CHEAP"
)

// GateDim is the axis TG-204 gates on: did the agent name the right cause. DiagDim is TG-201's
// deterministic companion (scored in Go from the typed diagnosis, so it carries no judge noise at all) and
// is reported beside it — a tier claim that moves the LLM axis but not the deterministic one is a claim
// about the judge, not about the agent.
var (
	GateDim = judge.Dimensions[0] // "correct_diagnosis" — sourced from the rubric, never re-declared
	DiagDim = judge.DimDiagnosisGrounded
)

// Call is one completion the tg-claude-proxy actually served: the ground truth for which BRAIN ran, what it
// cost and how long it took. Parsed from the proxy's structured "completion served" log lines.
//
// ServedModel is the load-bearing field. RequestedModel is what LiteLLM forwarded (the litellm_params
// model, e.g. "opus-cc"), which is NOT the arm's alias — two arms whose aliases differ can share a
// RequestedModel, and that is precisely the collapse this package detects.
type Call struct {
	At             time.Time
	ServedModel    string
	RequestedModel string
	Caller         string // the `user` TG sends, e.g. "runner:<external_ref>"
	DurationMs     int64
	CostUSD        float64
}

// proxyLine is the subset of the proxy's JSON log line this package reads. Fields it does not name are
// ignored, so a proxy that adds telemetry never breaks the parser.
type proxyLine struct {
	Timestamp      string  `json:"timestamp"`
	Message        string  `json:"message"`
	ServedModel    string  `json:"served_model"`
	RequestedModel string  `json:"requested_model"`
	Caller         string  `json:"caller"`
	DurationMs     int64   `json:"duration_ms"`
	CostUSD        float64 `json:"total_cost_usd"`
}

// completionServed is the proxy's message string for a served completion (deploy/claude-proxy/src/main.rs).
const completionServed = "completion served"

// ParseProxyLog reads tg-claude-proxy JSON log lines and returns the completions it served.
//
// ★ VACUITY FLOOR (house rule 3). This is a FILTER — it selects "completion served" lines out of a log that
// is mostly pool churn and rate-limit notices — so it MUST fail when it selects nothing. A silent empty
// result here is the worst available outcome: every arm would report 0 calls, $0.00, an empty served-model
// set, and (without this error) the collapse check below would have nothing to disagree about and could be
// read as "no collapse detected". A benchmark that measured nothing must say so, not return zero.
//
// It tolerates non-JSON and unparseable lines (the proxy's startup banner is not JSON) — those are skipped,
// not fatal — but a run that yields no served completion at all is an error the caller must surface.
func ParseProxyLog(raw string) ([]Call, error) {
	var out []Call
	var lines, jsonLines int
	for _, ln := range strings.Split(raw, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		lines++
		var p proxyLine
		if err := json.Unmarshal([]byte(ln), &p); err != nil {
			continue // not a structured line (banner/panic/partial write) — skipped, never fatal
		}
		jsonLines++
		if p.Message != completionServed {
			continue
		}
		c := Call{
			ServedModel: strings.TrimSpace(p.ServedModel), RequestedModel: strings.TrimSpace(p.RequestedModel),
			Caller: strings.TrimSpace(p.Caller), DurationMs: p.DurationMs, CostUSD: p.CostUSD,
		}
		if t, err := time.Parse(time.RFC3339Nano, p.Timestamp); err == nil {
			c.At = t.UTC()
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("proxy telemetry names no served completion: scanned %d line(s) (%d structured) and matched 0 %q — "+
			"the model-tier A/B cannot attribute a single call, so every arm would report an empty served-model set and $0.00 spend; "+
			"a run that measured nothing must not be reported as a run that found nothing",
			lines, jsonLines, completionServed)
	}
	return out, nil
}

// Window is the wall-clock interval one arm was measured in. Arms run SEQUENTIALLY (eval/tier-ab.sh
// serializes them under the gateway lock), so the window is what attributes a proxy call to an arm: the
// proxy's `caller` field is empty through today's gateway (LiteLLM drops `user`) and its `requested_model`
// is the litellm upstream, which collapsed arms share by definition. Time is the only discriminator left.
//
// ★ SUB-SECOND PRECISION IS LOAD-BEARING, and this is a defect the harness had and a live run found
// (2026-08-04). Boundaries were recorded with `date -u +%…%SZ` and Go's time.RFC3339, both of which TRUNCATE
// to the whole second. Truncating the START is harmless (it widens the window backwards); truncating the END
// silently NARROWS it, and it does so by up to a second at exactly the moment an arm's LAST call lands. The
// measured result: ARM-STRONG's only completion logged at 22:00:37.111 against an end of 22:00:37.000 and
// was dropped, as was ARM-CHEAP's at 22:00:39.744 against 22:00:39.000 — two of three arms reported UNKNOWN
// while the calls sat in the log. Worse than losing a call: the DECIDE-tier call is the last one an arm
// makes, so whole-second ends preferentially discard the tier TG-204 is asking about.
type Window struct {
	Start, End time.Time
}

// Contains reports whether a call falls in this arm's measurement window. BOTH ends are inclusive: a call
// logged in the same instant the arm started or ended belongs to it, and the alternative loses boundary
// calls in a harness whose windows are already only as precise as the clock that wrote them.
func (w Window) Contains(t time.Time) bool {
	return !t.Before(w.Start) && !t.After(w.End)
}

// AgentCallerPrefix is the `user` prefix the runner stamps on every AGENT-loop completion
// ("runner:"+ExternalRef, temporal/runner.InvestigateActivity).
//
// ★ THE JUDGE RUNS ON `primary` IN EVERY ARM. core/judge/rubric.json pins params.model="primary", so the
// offline scorer's own completions travel the SAME proxy as the arm it is scoring. Folding them in would
// (a) add the judge's brain to every arm's served-model signature, so an arm that genuinely ran haiku reads
// "haiku+opus" and the honest one-model answer is lost, and (b) bias ΔUSD and Δwall-clock toward ZERO by
// loading every arm with an identical constant. The judge is not part of the tier under test.
//
// ★ AND THIS FILTER IS INERT THROUGH TODAY'S GATEWAY — measured, not assumed. LiteLLM DROPS the OpenAI
// `user` field before calling an openai/-provider upstream, so the tg-claude-proxy logs caller="" for every
// request TG makes. Observed 2026-08-04: a preflight that posted "user":"runner:tierab-preflight" produced
// four proxy lines all reading `"caller":""`, and the prefix filter starved all three arms to zero calls.
// The harness FAILED CLOSED (TIER-AB: UNKNOWN) rather than mis-attributing, which is the right failure — but
// it means judge-exclusion cannot rest on this filter today. It rests on the WINDOW instead: the arm window
// is the session phase only, ending before judging begins (eval/phase.json, written by TestEvalCorpusOnBox).
//
// The filter is kept, implemented and tested because it is the correct mechanism and becomes load-bearing
// the moment LiteLLM forwards `user` (or TG calls the proxy directly). CallerFilterStarved exists so that
// day's regression arrives as a named diagnosis instead of three unexplained UNKNOWN arms. The gateway-side
// defect is TG-319.
const AgentCallerPrefix = "runner:"

// Arm is one measured arm: the tiers it DECLARED, the window it ran in, the scorecard it produced, and the
// calls the proxy actually served inside its window.
type Arm struct {
	Name            string
	InvestigateTier string // the TG_EVAL_ARM_INVESTIGATE alias this arm declared
	DecideTier      string // the TG_EVAL_ARM_DECIDE alias this arm declared
	Window          Window
	// CallerPrefix restricts attribution to completions this arm's AGENT made (see AgentCallerPrefix).
	// Empty means "attribute every call in the window".
	//
	// eval/tier-ab.sh sets it EMPTY today, and that is not laziness: LiteLLM drops the `user` field before
	// the proxy (TG-319), so a prefix matches nothing and starves every arm to UNKNOWN. The judge is kept
	// out by the WINDOW instead — the arm window is the session phase, which ends before judging starts.
	// Two mechanisms, and the one that currently works is the window; this field becomes the stronger of
	// the two the moment the gateway forwards `user`.
	CallerPrefix string
	Card         gate.Scorecard
	Calls        []Call
}

// Telemetry is one arm's measured cost/latency, derived from its attributed calls.
type Telemetry struct {
	Calls        int      `json:"calls"`
	MeanCallMs   float64  `json:"mean_call_ms"`
	TotalCallMs  int64    `json:"total_call_ms"`
	CostUSD      float64  `json:"cost_usd"`
	ServedModels []string `json:"served_models"` // the DISTINCT brains observed, sorted — the arm's true identity
}

// Telemetry summarizes the arm's attributed calls. An arm with no calls yields Calls==0 and an EMPTY
// ServedModels — deliberately not a fabricated default — which ArmSignature then reports as UNKNOWN.
func (a Arm) Telemetry() Telemetry {
	t := Telemetry{Calls: len(a.Calls)}
	seen := map[string]bool{}
	for _, c := range a.Calls {
		t.TotalCallMs += c.DurationMs
		t.CostUSD += c.CostUSD
		if c.ServedModel != "" && !seen[c.ServedModel] {
			seen[c.ServedModel] = true
			t.ServedModels = append(t.ServedModels, c.ServedModel)
		}
	}
	sort.Strings(t.ServedModels)
	if t.Calls > 0 {
		t.MeanCallMs = round2(float64(t.TotalCallMs) / float64(t.Calls))
	}
	t.CostUSD = round4(t.CostUSD)
	return t
}

// AttributeCalls slices the proxy's calls into each arm by (measurement window ∩ caller prefix). It is a
// pure function over (arms, calls) so the attribution rule is testable without a live proxy.
//
// Calls matching no arm are DROPPED and counted. Two distinct kinds land there and both must be excluded:
// traffic outside every window (production triage, a parallel campaign — the proxy is shared), and traffic
// inside a window from a caller that is not this arm's agent (the eval judge, which runs on `primary` in
// every arm — see AgentCallerPrefix). Silently folding either into an arm would corrupt precisely the axes
// TG-204 reports. The count is returned so the caller can disclose the exclusion instead of hiding it.
func AttributeCalls(arms []Arm, calls []Call) (out []Arm, unattributed int) {
	out = make([]Arm, len(arms))
	copy(out, arms)
	for i := range out {
		out[i].Calls = nil
	}
	for _, c := range calls {
		matched := false
		for i := range out {
			if !out[i].Window.Contains(c.At) {
				continue
			}
			if p := out[i].CallerPrefix; p != "" && !strings.HasPrefix(c.Caller, p) {
				continue
			}
			out[i].Calls = append(out[i].Calls, c)
			matched = true
		}
		if !matched {
			unattributed++
		}
	}
	return out, unattributed
}

// CallerFilterStarved names arms that had in-window proxy calls but were left with NONE by their caller
// prefix. It turns the single most likely operational failure of this harness — an arm-identity check that
// starves because the gateway did not forward the `user` field — from three unexplained UNKNOWN arms into
// one actionable sentence.
//
// It is a diagnosis, never a fallback: nothing in this package widens an arm's filter because the narrow one
// matched nothing. Silently falling back to "attribute everything" would fold the judge's `primary` calls
// into every arm at exactly the moment the operator is least able to notice.
func CallerFilterStarved(arms []Arm, calls []Call) []string {
	var starved []string
	for _, a := range arms {
		if a.CallerPrefix == "" || len(a.Calls) > 0 {
			continue
		}
		for _, c := range calls {
			if a.Window.Contains(c.At) {
				starved = append(starved, a.Name)
				break
			}
		}
	}
	return starved
}

// ArmSignature is an arm's OBSERVED identity: the sorted set of models that actually served it, joined.
// Two arms with the same signature ran the same brain(s) however different their declared aliases were.
//
// The empty-calls case returns the sentinel "" and the caller MUST treat it as UNKNOWN, never as "matches
// nothing else". Signature equality is how collapse is detected; if unknown compared unequal to everything,
// an arm with zero telemetry would certify as distinct from every other arm — a fail-OPEN in the one check
// this whole package exists to perform.
func ArmSignature(a Arm) string { return strings.Join(a.Telemetry().ServedModels, "+") }

// Outcome is the three-valued result, mirroring eval/gate's discipline: a comparison that could not vary
// its independent variable is neither a pass nor a regression — it is not a measurement.
type Outcome string

const (
	// OutcomeMeasured — every arm ran a distinct, known brain, so the deltas mean something.
	OutcomeMeasured Outcome = "measured"
	// OutcomeCollapsed — two or more arms were served by the SAME model, so the experiment measured one arm
	// more than once. NO deltas are emitted: an arm-collapsed Δ of 0.00 is the single most misleading number
	// this harness could print.
	OutcomeCollapsed Outcome = "collapsed"
	// OutcomeUnknown — at least one arm produced no served-model evidence, so its identity is unproven. Fails
	// CLOSED: an unverified arm is not a distinct arm.
	OutcomeUnknown Outcome = "unknown"
)

// ArmReport is one arm's published line.
type ArmReport struct {
	Name            string    `json:"name"`
	InvestigateTier string    `json:"investigate_tier"`
	DecideTier      string    `json:"decide_tier"`
	Signature       string    `json:"served_signature"`
	Telemetry       Telemetry `json:"telemetry"`
	GateDim         float64   `json:"correct_diagnosis"`
	DiagDim         float64   `json:"diagnosis_grounded"`
	Overall         float64   `json:"overall"`
	DecisionStepsA6 float64   `json:"decision_steps_a6a"`
	N               int       `json:"n"`
}

// Delta is one arm's difference from ARM-CONTROL on the axes TG-204 reports.
type Delta struct {
	Arm                string  `json:"arm"`
	CorrectDiagnosis   float64 `json:"d_correct_diagnosis"`
	DiagnosisGrounded  float64 `json:"d_diagnosis_grounded"`
	Overall            float64 `json:"d_overall"`
	DecisionStepsA6a   float64 `json:"d_decision_steps_a6a"`
	MeanCallMsA6b      float64 `json:"d_mean_call_ms_a6b"`
	CostUSD            float64 `json:"d_cost_usd"`
	BeatsControlOnDiag bool    `json:"beats_control_on_correct_diagnosis"`
}

// Verdict is the full deterministic result of a three-arm run.
type Verdict struct {
	Outcome Outcome     `json:"outcome"`
	Arms    []ArmReport `json:"arms"`
	// Deltas are populated ONLY on OutcomeMeasured. A collapsed or unknown run publishes none, because the
	// deltas of a collapsed experiment are the finding a reader would most want and least deserve.
	Deltas       []Delta    `json:"deltas"`
	Collapsed    [][]string `json:"collapsed_groups"`   // arms that shared a served model, grouped
	UnknownArms  []string   `json:"unknown_arms"`       // arms with no served-model evidence
	Unattributed int        `json:"unattributed_calls"` // proxy calls outside every arm window (disclosed, never folded in)
	Reasons      []string   `json:"reasons"`
	// Winner is the arm with the highest correct_diagnosis, and is set ONLY on a measured run. It is
	// deliberately a plain string and not a "the 53s tier is worth it" boolean: the decision is the owner's,
	// the measurement is this package's.
	Winner string `json:"winner,omitempty"`
	// Preflight marks a distinctness-only verdict (no corpus was run). A preflight that reads "measured"
	// means the ARMS are distinct, NOT that the tier question was answered — Render says so explicitly,
	// because "TIER-AB: MEASURED" on a run that scored nothing is the same class of lie as a gate that
	// passes a capability it never exercised (eval/gate's OutcomeInconclusive).
	Preflight bool `json:"preflight,omitempty"`
}

// Measured reports whether this verdict may be acted on. Exactly one Outcome is truthy here, for the same
// reason eval/gate.Verdict.Pass has exactly one: a caller that reads a boolean must never be handed a
// truthy value for a run that did not vary its independent variable.
func (v Verdict) Measured() bool { return v.Outcome == OutcomeMeasured }

// Distinctness answers the ONLY question that has to be settled before a three-arm A/B is worth running:
// did the arms actually run different models? It is the whole of Compare's first two checks and none of its
// arithmetic, so it can be run as a PREFLIGHT — one trivial completion per arm — instead of after three
// full corpus passes.
//
// That ordering is the point. TG-204's arms collapse today, and discovering it from a preflight costs three
// completions; discovering it from the deltas costs three corpus runs against a subscription-metered proxy
// and hands the reader a table of zeros to misread on the way. A cheap check that can refute the experiment
// belongs BEFORE the expensive one that assumes it.
func Distinctness(arms []Arm) Verdict {
	v := Verdict{Outcome: OutcomeMeasured, Preflight: true}
	for _, a := range arms {
		t := a.Telemetry()
		v.Arms = append(v.Arms, ArmReport{
			Name: a.Name, InvestigateTier: a.InvestigateTier, DecideTier: a.DecideTier,
			Signature: ArmSignature(a), Telemetry: t,
			GateDim: a.Card.DimMeans[GateDim], DiagDim: a.Card.DimMeans[DiagDim],
			Overall: a.Card.Overall, DecisionStepsA6: a.Card.MeanDecisionSteps, N: a.Card.N,
		})
	}

	// (1) UNKNOWN arms fail closed. An arm the proxy has no record of serving could have run anything.
	for _, a := range arms {
		if ArmSignature(a) == "" {
			v.UnknownArms = append(v.UnknownArms, a.Name)
		}
	}
	if len(v.UnknownArms) > 0 {
		v.Outcome = OutcomeUnknown
		v.Reasons = append(v.Reasons, fmt.Sprintf(
			"no served-model evidence for %v — the arm's identity is UNPROVEN, and an unverified arm is not a "+
				"distinct arm (the gateway's response echoes the requested alias, so it cannot supply this)", v.UnknownArms))
		return v
	}

	// (2) COLLAPSE: group arms by observed signature. This is the TG-204 check.
	bySig := map[string][]string{}
	var sigOrder []string
	for _, a := range arms {
		s := ArmSignature(a)
		if _, seen := bySig[s]; !seen {
			sigOrder = append(sigOrder, s)
		}
		bySig[s] = append(bySig[s], a.Name)
	}
	for _, s := range sigOrder {
		if names := bySig[s]; len(names) > 1 {
			v.Collapsed = append(v.Collapsed, names)
			v.Reasons = append(v.Reasons, fmt.Sprintf(
				"arms %v were all served by %s — this is ONE ARM measured %d times, so their Δ is judge noise, "+
					"not a tier effect; publishing it would license 'the expensive tier buys nothing' from a "+
					"measurement that never varied the tier", names, s, len(names)))
		}
	}
	if len(v.Collapsed) > 0 {
		v.Outcome = OutcomeCollapsed
	}
	return v
}

// Compare is the pure three-arm comparison. It performs NO I/O.
//
// Order of checks is deliberate. Arm DISTINCTNESS is settled FIRST (by Distinctness), before a single delta
// is computed, because every downstream number is meaningless without it — and because computing the deltas
// first and then deciding whether to print them is exactly the shape that leaks a collapsed 0.00 into a
// report.
func Compare(arms []Arm) Verdict {
	v := Distinctness(arms)
	v.Preflight = false
	if v.Outcome != OutcomeMeasured {
		return v // collapsed or unknown: no delta is computed, so none can leak
	}

	// (3) Only now, on genuinely distinct arms, are the deltas computed.
	ctrl, ok := findArm(v.Arms, ArmControl)
	if !ok {
		v.Outcome = OutcomeUnknown
		v.Reasons = append(v.Reasons, fmt.Sprintf("no %s arm in the run — every delta TG-204 reports is defined against it", ArmControl))
		return v
	}
	best := ctrl
	for _, a := range v.Arms {
		if a.Name == ArmControl {
			continue
		}
		v.Deltas = append(v.Deltas, Delta{
			Arm:                a.Name,
			CorrectDiagnosis:   round2(a.GateDim - ctrl.GateDim),
			DiagnosisGrounded:  round2(a.DiagDim - ctrl.DiagDim),
			Overall:            round2(a.Overall - ctrl.Overall),
			DecisionStepsA6a:   round2(a.DecisionStepsA6 - ctrl.DecisionStepsA6),
			MeanCallMsA6b:      round2(a.Telemetry.MeanCallMs - ctrl.Telemetry.MeanCallMs),
			CostUSD:            round4(a.Telemetry.CostUSD - ctrl.Telemetry.CostUSD),
			BeatsControlOnDiag: a.GateDim > ctrl.GateDim,
		})
		if a.GateDim > best.GateDim {
			best = a
		}
	}
	v.Winner = best.Name
	return v
}

func findArm(rs []ArmReport, name string) (ArmReport, bool) {
	for _, r := range rs {
		if r.Name == name {
			return r, true
		}
	}
	return ArmReport{}, false
}

// Render prints the human-readable three-arm report. A collapsed or unknown run prints its arms and its
// REASONS and no delta table at all — the absence is the finding.
func Render(v Verdict) string {
	var b strings.Builder
	if v.Preflight {
		b.WriteString("== TG-204 three-arm model-tier A/B — ARM-DISTINCTNESS PREFLIGHT (no corpus run) ==\n\n")
	} else {
		b.WriteString("== TG-204 three-arm model-tier A/B ==\n\n")
	}
	fmt.Fprintf(&b, "  %-13s %-11s %-11s %-32s %5s %7s %7s %9s %9s\n",
		"arm", "investigate", "decide", "SERVED (proxy ground truth)", "n", GateDim[:5]+".", "diag_g", "mean_ms", "usd")
	fmt.Fprintf(&b, "  %s\n", strings.Repeat("-", 118))
	for _, a := range v.Arms {
		sig := a.Signature
		if sig == "" {
			sig = "UNKNOWN (no proxy evidence)"
		}
		fmt.Fprintf(&b, "  %-13s %-11s %-11s %-32s %5d %7.2f %7.2f %9.0f %9.4f\n",
			a.Name, a.InvestigateTier, a.DecideTier, sig, a.N, a.GateDim, a.DiagDim,
			a.Telemetry.MeanCallMs, a.Telemetry.CostUSD)
	}
	if v.Unattributed > 0 {
		fmt.Fprintf(&b, "\n  %d proxy call(s) fell outside every arm window and were EXCLUDED (other traffic on the same proxy).\n", v.Unattributed)
	}
	switch {
	case v.Outcome == OutcomeMeasured && v.Preflight:
		// ★ A DISTINCT PREFLIGHT IS PERMISSION TO MEASURE, NOT A MEASUREMENT. Saying "MEASURED" here would
		// claim the tier question was answered by three one-token completions.
		b.WriteString("\nTIER-AB PREFLIGHT: ARMS ARE DISTINCT — the experiment CAN be run. Nothing about diagnosis\n" +
			"quality has been measured yet; run eval/tier-ab.sh for the corpus arms.\n")
	case v.Outcome == OutcomeMeasured:
		fmt.Fprintf(&b, "\n  %-13s %10s %10s %10s %12s %12s\n", "Δ vs control", "diagnosis", "diag_grnd", "overall", "steps(A6a)", "ms(A6b)")
		for _, d := range v.Deltas {
			fmt.Fprintf(&b, "  %-13s %+10.2f %+10.2f %+10.2f %+12.2f %+12.0f\n",
				d.Arm, d.CorrectDiagnosis, d.DiagnosisGrounded, d.Overall, d.DecisionStepsA6a, d.MeanCallMsA6b)
		}
		fmt.Fprintf(&b, "\nTIER-AB: MEASURED — highest %s: %s\n", GateDim, v.Winner)
	case v.Outcome == OutcomeCollapsed:
		b.WriteString("\nTIER-AB: COLLAPSED — NOT a result. The arms did not differ, so nothing about model tier was measured.\n")
	default:
		b.WriteString("\nTIER-AB: UNKNOWN — NOT a result. At least one arm's served model is unproven; the harness fails closed.\n")
	}
	for _, r := range v.Reasons {
		fmt.Fprintf(&b, "  - %s\n", r)
	}
	return b.String()
}

// JSON serializes the verdict for the archived quality record.
func (v Verdict) JSON() []byte {
	out, _ := json.MarshalIndent(v, "", "  ")
	return append(out, '\n')
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }
func round4(f float64) float64 { return math.Round(f*10000) / 10000 }
