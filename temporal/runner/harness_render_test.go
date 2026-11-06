package runner

import (
	"context"
	"errors"
	"testing"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/harnessid"
	"github.com/territory-grounder/grounder/core/ingest"
)

type renderCaptureSink struct {
	recs []harnessid.Record
	err  error
}

func (c *renderCaptureSink) Ensure(_ context.Context, rec harnessid.Record) (db.HarnessRenderOutcome, error) {
	c.recs = append(c.recs, rec)
	if c.err != nil {
		return db.HarnessRenderOutcome{}, c.err
	}
	return db.HarnessRenderOutcome{RenderInserted: len(c.recs) == 1, BuildInserted: len(c.recs) == 1}, nil
}

// TG-600: the activity hands the sink a record whose key is the fingerprint it persists on the session,
// and whose parts derive to that key under the preamble version — so the session row and the stored
// rendering can never name different configurations. A failing sink changes nothing about the session.
func TestRecordHarnessRenderHandsTheSinkTheSessionsOwnConfiguration(t *testing.T) {
	sink := &renderCaptureSink{}
	a := &Activities{D: Deps{HarnessRenders: sink}}
	ag := &agent.Agent{Tools: agent.NewReadOnlyToolSet(), Guidance: "## Guidance A (store-composed)"}
	fp := a.recordHarnessRender(context.Background(), ag)
	if fp != harnessFingerprint(ag) {
		t.Fatal("the returned fingerprint must be the one the session persists")
	}
	if len(sink.recs) != 1 {
		t.Fatalf("exactly one record per call, got %d", len(sink.recs))
	}
	rec := sink.recs[0]
	if rec.HarnessFP != fp || rec.PromptVersion != promptPreambleVersion {
		t.Fatalf("the record must carry the session's fingerprint and preamble version, got %s/%s", harnessid.Short(rec.HarnessFP), rec.PromptVersion)
	}
	if err := harnessid.Verify(rec); err != nil {
		t.Fatalf("the record must verify against its own parts: %v", err)
	}
	if rec.Parts.Guidance != ag.Guidance {
		t.Fatal("the recorded guidance half must be the session's own (trial arms included)")
	}
	if harnessid.Assemble(rec.Parts) != harnessid.Assemble(ag.HarnessParts()) {
		t.Fatal("the recorded parts must be the agent's parts verbatim")
	}

	failing := &Activities{D: Deps{HarnessRenders: &renderCaptureSink{err: errors.New("db down")}}}
	if got := failing.recordHarnessRender(context.Background(), ag); got != fp {
		t.Fatal("a failing sink must not change the fingerprint the session persists")
	}
	none := &Activities{D: Deps{}}
	if got := none.recordHarnessRender(context.Background(), ag); got != fp {
		t.Fatal("no sink: the fingerprint still persists")
	}
	if got := a.recordHarnessRender(context.Background(), nil); got != "" || len(sink.recs) != 1 {
		t.Fatal("a nil agent has no harness: nothing recorded, fingerprint unrecorded")
	}
}

// The investigate activity itself routes through the sink: one record per session, keyed by the
// fingerprint the result carries, for the class the session ran as (the configuration identity does not
// vary with the class, so the recorded parts are the canonical rendering).
func TestInvestigateActivityRecordsTheRenderingBehindItsFingerprint(t *testing.T) {
	sink := &renderCaptureSink{}
	deps := testDeps()
	deps.HarnessRenders = sink
	res, err := NewActivities(deps).InvestigateActivity(context.Background(), ingest.IncidentEnvelope{
		ExternalRef: "TG-600-render", Host: "web01", AlertRule: "NginxDown", Site: "dc1",
		Severity: ingest.SeverityWarning, Summary: "nginx: connection refused on :443",
	}, "DEEP_INVESTIGATION", ClusterMemberContext{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sink.recs) != 1 || sink.recs[0].HarnessFP != res.HarnessFP || res.HarnessFP == "" {
		t.Fatalf("the activity must record exactly the configuration its result names: recs=%d fp=%s", len(sink.recs), harnessid.Short(res.HarnessFP))
	}
}
