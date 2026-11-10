package runner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/instrumentid"
	"github.com/territory-grounder/grounder/core/judge"
)

type fakeInstrumentSource struct{ c instrumentid.Components }

func (f fakeInstrumentSource) Components(context.Context) instrumentid.Components { return f.c }

type identityCaptureSink struct {
	recs []instrumentid.Record
	err  error
}

func (s *identityCaptureSink) Ensure(_ context.Context, rec instrumentid.Record) error {
	s.recs = append(s.recs, rec)
	return s.err
}

// TG-672: the stamp composes the session-bound components the runner knows (build, harness fingerprint, op-schema
// catalog hash, rubric) with the process-level ones the source supplies, hands the sink the full record once, and
// returns the id the session row carries.
func TestRecordInstrumentIdentityComposesSessionAndProcessComponents(t *testing.T) {
	src := fakeInstrumentSource{c: instrumentid.Components{PolicyBundle: "sha256:666cdc1c8", ModelAliasHash: strings.Repeat("a", 64),
		SkillSetHash: strings.Repeat("b", 64), KnowledgeHash: strings.Repeat("c", 64)}}
	sink := &identityCaptureSink{}
	a := &Activities{D: Deps{Instrument: src, InstrumentIdentities: sink}}
	ag := &agent.Agent{Tools: agent.NewReadOnlyToolSet(), Guidance: "## Guidance A"}
	fp := strings.Repeat("e", 64)
	id := a.recordInstrumentIdentity(context.Background(), ag, fp)
	if len(id) != 64 || len(sink.recs) != 1 || sink.recs[0].ID != id {
		t.Fatalf("id=%q recs=%d", id, len(sink.recs))
	}
	c := sink.recs[0].Components
	if c.HarnessFP != fp || c.RubricVersion != judge.RubricVersion() || c.OpSchemaHash != ag.HarnessParts().Hashes().OpClassCatalog ||
		c.PolicyBundle != "sha256:666cdc1c8" || c.SkillSetHash != strings.Repeat("b", 64) || c.KnowledgeHash != strings.Repeat("c", 64) {
		t.Fatalf("components = %+v", c)
	}
	// Unrecorded = only the build (a test binary carries none); never a guessed value.
	if unrec := c.Unrecorded(); !(len(unrec) == 0 || (len(unrec) == 1 && unrec[0] == "build_sha")) {
		t.Fatalf("unrecorded = %v, want only build_sha (or nothing)", unrec)
	}
	// Same inputs ⇒ same id; a different harness ⇒ a different id.
	if again := a.recordInstrumentIdentity(context.Background(), ag, fp); again != id {
		t.Fatal("the identity is not a pure function of its components")
	}
	if other := a.recordInstrumentIdentity(context.Background(), ag, strings.Repeat("f", 64)); other == id {
		t.Fatal("a different harness fingerprint did not move the identity")
	}
}

// Without a source the process-level components are unrecorded and NAMED; without a sink the stamp still
// persists; a failing sink is logged and the session proceeds with its id.
func TestRecordInstrumentIdentityIsFailSoftAndNamesWhatItLacks(t *testing.T) {
	ag := &agent.Agent{Tools: agent.NewReadOnlyToolSet()}
	none := &Activities{D: Deps{}}
	id := none.recordInstrumentIdentity(context.Background(), ag, strings.Repeat("1", 64))
	if len(id) != 64 {
		t.Fatalf("no source, no sink: id=%q, want a stamp regardless", id)
	}
	failing := &identityCaptureSink{err: errors.New("db down")}
	a := &Activities{D: Deps{InstrumentIdentities: failing}}
	if got := a.recordInstrumentIdentity(context.Background(), ag, strings.Repeat("1", 64)); got != id {
		t.Fatalf("a failing sink changed the id (%q vs %q) — the stamp must not depend on the row write", got, id)
	}
	unrec := failing.recs[0].Components.Unrecorded()
	for _, want := range []string{"policy_bundle", "model_alias_hash", "skill_set_hash", "knowledge_hash"} {
		found := false
		for _, u := range unrec {
			found = found || u == want
		}
		if !found {
			t.Errorf("unrecorded %v lacks %s — a missing source must be named, not read as a blank value", unrec, want)
		}
	}
	if nilAgent := none.recordInstrumentIdentity(context.Background(), nil, ""); nilAgent != "" {
		t.Fatal("no agent ⇒ no stamp")
	}
}
