package syslogng

// THE PER-SESSION SEARCH BUDGET (TG-297).
//
// search-host-logs takes a caller-chosen fixed string and answers "is this in the device's syslog?".
// Every bound the tool carried was PER INVOCATION — a `grep -m` match cap, a byte cap, a line cap, a
// context deadline — and toolBox held no state at all, so nothing bounded how many times ONE
// investigation could ask. That is a confirmation oracle over the log's contents, and the anti-thrash
// veto that halts a repeating agent could not help: TrajectoryVeto keys on tool+ArgsKey
// (agent/trajectory.go), so a fresh `pattern` is a fresh step and the veto never binds.
//
// These oracles hold the four properties that make the cap real rather than decorative: it BINDS, it
// cannot be bought off by varying the pattern, it REFUSES out loud instead of returning an empty result,
// and a refusal is VISIBLE to the yield register. Each names the killing mutation that reds it.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/agent"
)

// hitRunner answers every read with a match, so nothing but the budget can stop a search.
func hitRunner() *fakeRunner {
	return &fakeRunner{result: RunResult{ExitCode: 0, Stdout: []byte("Jul 15 12:00:02 dc1fw01 %ASA-6-302014: Teardown TCP connection 1\n")}}
}

// searchWith builds the capped search tool. It fails rather than returning nil: a nil tool would make
// every assertion below vacuous.
func searchWith(t *testing.T, r Runner, opts ...Option) agent.Tool {
	t.Helper()
	tl := findTool(NewTools(testServers(), r, opts...), "search-host-logs")
	if tl == nil {
		t.Fatal("vacuity floor: search-host-logs is not registered, so a cap on it certifies nothing")
	}
	return tl
}

// boxOf reaches the shared seam so the clock can be swapped (the TTL oracle) without sleeping an hour.
func boxOf(t *testing.T, tl agent.Tool) *toolBox {
	t.Helper()
	s, ok := tl.(searchHostLogsTool)
	if !ok {
		t.Fatalf("search-host-logs is %T, not the tool that carries the budget", tl)
	}
	return s.b
}

// investigation stamps a session id the way Agent.Run does.
func investigation(id string) context.Context { return agent.WithSession(context.Background(), id) }

// searchFor is one probe of the device's log.
func searchFor(pattern string) map[string]string {
	return map[string]string{"host": "dc1fw01", "pattern": pattern}
}

// namesTheBound asserts a refusal tells the reader WHICH bound stopped it and WHICH knob moves it. A
// refusal that says only "no" leaves the operator unable to tell a cap from an outage.
func namesTheBound(t *testing.T, out string, cap int) {
	t.Helper()
	if !strings.Contains(out, strconv.Itoa(cap)) {
		t.Errorf("the refusal never names the cap it hit (want the number %d in it), got %q", cap, out)
	}
	if !strings.Contains(out, SearchSessionCapEnv) {
		t.Errorf("the refusal never names %s, so nobody reading it knows which knob raises the bound: %q", SearchSessionCapEnv, out)
	}
}

// ---- the bound itself ----

// KILLING MUTATION: delete the `if allowed, spent, capN := t.b.chargeSearch(...); !allowed { ... }` block
// from searchHostLogsTool.Invoke. RED — the 4th search of a 3-call budget reaches the log, and with it the
// 40th and the 400th: one investigation can ask the device's syslog an unbounded number of yes/no
// questions, which is the confirmation oracle TG-297 reported.
func TestOneInvestigationCannotSearchPastItsCap(t *testing.T) {
	f := hitRunner()
	tl := searchWith(t, f, WithSearchSessionCap(3))
	ctx := investigation("incident-a")

	for i := 1; i <= 3; i++ {
		res, err := tl.Invoke(ctx, searchFor(fmt.Sprintf("probe-%d", i)))
		if err != nil || !res.Success {
			t.Fatalf("search %d of a 3-call budget must be allowed, got success=%v err=%v output=%q", i, res.Success, err, res.Output)
		}
	}
	if f.calls != 3 {
		t.Fatalf("the budget let %d reads reach the log, want 3 — a cap that does not bound READS bounds nothing", f.calls)
	}

	res, err := tl.Invoke(ctx, searchFor("probe-4"))
	if err != nil {
		t.Fatalf("a spent budget must refuse honestly, never abort the session with a Go error: %v", err)
	}
	if res.Success {
		t.Fatal("the 4th search of a 3-call budget SUCCEEDED: the per-session cap does not bind, so one " +
			"investigation can enumerate the device's log without limit")
	}
	if f.calls != 3 {
		t.Fatalf("a refused search still reached the runner (calls=%d, want 3): the refusal must land BEFORE "+
			"the read, or the oracle is answered anyway and only the answer is withheld", f.calls)
	}
	namesTheBound(t, res.Output, 3)
}

// KILLING MUTATION: key the counter on the pattern (or on agent.ArgsKey(args)) instead of the session. RED —
// this is the precise evasion TG-297 describes: a fresh pattern is a fresh ArgsKey, which is why the
// anti-thrash veto never binds, and a per-pattern counter would inherit exactly that hole.
func TestVaryingThePatternBuysNoExtraSearches(t *testing.T) {
	f := hitRunner()
	tl := searchWith(t, f, WithSearchSessionCap(5))
	ctx := investigation("incident-enumerator")

	// 200 DISTINCT probes — the enumeration an oracle actually performs, not a repeated call.
	allowed := 0
	for i := 0; i < 200; i++ {
		if res, _ := tl.Invoke(ctx, searchFor(fmt.Sprintf("secret-candidate-%03d", i))); res.Success {
			allowed++
		}
	}
	if allowed != 5 || f.calls != 5 {
		t.Fatalf("200 distinct patterns bought %d successful searches and %d reads of the log, want 5 and 5: "+
			"varying the pattern still evades the bound", allowed, f.calls)
	}
}

// KILLING MUTATION: key the counter on a constant (e.g. always ""), so all sessions share one bucket. RED —
// the next incident of the day inherits a spent budget and is refused its very first search, which turns a
// safety bound into an outage.
func TestEachInvestigationGetsItsOwnBudget(t *testing.T) {
	f := hitRunner()
	tl := searchWith(t, f, WithSearchSessionCap(2))

	first := investigation("incident-a")
	for i := 0; i < 2; i++ {
		if res, _ := tl.Invoke(first, searchFor(fmt.Sprintf("a-%d", i))); !res.Success {
			t.Fatalf("incident-a search %d must be inside its own budget", i)
		}
	}
	if res, _ := tl.Invoke(first, searchFor("a-over")); res.Success {
		t.Fatal("vacuity floor: incident-a was not actually exhausted, so the isolation check below proves nothing")
	}

	second := investigation("incident-b")
	res, _ := tl.Invoke(second, searchFor("b-0"))
	if !res.Success {
		t.Fatalf("a NEW investigation was refused its first search because an earlier one spent the budget: %q", res.Output)
	}
}

// The cap bounds the ORACLE, not the whole lane. get-host-logs returns a bounded window whose contents the
// caller did not choose, so it is not a confirmation channel; capping it too would take the device-log
// window away from triage — the exact blindness SeamSyslogRead's Consequence describes — while doing
// nothing about the enumeration TG-297 is about.
func TestTheSearchBudgetDoesNotBoundGetHostLogs(t *testing.T) {
	f := hitRunner()
	tools := NewTools(testServers(), f, WithSearchSessionCap(1))
	search, logs := findTool(tools, "search-host-logs"), findTool(tools, "get-host-logs")
	if search == nil || logs == nil {
		t.Fatal("vacuity floor: both tools must exist for this to mean anything")
	}
	ctx := investigation("incident-a")

	if res, _ := search.Invoke(ctx, searchFor("burn-the-budget")); !res.Success {
		t.Fatal("vacuity floor: the single search was refused, so the budget was never spent")
	}
	if res, _ := search.Invoke(ctx, searchFor("second")); res.Success {
		t.Fatal("vacuity floor: the search budget is not actually spent")
	}
	for i := 0; i < 5; i++ {
		res, err := logs.Invoke(ctx, map[string]string{"host": "dc1fw01"})
		if err != nil || !res.Success {
			t.Fatalf("get-host-logs read %d was blocked by the SEARCH budget (%q): triage loses the device-log "+
				"window entirely, which is a bigger hole than the one being closed", i, res.Output)
		}
	}
}

// ---- the refusal must not read as "no matches" ----

// KILLING MUTATION: make the spent-budget path return `res.Success = true` with an empty/"no lines matching"
// output instead of a refusal. RED — and this is the whole reason the ticket specifies a refusal: "the log
// has no such line" and "I never looked" are the same sentence to the agent, so it concludes the error is
// absent from the log and proposes on that. A silent empty result is indistinguishable from evidence.
func TestASpentBudgetRefusesInsteadOfLookingLikeNoMatches(t *testing.T) {
	// The HONEST zero-match answer, taken from the tool itself rather than hard-coded, so this comparison
	// keeps meaning if that wording is ever reworded.
	quiet := &fakeRunner{result: RunResult{ExitCode: 1}} // grep exit 1 = ran, matched nothing
	honest, _ := searchWith(t, quiet, WithSearchSessionCap(1)).Invoke(investigation("quiet"), searchFor("NEVERMATCHES"))
	if !honest.Success || !strings.Contains(honest.Output, "no lines matching") {
		t.Fatalf("vacuity floor: the genuine no-match answer is %q (success=%v) — not the shape this test "+
			"compares against, so the comparison below certifies nothing", honest.Output, honest.Success)
	}

	f := hitRunner()
	tl := searchWith(t, f, WithSearchSessionCap(1))
	ctx := investigation("incident-a")
	if res, _ := tl.Invoke(ctx, searchFor("first")); !res.Success {
		t.Fatal("vacuity floor: the first search was refused, so the budget was never spent")
	}
	refused, _ := tl.Invoke(ctx, searchFor("second"))

	if refused.Success {
		t.Fatal("a budget refusal reports Success=true: to the agent that is a search that RAN, and a caller " +
			"cannot tell it from a log that genuinely holds no such line")
	}
	if strings.TrimSpace(refused.Output) == "" {
		t.Fatal("a budget refusal returned an EMPTY output — silence the agent will read as absence of evidence")
	}
	if strings.Contains(refused.Output, "no lines matching") || refused.Output == honest.Output {
		t.Fatalf("a budget refusal is worded like a genuine zero-match answer (%q): the agent will conclude the "+
			"pattern is not in the log and reason from a search that never ran", refused.Output)
	}
	if !strings.Contains(refused.Output, "refused") {
		t.Errorf("a budget refusal must say so in the word the other refusal paths use, got %q", refused.Output)
	}
	namesTheBound(t, refused.Output, 1)
}

// ---- visibility: a cap that eats reads must not be invisible ----

// KILLING MUTATION: report produced=true on the budget-refusal path (or drop the yield call from it). RED —
// a refusal returns a perfectly well-formed string, so any check that COUNTS INVOCATIONS sees a busy,
// healthy lane while every search in the investigation is being told no. Only the offered/produced pair
// separates a lane that is answering from one that is only replying (core/wiring/yield.go, TG-271).
func TestARefusedSearchIsReportedAsAReadThatProducedNothing(t *testing.T) {
	var mu sync.Mutex
	var offered, produced int
	observe := func(p bool) {
		mu.Lock()
		defer mu.Unlock()
		offered++
		if p {
			produced++
		}
	}

	f := hitRunner()
	tl := searchWith(t, f, WithSearchSessionCap(2), WithYield(observe))
	ctx := investigation("incident-a")
	for i := 0; i < 5; i++ { // two inside the budget, three refused by it
		tl.Invoke(ctx, searchFor(fmt.Sprintf("probe-%d", i)))
	}

	mu.Lock()
	defer mu.Unlock()
	if offered != 5 {
		t.Fatalf("the register saw %d attempted reads, want 5: a refusal that is not even OFFERED makes the "+
			"cap invisible in the other direction — the lane looks quiet rather than starved", offered)
	}
	if produced != 2 {
		t.Fatalf("the register was told %d reads produced, want 2 (only the two that reached the log): a "+
			"budget refusal counted as PRODUCED reports a flowing lane while the investigation is blind", produced)
	}
}

// The control that stops the alarm becoming wallpaper: a lane inside its budget must report as fully
// producing, including an honest zero-match grep. An alarm that fires on a healthy lane gets muted.
func TestAnUnrefusedSearchLaneReportsAsProducing(t *testing.T) {
	var offered, produced int
	quiet := &fakeRunner{result: RunResult{ExitCode: 1}} // ran, matched nothing — still a grounded observation
	tl := searchWith(t, quiet, WithSearchSessionCap(4), WithYield(func(p bool) {
		offered++
		if p {
			produced++
		}
	}))
	ctx := investigation("incident-a")
	for i := 0; i < 3; i++ {
		tl.Invoke(ctx, searchFor(fmt.Sprintf("probe-%d", i)))
	}
	if offered != 3 || produced != 3 {
		t.Fatalf("offered=%d produced=%d, want 3/3: a search that reached the log and matched nothing HAS "+
			"produced, and counting it as starved would alarm on every quiet device", offered, produced)
	}
}

// ---- configuration: there is no value that disables the bound ----

// KILLING MUTATION: let WithSearchSessionCap(0) (or NewTools' floor) leave searchCap at 0 and treat
// non-positive as "unlimited". RED — a blanked or fat-fingered config key would then silently remove the
// bound, which is this repo's most-repeated failure: a gate that stops binding and says nothing.
func TestANonPositiveCapRestoresTheDefaultRatherThanRemovingTheBound(t *testing.T) {
	for _, n := range []int{0, -1, -1000} {
		f := hitRunner()
		tl := searchWith(t, f, WithSearchSessionCap(n))
		if got := boxOf(t, tl).searchCap; got != DefaultSearchSessionCap {
			t.Fatalf("WithSearchSessionCap(%d) left the cap at %d, want the default %d — a config slip must "+
				"restore the sane bound, never remove it", n, got, DefaultSearchSessionCap)
		}
		// And behaviourally: a zero cap that were taken literally would refuse the FIRST search, which
		// looks exactly like an outage. The default must be in force instead.
		if res, _ := tl.Invoke(investigation("incident-a"), searchFor("first")); !res.Success {
			t.Fatalf("WithSearchSessionCap(%d) refused the first search (%q): a mis-set knob must not read as "+
				"a dead lane", n, res.Output)
		}
	}
}

func TestAPositiveCapIsHonoured(t *testing.T) {
	tl := searchWith(t, hitRunner(), WithSearchSessionCap(37))
	if got := boxOf(t, tl).searchCap; got != 37 {
		t.Fatalf("operator cap 37 did not bind (got %d): the console knob would read as set while every "+
			"read used the default, which is the TG-265 shape", got)
	}
}

// An unstamped context — any caller that reaches the tool without going through Agent.Run — must still be
// bounded. Every unstamped caller shares ONE bucket, which over-binds LOUDLY (the tool refuses and says
// why) rather than silently never binding: if the stamp is ever dropped from Run, this fails visibly
// instead of quietly restoring the unbounded oracle.
func TestAnUnstampedContextIsStillBounded(t *testing.T) {
	f := hitRunner()
	tl := searchWith(t, f, WithSearchSessionCap(2))
	var lastRefusal string
	allowed := 0
	for i := 0; i < 6; i++ {
		res, _ := tl.Invoke(context.Background(), searchFor(fmt.Sprintf("probe-%d", i)))
		if res.Success {
			allowed++
		} else {
			lastRefusal = res.Output
		}
	}
	if allowed != 2 {
		t.Fatalf("an unstamped caller made %d searches against a cap of 2: dropping the session stamp must "+
			"over-bind, never unbind", allowed)
	}
	namesTheBound(t, lastRefusal, 2)
}

// ---- the TTL sweep must never hand a live investigation a fresh budget ----

// KILLING MUTATION: move sweepLocked out of the `!known` branch so it runs on EVERY charge, and shorten
// searchSessionTTL below a session's length. RED in spirit and here directly: a sweep that can drop a row
// for a session that is still spending restores its budget mid-investigation — a bound that silently
// stops binding, which is worse than no bound because the boot log still advertises it.
func TestASweepNeverResetsALiveInvestigationsBudget(t *testing.T) {
	f := hitRunner()
	tl := searchWith(t, f, WithSearchSessionCap(2))
	b := boxOf(t, tl)
	clock := time.Now().UTC()
	b.now = func() time.Time { return clock }

	live := investigation("incident-live")
	for i := 0; i < 2; i++ {
		if res, _ := tl.Invoke(live, searchFor(fmt.Sprintf("live-%d", i))); !res.Success {
			t.Fatalf("vacuity floor: live search %d was refused, so no budget was ever spent", i)
		}
	}

	// A NEW investigation arrives — the only thing that triggers a sweep — while the first is still live.
	clock = clock.Add(30 * time.Minute) // well inside searchSessionTTL
	if res, _ := tl.Invoke(investigation("incident-other"), searchFor("other-0")); !res.Success {
		t.Fatalf("vacuity floor: the arriving session was refused (%q), so no sweep was triggered", res.Output)
	}
	if res, _ := tl.Invoke(live, searchFor("live-after-sweep")); res.Success {
		t.Fatal("a live investigation's spent budget was restored by another session's arrival: the cap " +
			"stops binding for exactly the session that already exhausted it")
	}
}

// The other half of the TTL: finished sessions must not accumulate forever. The worker runs for weeks, and
// one map row per investigation retained for the life of the process is a leak that nothing would report.
func TestFinishedSessionsAreEventuallyForgotten(t *testing.T) {
	f := hitRunner()
	tl := searchWith(t, f, WithSearchSessionCap(2))
	b := boxOf(t, tl)
	clock := time.Now().UTC()
	b.now = func() time.Time { return clock }

	for i := 0; i < 25; i++ {
		tl.Invoke(investigation(fmt.Sprintf("old-incident-%d", i)), searchFor("probe"))
	}
	b.mu.Lock()
	before := len(b.spend)
	b.mu.Unlock()
	if before != 25 {
		t.Fatalf("vacuity floor: %d sessions were tracked, want 25 — the growth this test bounds is not "+
			"happening, so its shrink assertion proves nothing", before)
	}

	clock = clock.Add(2 * searchSessionTTL)
	tl.Invoke(investigation("fresh-incident"), searchFor("probe")) // a new arrival sweeps

	b.mu.Lock()
	after := len(b.spend)
	b.mu.Unlock()
	if after != 1 {
		t.Fatalf("after %v idle, %d session rows are still retained (want just the fresh one): the budget "+
			"map grows without bound for the life of the worker", 2*searchSessionTTL, after)
	}
}

// ---- concurrency ----

// lockedRunner is fakeRunner's concurrency-safe sibling. The shared-counter bug this oracle exists to
// catch would otherwise be masked by a data race in the fixture itself under `go test -race`.
type lockedRunner struct {
	mu    sync.Mutex
	calls int
}

func (r *lockedRunner) Run(context.Context, Server, []string) (RunResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return RunResult{ExitCode: 0, Stdout: []byte("match\n")}, nil
}

// KILLING MUTATION: drop the mutex from chargeSearch (read the counter, then increment it unlocked). RED
// under -race, and wrong even without it: two cycles landing together each see "spent < cap" and both
// spend, so a concurrent agent buys extra reads precisely when it is asking fastest.
func TestConcurrentSearchesInOneInvestigationCannotOverspend(t *testing.T) {
	r := &lockedRunner{}
	tl := searchWith(t, r, WithSearchSessionCap(5))
	ctx := investigation("incident-a")

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tl.Invoke(ctx, searchFor(fmt.Sprintf("probe-%d", i)))
		}(i)
	}
	wg.Wait()

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls != 5 {
		t.Fatalf("50 concurrent searches on a 5-call budget reached the log %d times, want exactly 5", r.calls)
	}
}

// seqRunner answers each successive call from a scripted list, so a fallback can be observed as the
// SECOND call rather than inferred from the final output. It records every argv it was handed.
type seqRunner struct {
	results []RunResult
	argvs   [][]string
}

func (s *seqRunner) Run(_ context.Context, _ Server, argv []string) (RunResult, error) {
	s.argvs = append(s.argvs, append([]string(nil), argv...))
	if len(s.argvs)-1 < len(s.results) {
		return s.results[len(s.argvs)-1], nil
	}
	return s.results[len(s.results)-1], nil
}

// A DEFAULT READ MUST FALL BACK TO THE DATED FILE (TG-59).
//
// Sites disagree on whether a `today.log` current-file exists: measured live 2026-07-18, NL hosts have one
// and GR hosts keep only dated files (`<host>-YYYY-MM-DD.log`) — a per-site syslog-ng config difference,
// not a fault. The default read resolved to today.log alone, so it honestly failed on every GR host, and
// an operator reads that failure as "the device does not log here" when it logs perfectly well.
func TestDefaultReadFallsBackToTheDatedFileWhenTodayLogIsAbsent(t *testing.T) {
	r := &seqRunner{results: []RunResult{
		{ExitCode: 1, Stderr: []byte("tail: No such file")},                     // today.log missing (a GR host)
		{ExitCode: 0, Stdout: []byte("Jul 15 12:00:02 dc2fw01 %ASA-6-1\n")}, // the dated file answers
	}}
	tl := findTool(NewTools(testServers(), r), "get-host-logs")
	if tl == nil {
		t.Fatal("vacuity floor: get-host-logs is not registered, so this proves nothing")
	}
	res, err := tl.Invoke(context.Background(), map[string]string{"host": "dc1fw01"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if len(r.argvs) != 2 {
		t.Fatalf("runner was called %d time(s), want 2 — a missing today.log must be RETRIED against the "+
			"dated file, not reported as 'no log for that day'", len(r.argvs))
	}
	first, second := r.argvs[0][len(r.argvs[0])-1], r.argvs[1][len(r.argvs[1])-1]
	if !strings.HasSuffix(first, "/today.log") {
		t.Errorf("first attempt read %q, want today.log — the cheap current-file must still be tried first "+
			"so the sites that have it pay nothing for this fallback", first)
	}
	if strings.HasSuffix(second, "/today.log") || !strings.Contains(second, "-") {
		t.Errorf("second attempt read %q, want the dated <host>-YYYY-MM-DD.log path", second)
	}
	if !res.Success {
		t.Errorf("the dated file answered but the tool reported failure: %s", res.Output)
	}
}

// An EXPLICIT date must resolve to exactly ONE path. Falling back here would silently serve a different
// day than the caller asked for, which is a worse failure than an honest miss.
func TestAnExplicitDateNeverFallsBackToAnotherDay(t *testing.T) {
	r := &seqRunner{results: []RunResult{{ExitCode: 1, Stderr: []byte("tail: No such file")}}}
	tl := findTool(NewTools(testServers(), r), "get-host-logs")
	res, err := tl.Invoke(context.Background(), map[string]string{"host": "dc1fw01", "date": "2026-07-15"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if len(r.argvs) != 1 {
		t.Fatalf("runner was called %d time(s) for an EXPLICIT date, want 1 — a caller who named a day must "+
			"never be served a different one", len(r.argvs))
	}
	if res.Success {
		t.Error("an explicitly-requested missing day must report the honest miss, not a success")
	}
}

// THE SUBTLE ONE: for search, grep exit 1 means "the file WAS read and held no match". That is a real
// answer. Treating it as a missing file would re-run the search against a different file and could return
// matches from a day the caller did not ask about — a fallback turning a correct negative into a wrong
// positive.
func TestAZeroMatchSearchDoesNotFallThroughToAnotherFile(t *testing.T) {
	r := &seqRunner{results: []RunResult{{ExitCode: 1}}} // read fine, no match
	tl := findTool(NewTools(testServers(), r), "search-host-logs")
	res, err := tl.Invoke(context.Background(), map[string]string{"host": "dc1fw01", "pattern": "Teardown"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if len(r.argvs) != 1 {
		t.Fatalf("a zero-match grep (exit 1) triggered %d reads, want 1 — exit 1 means the file was READ "+
			"and matched nothing, which is an answer, not a missing file", len(r.argvs))
	}
	if !res.Success {
		t.Errorf("a zero-match search is a grounded observation and must succeed: %s", res.Output)
	}
}
