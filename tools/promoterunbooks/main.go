// Command promoterunbooks is the runbook-graduation half of TG-36's rail: it promotes the RUNBOOK-class
// drafts the seeder (tools/seedskills) created to PRODUCTION, making them live wiki pages at
// GET /v1/wiki/runbook/{name} — through the store's EXISTING governed transition path and nothing else.
//
// # The transition entry point
//
// Every status change goes through skillstore.Transition — the ONE audited state machine
// (core/skillstore/transition.go, REQ-1301) behind the console's POST /v1/skills/versions/{id}/promote
// verb, where it is executed by the worker's skillwrite.TransitionActivity
// (temporal/skillwrite/skillwrite.go) over the pgx store and the governance ledger. This tool binds the
// SAME pair — db.SkillStore + an audit.Ledger continued from the persisted tail — and calls the same
// function, so the TG-476 runbook lane (a RUNBOOK draft promotes STRAIGHT to production; trial is
// refused for the class), the one-production invariant, the incumbent supersede (REQ-1302), the
// mandatory rationale, and the ledger record all apply exactly as they do to an operator's click.
// Nothing here hand-rolls an UPDATE.
//
// # The TG-489 chain: transitions are CHAIN-NEUTRAL
//
// The distillate tamper chain links VERSION CREATION, not status: ChainFacts binds id, name, version,
// recomputed content hash, author, source, parent (core/skillstore/chain.go), and only CreateVersion /
// ImportCompiledVersion append (core/db/skillstore_chain.go). A Transition writes through
// db.SkillStore.UpdateVersion, which touches ONLY status, rationale, ledger_seq, status_changed_at —
// none of them chain-bound. Promotion therefore moves the chain not at all (the real-store drill in
// realstore_test.go proves head-before == head-after), so this tool touches no chain primitive: no
// EnsureChain (an initializing write a read-mostly tool has no business making), no verify bracket.
// The governed record of a promotion lives in the GOVERNANCE ledger instead, appended by Transition
// itself before the row is written.
//
// # The governance ledger — a coordinator act
//
// The worker is normally the ledger's single writer (transitions from the console run THERE for
// exactly that reason). This tool mirrors worker boot — Tail() → audit.NewLedgerFromTail().WithSink()
// — and appends from its own process, which is safe against forking by construction: governance_ledger's
// seq is the primary key, so if a live worker appends concurrently one side fails CLOSED with a
// duplicate-seq error (a refused row here, or one refused decision there — retried on the worker's next
// restart-continued chain) and the chain never forks. Like seeding, promoting a real store is a
// COORDINATOR act (TG-36): run it in a quiet window, and the tool ships disarmed — the default is a
// dry-run that writes nothing, and the real write requires BOTH --execute and TG_SEED_DSN.
//
// # What a run does
//
//   - selects from the store (ListSkills + SkillDetail — the console's own reads): identities of class
//     RUNBOOK only (the load-bearing filter: a same-named-convention SKILL-class draft by the same
//     author is structurally out of reach — skills earn production through a trial), then per name the
//     seeded rows (author tool:seedskills) with status draft;
//   - per row: Transition(draft→production) with a rationale citing TG-36/TG-488 and the wiki
//     destination; already-production rows skip (a re-run promotes zero); a foreign-authored runbook
//     draft, a foreign production incumbent, or any Transition error REFUSES that row and CONTINUES
//     (per-row isolation — promotion is per-artifact graduation; one bad row must not strand nineteen);
//   - post-run: every promoted name is re-read and must resolve as the production runbook row — the
//     exact condition the wiki serves;
//   - exit is non-zero when ANY row refused (the promoted rows STAY promoted — reconcile and re-run).
//
// Not merge-eval-gated: runbooks never compose into the agent seed (TestRunbookNeverComposes; the
// TG-476 compose filter) — promotion changes the WIKI read surface only.
//
// Usage:
//
//	TG_SEED_DSN=... go run ./tools/promoterunbooks            # DRY-RUN (default): print the plan, write nothing
//	TG_SEED_DSN=... go run ./tools/promoterunbooks --execute  # promote, then verify production rows
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/skillstore"
)

// dbStore adapts the pgx-backed skill store to the promoter's Store surface using only EXISTING
// store API: ListSkills/SkillDetail (the console's reads) and skillstore.Transition (the console
// promote verb's state machine) bound to the store and the tail-continued governance ledger.
type dbStore struct {
	s  *db.SkillStore
	lg skillstore.Ledger // nil in a dry-run — the plan never transitions
}

func (d dbStore) Skills(ctx context.Context) ([]Identity, error) {
	sums, err := d.s.ListSkills(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Identity, 0, len(sums))
	for _, s := range sums {
		out = append(out, Identity{Name: s.Name, Class: skillstore.ArtifactClass(s.ArtifactClass)})
	}
	return out, nil
}

func (d dbStore) Versions(ctx context.Context, name string) ([]VersionRow, error) {
	det, found, err := d.s.SkillDetail(ctx, name)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("skill %s vanished between list and detail", name)
	}
	out := make([]VersionRow, 0, len(det.Versions))
	for _, v := range det.Versions {
		out = append(out, VersionRow{ID: v.ID, Version: v.Version, Status: skillstore.Status(v.Status), Author: v.Author})
	}
	return out, nil
}

func (d dbStore) Transition(ctx context.Context, versionID int64, to skillstore.Status, rationale string) (skillstore.Version, error) {
	if d.lg == nil {
		return skillstore.Version{}, errors.New("dry-run store cannot transition — planning never writes (this is a bug)")
	}
	return skillstore.Transition(ctx, d.s, d.lg, versionID, to, rationale)
}

func printPlan(p Plan) {
	for _, it := range p.Items {
		fmt.Printf("  %-8s %-28s v%-14s (version id %d)  %s\n", it.Action, it.Name, it.Row.Version, it.Row.ID, it.Reason)
	}
}

func run() error {
	execute := flag.Bool("execute", false,
		"actually promote (requires TG_SEED_DSN). Without it this is a DRY-RUN: plan only, nothing written")
	flag.Parse()
	ctx := context.Background()

	dsn := os.Getenv("TG_SEED_DSN")
	if dsn == "" {
		return errors.New("TG_SEED_DSN required — the promote plan is decided against the store (a dry-run only reads; " +
			"--execute writes); promoting a real store is a coordinator act (TG-36), this tool never guesses a database")
	}
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect TG_SEED_DSN: %w", err)
	}
	defer pool.Close()
	st := dbStore{s: db.NewSkillStore(pool)}
	if *execute {
		// The worker-boot mirror: continue the governance chain from its persisted tail, every append
		// mirrored write-through (a sink failure fails the Transition closed — no unrecorded promotion).
		lstore := db.NewLedgerStore(pool)
		seq, hash, terr := lstore.Tail(ctx)
		if terr != nil {
			return fmt.Errorf("governance ledger tail: %w", terr)
		}
		st.lg = audit.NewLedgerFromTail(seq, hash).WithSink(lstore)
	}

	p, err := buildPlan(ctx, st)
	if err != nil {
		return err
	}
	if len(p.Items) == 0 {
		return errors.New("no seeded runbook rows found — has the TG-36 seeder (tools/seedskills) run against this " +
			"store? an empty promotion run is a broken premise, not a no-op")
	}
	printPlan(p)
	pc := p.counts()

	if !*execute {
		fmt.Printf("promoterunbooks DRY-RUN: would promote %d, skip %d, refuse %d — NOTHING WRITTEN\n",
			pc.Promoted, pc.Skipped, pc.Refused)
		if pc.Refused > 0 {
			return fmt.Errorf("dry-run found %d refusals — --execute would promote the other rows and exit non-zero "+
				"until these are reconciled", pc.Refused)
		}
		return nil
	}

	res := executePlan(ctx, p, st)
	for _, line := range res.TransitionRefusals {
		fmt.Println("  refuse  ", line)
	}
	if err := verifyPromoted(ctx, st, res.Promoted); err != nil {
		return err
	}
	fmt.Printf("promoterunbooks: promoted %d, skipped %d, refused %d — every promoted name re-reads as the "+
		"production runbook row (the wiki's resolve condition)\n", res.Counts.Promoted, res.Counts.Skipped, res.Counts.Refused)
	if res.Counts.Refused > 0 {
		return fmt.Errorf("%d rows refused — the %d promoted rows STAY promoted (per-row isolation); reconcile and "+
			"re-run (idempotent: already-production rows skip)", res.Counts.Refused, res.Counts.Promoted)
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "promoterunbooks:", err)
		os.Exit(1)
	}
}
