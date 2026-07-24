package policy

import (
	"context"
	_ "embed"
	"errors"
)

// defaultRuleSetJSON is the curated Semi-auto default policy ruleset shipped with TG, embedded as the SAME
// rules-as-data JSON an operator would author (loadable data, never a Go literal — the prose-loadable rule).
//
//go:embed default_ruleset.json
var defaultRuleSetJSON []byte

// DefaultRuleSetDocument returns the curated default policy-ruleset document — the OUT-OF-BOX baseline a fresh
// deployment seeds when no operator ruleset exists yet (the caller MUST seed absent-only, so an operator
// document is never clobbered). It grants `auto` to the conservative reversible op-class family —
// `restart-service`, `reload-service`, `restart-container`, and `start-guest` (bring a DOWN guest that should
// be up back up — reversible start↔stop; the harsher proxmox lifecycle verbs are floored) — and NOTHING else (each match still requires
// reversible=true; each still traverses novelty/band/mode/floor and the effect-leaf allowlist, so a
// curated-auto class actuates ONLY on an operator-allowlisted unit/container). The rules EXPLICITLY set
// min_confidence:0 to keep the confidence gate OFF (matching the proven live
// canary rule): an UNSET min_confidence would inherit the engine's 0.60 EffectiveParams fallback and clamp
// the curated `auto` to `approve` whenever the bound confidence is unset/low — leaving the seed inert — and
// gating autonomy on a self-reported, not-yet-calibrated confidence scalar is exactly what the settled
// calibration decision defers. So `auto` here is gated by the earned-graduation ladder, the per-incident
// novelty gate, the risk band, the mode chokepoint, and the never-auto floor — the reliable gates — never by
// confidence. Mutation stays Shadow by default, so this baseline never actuates until an operator deliberately
// escalates the mode. A COPY is returned so a caller can never mutate the embedded doc.
func DefaultRuleSetDocument() []byte {
	out := make([]byte, len(defaultRuleSetJSON))
	copy(out, defaultRuleSetJSON)
	return out
}

// DefaultGraduatedClasses is the curated set of op-classes a fresh deployment seeds to LevelAuto so the
// matching curated `auto` rules are HONORED out of the box — an ungraduated class downgrades `auto`→`approve`,
// so the ruleset seed alone would be inert. Every class here MUST be reversible and named by an `auto` rule in
// DefaultRuleSetDocument (a lockstep test enforces the two never drift). Seeding is idempotent + absent-only:
// a class is seeded ONLY when it has no persisted ladder state — the seed NEVER downgrades or overwrites a
// class that has earned autonomy or been operator-tuned — and it is mode-gated (Shadow default), so a seeded
// class actuates only once an operator escalates the mode. Every OTHER op-class still earns autonomy from
// zero through the ladder.
func DefaultGraduatedClasses() []string {
	return []string{"restart-service", "reload-service", "restart-container", "start-guest"}
}

// RulesetSeeder is the subset of the policy-ruleset store SeedDefaults needs (satisfied by db.PolicyRulesetStore).
type RulesetSeeder interface {
	Load(ctx context.Context) (RuleSet, []byte, error)
	Save(ctx context.Context, document []byte, updatedBy string) (RuleSet, error)
}

// GraduationSeeder is the subset of the graduation store SeedDefaults needs (satisfied by db.PolicyGraduationStore).
type GraduationSeeder interface {
	Load(ctx context.Context, opClass string) (ClassState, error)
	Save(ctx context.Context, st ClassState) error
}

// SeedDefaults establishes the out-of-box curated Semi-auto baseline on a FRESH deployment and returns the
// effective ruleset to run with. It is ABSENT-ONLY and idempotent end-to-end:
//   - the ruleset is seeded ONLY when the store reports ErrRulesetAbsent (no operator document) — an existing
//     operator OR previously-seeded document is never overwritten; a corrupt/other load error fails closed to
//     the empty ruleset (every action → approve) and does NOT seed over it;
//   - each curated op-class is graduated to LevelAuto ONLY when it has no persisted ladder state
//     (ErrClassAbsent) — an earned or operator-tuned class is never downgraded or overwritten.
//
// It writes autonomy defaults but never lifts the mode: mutation stays Shadow by default, so a seeded class
// actuates only once an operator deliberately escalates the mode. A seed write failure is logged and tolerated
// (fail-closed — the class simply stays at approve), never fatal to boot.
func SeedDefaults(ctx context.Context, rules RulesetSeeder, grad GraduationSeeder, logf func(string, ...any)) RuleSet {
	loaded, _, err := rules.Load(ctx)
	freshDeploy := errors.Is(err, ErrRulesetAbsent)
	switch {
	case freshDeploy:
		if seeded, serr := rules.Save(ctx, DefaultRuleSetDocument(), "default-seed"); serr == nil {
			logf("policy: seeded curated default ruleset (%d rules) on a fresh deployment", len(seeded.Rules))
			loaded = seeded
		} else {
			logf("policy: curated default ruleset seed failed: %v — fail-closed to empty (every action approve)", serr)
			loaded, freshDeploy = RuleSet{}, false // seed did not take — do NOT then graduate under an empty ruleset
		}
	case err != nil:
		loaded = RuleSet{} // corrupt/other load error ⇒ fail-closed; do NOT seed over it.
	}
	// Seed graduation ONLY on a fresh deploy (as part of the same baseline the ruleset seed established) — never
	// touch the earned-trust ladder when an operator already has a ruleset: they may deliberately leave a class
	// earning, and a custom ruleset need not grant these classes auto at all. Still absent-only per class.
	if freshDeploy {
		for _, opClass := range DefaultGraduatedClasses() {
			if _, gerr := grad.Load(ctx, opClass); errors.Is(gerr, ErrClassAbsent) {
				if serr := grad.Save(ctx, ClassState{OpClass: opClass, Level: LevelAuto, LastOutcome: OutcomeVerifiedClean}); serr == nil {
					logf("policy: seeded curated op-class %q to auto on a fresh deployment (mode-gated; Shadow default)", opClass)
				} else {
					logf("policy: graduation seed for %q failed: %v — the class stays approve (fail-closed)", opClass, serr)
				}
			}
		}
	}
	return loaded
}
