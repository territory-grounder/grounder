package runner

import (
	"context"
	"log"
	"strings"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/buildinfo"
	"github.com/territory-grounder/grounder/core/instrumentid"
	"github.com/territory-grounder/grounder/core/judge"
)

// recordInstrumentIdentity derives the session's INSTRUMENT identity (TG-672, F23; core/instrumentid) and records
// its components once per identity. The runner knows the session-bound components — the build it runs, the
// harness fingerprint just derived, the op-schema catalog hash from the same parts read, the rubric this build
// grades with — and asks Deps.Instrument for the process-level ones (policy bundle, model alias map, production
// skills, knowledge corpus head). A nil source, or a component the source could not read, leaves
// that component "" and NAMED on the identity row: unrecorded is a value, a guess never is.
//
// Observability only: a refused or failed row write is logged with the identity and the session proceeds; the
// returned id persists on the session row either way. The sink de-duplicates per process, so this is one INSERT
// per configuration, like the harness render.
func (a *Activities) recordInstrumentIdentity(ctx context.Context, ag *agent.Agent, harnessFP string) string {
	if ag == nil {
		return ""
	}
	c := instrumentid.Components{
		BuildSHA:      buildinfo.Revision(),
		HarnessFP:     harnessFP,
		OpSchemaHash:  ag.HarnessParts().Hashes().OpClassCatalog,
		RubricVersion: judge.RubricVersion(),
	}
	if a.D.Instrument != nil {
		src := a.D.Instrument.Components(ctx)
		c.PolicyBundle, c.ModelAliasHash, c.SkillSetHash, c.KnowledgeHash = src.PolicyBundle, src.ModelAliasHash, src.SkillSetHash, src.KnowledgeHash
	}
	id := c.ID()
	if a.D.InstrumentIdentities != nil {
		if err := a.D.InstrumentIdentities.Ensure(ctx, instrumentid.Record{ID: id, Components: c}); err != nil {
			log.Printf("instrument identity: %s NOT recorded: %v (observability only — the session proceeds; its instrument_id still persists)", id[:12], err)
		}
	}
	if unrec := c.Unrecorded(); len(unrec) > 0 {
		log.Printf("instrument identity: %s has unrecorded component(s) %s — the identity row names them; a cohort under it is comparable only on the recorded ones", id[:12], strings.Join(unrec, ","))
	}
	return id
}
