package runner

import (
	"context"
	"log"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/buildinfo"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/harnessid"
)

// The session's HARNESS identity persisted as session_triage.harness_fp (TG-575, spec/020 REQ-2009
// lineage) is harnessid.Fingerprint(parts, promptPreambleVersion): the agent's system-half parts
// (protocol prose + tool catalog + op-class catalog incl. overlay + guidance half) bound to the
// trusted-preamble version. With model_tier, decision_model_tier and skill_loads on the same row it
// names the full configuration a session ran under, so a later reader — the flywheel's regression
// trigger, an eval arm, the tracer — can refuse to pool sessions from different configurations. Hex
// SHA-256; "" means unrecorded. Derived in recordHarnessRender from ONE read of the parts (TG-600).

// HarnessRenderSink records the rendering behind a fingerprint (TG-600, migration 0117). The production
// implementation is db.HarnessRenderStore; nil means "fingerprints persist, renderings do not".
type HarnessRenderSink interface {
	Ensure(ctx context.Context, rec harnessid.Record) (db.HarnessRenderOutcome, error)
}

// recordHarnessRender reads the agent's parts ONCE and derives everything from that read — the key the
// sink verifies, the record it stores and the fingerprint the session persists — so a runtime overlay
// ratified between two reads of the op-class catalog can never key the render under one configuration
// and the row under another (review finding on !1769). When a sink is wired it records the rendering
// behind the fingerprint (TG-600, Harness² H2-1c: the harness is a readable, fingerprinted artifact, not
// just a hash). The sink de-duplicates per process, so this is one INSERT per configuration — the FIRST
// session under a new build or guidance row writes it; every later one is a cache hit. Observability
// only: a refused or failed write is logged with the fingerprint and the session proceeds, and the
// returned value is the session's harness_fp either way.
func (a *Activities) recordHarnessRender(ctx context.Context, ag *agent.Agent) string {
	if ag == nil {
		return ""
	}
	parts := ag.HarnessParts()
	fp := harnessid.Fingerprint(parts, promptPreambleVersion)
	if a.D.HarnessRenders == nil {
		return fp
	}
	rec := harnessid.Record{HarnessFP: fp, PromptVersion: promptPreambleVersion, BuildSHA: buildinfo.Revision(), Parts: parts}
	out, err := a.D.HarnessRenders.Ensure(ctx, rec)
	if err != nil {
		log.Printf("harness render: configuration %s NOT recorded: %v (observability only — the session proceeds; its harness_fp still persists)", harnessid.Short(fp), err)
		return fp
	}
	if out.RenderInserted || out.BuildInserted {
		log.Printf("harness render: configuration %s recorded (new rendering=%t, build %s new=%t, %d bytes, %s) — read/diff with tools/harnessdiff", harnessid.Short(fp), out.RenderInserted, buildinfo.Describe(), out.BuildInserted, parts.Bytes(), promptPreambleVersion)
	}
	return fp
}
