package acceptance

import (
	"bytes"
	"context"
	"fmt"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/policy"
)

// T-020-6 binds REQ-2018: a past policy_decision joins the EXACT immutable ruleset version in force when it
// decided, not the singleton latest-wins state. The oracle drives the REAL db.MemRulesetVersionStore (first-wins
// archive keyed by bundle_version, mirroring the pgx immutability) with two genuinely-parsed rulesets whose
// bundle_versions are derived by policy.BundleVersion — it archives version A (the decision's ruleset), then a
// later ruleset change to B, then resolves the decision's bundle_version and asserts it returns doc A, NOT the
// now-current B. The pgx round-trip (core/db TestRulesetVersionRoundTrip, DSN-gated) proves the real SQL.
func init() {
	stepRegistrars = append(stepRegistrars, registerRulesetVersionSteps)
}

type rulesetVersionWorld struct {
	store      *db.MemRulesetVersionStore
	docA, docB []byte
	vA         string // the bundle_version the past decision was stamped with
	resolved   []byte
	resolvedOK bool
}

func registerRulesetVersionSteps(sc *godog.ScenarioContext) {
	w := &rulesetVersionWorld{}

	sc.Step(`^a policy_decision written under one ruleset and a later ruleset change$`, func() error {
		w.store = db.NewMemRulesetVersionStore()
		// Two genuinely-distinct rulesets → two distinct bundle_versions via the canonical hash.
		w.docA = []byte(`{"rules":[{"id":"vA-rule","verdict":"deny","match":{"argv_pattern":"rm -rf /"}}]}`)
		w.docB = []byte(`{"rules":[{"id":"vB-rule","verdict":"auto","match":{"op_class":"restart-service"}}]}`)
		rsA, err := policy.ParseRuleSet(w.docA)
		if err != nil {
			return fmt.Errorf("parse ruleset A: %w", err)
		}
		rsB, err := policy.ParseRuleSet(w.docB)
		if err != nil {
			return fmt.Errorf("parse ruleset B: %w", err)
		}
		w.vA = policy.BundleVersion(rsA)
		vB := policy.BundleVersion(rsB)
		if w.vA == "" || w.vA == vB {
			return fmt.Errorf("bundle_versions not distinct: vA=%q vB=%q", w.vA, vB)
		}
		// The decision was written while ruleset A was active (archive A), then the operator changed to B
		// (archive B; the singleton is now B). The past decision still carries bundle_version vA.
		if err := w.store.Save(context.Background(), w.vA, w.docA, len(rsA.Rules), "op-a"); err != nil {
			return err
		}
		if err := w.store.Save(context.Background(), vB, w.docB, len(rsB.Rules), "op-b"); err != nil {
			return err
		}
		return nil
	})

	sc.Step(`^the past decision is joined to the versioned ruleset record by its bundle_version$`, func() error {
		doc, ok, err := w.store.Get(context.Background(), w.vA)
		if err != nil {
			return err
		}
		w.resolved, w.resolvedOK = doc, ok
		return nil
	})

	sc.Step(`^it resolves the exact immutable ruleset document in force when it decided rather than the singleton latest-wins state$`, func() error {
		if !w.resolvedOK {
			return fmt.Errorf("bundle_version %q did not resolve to an archived ruleset", w.vA)
		}
		if !bytes.Equal(w.resolved, w.docA) {
			return fmt.Errorf("resolved the wrong document: got %s, want the ruleset in force when it decided (%s)", w.resolved, w.docA)
		}
		if bytes.Equal(w.resolved, w.docB) {
			return fmt.Errorf("resolved the CURRENT singleton (B) instead of the immutable version in force (A)")
		}
		return nil
	})
}
