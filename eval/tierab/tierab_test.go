package tierab

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/eval/gate"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

// servedLine builds one tg-claude-proxy "completion served" log line, in the shape the real proxy emits
// (verified against a live tail of tg-claude-proxy on dc1claude01, 2026-08-04).
func servedLine(ts, served, requested string, ms int64, usd float64) string {
	return fmt.Sprintf(`{"timestamp":"%s","level":"INFO","message":"completion served","request_id":"req-000001",`+
		`"duration_ms":%d,"total_cost_usd":%v,"prompt_tokens":155,"completion_tokens":4,`+
		`"served_model":"%s","caller":"runner:IFR-1","requested_model":"%s","target":"claudecode_runner"}`,
		ts, ms, usd, served, requested)
}

func card(n int, correctDiagnosis, diagGrounded, overall, steps float64) gate.Scorecard {
	return gate.Scorecard{
		N: n, Judged: n, Overall: overall, MeanDecisionSteps: steps,
		DimMeans: map[string]float64{GateDim: correctDiagnosis, DiagDim: diagGrounded},
	}
}

// armWith builds an arm already carrying its attributed calls (bypassing AttributeCalls, which has its own
// tests) so the collapse logic is exercised in isolation.
func armWith(name, inv, dec string, c gate.Scorecard, served ...string) Arm {
	a := Arm{Name: name, InvestigateTier: inv, DecideTier: dec, Card: c,
		Window: Window{Start: at("2026-08-04T00:00:00Z"), End: at("2026-08-04T23:59:59Z")}}
	for _, s := range served {
		a.Calls = append(a.Calls, Call{At: at("2026-08-04T12:00:00Z"), ServedModel: s, DurationMs: 1000, CostUSD: 0.001})
	}
	return a
}

// ★ THE KILLING MUTATION (executed 2026-08-04). Delete the collapse branch in Compare — i.e. let it fall
// through to the delta computation whenever no arm is UNKNOWN — and this test goes RED with:
//
//	"a three-arm A/B in which every arm was served claude-opus-5 was reported as MEASURED, and it published
//	 2 delta row(s): those Δ are judge noise on ONE arm run three times, and reading them as a tier effect is
//	 exactly the 'retire the 53s tier' decision TG-204 must not license"
//
// Restored: green. This is TG-204's live shape — measured against the DEPLOYED litellm config on
// dc1tg01 (2026-08-04), `fast`, `primary` and `opus-cc` ALL resolve to openai/opus-cc, so the three
// arms the ticket specifies are one arm measured three times.
func TestCollapsedArmsAreRefusedRatherThanPublishedAsAZeroDelta(t *testing.T) {
	// The literal TG-204 arms, with the served model every one of them actually gets today.
	arms := []Arm{
		armWith(ArmControl, "fast", "primary", card(8, 3.90, 4.10, 3.55, 2.1), "claude-opus-5"),
		armWith(ArmStrong, "primary", "primary", card(8, 3.95, 4.05, 3.60, 2.0), "claude-opus-5"),
		armWith(ArmCheap, "fast", "fast", card(8, 3.85, 4.15, 3.50, 2.2), "claude-opus-5"),
	}
	v := Compare(arms)

	if v.Outcome != OutcomeCollapsed || v.Measured() {
		t.Fatalf("a three-arm A/B in which every arm was served claude-opus-5 was reported as %s, and it published "+
			"%d delta row(s): those Δ are judge noise on ONE arm run three times, and reading them as a tier effect "+
			"is exactly the 'retire the 53s tier' decision TG-204 must not license", v.Outcome, len(v.Deltas))
	}
	if len(v.Deltas) != 0 {
		t.Fatalf("a collapsed run published %d delta(s) — a collapsed Δ is the most misleading number this harness "+
			"can print, because Δ≈0.00 reads as 'the expensive tier buys nothing': %+v", len(v.Deltas), v.Deltas)
	}
	if v.Winner != "" {
		t.Fatalf("a collapsed run named a winner (%q) — there is no winner between an arm and itself", v.Winner)
	}
	if len(v.Collapsed) != 1 || len(v.Collapsed[0]) != 3 {
		t.Fatalf("the collapse group must name all three arms so the report is actionable, got %v", v.Collapsed)
	}
	// The rendered report must SAY it, not merely omit the deltas: a reader skimming for a number must be
	// stopped, and an empty table reads as "no difference found".
	txt := Render(v)
	for _, want := range []string{"COLLAPSED", "NOT a result", "claude-opus-5"} {
		if !strings.Contains(txt, want) {
			t.Errorf("the rendered collapsed report never says %q:\n%s", want, txt)
		}
	}
	if strings.Contains(txt, "Δ vs control") {
		t.Errorf("a collapsed report rendered the delta table:\n%s", txt)
	}
}

// An arm with no proxy evidence must fail CLOSED. If ArmSignature's "" compared unequal to every other
// signature, an arm nobody can prove ran anything would certify as DISTINCT from all the others — a
// fail-open in the one check this package exists to perform.
func TestAnArmWithNoServedModelEvidenceFailsClosed(t *testing.T) {
	arms := []Arm{
		armWith(ArmControl, "fast", "primary", card(8, 3.9, 4.1, 3.5, 2.1), "claude-opus-5"),
		armWith(ArmStrong, "arm-opus", "arm-opus", card(8, 4.4, 4.3, 3.9, 1.9), "claude-opus-5"),
		armWith(ArmCheap, "arm-haiku", "arm-haiku", card(8, 3.1, 3.4, 3.0, 3.4)), // NO calls observed
	}
	v := Compare(arms)
	if v.Outcome != OutcomeUnknown {
		t.Fatalf("an arm with zero served-model evidence produced outcome %s — an unverified arm must never be "+
			"treated as a distinct arm", v.Outcome)
	}
	if len(v.UnknownArms) != 1 || v.UnknownArms[0] != ArmCheap {
		t.Fatalf("the unknown arm must be NAMED so it can be re-run, got %v", v.UnknownArms)
	}
	if len(v.Deltas) != 0 {
		t.Fatalf("an unknown-arm run published deltas: %+v", v.Deltas)
	}
}

// ★ VACUITY FLOOR (house rule 3): ParseProxyLog is a filter, so it must FAIL when it matches nothing.
// KILLING MUTATION (executed): replace the `len(out) == 0` error with `return out, nil`. RED — "a proxy log
// containing no served completion parsed CLEANLY". With that mutation live, every arm reports 0 calls, an
// empty served-model set and $0.00, and the run reads as telemetry-free rather than as un-measured.
func TestParseProxyLogFailsWhenNoCompletionWasServed(t *testing.T) {
	// A realistic no-match log: pool churn + rate-limit notices, zero completions.
	raw := strings.Join([]string{
		`{"timestamp":"2026-08-04T21:36:51.217516Z","level":"INFO","message":"warm worker spawned","pool_worker_id":7}`,
		`{"timestamp":"2026-08-04T21:36:53.967327Z","level":"INFO","message":"claude subscription rate-limit window","rl_utilization":0.62}`,
		`tg-claude-proxy listening on 0.0.0.0:8094`, // the non-JSON startup banner
	}, "\n")
	calls, err := ParseProxyLog(raw)
	if err == nil {
		t.Fatalf("a proxy log containing no served completion parsed CLEANLY into %d call(s) — a filter that "+
			"matches nothing must fail, or an unmeasured run is indistinguishable from a cheap one", len(calls))
	}
	for _, want := range []string{"scanned", "matched 0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the vacuity error must say how much it scanned (so a reader can tell an empty log from a "+
				"wrong filter); missing %q in: %v", want, err)
		}
	}
}

// The parser must find real lines through the noise — the other half of the vacuity floor: a filter that
// can never match is as useless as one that always matches.
func TestParseProxyLogFindsServedCompletionsAmongNoise(t *testing.T) {
	raw := strings.Join([]string{
		`not json at all`,
		`{"message":"warm worker spawned"}`,
		servedLine("2026-08-04T21:36:53.967395Z", "claude-haiku-4-5-20251001", "haiku", 2750, 0.000639),
		servedLine("2026-08-04T21:36:56.229385Z", "claude-opus-5", "opus", 2231, 0.000875),
	}, "\n")
	calls, err := ParseProxyLog(raw)
	if err != nil {
		t.Fatalf("ParseProxyLog: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("want 2 served completions, got %d", len(calls))
	}
	if calls[0].ServedModel != "claude-haiku-4-5-20251001" || calls[0].RequestedModel != "haiku" {
		t.Errorf("served/requested model mis-parsed: %+v", calls[0])
	}
	if calls[1].DurationMs != 2231 || calls[1].CostUSD != 0.000875 {
		t.Errorf("duration/cost mis-parsed: %+v", calls[1])
	}
	if calls[0].At.IsZero() {
		t.Error("timestamp mis-parsed — window attribution depends on it entirely")
	}
}

// ★ THE ALIAS ECHO IS NOT EVIDENCE. requested_model is what LiteLLM forwarded upstream, and two arms whose
// DECLARED aliases differ share it whenever the aliases resolve to the same litellm entry. Probed live
// 2026-08-04: aliases `fast` and `primary` both resolve to openai/opus-cc, so the proxy sees
// requested_model="opus-cc" for both. Only served_model separates the arms — this test pins that the
// signature is built from served_model and would be blind if it used requested_model.
func TestTheArmSignatureIsBuiltFromTheServedModelNotTheRequestedOne(t *testing.T) {
	// Two arms, DIFFERENT requested models at the proxy, but the SAME brain served. If the signature keyed
	// on requested_model these would look distinct; they are not.
	a := Arm{Name: ArmControl, Calls: []Call{{ServedModel: "claude-opus-5", RequestedModel: "opus-cc"}}}
	b := Arm{Name: ArmStrong, Calls: []Call{{ServedModel: "claude-opus-5", RequestedModel: "opus"}}}
	if ArmSignature(a) != ArmSignature(b) {
		t.Fatalf("two arms served the SAME model (claude-opus-5) got different signatures %q vs %q — the "+
			"signature is keyed on something other than the served model, so a collapsed A/B would certify",
			ArmSignature(a), ArmSignature(b))
	}
	// And two arms with the same requested model but different brains must NOT collapse.
	c := Arm{Name: ArmCheap, Calls: []Call{{ServedModel: "claude-haiku-4-5-20251001", RequestedModel: "opus-cc"}}}
	if ArmSignature(a) == ArmSignature(c) {
		t.Fatal("opus and haiku produced the same signature — genuinely distinct arms would be refused")
	}
}

// Traffic outside an arm's window belongs to somebody else (production triage, a parallel campaign) and
// folding it in would inflate the cost and latency axes TG-204 reports. It must be dropped AND counted.
func TestAttributeCallsExcludesAndDisclosesOutOfWindowTraffic(t *testing.T) {
	arms := []Arm{
		{Name: ArmControl, Window: Window{Start: at("2026-08-04T10:00:00Z"), End: at("2026-08-04T10:30:00Z")}},
		{Name: ArmStrong, Window: Window{Start: at("2026-08-04T10:31:00Z"), End: at("2026-08-04T11:00:00Z")}},
	}
	calls := []Call{
		{At: at("2026-08-04T09:59:59Z"), ServedModel: "claude-opus-5", CostUSD: 9}, // before  — someone else's
		{At: at("2026-08-04T10:00:00Z"), ServedModel: "claude-opus-5", CostUSD: 1}, // exactly at Start
		{At: at("2026-08-04T10:30:00Z"), ServedModel: "claude-opus-5", CostUSD: 1}, // exactly at End
		{At: at("2026-08-04T10:30:30Z"), ServedModel: "claude-opus-5", CostUSD: 9}, // in the gap between arms
		{At: at("2026-08-04T10:45:00Z"), ServedModel: "claude-haiku-4-5-20251001", CostUSD: 2},
	}
	out, unattributed := AttributeCalls(arms, calls)
	if unattributed != 2 {
		t.Fatalf("unattributed=%d, want 2 (the pre-window call and the inter-arm gap call)", unattributed)
	}
	if got := out[0].Telemetry(); got.Calls != 2 || got.CostUSD != 2 {
		t.Fatalf("ARM-CONTROL attributed %d call(s)/$%.4f, want 2/$2 — boundary calls must be INCLUSIVE, and "+
			"foreign traffic must never enter an arm's spend", got.Calls, got.CostUSD)
	}
	if got := out[1].Telemetry(); got.Calls != 1 || got.CostUSD != 2 {
		t.Fatalf("ARM-STRONG attributed %d call(s)/$%.4f, want 1/$2", got.Calls, got.CostUSD)
	}
}

// ★ THE EVAL JUDGE RUNS ON `primary` IN EVERY ARM (core/judge/rubric.json pins params.model="primary"), so
// its completions travel the SAME proxy inside the SAME window as the arm being scored. They must not be
// attributed to the arm.
//
// KILLING MUTATION (executed 2026-08-04): drop the CallerPrefix check in AttributeCalls (keep the window
// check only). RED — "ARM-CHEAP's served-model signature is claude-haiku-4-5-20251001+claude-opus-5: the
// judge's own `primary` calls were folded into the arm under test, so an arm that genuinely ran ONE cheap
// brain reports two, and ΔUSD/Δwall-clock are loaded with a constant identical in every arm".
func TestTheJudgesOwnCallsAreNotAttributedToTheArmItIsScoring(t *testing.T) {
	w := Window{Start: at("2026-08-04T10:00:00Z"), End: at("2026-08-04T10:30:00Z")}
	arms := []Arm{{Name: ArmCheap, InvestigateTier: "arm-haiku", DecideTier: "arm-haiku",
		Window: w, CallerPrefix: AgentCallerPrefix, Card: card(8, 3.1, 3.4, 3.0, 3.4)}}
	calls := []Call{
		{At: at("2026-08-04T10:05:00Z"), ServedModel: "claude-haiku-4-5-20251001", Caller: "runner:IFR-1", DurationMs: 400, CostUSD: 0.0004},
		{At: at("2026-08-04T10:06:00Z"), ServedModel: "claude-haiku-4-5-20251001", Caller: "runner:IFR-2", DurationMs: 400, CostUSD: 0.0004},
		// The judge, in-window, on `primary` -> the opus brain. NOT this arm's tier.
		{At: at("2026-08-04T10:20:00Z"), ServedModel: "claude-opus-5", Caller: "eval-judge", DurationMs: 9000, CostUSD: 0.05},
	}
	out, unattributed := AttributeCalls(arms, calls)
	if got := ArmSignature(out[0]); got != "claude-haiku-4-5-20251001" {
		t.Fatalf("ARM-CHEAP's served-model signature is %s: the judge's own `primary` calls were folded into the "+
			"arm under test, so an arm that genuinely ran ONE cheap brain reports two, and ΔUSD/Δwall-clock are "+
			"loaded with a constant identical in every arm", got)
	}
	tel := out[0].Telemetry()
	if tel.Calls != 2 || tel.CostUSD != 0.0008 || tel.MeanCallMs != 400 {
		t.Fatalf("arm telemetry = %d call(s)/$%.4f/%.0fms, want 2/$0.0008/400ms — the judge's 9s, $0.05 call leaked in",
			tel.Calls, tel.CostUSD, tel.MeanCallMs)
	}
	if unattributed != 1 {
		t.Errorf("the excluded judge call must be DISCLOSED in the unattributed count, got %d", unattributed)
	}
}

// The happy path: genuinely distinct arms produce the deltas TG-204 asks for, on every axis it names.
func TestDistinctArmsPublishTheDeltasOnEveryAxisTheTicketNames(t *testing.T) {
	arms := []Arm{
		armWith(ArmControl, "fast", "primary", card(8, 3.90, 4.10, 3.55, 2.10), "claude-opus-5"),
		armWith(ArmCheap, "arm-haiku", "arm-haiku", card(8, 3.10, 3.40, 3.00, 3.40), "claude-haiku-4-5-20251001"),
	}
	// Give the cheap arm a distinct cost/latency so ΔUSD and Δms are non-trivially checked.
	arms[1].Calls[0].DurationMs = 400
	arms[1].Calls[0].CostUSD = 0.0004

	v := Compare(arms)
	if !v.Measured() {
		t.Fatalf("two arms on genuinely different brains were not MEASURED: %s %v", v.Outcome, v.Reasons)
	}
	if len(v.Deltas) != 1 {
		t.Fatalf("want 1 delta row vs control, got %d", len(v.Deltas))
	}
	d := v.Deltas[0]
	if d.CorrectDiagnosis != -0.80 {
		t.Errorf("Δcorrect_diagnosis = %+.2f, want -0.80 (the axis TG-204 gates on)", d.CorrectDiagnosis)
	}
	if d.DiagnosisGrounded != -0.70 {
		t.Errorf("Δdiagnosis_grounded = %+.2f, want -0.70 (TG-201's deterministic companion)", d.DiagnosisGrounded)
	}
	if d.DecisionStepsA6a != 1.30 {
		t.Errorf("Δdecision_steps (A6a) = %+.2f, want +1.30", d.DecisionStepsA6a)
	}
	if d.MeanCallMsA6b != -600 {
		t.Errorf("Δmean call ms (A6b wall-clock) = %+.0f, want -600", d.MeanCallMsA6b)
	}
	if d.CostUSD != -0.0006 {
		t.Errorf("ΔUSD = %+.4f, want -0.0006", d.CostUSD)
	}
	if d.BeatsControlOnDiag {
		t.Error("a strictly worse arm was marked as beating control")
	}
	if v.Winner != ArmControl {
		t.Errorf("winner = %q, want %s (the higher correct_diagnosis)", v.Winner, ArmControl)
	}
	if txt := Render(v); !strings.Contains(txt, "Δ vs control") || !strings.Contains(txt, "MEASURED") {
		t.Errorf("a measured run must render its delta table:\n%s", txt)
	}
}

// ★ A DISTINCT PREFLIGHT IS PERMISSION TO MEASURE, NOT A MEASUREMENT. The preflight costs three one-token
// completions and answers only "can these arms differ" — if its report said "MEASURED" beside a table of
// zeros, it would claim the tier question was settled by three trivial calls. That is the same class of lie
// as eval/gate certifying a capability it never exercised (TG-258), and it is worse here because the
// preflight is the CHEAP path everyone will actually run.
//
// KILLING MUTATION (executed 2026-08-04): remove the `v.Outcome == OutcomeMeasured && v.Preflight` branch in
// Render so a distinct preflight falls through to the measured headline. RED — "a preflight over EMPTY
// scorecards rendered the measured-run headline and a Δ table of zeros: three one-token completions cannot
// answer whether the reasoning tier buys diagnosis quality".
func TestADistinctPreflightDoesNotClaimTheTierQuestionWasAnswered(t *testing.T) {
	// No scorecards at all — a preflight scores nothing.
	arms := []Arm{
		{Name: ArmControl, InvestigateTier: "arm-haiku", DecideTier: "arm-opus",
			Calls: []Call{{ServedModel: "claude-haiku-4-5-20251001"}}},
		{Name: ArmStrong, InvestigateTier: "arm-opus", DecideTier: "arm-opus",
			Calls: []Call{{ServedModel: "claude-opus-5"}}},
	}
	v := Distinctness(arms)
	if v.Outcome != OutcomeMeasured || !v.Preflight {
		t.Fatalf("distinct arms preflight: outcome=%s preflight=%v, want measured/true", v.Outcome, v.Preflight)
	}
	if len(v.Deltas) != 0 || v.Winner != "" {
		t.Fatalf("a preflight computed deltas (%d) / named a winner (%q) — it ran no corpus", len(v.Deltas), v.Winner)
	}
	txt := Render(v)
	if strings.Contains(txt, "TIER-AB: MEASURED") || strings.Contains(txt, "Δ vs control") {
		t.Fatalf("a preflight over EMPTY scorecards rendered the measured-run headline and a Δ table of zeros: "+
			"three one-token completions cannot answer whether the reasoning tier buys diagnosis quality:\n%s", txt)
	}
	for _, want := range []string{"PREFLIGHT", "ARMS ARE DISTINCT", "Nothing about diagnosis"} {
		if !strings.Contains(txt, want) {
			t.Errorf("the preflight report must say %q:\n%s", want, txt)
		}
	}
}

// A COLLAPSED preflight is the cheap refutation the whole ordering exists for: it must reach the same
// verdict as the full run, from three completions instead of three corpus passes.
func TestACollapsedPreflightRefutesTheExperimentBeforeAnyCorpusRuns(t *testing.T) {
	arms := []Arm{
		{Name: ArmControl, InvestigateTier: "fast", DecideTier: "primary", Calls: []Call{{ServedModel: "claude-opus-5"}}},
		{Name: ArmStrong, InvestigateTier: "primary", DecideTier: "primary", Calls: []Call{{ServedModel: "claude-opus-5"}}},
		{Name: ArmCheap, InvestigateTier: "fast", DecideTier: "fast", Calls: []Call{{ServedModel: "claude-opus-5"}}},
	}
	v := Distinctness(arms)
	if v.Outcome != OutcomeCollapsed {
		t.Fatalf("the preflight over TG-204's literal arms (all served claude-opus-5) returned %s — the cheap "+
			"refutation failed and three corpus passes would have been burned to reach the same answer", v.Outcome)
	}
	if len(v.Collapsed) != 1 || len(v.Collapsed[0]) != 3 {
		t.Fatalf("the collapse group must name all three arms, got %v", v.Collapsed)
	}
}

// ★ A STARVED CALLER FILTER MUST BE DIAGNOSED, NOT JUST FAILED. Measured live 2026-08-04: LiteLLM drops the
// OpenAI `user` field before an openai/-provider upstream, so the tg-claude-proxy logged caller="" for all
// four preflight probes and the "runner:" prefix starved every arm to zero calls. The harness correctly
// reported TIER-AB: UNKNOWN — and named no cause, which sends the operator to debug the proxy instead of the
// filter. CallerFilterStarved is the difference.
//
// It must NOT widen the filter: falling back to "attribute everything" would fold the judge's primary-tier
// calls into every arm at the exact moment nobody is watching.
func TestAStarvedCallerFilterIsDiagnosedAndNeverWidened(t *testing.T) {
	w := Window{Start: at("2026-08-04T10:00:00Z"), End: at("2026-08-04T10:30:00Z")}
	arms := []Arm{
		{Name: ArmControl, Window: w, CallerPrefix: AgentCallerPrefix},
		{Name: ArmStrong, Window: Window{Start: at("2026-08-04T11:00:00Z"), End: at("2026-08-04T11:30:00Z")}, CallerPrefix: AgentCallerPrefix},
	}
	// In-window calls exist for ARM-CONTROL, but the gateway stripped `user` so caller is empty.
	calls := []Call{{At: at("2026-08-04T10:05:00Z"), ServedModel: "claude-opus-5", Caller: ""}}

	out, _ := AttributeCalls(arms, calls)
	if len(out[0].Calls) != 0 {
		t.Fatal("the caller filter WIDENED to admit an empty caller — a silent fallback would fold the judge's " +
			"primary-tier calls into every arm exactly when it is least noticeable")
	}
	starved := CallerFilterStarved(out, calls)
	if len(starved) != 1 || starved[0] != ArmControl {
		t.Fatalf("CallerFilterStarved = %v, want [%s]: an arm with in-window traffic and zero attributed calls "+
			"is the LiteLLM user-stripping signature, and an operator who is not told that debugs the proxy instead",
			starved, ArmControl)
	}
	// ARM-STRONG had no in-window traffic at all — a genuinely idle window is NOT a starved filter, and
	// reporting it as one would send the operator chasing a cause that is not there.
	if len(starved) > 1 {
		t.Errorf("an arm with no in-window traffic was reported as filter-starved: %v", starved)
	}
}

// ★ REGRESSION, FOUND BY RUNNING IT (2026-08-04). The driver recorded arm windows with `date +%…%SZ` and
// the harness with Go's time.RFC3339 — both TRUNCATE to the whole second. A truncated END narrows the
// window, and an arm's LAST call is the one that lands closest to it. Live preflight, real numbers:
// ARM-STRONG's only completion was logged at 22:00:37.111 against an end of 22:00:37.000 and was dropped;
// ARM-CHEAP's at 22:00:39.744 against 22:00:39.000, dropped. Two of three arms reported UNKNOWN with their
// calls sitting in the log — and the discarded call is preferentially the DECIDE-tier one, which is the
// tier TG-204 is actually asking about.
//
// The fix is sub-second boundaries at every writer (eval/phase.json uses RFC3339Nano; the shell uses %3N).
// This test pins the CONSUMER's half: a window whose end carries fractional precision must keep the call
// that lands inside it.
//
// KILLING MUTATION (executed 2026-08-04): truncate both bounds in Contains
// (`w.Start.Truncate(time.Second)` / `w.End.Truncate(time.Second)`). RED — "a call at
// 2026-08-04T22:00:37.111Z was dropped from a window ending 2026-08-04T22:00:37.5Z: whole-second truncation
// discards each arm's last call, which is its decide-tier call".
func TestSubSecondWindowBoundariesKeepEachArmsLastCall(t *testing.T) {
	w := Window{Start: at("2026-08-04T22:00:32.000Z"), End: at("2026-08-04T22:00:37.500Z")}
	last := at("2026-08-04T22:00:37.111Z") // the live ARM-STRONG completion that whole-second ends dropped
	if !w.Contains(last) {
		t.Fatalf("a call at %s was dropped from a window ending %s: whole-second truncation discards each arm's "+
			"last call, which is its decide-tier call",
			last.Format(time.RFC3339Nano), w.End.Format(time.RFC3339Nano))
	}
	arms := []Arm{{Name: ArmStrong, Window: w, Card: card(8, 4.4, 4.3, 3.9, 1.9)}}
	out, unattributed := AttributeCalls(arms, []Call{{At: last, ServedModel: "claude-opus-5", DurationMs: 2231, CostUSD: 0.000875}})
	if ArmSignature(out[0]) != "claude-opus-5" || unattributed != 0 {
		t.Fatalf("the arm's only call was not attributed (signature=%q unattributed=%d) — this is the live "+
			"failure where two of three arms read UNKNOWN with their calls in the log",
			ArmSignature(out[0]), unattributed)
	}
	// And a call genuinely past the end is still excluded — the fix must not become "attribute everything".
	if w.Contains(at("2026-08-04T22:00:37.501Z")) {
		t.Error("a call after the window end was admitted — the boundary must still bound")
	}
}

// The gated axis must be the RUBRIC's, never a re-declared literal that can drift from core/judge — the
// same discipline eval/gate.Dimensions follows.
func TestTheGatedAxisIsSourcedFromTheRubric(t *testing.T) {
	if GateDim != "correct_diagnosis" {
		t.Fatalf("GateDim = %q, want correct_diagnosis (TG-204 gates on it)", GateDim)
	}
	if GateDim != judge.Dimensions[0] {
		t.Fatalf("GateDim %q is not the rubric's first dimension %q — a re-declared copy has drifted", GateDim, judge.Dimensions[0])
	}
	if DiagDim != judge.DimDiagnosisGrounded {
		t.Fatalf("DiagDim %q != judge.DimDiagnosisGrounded %q", DiagDim, judge.DimDiagnosisGrounded)
	}
}

// A run missing ARM-CONTROL has no reference for any delta TG-204 defines; it must not silently pick one.
func TestARunWithoutTheControlArmIsNotAMeasurement(t *testing.T) {
	arms := []Arm{
		armWith(ArmStrong, "arm-opus", "arm-opus", card(8, 4.4, 4.3, 3.9, 1.9), "claude-opus-5"),
		armWith(ArmCheap, "arm-haiku", "arm-haiku", card(8, 3.1, 3.4, 3.0, 3.4), "claude-haiku-4-5-20251001"),
	}
	v := Compare(arms)
	if v.Measured() {
		t.Fatal("a run with no ARM-CONTROL was certified — every TG-204 delta is defined against control")
	}
}
