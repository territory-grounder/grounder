package acceptance

import (
	"context"
	"fmt"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/judge"
)

// T-020-9 binds REQ-2009: the session triage record carries the prompt/seed/model provenance. The oracle
// drives the REAL write→read seam (db.MemTriageStore.RecordTriage → SessionIdentity) so a dropped field fails
// here without a database; the pgx round-trip (core/db TestSessionIdentityRoundTrip, DSN-gated) proves the SQL
// actually persists it. Observability only — the tracer imports no safety/policy/actuation package.
func init() {
	stepRegistrars = append(stepRegistrars, registerSessionIdentitySteps)
}

type sessionIdentityWorld struct {
	store *db.MemTriageStore
	ref   string
	row   judge.TriageRow
}

func registerSessionIdentitySteps(sc *godog.ScenarioContext) {
	w := &sessionIdentityWorld{}

	sc.Step(`^a classified session composed from a seed a prompt version and a model tier$`, func() error {
		w.store = db.NewMemTriageStore()
		w.ref = "ext-req2009"
		// A composed session: the investigate activity stamps the preamble version, the SHA-256 of the composed
		// seed (hash only — never the seed text; INV-13), and the LLM tier onto the terminal triage row.
		w.row = judge.TriageRow{
			ExternalRef:   w.ref,
			Host:          "librespeed01",
			AlertRule:     "iface_down",
			Outcome:       "proposal",
			Proposed:      true,
			PromptVersion: "preamble/1",
			SeedHash:      "9c1185a5c5e9fc54612808977ee8f548b2258d31",
			ModelTier:     "fast",
		}
		return nil
	})

	sc.Step(`^the session triage record is written$`, func() error {
		return w.store.RecordTriage(context.Background(), w.row)
	})

	sc.Step(`^the record carries the seed_hash the prompt_version and the model_tier so the inspector shows which prompts and tier composed each step$`, func() error {
		id, ok, err := w.store.SessionIdentity(context.Background(), w.ref)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("written session %q read back as absent — the record did not persist", w.ref)
		}
		if id.SeedHash != w.row.SeedHash {
			return fmt.Errorf("seed_hash: got %q, want %q (dropped at the write→read seam)", id.SeedHash, w.row.SeedHash)
		}
		if id.PromptVersion != w.row.PromptVersion {
			return fmt.Errorf("prompt_version: got %q, want %q", id.PromptVersion, w.row.PromptVersion)
		}
		if id.ModelTier != w.row.ModelTier {
			return fmt.Errorf("model_tier: got %q, want %q", id.ModelTier, w.row.ModelTier)
		}
		// Non-empty by construction: an inspector that shows "which prompts and tier composed each step" needs
		// real values, not the pre-fix empty defaults.
		if id.SeedHash == "" || id.PromptVersion == "" || id.ModelTier == "" {
			return fmt.Errorf("provenance read back empty: %+v", id)
		}
		return nil
	})
}
