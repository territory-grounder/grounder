package acceptance

import (
	"context"
	"fmt"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/safety"
)

// T-020-4 binds REQ-2006: a sealed action_manifest is backfilled with the human approval choice and the
// post-execution mechanical verdict — the two NON-HASHED lifecycle labels the decision tracer needs to show how
// an action resolved, which the seal writer left NULL. The oracle drives the REAL db.MemManifestStore
// (Seal → BackfillLifecycle → Lifecycle read) so the seal→backfill→read seam is proven WITHOUT a database; the
// pgx round-trip (core/db TestManifestLifecycleRoundTrip, DSN-gated) proves the real SQL. Observe-only: the
// backfill touches only the two non-hashed columns, so it cannot tamper the content-addressed binding (INV-07).
func init() {
	stepRegistrars = append(stepRegistrars, registerManifestBackfillSteps)
}

type manifestBackfillWorld struct {
	store    *db.MemManifestStore
	actionID string
}

func registerManifestBackfillSteps(sc *godog.ScenarioContext) {
	w := &manifestBackfillWorld{}

	sc.Step(`^an action that was approved and then executed and verified$`, func() error {
		m, err := manifest.New(
			manifest.Action{Target: "librespeed01", OpClass: "restart-service", Op: "restart", Reversible: true},
			safety.BandAuto, "ph-req2006", "predh-req2006")
		if err != nil {
			return err
		}
		w.store = db.NewMemManifestStore()
		w.actionID = m.ActionID
		return w.store.Seal(context.Background(), m)
	})

	sc.Step(`^the manifest-lifecycle writer seals the row$`, func() error {
		// The approval choice is known after the vote; the verdict after verify — the runner backfills each as it
		// becomes known. COALESCE semantics: the later verdict backfill must NOT erase the earlier approval choice.
		if err := w.store.BackfillLifecycle(context.Background(), w.actionID, "approved", ""); err != nil {
			return err
		}
		return w.store.BackfillLifecycle(context.Background(), w.actionID, "", safety.VerdictMatch)
	})

	sc.Step(`^the action_manifest carries the approval_choice and the post-execution verdict rather than leaving them NULL$`, func() error {
		lc, ok, err := w.store.Lifecycle(context.Background(), w.actionID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("sealed manifest %q read back as absent", w.actionID)
		}
		if lc.ApprovalChoice != "approved" {
			return fmt.Errorf("approval_choice: got %q, want %q (the later verdict backfill erased it?)", lc.ApprovalChoice, "approved")
		}
		if lc.Verdict != "match" {
			return fmt.Errorf("verdict: got %q, want %q", lc.Verdict, "match")
		}
		// REQ-2006: "rather than leaving them NULL" — both labels must be non-empty.
		if lc.ApprovalChoice == "" || lc.Verdict == "" {
			return fmt.Errorf("lifecycle labels left NULL: approval=%q verdict=%q", lc.ApprovalChoice, lc.Verdict)
		}
		return nil
	})
}
