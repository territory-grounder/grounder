package openobserve

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/estate"
)

// fixtureGraph is the ticket's own worked example: an upstream switch (dc1sw01) with two downstream hosts
// that depend on it. correlate-logs on the switch must expand to BOTH downstream devices — the multi-host
// blast radius the single-host syslog tools cannot see.
func fixtureGraph() *estate.Graph {
	g := estate.NewGraph()
	sw := estate.Entity{Type: estate.TypeNetworkDevice, Name: "dc1sw01"}
	app1 := estate.Entity{Type: estate.TypeHost, Name: "dc1app01"}
	app2 := estate.Entity{Type: estate.TypeHost, Name: "dc1app02"}
	site := estate.Entity{Type: estate.TypeSite, Name: "nllei"}
	g.Upsert(estate.Edge{From: app1, To: sw, Rel: estate.RelDependsOn, Confidence: 0.90, Source: estate.SourceNetbox})
	g.Upsert(estate.Edge{From: app2, To: sw, Rel: estate.RelDependsOn, Confidence: 0.90, Source: estate.SourceNetbox})
	// A site membership: the correlation must NOT pull the whole site in as a "host".
	g.Upsert(estate.Edge{From: sw, To: site, Rel: estate.RelMemberOf, Confidence: 0.90, Source: estate.SourceNetbox})
	return g
}

func hit(host, message string, micros int64) map[string]any {
	return map[string]any{"_timestamp": micros, "host": host, "message": message}
}

func hitsBody(hits ...map[string]any) string {
	b, _ := json.Marshal(map[string]any{"took": 5, "total": len(hits), "hits": hits})
	return string(b)
}

func newToolFixture(t *testing.T, g *estate.Graph, opts ...CorrelateOption) (agent.Tool, *fakeDoer) {
	t.Helper()
	t.Setenv(readTokenEnv, ingestToken)
	f := &fakeDoer{}
	r := NewReader("https://openobserve.example/api/default", config.SecretRef("env:"+readTokenEnv), WithReaderHTTPClient(f))
	tools := NewCorrelateTools(r, func() *estate.Graph { return g }, opts...)
	if len(tools) != 1 {
		t.Fatalf("a configured reader + graph must yield exactly one tool, got %d", len(tools))
	}
	return tools[0], f
}

// TestCorrelateSpansBlastRadiusHosts is THE RED-PROVE of the correlation: a host with downstream dependents
// must produce a search across ALL of them AND host-attributed output from more than one host. A single-host
// result on a multi-host blast radius is the bug this tool exists to fix.
func TestCorrelateSpansBlastRadiusHosts(t *testing.T) {
	tool, f := newToolFixture(t, fixtureGraph())
	f.respRet = hitsBody(
		hit("dc1sw01", "%LINK-3-UPDOWN GigabitEthernet0/1 down", 1786000000000002),
		hit("dc1app01", "lost route to gateway", 1786000000000001),
		hit("dc1app02", "lost route to gateway", 1786000000000000),
	)
	res, err := tool.Invoke(context.Background(), map[string]string{"host": "dc1sw01"})
	if err != nil {
		t.Fatalf("Invoke must not return a Go error: %v", err)
	}
	if !res.Success {
		t.Fatalf("a successful correlation must be Success: %q", res.Output)
	}

	// (a) THE QUERY spanned the blast radius: the recorded SQL must name all three hosts. If expansion were
	// broken and only the incident host were searched, this fails — the red-prove.
	w := decodeSearchBody(t, f.reqs[0].body)
	for _, h := range []string{"dc1sw01", "dc1app01", "dc1app02"} {
		if !strings.Contains(w.Query.SQL, "'"+h+"'") {
			t.Errorf("SQL must correlate across %s (blast radius not expanded): %q", h, w.Query.SQL)
		}
	}
	// The site membership must NOT have been pulled in as a host.
	if strings.Contains(w.Query.SQL, "'nllei'") {
		t.Errorf("a member_of site must not be searched as a host: %q", w.Query.SQL)
	}

	// (b) THE OUTPUT is attributed to more than one host.
	if !strings.Contains(res.Output, "[dc1app01]") || !strings.Contains(res.Output, "[dc1app02]") {
		t.Errorf("output must attribute lines to the downstream hosts, got:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "from 3 host(s)") {
		t.Errorf("output must report the distinct-host count, got:\n%s", res.Output)
	}
	// the header names the full host set it queried, incident host first (insertion order).
	if !strings.Contains(res.Output, "[dc1sw01, dc1app01, dc1app02]") {
		t.Errorf("output header must name the queried host set (incident first), got:\n%s", res.Output)
	}
}

// TestReadErrorIsHonestNotEmpty: a failed READ must return Success=false and say plainly it is NOT "no logs",
// so a triage agent cannot read it as an empty window and conclude the fault is not in the logs.
func TestReadErrorIsHonestNotEmpty(t *testing.T) {
	tool, f := newToolFixture(t, fixtureGraph())
	f.status = 503
	f.respRet = "upstream down"
	res, err := tool.Invoke(context.Background(), map[string]string{"host": "dc1sw01"})
	if err != nil {
		t.Fatalf("Invoke must not surface a Go error: %v", err)
	}
	if res.Success {
		t.Fatalf("a failed read must not be Success: %q", res.Output)
	}
	if !strings.Contains(res.Output, "could not correlate") || !strings.Contains(res.Output, "NOT") || !strings.Contains(res.Output, "no logs") {
		t.Errorf("a read failure must be named as one, not as an absence of logs, got: %q", res.Output)
	}
	// The killing property: a read error must NOT render like a real empty result.
	if strings.Contains(res.Output, "NO matching log lines") {
		t.Errorf("a read error masqueraded as an empty result: %q", res.Output)
	}
}

// TestEmptyResultIsDistinctFromReadError: a SUCCEEDED read that matched nothing is a grounded observation,
// Success=true, and worded so it cannot be confused with the read-failure case above.
func TestEmptyResultIsDistinctFromReadError(t *testing.T) {
	tool, f := newToolFixture(t, fixtureGraph())
	f.respRet = hitsBody() // 200, zero hits
	res, err := tool.Invoke(context.Background(), map[string]string{"host": "dc1sw01"})
	if err != nil {
		t.Fatalf("Invoke error: %v", err)
	}
	if !res.Success {
		t.Fatalf("a successful empty read must be Success: %q", res.Output)
	}
	if !strings.Contains(res.Output, "NO matching log lines") || !strings.Contains(res.Output, "real empty result") {
		t.Errorf("an empty result must say the read SUCCEEDED and found nothing, got: %q", res.Output)
	}
	if strings.Contains(res.Output, "could not correlate") {
		t.Errorf("an empty result must not read like a failure: %q", res.Output)
	}
}

// TestCapTruncatesAndSaysSo: past the hit cap the tool truncates and says so.
func TestCapTruncatesAndSaysSo(t *testing.T) {
	tool, f := newToolFixture(t, fixtureGraph(), withCorrelateMaxHits(2))
	f.respRet = hitsBody(
		hit("dc1app01", "one", 3),
		hit("dc1app01", "two", 2),
		hit("dc1app02", "three", 1),
	)
	res, err := tool.Invoke(context.Background(), map[string]string{"host": "dc1sw01"})
	if err != nil {
		t.Fatalf("Invoke error: %v", err)
	}
	if !strings.Contains(res.Output, "truncated to the response cap") {
		t.Errorf("a capped result must say it truncated, got: %q", res.Output)
	}
}

// TestMaliciousPatternIsEscapedIntoTheQuery: an alert-derived pattern with SQL metacharacters reaches the
// search body as a doubled-quote literal, never as query structure (the tool-level half of the injection lock).
func TestMaliciousPatternIsEscapedIntoTheQuery(t *testing.T) {
	tool, f := newToolFixture(t, fixtureGraph())
	f.respRet = hitsBody()
	_, err := tool.Invoke(context.Background(), map[string]string{"host": "dc1sw01", "pattern": "x') OR ('1'='1"})
	if err != nil {
		t.Fatalf("Invoke error: %v", err)
	}
	w := decodeSearchBody(t, f.reqs[0].body)
	if !strings.Contains(w.Query.SQL, `match_all('x'') OR (''1''=''1')`) {
		t.Errorf("the alert pattern must be escaped into a string literal, got: %q", w.Query.SQL)
	}
}

// TestUnconfiguredLeavesToolUnregistered: no reader or no graph ⇒ no tool, so an absent OpenObserve leaves
// the tool structurally unregistered rather than live-but-erroring.
func TestUnconfiguredLeavesToolUnregistered(t *testing.T) {
	g := func() *estate.Graph { return fixtureGraph() }
	if got := NewCorrelateTools(NewReader("", config.SecretRef("env:X")), g); got != nil {
		t.Errorf("no reader ⇒ no tools, got %d", len(got))
	}
	if got := NewCorrelateTools(NewReader("https://x/api/default", config.SecretRef("env:X")), nil); got != nil {
		t.Errorf("no graph ⇒ no tools, got %d", len(got))
	}
	// End to end through a ToolSet: an unconfigured connector leaves correlate-logs absent from the agent's set.
	ts := agent.NewReadOnlyToolSet()
	for _, tl := range NewCorrelateTools(NewReader("", config.SecretRef("env:X")), g) {
		_ = ts.Register(tl)
	}
	if _, ok := ts.Get("correlate-logs"); ok {
		t.Error("correlate-logs must not be registered when OpenObserve is unconfigured")
	}
}

// TestCorrelateToolIsReadOnlyAndRegisters: the tool is read-only (the ToolSet refuses a write tool) and
// registers under its name.
func TestCorrelateToolIsReadOnlyAndRegisters(t *testing.T) {
	tool, _ := newToolFixture(t, fixtureGraph())
	if !tool.ReadOnly() {
		t.Fatal("correlate-logs must be read-only")
	}
	ts := agent.NewReadOnlyToolSet()
	if err := ts.Register(tool); err != nil {
		t.Fatalf("read-only tool must register: %v", err)
	}
	if _, ok := ts.Get("correlate-logs"); !ok {
		t.Fatal("correlate-logs must be registered")
	}
}

// TestSessionCapRefusesAndSaysSo: past the per-session cap the tool REFUSES and says so — never an empty
// result. A different pattern is a different step, so only a session bound catches enumeration.
func TestSessionCapRefusesAndSaysSo(t *testing.T) {
	tool, f := newToolFixture(t, fixtureGraph(), WithCorrelateSessionCap(1))
	f.respRet = hitsBody(hit("dc1app01", "x", 1))
	ctx := context.Background()

	if res, _ := tool.Invoke(ctx, map[string]string{"host": "dc1sw01"}); !res.Success {
		t.Fatalf("the first correlation must succeed: %q", res.Output)
	}
	res, _ := tool.Invoke(ctx, map[string]string{"host": "dc1sw01"})
	if res.Success {
		t.Fatalf("the second correlation past the cap must refuse: %q", res.Output)
	}
	if !strings.Contains(res.Output, "REFUSAL") || !strings.Contains(res.Output, "per-session cap") {
		t.Errorf("a spent budget must name the refusal, not read as empty: %q", res.Output)
	}
}

// TestUnknownHostSearchesItAloneWithANote: a host the graph does not know is still searched alone, with the
// reason stated — one host's logs beat none, and a silent single-host search would be the empty-vs-broken trap.
func TestUnknownHostSearchesItAloneWithANote(t *testing.T) {
	tool, f := newToolFixture(t, estate.NewGraph()) // empty graph
	f.respRet = hitsBody(hit("dc1xyz01", "boot", 1))
	res, err := tool.Invoke(context.Background(), map[string]string{"host": "dc1xyz01"})
	if err != nil {
		t.Fatalf("Invoke error: %v", err)
	}
	if !res.Success {
		t.Fatalf("a single-host correlation must still succeed: %q", res.Output)
	}
	w := decodeSearchBody(t, f.reqs[0].body)
	if !strings.Contains(w.Query.SQL, "'dc1xyz01'") {
		t.Errorf("the named host must still be searched, got: %q", w.Query.SQL)
	}
	if !strings.Contains(res.Output, "empty") && !strings.Contains(res.Output, "only") {
		t.Errorf("the output must state the blast radius could not be expanded, got: %q", res.Output)
	}
}

// TestInvalidHostRefused: a host name outside the allowlist is refused before any read.
func TestInvalidHostRefused(t *testing.T) {
	tool, f := newToolFixture(t, fixtureGraph())
	res, _ := tool.Invoke(context.Background(), map[string]string{"host": "a'b;drop"})
	if res.Success {
		t.Fatalf("a host with a disallowed character must be refused: %q", res.Output)
	}
	if !strings.Contains(res.Output, "refused") {
		t.Errorf("output must name the refusal, got: %q", res.Output)
	}
	if len(f.reqs) != 0 {
		t.Error("a refused host must issue no search")
	}
}

// TestWindowCentresOnAnchorInMicroseconds: the ±window is applied around the given alert time and shipped in
// microseconds.
func TestWindowCentresOnAnchorInMicroseconds(t *testing.T) {
	tool, f := newToolFixture(t, fixtureGraph())
	f.respRet = hitsBody()
	_, err := tool.Invoke(context.Background(), map[string]string{
		"host": "dc1sw01", "at": "2026-08-14T12:00:00Z", "minutes": "10",
	})
	if err != nil {
		t.Fatalf("Invoke error: %v", err)
	}
	w := decodeSearchBody(t, f.reqs[0].body)
	// 2026-08-14T12:00:00Z ± 10 minutes ⇒ a 20-minute-wide window, in microseconds.
	if w.Query.EndTime-w.Query.StartTime != int64(20)*60*1_000_000 {
		t.Errorf("the window must be ±10 minutes (1.2e9 microseconds wide), got %d", w.Query.EndTime-w.Query.StartTime)
	}
}
