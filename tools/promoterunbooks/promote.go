package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/territory-grounder/grounder/core/skillstore"
)

// seedAuthor is the provenance the TG-36 seeder (tools/seedskills) stamps on every row it creates —
// the ONLY author whose runbook drafts this tool graduates. Anyone else's draft is not ours to promote.
const seedAuthor = "tool:seedskills"

// promoteActor names this tool in every rationale it writes (the transition machine appends the
// rationale to the row's append-only log and the governance ledger records it).
const promoteActor = "tool:promoterunbooks"

// ---------------------------------------------------------------------------------------------------
// The store surface.
// ---------------------------------------------------------------------------------------------------

// Identity is one skill identity row's selection surface: the name and the LOAD-BEARING class. The
// class filter is what keeps a SKILL-class draft — same seeded author, same draft status — out of this
// tool's reach entirely: skills earn production through a trial, only runbooks publish directly.
type Identity struct {
	Name  string
	Class skillstore.ArtifactClass
}

// VersionRow is the slice of one skill_version row the promotion decision reads.
type VersionRow struct {
	ID      int64
	Version string
	Status  skillstore.Status
	Author  string
}

// Store is the narrow surface the promoter drives — the EXISTING skill-store API, nothing else. The
// pgx-backed db.SkillStore satisfies it via dbStore (main.go): reads through ListSkills/SkillDetail
// (the console's library and history reads) and the write through skillstore.Transition — the ONE
// audited state machine behind the console's POST /v1/skills/versions/{id}/promote verb (executed
// there by the worker's skillwrite.TransitionActivity). Tests drive the same state machine over the
// in-memory skillstore.MemStore.
type Store interface {
	Skills(ctx context.Context) ([]Identity, error)
	Versions(ctx context.Context, name string) ([]VersionRow, error)
	Transition(ctx context.Context, versionID int64, to skillstore.Status, rationale string) (skillstore.Version, error)
}

// ---------------------------------------------------------------------------------------------------
// Plan.
// ---------------------------------------------------------------------------------------------------

// Action is one name's planned disposition.
type Action string

const (
	ActionPromote Action = "promote" // Transition(draft → production) on the seeded draft row
	ActionSkip    Action = "skip"    // already production, or nothing pending — a re-run is a no-op
	ActionRefuse  Action = "refuse"  // a runbook draft this tool must not graduate — a human decides
)

// PlanItem is one runbook name's decided action; Row is the version row the decision is about.
type PlanItem struct {
	Name   string
	Action Action
	Row    VersionRow
	Reason string
}

// Plan is the decided run over every runbook-class identity with seeded involvement or pending drafts.
type Plan struct{ Items []PlanItem }

// Counts is the run's honest arithmetic.
type Counts struct{ Promoted, Skipped, Refused int }

func (p Plan) counts() Counts {
	var c Counts
	for _, it := range p.Items {
		switch it.Action {
		case ActionPromote:
			c.Promoted++ // planned promotes; executePlan converts plan to fact
		case ActionSkip:
			c.Skipped++
		case ActionRefuse:
			c.Refused++
		}
	}
	return c
}

// decide is the promotion law for ONE runbook-class name's version rows (listed=false: the name has
// no seeded involvement and nothing pending — out of this tool's scope, e.g. a flywheel-owned runbook):
//
//   - a seeded draft exists, but a production incumbent from ANOTHER writer would be retired by the
//     promote's incumbent-supersede (REQ-1302) → refuse (superseding someone else's live wiki page is
//     an operator's call, not the seeding rail's);
//   - a seeded draft exists, but a NEWER seeded production row already serves → skip (promoting the
//     leftover older draft would supersede the newer page — a downgrade, not a graduation);
//   - a seeded draft exists → promote the NEWEST one;
//   - a seeded row is already production → skip (idempotency: run 2 promotes zero);
//   - seeded rows exist but none pending (operator rejected/retired them) → skip, the decision stands;
//   - only FOREIGN-authored drafts exist → refuse (someone else's draft is not ours to promote).
func decide(name string, rows []VersionRow) (PlanItem, bool) {
	var seededDraft, seededProd, incumbent, foreignDraft, newestSeeded VersionRow
	for _, r := range rows {
		seeded := r.Author == seedAuthor
		switch {
		case seeded && r.Status == skillstore.StatusDraft && r.ID > seededDraft.ID:
			seededDraft = r
		case seeded && r.Status == skillstore.StatusProduction:
			seededProd = r
		case !seeded && r.Status == skillstore.StatusDraft && r.ID > foreignDraft.ID:
			foreignDraft = r
		}
		if r.Status == skillstore.StatusProduction {
			incumbent = r
		}
		if seeded && r.ID > newestSeeded.ID {
			newestSeeded = r
		}
	}
	switch {
	case seededDraft.ID != 0 && incumbent.ID != 0 && incumbent.Author != seedAuthor:
		return PlanItem{Name: name, Action: ActionRefuse, Row: seededDraft, Reason: fmt.Sprintf(
			"promoting would retire production v%s authored by %q (incumbent supersede, REQ-1302) — superseding another writer's live page is an operator's call", incumbent.Version, incumbent.Author)}, true
	case seededDraft.ID != 0 && seededProd.ID > seededDraft.ID:
		return PlanItem{Name: name, Action: ActionSkip, Row: seededDraft, Reason: fmt.Sprintf(
			"production v%s is newer than the leftover seeded draft v%s — nothing to graduate", seededProd.Version, seededDraft.Version)}, true
	case seededDraft.ID != 0:
		return PlanItem{Name: name, Action: ActionPromote, Row: seededDraft,
			Reason: "seeded runbook draft → production (TG-476 lane: the class never trials)"}, true
	case seededProd.ID != 0:
		return PlanItem{Name: name, Action: ActionSkip, Row: seededProd,
			Reason: fmt.Sprintf("already production (v%s) — a re-run is a no-op", seededProd.Version)}, true
	case foreignDraft.ID != 0:
		return PlanItem{Name: name, Action: ActionRefuse, Row: foreignDraft, Reason: fmt.Sprintf(
			"runbook draft v%s is authored by %q, not %s — someone else's draft is not ours to promote", foreignDraft.Version, foreignDraft.Author, seedAuthor)}, true
	case newestSeeded.ID != 0:
		return PlanItem{Name: name, Action: ActionSkip, Row: newestSeeded, Reason: fmt.Sprintf(
			"no pending seeded draft — the newest seeded row is %s (an operator decision stands)", newestSeeded.Status)}, true
	}
	return PlanItem{}, false
}

// buildPlan decides every runbook-class identity against the store. The class filter sits HERE, at
// selection: a non-runbook identity never reaches decide, so a matching-author matching-status
// SKILL-class draft is structurally untouchable by this tool.
func buildPlan(ctx context.Context, st Store) (Plan, error) {
	ids, err := st.Skills(ctx)
	if err != nil {
		return Plan{}, fmt.Errorf("list skills: %w", err)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].Name < ids[j].Name })
	var p Plan
	for _, id := range ids {
		if skillstore.DefaultClass(id.Class) != skillstore.ClassRunbook {
			continue
		}
		rows, err := st.Versions(ctx, id.Name)
		if err != nil {
			return Plan{}, fmt.Errorf("read versions of %s: %w", id.Name, err)
		}
		if it, listed := decide(id.Name, rows); listed {
			p.Items = append(p.Items, it)
		}
	}
	return p, nil
}

// rationaleFor is the fixed graduation sentence every promoted row carries (the transition machine
// prefixes "[production]" in the row log and ledger-records it).
func rationaleFor(name string) string {
	return fmt.Sprintf("promoted draft→production by %s (TG-36 runbook-graduation rail; owner ruling TG-488): "+
		"a RUNBOOK-class draft publishes straight to production — the class never trials (TG-476 lane; the "+
		"one-production invariant and incumbent supersede apply) — and this page now serves at the wiki "+
		"destination GET /v1/wiki/runbook/%s.", promoteActor, name)
}

// Result is an executed run: what actually promoted, plus any per-row transition refusals.
type Result struct {
	Counts             Counts
	Promoted           []PlanItem // as-executed, the input to post-run verification
	TransitionRefusals []string
}

// executePlan runs the plan with PER-ROW isolation: a Transition error refuses THAT row and continues
// — unlike the sibling seeder's whole-run refusal, because promotion is per-artifact graduation (each
// row's draft→production is its own complete, already-governed act; one bad row must not strand the
// other nineteen), whereas seeding validates one corpus whose internal drift poisons every write.
// Planned refusals are never attempted; the caller exits non-zero when ANY row refused.
//
// Each promote RE-DECIDES against live state at the moment of actuation, not from the plan snapshot.
// skillstore.Transition's incumbent-supersede (REQ-1302, transition.go) retires whatever is production
// RIGHT NOW with NO author check, so if a foreign page became production between buildPlan and here (a
// concurrent worker append — the seq-PK collision the package doc describes serializes the LEDGER, not
// this window), the plan-time foreign-incumbent refusal would be bypassed and a stranger's live wiki
// page silently retired. Re-running the SAME decide law over fresh rows closes that TOCTOU: the happy
// path is unchanged (fresh rows == planned rows), and only a genuine concurrent modification diverts —
// a now-Refuse verdict re-refuses (non-zero exit), a now-Skip verdict (our row already promoted, or a
// newer page serves) is benign, and a promote of a DIFFERENT row than planned is refused for a clean
// idempotent re-run rather than acted on unseen.
func executePlan(ctx context.Context, p Plan, st Store) Result {
	var res Result
	for _, it := range p.Items {
		switch it.Action {
		case ActionSkip:
			res.Counts.Skipped++
		case ActionRefuse:
			res.Counts.Refused++
		case ActionPromote:
			rows, err := st.Versions(ctx, it.Name)
			if err != nil {
				res.Counts.Refused++
				res.TransitionRefusals = append(res.TransitionRefusals, fmt.Sprintf(
					"%s (version id %d): re-read before promote failed — %v", it.Name, it.Row.ID, err))
				continue
			}
			switch live, _ := decide(it.Name, rows); {
			case live.Action == ActionSkip:
				// A concurrent writer already made this moot (our row promoted, or a newer page serves).
				res.Counts.Skipped++
				continue
			case live.Action != ActionPromote || live.Row.ID != it.Row.ID:
				res.Counts.Refused++
				res.TransitionRefusals = append(res.TransitionRefusals, fmt.Sprintf(
					"%s v%s (version id %d): live state diverged since selection (now %s: %s) — not promoted",
					it.Name, it.Row.Version, it.Row.ID, live.Action, live.Reason))
				continue
			}
			v, err := st.Transition(ctx, it.Row.ID, skillstore.StatusProduction, rationaleFor(it.Name))
			if err != nil {
				res.Counts.Refused++
				res.TransitionRefusals = append(res.TransitionRefusals, fmt.Sprintf(
					"%s v%s (version id %d): transition refused — %v", it.Name, it.Row.Version, it.Row.ID, err))
				continue
			}
			res.Counts.Promoted++
			it.Reason = fmt.Sprintf("promoted (ledger seq %d)", v.LedgerSeq)
			res.Promoted = append(res.Promoted, it)
		}
	}
	return res
}

// verifyPromoted re-reads every promoted name through the store and requires the promoted row to BE
// the production row — the exact resolve condition the wiki serves (wikiRunbookPageHandler: a
// runbook-class identity with a production version answers GET /v1/wiki/runbook/{name}).
func verifyPromoted(ctx context.Context, st Store, promoted []PlanItem) error {
	var missing []string
	for _, it := range promoted {
		rows, err := st.Versions(ctx, it.Name)
		if err != nil {
			return fmt.Errorf("post-run verify %s: %w", it.Name, err)
		}
		ok := false
		for _, r := range rows {
			ok = ok || (r.ID == it.Row.ID && r.Status == skillstore.StatusProduction)
		}
		if !ok {
			missing = append(missing, fmt.Sprintf("%s (version id %d)", it.Name, it.Row.ID))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("post-run verification FAILED — promoted rows not re-readable as production: %s",
			strings.Join(missing, ", "))
	}
	return nil
}
