package acceptance

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/harnessid"
)

func init() {
	stepRegistrars = append(stepRegistrars, registerHarnessRenderSteps)
}

// REQ-2025 (TG-600, Harness² H2-1c): the rendering behind a harness fingerprint is recorded once, on the
// configuration's first session, under a key that derives from its components — so a stored rendering IS
// the bytes the model received, and a forged key is refused. Drives the real agent parts, the real
// harnessid math and the store's in-memory twin (the SAME Verify law the pgx store runs; the pgx path is
// pinned by core/db TestHarnessRenderStoreRoundTrip).
func registerHarnessRenderSteps(sc *godog.ScenarioContext) {
	const version = "preamble/acceptance"
	var (
		ag     *agent.Agent
		rec    harnessid.Record
		store  *db.MemHarnessRenderStore
		first  db.HarnessRenderOutcome
		again  db.HarnessRenderOutcome
		forged error
	)
	sc.Step(`^an agent configuration rendered into its four effective components and bound to a preamble version$`, func() error {
		ag = &agent.Agent{Tools: agent.NewReadOnlyToolSet(), Guidance: "## Guidance (acceptance arm)"}
		parts := ag.HarnessParts()
		if strings.TrimSpace(parts.Prose) == "" || strings.TrimSpace(parts.ToolCatalog) == "" || strings.TrimSpace(parts.OpClassCatalog) == "" || parts.Guidance != ag.Guidance {
			return fmt.Errorf("the four effective components must all be rendered: %+v", parts)
		}
		rec = harnessid.Record{HarnessFP: harnessid.Fingerprint(parts, version), PromptVersion: version, BuildSHA: "build-acceptance", Parts: parts}
		store = db.NewMemHarnessRenderStore()
		return nil
	})
	sc.Step(`^the first session under that configuration records its rendering and a later session records it again and a forged record carries another configuration's fingerprint$`, func() error {
		var err error
		if first, err = store.Ensure(context.Background(), rec); err != nil {
			return fmt.Errorf("first session: %w", err)
		}
		if again, err = store.Ensure(context.Background(), rec); err != nil {
			return fmt.Errorf("later session: %w", err)
		}
		other := rec
		other.Parts.Guidance = "## a different guidance half (another configuration)"
		bad := rec
		bad.HarnessFP = harnessid.Fingerprint(other.Parts, version) // another configuration's key over THIS rendering
		_, forged = store.Ensure(context.Background(), bad)
		return nil
	})
	sc.Step(`^one rendering and one build row exist the forged record is refused and the stored components reassemble to the exact system half that hashes to the fingerprint$`, func() error {
		if !first.RenderInserted || !first.BuildInserted {
			return fmt.Errorf("the first session must write the rendering and its build row, got %+v", first)
		}
		if again.RenderInserted || again.BuildInserted {
			return fmt.Errorf("a later session under the same configuration must write nothing, got %+v", again)
		}
		if forged == nil || !strings.Contains(forged.Error(), "does not derive") {
			return fmt.Errorf("a record keyed by another configuration's fingerprint must be refused, got %v", forged)
		}
		stored, builds, ok := store.Get(rec.HarnessFP)
		if !ok || store.Len() != 1 || len(builds) != 1 {
			return errors.New("exactly one rendering with one build row must exist")
		}
		want := harnessid.Assemble(ag.HarnessParts())
		if harnessid.Assemble(stored.Parts) != want {
			return errors.New("the stored components must reassemble to the exact system half the model received")
		}
		if harnessid.Bind(harnessid.ContentHash(want), version) != rec.HarnessFP {
			return errors.New("the assembled system half must hash to the fingerprint the session persists")
		}
		return nil
	})
}
