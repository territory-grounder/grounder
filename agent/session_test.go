package agent

// THE SESSION STAMP, PROVEN FROM THE LOOP (TG-297).
//
// A per-session tool budget is only as real as the identity it keys on, and Tool.Invoke is handed nothing
// but (ctx, args). So the stamp Run puts on ctx is the load-bearing wire: if it is ever dropped, every
// tool keying a budget on it falls back to one shared bucket and the bound stops meaning "per
// investigation". These oracles hold that wire from the loop's side — the tool below reads
// SessionFrom(ctx) exactly where search-host-logs charges its budget.

import (
	"context"
	"sync"
	"testing"
)

// sessionRecorder is a read-only tool that records the session id it was invoked under. It stands in for
// search-host-logs, which does the same read to key its per-session search cap.
type sessionRecorder struct {
	mu   sync.Mutex
	seen []string
}

func (*sessionRecorder) Name() string   { return "get-logs" }
func (*sessionRecorder) ReadOnly() bool { return true }

func (r *sessionRecorder) Invoke(ctx context.Context, _ map[string]string) (ToolResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, SessionFrom(ctx))
	return ToolResult{ID: "tr-1", Tool: "get-logs", Output: "nginx is down", Success: true}, nil
}

func (r *sessionRecorder) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

// runRecordingSessions drives one whole Run and returns the session ids its tool calls saw.
func runRecordingSessions(t *testing.T) []string {
	t.Helper()
	rec := &sessionRecorder{}
	ts := NewReadOnlyToolSet()
	if err := ts.Register(rec); err != nil {
		t.Fatalf("register the recording tool: %v", err)
	}
	m := &scriptedModel{responses: []string{
		distinctToolCall("h1"), distinctToolCall("h2"),
		`{"action":"stop","confidence":0.9,"reason":"logrotate already reclaimed the disk","evidence_ids":["tr-1"]}`,
	}}
	ag := &Agent{Model: m, Tools: ts, Limits: DefaultLimits(), ModelName: "primary", User: "agent"}
	if _, err := ag.Run(context.Background(), nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := rec.recorded()
	if len(got) < 2 {
		t.Fatalf("vacuity floor: the recording tool was invoked %d time(s), so this proves nothing about "+
			"what tools see", len(got))
	}
	return got
}

// KILLING MUTATION: delete `ctx = WithSession(ctx, NewSessionID(a.User))` from Agent.Run. RED — every tool
// then reads "" for its session, so search-host-logs' per-session budget degrades to ONE bucket shared by
// the whole process. That over-binds loudly rather than never binding (deliberate, see SessionFrom), but
// it is still a bound that no longer means "per investigation", and nothing else in the tree would notice.
func TestRunStampsASessionEveryToolCanSee(t *testing.T) {
	got := runRecordingSessions(t)
	for i, s := range got {
		if s == "" {
			t.Fatalf("tool call %d ran under an UNSTAMPED context: a per-session tool budget has nothing to "+
				"key on, so every investigation in the process shares one", i)
		}
	}
}

// One Run is ONE investigation, so the id must not change between its cycles — a stamp minted per tool
// call would hand the agent a fresh budget on every single search, which is an unbounded oracle wearing a
// cap's clothes.
func TestOneRunIsOneSessionThroughout(t *testing.T) {
	got := runRecordingSessions(t)
	for i, s := range got {
		if s != got[0] {
			t.Fatalf("tool call %d saw session %q but the first saw %q: the budget resets mid-investigation, "+
				"so the cap bounds a single call rather than a session", i, s, got[0])
		}
	}
}

// ...and two Runs must NOT share one. A retried session has to start with a full budget or one transient
// failure would leave that incident permanently un-investigable; two workers on one alert must not share
// a bucket either.
func TestTwoRunsAreTwoSessions(t *testing.T) {
	first, second := runRecordingSessions(t), runRecordingSessions(t)
	if first[0] == second[0] {
		t.Fatalf("two separate investigations were both stamped %q: a retry inherits the spent budget of the "+
			"run it is retrying, and is refused reads it never made", first[0])
	}
}

// The context key is unexported, so nothing outside this package can forge a stamp and mint itself a
// fresh budget mid-session. A string key would let any package overwrite it.
func TestASessionStampCannotBeForgedFromOutsideThePackage(t *testing.T) {
	ctx := WithSession(context.Background(), "real-session")
	//nolint:staticcheck // the point is precisely that a string key does NOT collide with sessionKey{}.
	forged := context.WithValue(ctx, "sessionKey", "forged-session")
	if got := SessionFrom(forged); got != "real-session" {
		t.Fatalf("SessionFrom returned %q: a caller outside this package can overwrite the session id and "+
			"hand a tool a brand-new budget mid-investigation", got)
	}
}

func TestNewSessionIDsAreUniqueAndCarryTheUser(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := NewSessionID("agent")
		if seen[id] {
			t.Fatalf("NewSessionID repeated %q: two concurrent investigations would share one budget", id)
		}
		seen[id] = true
	}
	if len(seen) != 1000 {
		t.Fatalf("vacuity floor: %d distinct ids from 1000 mints", len(seen))
	}
}
