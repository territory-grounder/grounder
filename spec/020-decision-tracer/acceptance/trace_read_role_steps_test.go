package acceptance

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/httpapi"
	"github.com/territory-grounder/grounder/core/trace"
)

// T-020-11 binds REQ-2014: the decision-tracer detail endpoint is gated behind a DISTINCT, elevated trace-read
// role. The oracle drives the REAL httpapi routes (GET /v1/sessions/{external_ref} under AuthTraceRead, and the
// AuthReadOnly console index) with a PLAIN read-only operator session: the trace surface REFUSES it (403 — it
// authenticated but lacks the elevated role), while the read-only console surface admits the SAME session —
// proving the gate is separate from and strictly more elevated than read-only. The admit side (an admin-eligible
// session is admitted to the trace surface) is proven by core/auth TestTraceReadRole.
func init() {
	stepRegistrars = append(stepRegistrars, registerTraceReadRoleSteps)
}

type traceReadWorld struct {
	srv          *httptest.Server
	cookie       *http.Cookie
	traceCode    int
	readOnlyCode int
}

func registerTraceReadRoleSteps(sc *godog.ScenarioContext) {
	w := &traceReadWorld{}

	sc.Step(`^the detail endpoint the step channel and the console walk and a principal without the trace-read role$`, func() error {
		store := auth.NewMemSessionStore()
		ops := auth.MemOperators{"alice": {Name: "alice", TokenSHA256: sha256.Sum256([]byte("t0ken"))}}
		sa, err := auth.NewSessionAuthenticator([]byte("0123456789abcdef0123456789abcdef"), store, ops, time.Hour)
		if err != nil {
			return err
		}
		sa.Secure = false
		v := &auth.Verifier{}
		v.EnableBrowserSessions(sa)
		// A PLAIN read-only operator session: it authenticates to the console but is NOT admin-eligible, so it
		// holds only the read-only role, never the elevated trace-read role.
		cookie, _, err := sa.Login(context.Background(), "alice", "t0ken", "192.0.2.1:1234")
		if err != nil {
			return err
		}
		w.cookie = cookie
		rt := auth.NewRouter(v)
		httpapi.Register(rt, httpapi.Deps{SessionDetailRead: fixedDetailReader{tr: trace.Assemble("ext-req2014", sealedSpine())}})
		w.srv = httptest.NewServer(rt.Mux())
		return nil
	})

	sc.Step(`^the principal requests the trace surface$`, func() error {
		if w.srv == nil {
			return fmt.Errorf("no server built")
		}
		defer w.srv.Close()
		w.traceCode = traceReadGetWith(w.srv.URL+"/v1/sessions/ext-req2014", w.cookie)
		w.readOnlyCode = traceReadGetWith(w.srv.URL+"/v1/sessions", w.cookie)
		return nil
	})

	sc.Step(`^the trace surface is gated behind a distinct elevated trace-read role separate from the current read-only surface and the principal without it is refused$`, func() error {
		// REFUSED at the elevated trace surface — a 403 (authenticated but lacks the trace-read role), NOT a
		// 401: the session is a valid read-only credential; it is the missing ELEVATED role that refuses it.
		if w.traceCode != http.StatusForbidden {
			return fmt.Errorf("trace surface: got %d, want 403 (a principal without the trace-read role must be refused)", w.traceCode)
		}
		// SEPARATE from the read-only surface: the SAME session is NOT refused by the AuthReadOnly console
		// index — so the trace gate is distinct and strictly more elevated, not merely the same read-only auth.
		if w.readOnlyCode == http.StatusUnauthorized || w.readOnlyCode == http.StatusForbidden {
			return fmt.Errorf("read-only console surface refused the same session (%d) — the trace gate is not distinct from read-only", w.readOnlyCode)
		}
		return nil
	})
}

// traceReadGetWith issues a GET carrying the given session cookie and returns the status code.
func traceReadGetWith(url string, cookie *http.Cookie) int {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return -1
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}
