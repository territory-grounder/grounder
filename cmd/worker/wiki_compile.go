package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/knowledge"
	"github.com/territory-grounder/grounder/core/wikicompile"
)

// wikiCompileDeps is everything the lane reads. Interfaces rather than concrete stores so the lane's own
// oracle can drive it without a database — the compile logic is pure (core/wikicompile) and this file is
// the impure shell around it, so the shell is where a fake belongs.
type wikiCompileDeps struct {
	Roster       func(ctx context.Context) ([]string, error)
	SourceCounts func(ctx context.Context) (sessions, hosts int, err error)
	PriorFor     func(ctx context.Context, host string, limit int) ([]db.PriorTriage, error)
	RuleSessions func(ctx context.Context, limit int) ([]db.WikiRuleSession, error)
	// Ratified / Candidates back the op-class pages' STANDING section. Both may fail independently of
	// the session read, and a failure is reported as unknown rather than as "not ratified".
	Ratified   func(ctx context.Context) (map[string]bool, error)
	Candidates func(ctx context.Context) (map[string]string, error)
	// Decisions backs the governance digest. Independent of every other read: a failed ledger read costs
	// the digest and nothing else.
	Decisions func(ctx context.Context) ([]db.WikiDecisionTally, int, error)
	// Seams reports every DECLARED wiring seam and whether it is live, for the lane-health page.
	Seams  func() []wikicompile.SeamStatus
	Edges  func(ctx context.Context) ([]wikicompile.HostEdge, error)
	Corpus func() []knowledge.Incident
	Now    func() time.Time
}

// perHostSessionLimit bounds one host's read. It is deliberately far above the console's old 200-row
// ESTATE-WIDE window, because this read is PER HOST: the window that starved the console was global, so a
// host with 40 incidents could show none of them while a noisier neighbour filled the page.
const perHostSessionLimit = 500

// ruleSessionLimit bounds the ONE read behind every rule page. Generous because the pages group across
// the whole estate and an undercount argues the wrong way — recurrence is the decision input. Hitting it
// is reported on every page as a FLOOR rather than swallowed.
const ruleSessionLimit = 5000

// compileWikiArticles reads the spine, compiles per-host pages, and replaces the envelope atomically.
//
// FAILURE POLICY, which is the whole design. A roster read that fails aborts the compile and leaves the
// PREVIOUS envelope in place: publishing a partial wiki would silently retire every host it could not see,
// and a stale-but-complete wiki is more honest than a fresh-and-truncated one (the surface renders the
// envelope's compiled_at, so staleness is visible). A single HOST's read failing does not abort — that
// host is skipped WITH ITS REASON carried into the envelope, because one unreachable row must not blind
// the other 77, and because a skipped host must be visibly skipped rather than silently absent.
// It returns the PAIR the seam-yield register needs — hosts OFFERED by the roster and articles PRODUCED —
// rather than the article count alone. Returning only the numerator is the exact habit that let a lane
// report "12 articles written" while the roster it was supposed to cover held 78 hosts: a number with no
// denominator cannot be wrong, because there is nothing for it to disagree with.
func compileWikiArticles(ctx context.Context, path string, d wikiCompileDeps) (offered int, produced int, err error) {
	roster, err := d.Roster(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("roster: %w", err)
	}
	edges, err := d.Edges(ctx)
	if err != nil {
		// The estate graph is ENRICHMENT, not the spine. Losing it costs the dependency sections, which
		// then honestly say they have nothing — far better than abandoning the whole compile.
		edges = nil
	}
	corpus := d.Corpus()

	facts := make([]wikicompile.HostFacts, 0, len(roster))
	for _, host := range roster {
		f := wikicompile.HostFacts{Host: host}
		rows, rerr := d.PriorFor(ctx, host, perHostSessionLimit)
		if rerr != nil {
			f.SessionsErr = rerr // carried, never swallowed — CompileHosts turns this into a Skip
		} else {
			f.SessionsTruncated = len(rows) >= perHostSessionLimit
			for _, r := range rows {
				f.Sessions = append(f.Sessions, wikicompile.HostSession{
					ExternalRef: r.ExternalRef, AlertRule: r.AlertRule, Outcome: r.Outcome,
					OpClass: r.OpClass, Proposed: r.Proposed, Mutated: r.Mutated,
					ConfirmedClear: r.ConfirmedClear, Conclusion: r.Conclusion, CreatedAt: r.CreatedAt,
				})
			}
		}
		for _, e := range edges {
			if e.From == host || e.To == host {
				f.Edges = append(f.Edges, e)
			}
		}
		for _, c := range corpus {
			if c.Host == host {
				f.Precedents = append(f.Precedents, wikicompile.HostPrecedent{
					ExternalRef: c.ExternalRef, AlertRule: c.AlertRule,
					Summary: c.Summary, Resolution: c.Resolution, ResolvedAt: c.ResolvedAt,
				})
			}
		}
		facts = append(facts, f)
	}

	articles, skips := wikicompile.CompileHosts(wikicompile.HostInputs{Facts: facts})

	sessions, hosts, cerr := d.SourceCounts(ctx)
	// "host_articles", not "articles": the coverage page is appended below and is itself an article, so
	// a bare "articles" count would read as inconsistent with the envelope it sits in.
	sources := map[string]int{"hosts_in_roster": len(roster), "host_articles": len(articles), "skipped": len(skips)}
	if cerr == nil {
		sources["triage_sessions"] = sessions
		sources["distinct_hosts"] = hosts
	}

	// PER-RULE PAGES. One bounded read, folded into rule FAMILIES here in the impure shell — the pure
	// compiler stays dependency-free, and knowledge.CanonicalRule keeps its single home as the alias
	// authority. Production carries one fault class under four source names; keying on the raw string
	// would split that page four ways and hide the recurrence that makes it worth reading.
	//
	// A failed read costs the rule pages and nothing else: the host pages and the coverage page are
	// already built, and dropping the whole compile because one read failed would retire every host page
	// too. The coverage page reports what was actually produced either way.
	if d.RuleSessions != nil {
		if rows, rerr := d.RuleSessions(ctx, ruleSessionLimit); rerr != nil {
			log.Printf("wiki compile: rule sessions unreadable: %v (rule pages skipped, host pages unaffected)", rerr)
			sources["rule_sessions_error"] = 1
		} else {
			rsess := make([]wikicompile.RuleSession, 0, len(rows))
			for _, r := range rows {
				rsess = append(rsess, wikicompile.RuleSession{
					ExternalRef: r.ExternalRef, Host: r.Host,
					Rule:    knowledge.CanonicalRule(r.AlertRule),
					RawRule: r.AlertRule,
					Outcome: r.Outcome, OpClass: r.OpClass,
					Mutated: r.Mutated, ConfirmedClear: r.ConfirmedClear, CreatedAt: r.CreatedAt,
				})
			}
			ruleArts, ruleSkips := wikicompile.CompileRules(wikicompile.RuleInputs{
				Sessions:  rsess,
				Truncated: len(rows) >= ruleSessionLimit,
			})
			articles = append(articles, ruleArts...)
			skips = append(skips, ruleSkips...)
			sources["rule_sessions"] = len(rows)
			sources["rule_pages"] = len(ruleArts)

			// PER-OP-CLASS PAGES, from the SAME sessions — every triage row already carries its op_class,
			// so a second read would buy nothing.
			//
			// Keyed on USE, not on ratification: production holds ZERO ratified classes against seven
			// actually used and 460 executions, so a page set gated on the catalogue would render nothing
			// and read as "nothing to see" rather than "the earning ladder is not built yet".
			ratified, rok := map[string]bool{}, true
			if d.Ratified != nil {
				if m, err := d.Ratified(ctx); err != nil {
					// UNKNOWN, not empty. A failed read must never render as a confident "not ratified"
					// on every page — that is a claim about the catalogue derived from not having seen it.
					log.Printf("wiki compile: ratified catalogue unreadable: %v (standing reported as unknown)", err)
					rok = false
				} else {
					ratified = m
				}
			} else {
				rok = false
			}
			cands := map[string]string{}
			if d.Candidates != nil {
				if m, err := d.Candidates(ctx); err != nil {
					log.Printf("wiki compile: candidacy unreadable: %v (candidacy omitted from op-class pages)", err)
				} else {
					cands = m
				}
			}
			opArts, opSkips := wikicompile.CompileOpClasses(wikicompile.OpClassInputs{
				Sessions: rsess, Ratified: ratified, RatifiedKnown: rok,
				Candidates: cands, Truncated: len(rows) >= ruleSessionLimit,
			})
			articles = append(articles, opArts...)
			skips = append(skips, opSkips...)
			sources["opclass_pages"] = len(opArts)
			sources["opclasses_ratified"] = len(ratified)
		}
	}

	// THE GOVERNANCE DECISIONS DIGEST. The ledger is hash-chained, complete, and unreadable at scale
	// (8,570 rows in production); the console shows it row by row, which answers "what happened at seq
	// 8412" and nothing else. This reports its SHAPE — and specifically the 1,616 withheld decisions,
	// which are TG declining to act and are invisible outside the chain.
	if d.Decisions != nil {
		if tallies, total, derr := d.Decisions(ctx); derr != nil {
			log.Printf("wiki compile: governance ledger unreadable: %v (decisions digest skipped)", derr)
			sources["decisions_error"] = 1
		} else {
			dt := make([]wikicompile.DecisionTally, 0, len(tallies))
			for _, t := range tallies {
				dt = append(dt, wikicompile.DecisionTally{
					Decision: t.Decision, Total: t.Total, Withheld: t.Withheld, Newest: t.Newest,
				})
			}
			articles = append(articles, wikicompile.CompileDecisions(wikicompile.DecisionInputs{
				Tallies: dt, LedgerTotal: total,
			}))
			sources["ledger_rows"] = total
			sources["decision_kinds"] = len(dt)
		}
	}

	// THE LANE HEALTH PAGE. The manifest's consequence prose — what each darkness costs — currently
	// reaches a boot log and a ledger row, neither of which anyone reads at 3am. This renders it where
	// someone might.
	if d.Seams != nil {
		if seams := d.Seams(); len(seams) > 0 {
			articles = append(articles, wikicompile.CompileLanes(wikicompile.LaneInputs{Seams: seams}))
			dark := 0
			for _, sm := range seams {
				if sm.Dark {
					dark++
				}
			}
			sources["seams_declared"] = len(seams)
			sources["seams_dark"] = dark
		}
	}

	// The coverage page, compiled from THIS pass's own results. It is appended to the same article set it
	// describes, so the two cannot drift: there is no second source of truth for it to disagree with. The
	// predecessor's equivalent asserts a hardcoded Status per source and gets at least two of them wrong in
	// the run that prints them (wiki-compile.py:1092).
	articles = append(articles, wikicompile.CompileCoverage(wikicompile.CoverageInputs{
		RosterSize:  len(roster),
		Articles:    articles,
		Skipped:     skips,
		Sources:     sources,
		EstateEdges: len(edges),
		CorpusRows:  len(corpus),
	}))

	env := wikicompile.Envelope{
		SchemaVersion: wikicompile.SchemaVersion,
		CompiledAt:    d.Now().UTC(),
		Sources:       sources,
		Skipped:       skips,
		Articles:      articles,
	}

	// Atomic replace, same discipline as the corpus write: a reader must never observe a torn file.
	tmp := path + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return 0, 0, fmt.Errorf("create %s: %w", tmp, err)
	}
	if werr := wikicompile.WriteArticles(out, env); werr != nil {
		out.Close()
		os.Remove(tmp)
		return 0, 0, fmt.Errorf("serialize: %w", werr)
	}
	if cerr := out.Close(); cerr != nil {
		os.Remove(tmp)
		return 0, 0, fmt.Errorf("close %s: %w", tmp, cerr)
	}
	if rerr := os.Rename(tmp, path); rerr != nil {
		os.Remove(tmp)
		return 0, 0, fmt.Errorf("replace %s: %w", path, rerr)
	}
	return len(roster), len(articles), nil
}
