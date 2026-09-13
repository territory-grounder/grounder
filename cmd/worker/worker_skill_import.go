package main

// Compiled-registry SEEDING of the skill store, carved out of main()'s composition root (TG-501 LOC-debt
// paydown). Seeds the compiled skills as production rows (identities first, conservative-remediation pinned),
// the base-prompt GUIDANCE row (ClassPrompt, flywheel-eligible), and the judge-rubric MIRROR row — each with
// the same supersede-on-version-bump idiom, degrading to the embedded fallback on error so composition is
// never blocked. Behaviour is unchanged by the move.

import (
	"context"
	"log"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/agent/skills"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/execclass"
	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/skillstore"
	skillcorpus "github.com/territory-grounder/grounder/skills"
)

// importCompiledSkills idempotently seeds the skill store from the compiled registry (spec/014
// REQ-1304): identities first (conservative-remediation pinned — the hard floor), then each compiled
// body as a production row. A compiled UPGRADE (new version in code) supersedes a prior compiled-import
// row through the audited Transition; a GRADUATED store row is never displaced. Degrades on error —
// composition falls back to the compiled registry regardless.
func importCompiledSkills(ctx context.Context, st *db.SkillStore, lg *audit.Ledger) {
	compiled := skills.Default().All()
	imported := 0
	for i, sk := range compiled {
		pinned := sk.Name == "conservative-remediation"
		// artifact_class stamped EXPLICITLY (never left to the column default): every compiled-registry
		// import IS an agent skill — the founding class of the store (REQ-1315, ADR-0017).
		if err := st.PutSkill(ctx, skillstore.Skill{Name: sk.Name, Kind: "behavioral", Pinned: pinned, Class: skillstore.ClassSkill, Position: i}); err != nil {
			log.Printf("skills: import identity %s: %v (degraded — compiled fallback covers it)", sk.Name, err)
			continue
		}
		// The compiled registry's selectors are Go funcs; their declarative equivalent for the store row
		// is the closed-vocabulary predicate. Compiled `always` skills map to the empty predicate; the
		// exec-class-scoped ones to the standard/deep pair (the compiled func remains authoritative for
		// composition of compiled-origin bodies — this row is the console/library representation).
		aw := skillstore.AppliesWhen{}
		if !sk.AppliesWhen(skills.Context{Phase: skills.PhaseInvestigate, ExecClass: execclass.FastAgent}) {
			aw = skillstore.AppliesWhen{ExecClasses: []string{string(execclass.StandardAgent), string(execclass.DeepInvestigation)}}
		}
		if cur, ok, err := st.ProductionVersion(ctx, sk.Name); err == nil && ok &&
			cur.Source == "compiled-import" && cur.Version != sk.Version {
			if _, terr := skillstore.Transition(ctx, st, lg, cur.ID, skillstore.StatusRetired,
				"compiled registry upgraded to v"+sk.Version); terr != nil {
				log.Printf("skills: supersede compiled %s v%s: %v", sk.Name, cur.Version, terr)
				continue
			}
		}
		if err := st.ImportCompiledVersion(ctx, sk.Name, sk.Version, sk.Body, aw); err != nil {
			// After a successful supersede-retire a failed import leaves the skill with NO production
			// row (crash window). Composition is unaffected (total compiled fallback) and the next boot
			// heals it (the NOT EXISTS guard admits the import), but retry once and log LOUDLY so the
			// console's library view being production-less for this skill is never a silent mystery.
			if rerr := st.ImportCompiledVersion(ctx, sk.Name, sk.Version, sk.Body, aw); rerr != nil {
				log.Printf("skills: import %s v%s FAILED TWICE: %v — library shows no production row until the next boot (composition unaffected)", sk.Name, sk.Version, rerr)
				continue
			}
		}
		imported++
	}
	log.Printf("skills: store seeded from the compiled registry (%d/%d skills)", imported, len(compiled))

	// C-3b (TG-114 leaf 2's flywheel half): the base prompt's GUIDANCE half as a ClassPrompt row, seeded
	// from the embedded bytes with the same supersede-on-version-bump idiom as a compiled skill. UNPINNED
	// by design — the prompt class is a first-class graduating artifact (the epic's settled Q1; the C-6
	// arming grant), so the flywheel may draft/trial/graduate it under its full existing rigor; the pin
	// stays the operator's standing brake. NewFromStore's class law (REQ-1316) keeps this row out of the
	// <behavioral_guidance> skill block; ONLY the runner's composeBasePrompt reads it, into the preamble's
	// guidance slot, with the embed as the total fallback.
	gbody, gver := agent.BasePromptGuidance()
	// Kind is the 0009 display taxonomy, CHECK-constrained to {behavioral, catalog} — "prompt" violated it
	// live (skill_kind_check 23514, caught in the 41bbceba boot log; the embed fallback held). The guidance
	// IS behavioral prose, so 'behavioral' is honest; the CLASS column is what says prompt. The gated DB
	// drill (TestBasePromptSeedRowSatisfiesTheSchema) now seeds this exact row against the real schema.
	if err := st.PutSkill(ctx, skillstore.Skill{Name: "base-prompt-guidance", Kind: "behavioral", Pinned: false, Position: 999, Class: skillstore.ClassPrompt}); err != nil {
		log.Printf("baseprompt: identity row: %v (degraded — the embedded guidance composes)", err)
	} else {
		if cur, ok, err := st.ProductionVersion(ctx, "base-prompt-guidance"); err == nil && ok &&
			cur.Source == "compiled-import" && cur.Version != gver {
			if _, terr := skillstore.Transition(ctx, st, lg, cur.ID, skillstore.StatusRetired,
				"embedded base-prompt-guidance upgraded to v"+gver); terr != nil {
				log.Printf("baseprompt: supersede v%s: %v", cur.Version, terr)
			}
		}
		if err := st.ImportCompiledVersion(ctx, "base-prompt-guidance", gver, gbody, skillstore.AppliesWhen{}); err != nil {
			if rerr := st.ImportCompiledVersion(ctx, "base-prompt-guidance", gver, gbody, skillstore.AppliesWhen{}); rerr != nil {
				log.Printf("baseprompt: import v%s FAILED TWICE: %v — the embedded guidance composes until the next boot", gver, rerr)
			}
		}
		log.Printf("baseprompt: guidance row seeded (v%s, class=prompt, unpinned — flywheel-eligible per the C-6 grant; embed remains the floor)", gver)
	}

	// TG-474 (ADR-0017): the judge-rubric MIRROR row — a pinned, rubric-class projection of the embedded
	// rubric so the console/library states WHICH rubric identity judges every session, off the same read
	// surface as every other prose artifact. The EMBED stays the sole runtime authority (core/judge reads
	// only its go:embed bytes); this row is a projection, and the class law (ErrRubricNeverDrafts) plus the
	// pin refuse every store-side edit. Version = judge.RubricVersion(), so a rubric bump supersedes and
	// re-imports on the next boot exactly like a compiled-skill upgrade; the drift check below says LOUDLY
	// when the stored projection stops matching the embed (an out-of-band SQL edit — the row lies).
	if err := st.PutSkill(ctx, skillstore.Skill{Name: "judge-rubric", Kind: "catalog", Pinned: true, Position: 1000, Class: skillstore.ClassRubric}); err != nil {
		log.Printf("skills: rubric mirror parent row: %v (the library will not state the judging rubric until the next boot)", err)
		return
	}
	rv, rbody := judge.RubricVersion(), string(judge.RubricJSON())
	if cur, ok, err := st.ProductionVersion(ctx, "judge-rubric"); err == nil && ok &&
		cur.Source == "compiled-import" && cur.Version != rv {
		if _, terr := skillstore.Transition(ctx, st, lg, cur.ID, skillstore.StatusRetired,
			"embedded judge rubric upgraded to "+rv); terr != nil {
			log.Printf("skills: supersede rubric mirror %s: %v", cur.Version, terr)
		}
	}
	if err := st.ImportCompiledVersion(ctx, "judge-rubric", rv, rbody, skillstore.AppliesWhen{}); err != nil {
		log.Printf("skills: rubric mirror import %s: %v", rv, err)
	}
	if cur, ok, err := st.ProductionVersion(ctx, "judge-rubric"); err == nil && ok {
		if cur.ContentHash != skillstore.ContentHash(rbody, skillstore.AppliesWhen{}) {
			log.Printf("skills: RUBRIC MIRROR DRIFT — the stored judge-rubric projection (v%s) does not match the embedded rubric (%s); the row LIES about what judges sessions until re-imported (out-of-band edit?)", cur.Version, rv)
		}
	}

	seedRunbookCorpus(ctx, st, lg)
}

// seedRunbookCorpus seeds the embedded runbook corpus (skills/runbook, package skillcorpus) as PRODUCTION
// rows — TG-529: merged runbook packs used to stop at the repo boundary, reachable in no venue until an
// operator ran seedskills --execute AND promoterunbooks --execute by hand; every future pack met the same
// fate silently. Owner-ruled 2026-08-22: worker-boot seed + auto-promote. Runbooks never compose into the
// agent seed (REQ-1316) — production here is a WIKI-visibility promotion, not an agent-behavior change.
//
// Same idiom as the compiled-skill import above: identity upsert, supersede a PRIOR BOOT-SEEDED row on a
// corpus version bump, ImportCompiledVersion (production, chain-appended, idempotent). Rows another author
// owns are never displaced: the import's own SQL skips when ANY production row exists, and a non-boot-
// sourced row (the 2026-08-14 manual distill seeds, a console edit) is counted and said loudly below
// rather than superseded — converging those is a deliberate manual re-seed, never a boot side effect.
// A corpus that cannot parse WHOLE refuses whole — a silent subset-seed would read as delivered.
func seedRunbookCorpus(ctx context.Context, st *db.SkillStore, lg *audit.Ledger) {
	rbs, err := skillcorpus.Runbooks()
	if err != nil {
		log.Printf("skills: runbook corpus REFUSED — nothing seeded this boot, the wiki serves whatever rows already exist: %v", err)
		return
	}
	if len(rbs) == 0 {
		log.Print("skills: runbook corpus is EMPTY — nothing to seed (a corpus/embed defect, not a clean state; the skills/runbook tree is never empty)")
		return
	}
	seeded, present := 0, 0
	for i, rb := range rbs {
		if err := st.PutSkill(ctx, skillstore.Skill{Name: rb.Name, Kind: "catalog", Pinned: false,
			Class: skillstore.ClassRunbook, Position: 2000 + i, Description: rb.Description}); err != nil {
			log.Printf("skills: runbook identity %s: %v (this pack stays unreachable until the next boot)", rb.Name, err)
			continue
		}
		if cur, ok, err := st.ProductionVersion(ctx, rb.Name); err == nil && ok {
			if cur.Source == "compiled-import" && cur.Version != rb.Version {
				if _, terr := skillstore.Transition(ctx, st, lg, cur.ID, skillstore.StatusRetired,
					"runbook corpus upgraded to v"+rb.Version); terr != nil {
					log.Printf("skills: supersede runbook %s v%s: %v", rb.Name, cur.Version, terr)
					continue
				}
			} else if cur.Source != "compiled-import" {
				present++
				continue
			}
		}
		if err := st.ImportCompiledVersion(ctx, rb.Name, rb.Version, rb.Body, skillstore.AppliesWhen{}); err != nil {
			log.Printf("skills: runbook import %s v%s: %v", rb.Name, rb.Version, err)
			continue
		}
		seeded++
	}
	log.Printf("skills: runbook corpus seeded — %d boot-seeded, %d held by another author (manual/console rows win), of %d embedded (TG-529; wiki-only, never composes)", seeded, present, len(rbs))
}
