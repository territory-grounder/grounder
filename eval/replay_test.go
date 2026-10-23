package eval

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/trace"
)

// isReplayServed reports whether a tool on a replay surface is served from the recording (either replay type).
func isReplayServed(t agent.Tool) bool {
	_, ok := t.(replayTool)
	return ok
}

func fixtureArmedIncidents(t *testing.T) []Incident {
	t.Helper()
	c, err := LoadCorpus("corpus.json")
	if err != nil {
		t.Fatalf("LoadCorpus: %v", err)
	}
	var out []Incident
	for _, inc := range c {
		if inc.FixtureArmed() {
			out = append(out, inc)
		}
	}
	if len(out) == 0 {
		t.Fatal("corpus has no fixture-armed incident")
	}
	return out
}

// TG-580's oracle: every captured (tool, host) observation of every fixture-armed corpus incident replays
// BYTE-IDENTICALLY through the replay tool-set — same Output, same Success, the recorded result id — when the
// agent asks for that tool on that host. The fixture arm and the replay arm are two readings of one record.
func TestReplayServesEveryFixtureObservationByteIdentically(t *testing.T) {
	incs := fixtureArmedIncidents(t)
	served := 0
	for _, inc := range incs {
		ts := NewReplayToolSet(RecordedFromFixtures(inc), inc, estate.NewGraph())
		for key, want := range inc.ToolFixtures {
			tool, host, _ := strings.Cut(key, "|")
			tl, ok := ts.Get(tool)
			if !ok {
				t.Fatalf("%s: replay toolset lacks %s", inc.ExternalRef, tool)
			}
			got, err := tl.Invoke(context.Background(), map[string]string{"host": host})
			if err != nil {
				t.Fatalf("%s %s: %v", inc.ExternalRef, key, err)
			}
			if got.Output != want.Output || got.Success != want.Success || got.ID != fixtureResultID(tool, host) || got.Tool != tool {
				t.Fatalf("%s %s: replay differs from the recording:\n got  %+v\n want %+v", inc.ExternalRef, key, got, want)
			}
			served++
		}
	}
	if served < 8 {
		t.Fatalf("expected the captured corpus to exercise at least 8 observations, replayed %d", served)
	}
	t.Logf("replayed %d recorded observations across %d fixture-armed incidents byte-identically", served, len(incs))
}

// A call the recording never answered is a DISTINCT state: Success=false, its own id, a body that names it —
// never an empty result and never the fixture arm's miss shape. Three novel shapes: a host the session never
// looked at (while the same tool HAS other hosts recorded), a tool it never called, and a no-target call on a
// tool with no recording at all.
func TestReplayRefusesNovelCallsWithItsOwnState(t *testing.T) {
	inc := fixtureArmedIncidents(t)[0]
	ts := NewReplayToolSet(RecordedFromFixtures(inc), inc, estate.NewGraph())
	var recordedTool string
	for key := range inc.ToolFixtures {
		recordedTool, _, _ = strings.Cut(key, "|")
		break
	}
	for _, tc := range []struct {
		name, tool string
		args       map[string]string
	}{
		{"unrecorded host on a recorded tool", recordedTool, map[string]string{"host": "never-observed-host-xyz"}},
		{"no-target call on a tool the session never used", "search-host-logs", map[string]string{}},
		{"targeted call on a tool the session never used", "check-host-load", map[string]string{"host": "web01"}},
	} {
		tl, ok := ts.Get(tc.tool)
		if !ok {
			t.Fatalf("%s: toolset lacks %s", tc.name, tc.tool)
		}
		got, err := tl.Invoke(context.Background(), tc.args)
		if err != nil {
			t.Fatalf("%s: a refusal is a result, not an error: %v", tc.name, err)
		}
		if got.Success || !strings.HasPrefix(got.Output, ReplayMissMarker) || !strings.HasPrefix(got.ID, "replay-miss-") {
			t.Fatalf("%s: want the distinct replay refusal, got %+v", tc.name, got)
		}
		if strings.Contains(got.Output, "not present in deployment") || got.Output == "" {
			t.Fatalf("%s: a replay miss must never wear the fixture arm's miss shape or be empty: %q", tc.name, got.Output)
		}
		if !strings.Contains(got.Output, inc.ExternalRef) || !strings.Contains(got.Output, tc.tool) {
			t.Fatalf("%s: the refusal must name the session and the tool: %q", tc.name, got.Output)
		}
	}
}

// Target-less calls replay in cycle order and then repeat the last observation (a deterministic re-read); a
// targeted call that the session answered twice replays both payloads in order, then repeats; a truncated
// recording says so in the replayed body.
func TestReplayOrdinalOrderRepeatAndTruncationHonesty(t *testing.T) {
	rec := RecordedSession{ExternalRef: "TG-replay-1", Calls: []RecordedCall{
		{Cycle: 1, Tool: "get-active-alerts", EvidenceID: "lnms-alerts-", Output: "alerts@1", Success: true},
		{Cycle: 2, Tool: "get-device-status", EvidenceID: "lnms-dev-web01", Output: "status@2", Success: true},
		{Cycle: 3, Tool: "get-active-alerts", EvidenceID: "lnms-alerts-", Output: "alerts@3", Success: true},
		{Cycle: 4, Tool: "get-device-status", EvidenceID: "lnms-dev-web01", Output: "status@4-prefix", Success: false, Truncated: true},
	}}
	inc := Incident{ExternalRef: "TG-replay-1", Host: "web01", AlertRule: "R"}
	ts := NewReplayToolSet(rec, inc, estate.NewGraph())
	alerts, _ := ts.Get("get-active-alerts")
	var outs []string
	for i := 0; i < 3; i++ {
		r, _ := alerts.Invoke(context.Background(), map[string]string{})
		outs = append(outs, r.Output)
	}
	if outs[0] != "alerts@1" || outs[1] != "alerts@3" || outs[2] != "alerts@3"+ReplayExhaustedNote {
		t.Fatalf("target-less calls must replay in cycle order then repeat the last with the exhaustion note: %q", outs)
	}
	status, _ := ts.Get("get-device-status")
	r1, _ := status.Invoke(context.Background(), map[string]string{"host": "web01.example.net"}) // domain stripped like the real tools
	r2, _ := status.Invoke(context.Background(), map[string]string{"host": "WEB01"})
	r3, _ := status.Invoke(context.Background(), map[string]string{"host": "web01"})
	if r1.Output != "status@2" || !r1.Success {
		t.Fatalf("first targeted replay must be the first recorded observation: %+v", r1)
	}
	if !strings.HasPrefix(r2.Output, "status@4-prefix") || !strings.Contains(r2.Output, "truncated at capture") || r2.Success {
		t.Fatalf("second targeted replay must be the second recording, flagged truncated, with its recorded success: %+v", r2)
	}
	if !strings.HasPrefix(r3.Output, "status@4-prefix") || !strings.HasSuffix(r3.Output, ReplayExhaustedNote) || r3.ID != r2.ID {
		t.Fatalf("once exhausted, the last observation repeats WITH the exhaustion note: %+v", r3)
	}
	if strings.Contains(r2.Output, ReplayExhaustedNote) || strings.Contains(r1.Output, ReplayExhaustedNote) {
		t.Fatalf("an unserved observation must carry no exhaustion note: %+v / %+v", r1, r2)
	}
	// A call that OMITS the host is never served another host's observation: get-device-status has only
	// host-targeted recordings, so a target-less call is a miss, not web01's payload.
	if bare, _ := status.Invoke(context.Background(), map[string]string{}); bare.Success || !strings.HasPrefix(bare.Output, ReplayMissMarker) {
		t.Fatalf("a target-less call on a tool with only targeted recordings must be refused: %+v", bare)
	}
	other, _ := status.Invoke(context.Background(), map[string]string{"host": "db01"})
	if other.Success || !strings.HasPrefix(other.Output, ReplayMissMarker) {
		t.Fatalf("a host the session never observed must be refused even though the tool has recordings: %+v", other)
	}
}

// LoadRecordedSession joins steps (per-cycle outcome) with evidence (payload): a failed cycle replays as
// Success=false; a session with no evidence yields an empty recording — a real answer, not an error.
func TestLoadRecordedSessionJoinsStepsAndEvidence(t *testing.T) {
	ctx := context.Background()
	steps := db.NewMemAgentStepStore()
	ev := db.NewMemAgentStepEvidenceStore()
	ref := "TG-replay-2"
	for _, s := range []trace.AgentStep{
		{ExternalRef: ref, Cycle: 1, Tool: "get-device-status", Observation: "ok", Outcome: "success"},
		{ExternalRef: ref, Cycle: 2, Tool: "check-host-disk", Observation: "timeout", Outcome: "failure"},
	} {
		if err := steps.Emit(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range []trace.AgentStepEvidence{
		{ExternalRef: ref, Cycle: 2, EvidenceID: "check-host-disk-web01", Tool: "check-host-disk", Payload: "ssh: timeout"},
		{ExternalRef: ref, Cycle: 1, EvidenceID: "lnms-dev-web01", Tool: "get-device-status", Payload: "device web01: up"},
	} {
		if err := ev.EmitEvidence(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	rec, err := LoadRecordedSession(ctx, steps, ev, ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Calls) != 2 || rec.Calls[0].Cycle != 1 || rec.Calls[0].Output != "device web01: up" || !rec.Calls[0].Success ||
		rec.Calls[1].Cycle != 2 || rec.Calls[1].Success {
		t.Fatalf("recording must be cycle-ordered with the step outcome joined in: %+v", rec.Calls)
	}
	empty, err := LoadRecordedSession(ctx, steps, ev, "TG-never-recorded")
	if err != nil || len(empty.Calls) != 0 {
		t.Fatalf("a session with no evidence is an EMPTY recording, not an error: %+v %v", empty, err)
	}
}

// tripwireRT fails the test on ANY HTTP round-trip: the replay arm must never dial out.
type tripwireRT struct{ t *testing.T }

func (tw tripwireRT) RoundTrip(r *http.Request) (*http.Response, error) {
	tw.t.Fatalf("replay arm dialed out over HTTP: %s %s", r.Method, r.URL)
	return nil, errors.New("dial-out")
}

// The no-live-network guarantee for the replay arm, the same two ways the fixture arm proves it: every served
// name resolves to a replayTool (pure lookup; no client, no runner anywhere in the type) or a pure in-memory
// tool, and every tool is invoked (hit, miss) with the default transport replaced by a tripwire.
func TestReplayToolSetNeverDialsOut(t *testing.T) {
	inc := fixtureArmedIncidents(t)[0]
	ts := NewReplayToolSet(RecordedFromFixtures(inc), inc, estate.NewGraph())
	wantNames := append([]string{"get-device-context", "get-estate-context"}, fixtureServedTools...)
	sort.Strings(wantNames)
	if got := strings.Join(ts.Names(), ","); got != strings.Join(wantNames, ",") {
		t.Fatalf("replay toolset surface: got %s want %s", got, strings.Join(wantNames, ","))
	}
	for _, name := range ts.Names() {
		tl, _ := ts.Get(name)
		switch name {
		case "get-device-context", "get-estate-context": // real in both arms (this recording holds no estate read)
		default:
			if !isReplayServed(tl) {
				t.Fatalf("%s on a replay toolset is %T, not a replay tool — it could reach the network", name, tl)
			}
		}
	}
	prev := http.DefaultTransport
	http.DefaultTransport = tripwireRT{t}
	defer func() { http.DefaultTransport = prev }()
	for _, name := range ts.Names() {
		tl, _ := ts.Get(name)
		for _, args := range []map[string]string{{"host": inc.Host}, {"host": "never-observed"}, {}} {
			if _, err := tl.Invoke(context.Background(), args); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
	}
}

// The matcher follows the REAL result-id conventions (librenms normHost keeps the id as `<family>-<host>` with the
// DNS domain stripped; hostdiag/syslogng sanitizeID turns every non-alphanumeric run into "-", trims, caps at 48)
// and it is EQUALITY after the family prefix — never a substring: a hyphenated host's fragment, a tool-name
// fragment or a longer host sharing a prefix must not serve, and an IP or a dotted raw host must still match.
func TestTargetMatchingUsesTheRealIDConventionsAndNeverSubstrings(t *testing.T) {
	long := strings.Repeat("h", 60) // sanitizeID caps the id at 48 characters
	for _, tc := range []struct {
		tool, id, call string
		want           bool
	}{
		{"get-device-status", "lnms-dev-web01", "web01", true},
		{"get-device-status", "lnms-dev-web01", "WEB01.example.net", true}, // librenms normalizes the FQDN away
		{"get-device-status", "lnms-dev-web01", "web011", false},
		{"get-device-status", "lnms-dev-web11", "web1", false},
		{"get-device-status", "lnms-dev-tg78-pve-guest-02", "guest-02", false}, // hyphenated-host fragment
		{"get-device-status", "lnms-dev-tg78-pve-guest-02", "pve", false},
		{"get-device-status", "lnms-dev-tg78-pve-guest-02", "tg78-pve-guest-02", true},
		{"get-device-status", "lnms-dev-10.0.0.5", "10.0.0.5", true}, // librenms keeps a dotted IP
		{"check-host-disk", "check-host-disk-web01", "host", false},  // tool-name fragment
		{"check-host-disk", "check-host-disk-web01", "disk", false},
		{"check-host-disk", "check-host-disk-web-01", "web", false},
		{"check-host-disk", "check-host-disk-web-01", "web-01", true},
		{"check-host-disk", "check-host-disk-10-0-0-5", "10.0.0.5", true},                   // sanitizeID dashed the IP
		{"get-host-logs", "syslogng-logs-web01.example.net", "web01.example.net", true},     // syslogng's sanitizeID KEEPS dots
		{"get-host-logs", "syslogng-logs-web01.example.net", "web01", false},                // …and the bare label is not that recording
		{"check-host-disk", "check-host-disk-web01-example-net", "web01.example.net", true}, // hostdiag dashes the FQDN
		{"check-host-disk", "check-host-disk-web01-example-net", "web01", false},            // strict the other way too
		{"get-host-logs", "syslogng-logs-" + long[:48], long, true},                         // 48-char truncation
		{"get-active-alerts", "lnms-alerts-", "", true},                                     // target-less ↔ target-less
		{"get-active-alerts", "lnms-alerts-", "web01", false},                               // a targeted call never takes a target-less recording
		{"get-active-alerts", "lnms-alerts-web01", "", false},                               // and the reverse
		{"get-device-status", "lnms-dev-web01", "", false},
		{"get-estate-context", "estate-ctx-web01", "web01", true},                         // modules/estate: estate-ctx-<sanitizeID(host)>
		{"get-estate-context", "estate-ctx-web01.example.net", "web01.example.net", true}, // its sanitizer KEEPS dots
		{"get-estate-context", "estate-ctx-web01.example.net", "web01", false},            // the bare label is not that recording
		{"get-estate-context", "estate-ctx-web01", "web011", false},
		{"get-estate-context", "estate-ctx-web01-", "web01!", true},   // estate maps a trailing special one-for-one
		{"get-estate-context", "estate-ctx-fe80--1", "fe80::1", true}, // …and does not collapse runs (IPv6)
		{"get-estate-context", "estate-ctx-unnamed", "", true},        // "no host given" ↔ target-less
		{"get-estate-context", "estate-ctx-unnamed", "web01", false},
		{"get-estate-context", "estate-ctx-" + strings.Repeat("h", 50) + "-x", strings.Repeat("h", 50) + "!x", true}, // no 48 cap
	} {
		recorded, known := RecordedTarget(tc.tool, tc.id)
		got := known && targetMatches(recorded, tc.call)
		if got != tc.want {
			t.Errorf("%s id=%q call=%q: match=%v want %v (recorded target %q known=%v)", tc.tool, tc.id, tc.call, got, tc.want, recorded, known)
		}
	}
	// An id in no known family is UNREADABLE, not target-less: a target-less call must not be answered by it.
	if recorded, known := RecordedTarget("get-active-alerts", "mystery-42"); known || recorded != "" {
		t.Fatalf("an unknown id family must read as unknown, got %q known=%v", recorded, known)
	}
}

// The estate tool is REAL in both deterministic arms, but a replay's graph is not the graph the session saw: when
// the recording holds an estate observation it is served from the recording (byte-identical for that host, the
// replay refusal for another host); a recording without one keeps the real tool, as the fixture arm does.
func TestReplayServesARecordedEstateReadOverTheLiveGraph(t *testing.T) {
	// Three recorded reads of the same host (three cycles): one per argument spelling below, then the exhausted repeat.
	var calls []RecordedCall
	for cyc := 1; cyc <= 3; cyc++ {
		calls = append(calls, RecordedCall{Cycle: cyc, Tool: "get-estate-context", EvidenceID: "estate-ctx-web01", Output: "graph as the session saw it", Success: true})
	}
	rec := RecordedSession{ExternalRef: "x-1", Calls: calls}
	ts := NewReplayToolSet(rec, Incident{ExternalRef: "x-1"}, estate.NewGraph())
	tl, ok := ts.Get("get-estate-context")
	if !ok {
		t.Fatal("get-estate-context must stay on the replay surface")
	}
	if !isReplayServed(tl) {
		t.Fatalf("with a recorded estate read the tool must be served from the recording, got %T", tl)
	}
	// The replayed agent's PROMPT is unchanged: the replay tool publishes the real estate tool's catalog entry.
	real0, _ := NewFixtureToolSet(Incident{ExternalRef: "x-1"}, estate.NewGraph()).Get("get-estate-context")
	aci, ok := tl.(agent.ACITool)
	if !ok || aci.Description() != real0.(agent.ACITool).Description() || len(aci.Params()) != len(real0.(agent.ACITool).Params()) || aci.Params()[0].Name != "host" {
		t.Fatalf("the replay tool must carry the displaced tool's Description and Params, got %T", tl)
	}
	for _, args := range []map[string]string{{"host": "web01"}, {"target": "web01"}, {"hostname": "WEB01"}} {
		got, err := tl.Invoke(context.Background(), args)
		if err != nil || got.ID != "estate-ctx-web01" || got.Output != "graph as the session saw it" || !got.Success {
			t.Fatalf("the recorded estate read must come back byte-identically for %v: %+v err=%v", args, got, err)
		}
	}
	if again, _ := tl.Invoke(context.Background(), map[string]string{"host": "web01"}); !strings.HasSuffix(again.Output, ReplayExhaustedNote) {
		t.Fatalf("a fourth read repeats the last observation and SAYS so: %q", again.Output)
	}
	if miss, _ := tl.Invoke(context.Background(), map[string]string{"host": "web02"}); miss.Success || !strings.HasPrefix(miss.Output, ReplayMissMarker) {
		t.Fatalf("another host is a replay miss, never the live graph: %+v", miss)
	}
	// Without a recorded estate read the REAL tool stays (fixture-arm parity), and it is not a replayTool.
	bare := NewReplayToolSet(RecordedSession{ExternalRef: "x-2"}, Incident{ExternalRef: "x-2"}, estate.NewGraph())
	real, _ := bare.Get("get-estate-context")
	if _, isReplay := real.(replayTool); isReplay {
		t.Fatal("a recording without an estate read must keep the real estate tool")
	}
}

// Same-cycle rows come back in INSERTION order from the in-memory twin, exactly as the pgx store orders them —
// the two must never disagree, or a replay oracle could pass on one and fail on the other.
func TestMemEvidenceListerKeepsInsertionOrderWithinACycle(t *testing.T) {
	ctx := context.Background()
	ev := db.NewMemAgentStepEvidenceStore()
	for _, id := range []string{"zzz-second", "aaa-first"} { // alphabetical order would invert these
		if err := ev.EmitEvidence(ctx, trace.AgentStepEvidence{ExternalRef: "TG-order", Cycle: 1, EvidenceID: id, Tool: "t", Payload: id}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ev.EvidenceFor(ctx, "TG-order")
	if err != nil || len(got) != 2 || got[0].EvidenceID != "zzz-second" || got[1].EvidenceID != "aaa-first" {
		t.Fatalf("want insertion order within a cycle, got %+v err=%v", got, err)
	}
}
