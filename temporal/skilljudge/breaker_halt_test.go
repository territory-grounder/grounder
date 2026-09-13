package skilljudge

// TG-221 / PORT-FIDELITY-AUDIT finding #24 — the judging lane FAILS LOUD when the production model breaker
// trips, and never leaves a silently-unjudged session behind.
//
// This drives the REAL path: a real *model.Gateway holding a real core/breaker over a real httptest server
// that is down, wired as Deps.Model exactly as cmd/worker wires it. Nothing about the gateway, the breaker
// or the judge activity is stubbed — only the triage Store is the in-memory fake CI already requires
// (no Postgres in CI, constraint D5).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/breaker"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/judge"
)

func TestJudgeBatchHaltsLoudlyOnAnOpenModelCircuit(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"gateway flap"}}`))
	}))
	defer srv.Close()
	os.Setenv("TG_TEST_JUDGE_KEY", "k")

	gw := model.NewGateway(srv.URL, config.SecretRef("env:TG_TEST_JUDGE_KEY"))
	gw.Breakers = model.NewBreakers(breaker.NewMemStore(),
		breaker.WithThreshold(2), breaker.WithCooldown(time.Hour))

	var rows []judge.TriageRow
	for i := 0; i < 8; i++ {
		rows = append(rows, row(fmt.Sprintf("TG-flap%d", i)))
	}
	st := newMemTriage(rows...)
	acts := &Activities{D: Deps{Model: gw, Store: st}}

	out, err := acts.JudgeBatchActivity(context.Background(), time.Now().UTC())
	if err == nil {
		t.Fatal("an OPEN model circuit must fail the activity LOUDLY — a quiet `Skipped: N` is how a judge " +
			"stays dead for weeks")
	}
	if !errors.Is(err, breaker.ErrOpen) {
		t.Fatalf("the halt must carry the breaker cause so an operator can tell a trip from a bad batch: %v", err)
	}
	if out.Judged != 0 {
		t.Fatalf("nothing was judgeable, yet Judged=%d", out.Judged)
	}
	// THE ANTI-EMPTY-SCORECARD ASSERTION: not one session may be marked judged, and not one judgment row may
	// exist, when the judge could not reach a model. A marked-but-unjudged session is never re-judged.
	for _, r := range rows {
		if st.judged[r.ExternalRef] {
			t.Fatalf("%s was marked judged without a judgment — the next run would never re-judge it", r.ExternalRef)
		}
		if len(st.judgments[r.ExternalRef]) != 0 {
			t.Fatalf("%s carries fabricated judgment rows: %v", r.ExternalRef, st.judgments[r.ExternalRef])
		}
	}
	// And the batch was genuinely BOUNDED: the breaker's threshold, not the batch size, decided how many
	// round trips a dead gateway absorbed. Without the breaker this is 8 — the unbounded degradation the
	// finding names.
	if got := hits.Load(); got != 2 {
		t.Fatalf("the dead gateway absorbed %d round trips for an 8-row batch; the breaker should have bounded "+
			"it at the threshold (2)", got)
	}
}

// The complement, so the halt is not a blanket abort: a per-session fault (an unparseable reply) still
// skips just that session and the batch continues — the breaker changes the SYSTEMIC case only.
func TestPerSessionFaultStillSkipsWithoutHalting(t *testing.T) {
	st := newMemTriage(row("TG-ok1"), row("TG-garbled"), row("TG-ok2"))
	mdl := &scriptedJudge{verdicts: map[string]string{
		"TG-ok1": goodVerdict, "TG-ok2": goodVerdict, "TG-garbled": "not json",
	}}
	acts := &Activities{D: Deps{Model: mdl, Store: st}}
	out, err := acts.JudgeBatchActivity(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("a single unparseable reply must NOT fail the batch: %v", err)
	}
	if out.Judged != 2 || out.Skipped != 1 {
		t.Fatalf("want judged=2 skipped=1, got %+v", out)
	}
}
