package agent

import (
	"context"
	"strconv"
	"sync/atomic"
)

// THE SESSION IDENTITY A TOOL CAN SEE (TG-297).
//
// Tool.Invoke takes (ctx, args) and nothing else, so a tool that needs a SESSION-level bound — as opposed
// to the per-invocation bounds every read tool already carries — has nothing to key one on. search-host-logs
// was the specimen: every bound in modules/observability/syslogng is per-call (a `tail -n`, a `grep -m`, a
// byte cap, a context timeout), the toolBox held no counter, and varying the search pattern produces a fresh
// ArgsKey, so TrajectoryVeto never binds either. One investigation could therefore run an unbounded number
// of fixed-string probes against a device's syslog — a confirmation oracle with no session bound anywhere.
//
// The id is stamped ONCE per Run, because one Run of the loop IS one investigation session. It is minted
// fresh rather than being the incident's external ref on purpose: a retried session must start with a full
// budget, or one transient failure would leave that incident permanently un-investigable.
//
// It is IDENTITY ONLY. Nothing derived from it becomes control flow (INV-08) and it is never rendered into a
// prompt: a tool may use it to key its own counters, and that is all.

// sessionKey is the unexported context key. Unexported so nothing outside this package can forge or
// overwrite a session id and hand a tool a fresh budget mid-session.
type sessionKey struct{}

// sessionSeq makes each Run's id unique within the process, so two concurrent sessions for the SAME
// incident (a retry racing its predecessor, two workers on one alert) never share a budget.
var sessionSeq atomic.Uint64

// WithSession stamps a session id on ctx. Callers other than Run should not need this; it is exported so
// the oracle can construct a stamped context without driving the whole loop.
func WithSession(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, sessionKey{}, id)
}

// SessionFrom returns the session id stamped on ctx, or "" when nothing stamped one.
//
// A tool keying a budget on "" gets ONE SHARED bucket for every unstamped caller, and that is the
// deliberate direction: if the stamp is ever dropped from Run, the budget over-binds loudly (the tool
// refuses and says why) instead of silently never binding — which is this repo's most-repeated bug and
// exactly the shape TG-297 reported.
func SessionFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(sessionKey{}).(string)
	return id
}

// NewSessionID mints a process-unique session id, prefixed with the caller's user label so a budget
// refusal in a log line is traceable back to the incident that spent it.
func NewSessionID(user string) string {
	return user + "#" + strconv.FormatUint(sessionSeq.Add(1), 10)
}
