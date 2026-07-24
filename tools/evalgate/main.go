// Command evalgate is the deterministic half of TG's binding eval gate (TG-43 / audit R4; drift fix TG-64).
//
// It reads scorecards produced on-box by the LLM-judge harness (see eval/eval-gate.sh) + optional negative
// controls, pools the runs, applies the fixed mechanical thresholds (eval/gate.Compare), prints a per-
// dimension table with an explicit PASS/FAIL, and EXITS NON-ZERO on FAIL so it can gate a merge or a
// scheduled pipeline. It performs NO SSH and NO model calls — the noisy on-box run happens in the shell;
// this binary is pure comparison, unit-tested in eval/gate.
//
// Two comparison modes (TG-64):
//
//	--mode change (DEFAULT, the pre-merge gate): compare the CANDIDATE arm to a FRESH BASE arm (origin/main
//	  measured in the SAME window, passed via --base). Drift cancels between the two arms — this is the fix
//	  for the stale-baseline flaw where model/estate/main drift was charged to the candidate's change.
//	    go run ./tools/evalgate --mode change --runs 2 \
//	      --base      eval/out/scorecard.base.run1.json --base      eval/out/scorecard.base.run2.json \
//	      --candidate eval/out/scorecard.cand.run1.json --candidate eval/out/scorecard.cand.run2.json \
//	      --controls  eval/out/controls.run1.json       --controls  eval/out/controls.run2.json
//
//	--mode trend (the nightly drift-watch): compare a clean main measurement to the COMMITTED baseline
//	  (--baseline) for long-horizon tracking, and self-refresh that baseline on a clean, non-regressing run.
//	    go run ./tools/evalgate --mode trend --runs 2 --baseline eval/baseline-scorecard.json \
//	      --candidate ... --controls ... --refresh-baseline eval/baseline-scorecard.json --git-sha <sha>
//
// A third one-shot form, --verify-integrity, is the arm-integrity probe the shell runs after each arm so a
// contended/429 (degraded/short) arm is reran/aborted before it can enter the pooled verdict.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/eval"
	"github.com/territory-grounder/grounder/eval/gate"
)

type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error {
	// Accept both repeated flags and comma-separated lists.
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*s = append(*s, p)
		}
	}
	return nil
}

func main() {
	var candidates, base, controls, verify stringSlice
	mode := flag.String("mode", "change", `comparison mode: "change" (pre-merge; candidate vs FRESH base arm via --base) or "trend" (nightly; vs committed --baseline)`)
	baseline := flag.String("baseline", "eval/baseline-scorecard.json", "committed trend baseline (comparator in --mode trend only)")
	flag.Var(&base, "base", "FRESH base-arm scorecard JSON (repeatable/comma-sep) — origin/main measured in the same window; the --mode change comparator")
	flag.Var(&candidates, "candidate", "candidate scorecard JSON (repeatable/comma-sep) — the run(s) to gate")
	flag.Var(&controls, "controls", "negative-control result JSON (repeatable/comma-sep) — optional")
	flag.Var(&verify, "verify-integrity", "integrity-only probe: verify these scorecard(s) are complete (Judged==N, Errors==0, n>0) and exit; used by the shell after each arm")
	expectN := flag.Int("expect-n", 0, "expected full corpus size for integrity (0 = trust each card's own n; a TG_EVAL_LIMIT smoke pass)")
	holdout := flag.String("holdout", "", "holdout scorecard JSON — switches to holdout-gap mode (the §1.3 >20pt overfitting check)")
	refreshBaseline := flag.String("refresh-baseline", "", "trend mode only: path to REWRITE with the clean pooled measurement (self-updating trend baseline)")
	gitSHA := flag.String("git-sha", "", "git SHA recorded when refreshing the trend baseline")
	measuredAt := flag.String("measured-at", time.Now().UTC().Format("2006-01-02"), "date recorded on a refreshed/synthetic baseline (default today, UTC)")
	runs := flag.Int("runs", 0, "expected number of candidate runs (pooling protocol); 0 = accept whatever is given")
	// Discovery-flywheel mode (design-wisdom #10): ADDITIVE promotion of live-scored deviations into the
	// deterministic falsifiability regression suite. It NEVER runs the change/trend gate, NEVER touches the
	// gate's scoring/thresholds/existing cases, and NEVER feeds the sealed holdout.
	discovery := flag.Bool("discovery", false, "discovery-flywheel mode: promote reproduced, settled, de-duplicated, holdout-safe scored deviations from the rolling discovery corpus into the falsifiability regression suite (additive+audited); never touches the change/trend gate or the sealed holdout")
	discoveryCorpus := flag.String("discovery-corpus", "eval/discovery-corpus.json", "the rolling discovery corpus (captured scored deviations)")
	promotedOut := flag.String("promoted-out", "eval/discovery-promoted.json", "the append-only promoted falsifiability scenarios file (the regression suite grows here)")
	fixturePath := flag.String("fixture", "eval/falsifiability_fixture.json", "the FROZEN hand-authored falsifiability fixture (a dedup source; never modified)")
	holdoutCorpus := flag.String("holdout-corpus", "eval/holdout-corpus.json", "the SEALED holdout (never a promotion target)")
	estatePath := flag.String("estate", "eval/estate_fixture.json", "estate graph snapshot used to settle each promoted case's known-correct expected outcome")
	minRepro := flag.Int("min-reproductions", eval.DefaultPromotionCriteria().MinReproductions, "promotion criterion: minimum times a deviation must reproduce before it graduates")
	maxPromote := flag.Int("max-promotions", 0, "0 = unbounded; else cap promotions per run and LOG the dropped remainder (no silent cap)")
	dryRun := flag.Bool("dry-run", false, "discovery mode: print the audit report without writing the promoted file")
	overallDrop := flag.Float64("overall-drop", gate.DefaultThresholds().OverallDrop, "max allowed drop in overall mean")
	dimDrop := flag.Float64("dim-drop", gate.DefaultThresholds().DimDrop, "max allowed drop in any non-safety dimension")
	safetyDrop := flag.Float64("safety-drop", gate.DefaultThresholds().SafetyDrop, "max allowed drop in the safety-analog band")
	emitJSON := flag.Bool("json", false, "print the verdict as JSON (in addition to the table)")
	flag.Parse()

	// --- Discovery-flywheel mode (design-wisdom #10): additive, audited promotion; never runs the gate. ---
	if *discovery {
		runDiscovery(discoveryConfig{
			corpus:  *discoveryCorpus,
			out:     *promotedOut,
			fixture: *fixturePath,
			holdout: *holdoutCorpus,
			estate:  *estatePath,
			crit:    eval.PromotionCriteria{MinReproductions: *minRepro, MaxPromotions: *maxPromote},
			dryRun:  *dryRun,
		})
		return
	}

	// --- Integrity-only probe (TG-64): the shell runs this after each arm to catch a degraded/429 run. ---
	if len(verify) > 0 {
		var cards []gate.Scorecard
		for _, p := range verify {
			c, err := gate.LoadScorecard(p)
			if err != nil {
				fatal("%v", err)
			}
			cards = append(cards, c)
		}
		if probs := gate.VerifyIntegrity("scorecard", cards, *expectN); len(probs) > 0 {
			fmt.Println("INTEGRITY: DEGRADED — this arm must be reran (not pooled):")
			for _, p := range probs {
				fmt.Printf("  - %s\n", p)
			}
			os.Exit(1)
		}
		fmt.Printf("INTEGRITY: OK — %d scorecard(s) complete (all sessions judged, 0 errors).\n", len(cards))
		return
	}

	// Holdout-gap mode (make eval-holdout): report the regression-vs-holdout gap and fail if > 20 points.
	if *holdout != "" {
		runHoldout(*baseline, *holdout, candidates)
		return
	}

	if len(candidates) == 0 {
		fatal("no --candidate scorecard given (need at least one on-box run to gate)")
	}
	if *runs > 0 && len(candidates) != *runs {
		fatal("--runs %d but %d candidate scorecard(s) given — pool exactly %d paired runs", *runs, len(candidates), *runs)
	}

	cands := loadScorecards(candidates, "candidate")
	// Defense in depth: a degraded candidate arm is an INTEGRITY error (exit 2), never a silent regression.
	if probs := gate.VerifyIntegrity("candidate", cands, *expectN); len(probs) > 0 {
		fatal("candidate arm integrity failed (rerun the arm):\n  - %s", strings.Join(probs, "\n  - "))
	}
	ctrlRuns := loadControls(controls)
	th := gate.Thresholds{OverallDrop: *overallDrop, DimDrop: *dimDrop, SafetyDrop: *safetyDrop}

	switch *mode {
	case "change":
		if len(base) == 0 {
			fatal("--mode change requires --base scorecard(s) (the fresh origin/main arm measured in the same window). For the committed-baseline comparison use --mode trend.")
		}
		if *runs > 0 && len(base) != *runs {
			fatal("--runs %d but %d base scorecard(s) given — the base arm must be run the same %d times", *runs, len(base), *runs)
		}
		baseCards := loadScorecards(base, "base")
		if probs := gate.VerifyIntegrity("base", baseCards, *expectN); len(probs) > 0 {
			fatal("base arm integrity failed (rerun the arm):\n  - %s", strings.Join(probs, "\n  - "))
		}
		if probs := gate.VerifyComparable(baseCards, cands); len(probs) > 0 {
			fatal("%s", strings.Join(probs, "\n  "))
		}
		comparator := gate.PoolToBaseline(baseCards, *measuredAt, *gitSHA)
		v := gate.Compare(comparator, cands, ctrlRuns, th)
		printReport("FRESH BASE ARM (origin/main, same window)", comparator, v, len(baseCards))
		emit(v, *emitJSON)
		if !v.Pass {
			os.Exit(1)
		}

	case "trend":
		base, err := gate.LoadBaseline(*baseline)
		if err != nil {
			fatal("load baseline: %v", err)
		}
		v := gate.Compare(base, cands, ctrlRuns, th)
		printReport(fmt.Sprintf("COMMITTED trend baseline (%s @ %s)", base.MeasuredAt, shortSHA(base.GitSHA)), base, v, base.Runs)
		emit(v, *emitJSON)
		// Self-refresh (TG-64): auto-update the committed baseline ONLY on a clean, non-regressing run, so the
		// long-horizon anchor tracks main and never goes stale. A regressing run files an issue and does NOT
		// refresh — the baseline is never lowered to hide a regression.
		if *refreshBaseline != "" {
			if v.Pass {
				nb := gate.BuildRefreshedBaseline(cands, *gitSHA, *measuredAt)
				if err := gate.WriteBaseline(*refreshBaseline, nb); err != nil {
					fatal("refresh trend baseline: %v", err)
				}
				fmt.Printf("\nTREND: baseline self-refreshed → %s (overall %.2f @ %s) — the anchor now tracks main.\n", *refreshBaseline, nb.Scorecard.Overall, shortSHA(*gitSHA))
			} else {
				fmt.Printf("\nTREND: baseline NOT refreshed — this run regressed vs the committed baseline (issue should be filed); the baseline is never lowered to hide a regression.\n")
			}
		}
		if !v.Pass {
			os.Exit(1)
		}

	default:
		fatal("unknown --mode %q (want \"change\" or \"trend\")", *mode)
	}
}

type discoveryConfig struct {
	corpus, out, fixture, holdout, estate string
	crit                                  eval.PromotionCriteria
	dryRun                                bool
}

// runDiscovery is the deterministic promotion driver: it graduates qualifying scored deviations from the
// rolling discovery corpus into the append-only falsifiability regression suite and prints the AUDIT report.
// It performs NO gate comparison and NO model/SSH calls. It is ADDITIVE (appends new scenarios, de-duplicated),
// and it NEVER writes the sealed holdout. Exit 0 on success; 2 on an I/O error.
func runDiscovery(c discoveryConfig) {
	g, err := eval.LoadEstateGraph(c.estate)
	if err != nil {
		fatal("discovery: %v", err)
	}
	corpus, err := eval.LoadDiscoveryCorpus(c.corpus)
	if err != nil {
		fatal("discovery: %v", err)
	}
	frozen, err := eval.LoadFalsifiability(c.fixture)
	if err != nil {
		fatal("discovery: %v", err)
	}
	holdout, err := eval.HoldoutHosts(c.holdout)
	if err != nil {
		fatal("discovery: load sealed holdout (the promotion guard must never fail open): %v", err)
	}
	existingPromoted, err := eval.LoadPromoted(c.out)
	if err != nil {
		fatal("discovery: %v", err)
	}

	fresh, report := eval.PromoteDiscovery(g, corpus, frozen, existingPromoted, holdout, c.crit)
	merged := eval.AppendPromoted(existingPromoted, fresh)

	fmt.Println("== TG discovery flywheel — promote scored deviations into the falsifiability regression suite ==")
	fmt.Printf("discovery corpus : %s (%d case(s))\n", c.corpus, len(corpus.Cases))
	fmt.Printf("promoted file    : %s (%d existing -> %d after this run)\n", c.out, len(existingPromoted), len(merged))
	fmt.Printf("criteria         : min-reproductions=%d max-promotions=%d\n\n", c.crit.MinReproductions, c.crit.MaxPromotions)
	fmt.Printf("PROMOTED (%d): %v\n", len(report.Promoted), report.Promoted)
	fmt.Printf("SKIPPED  (%d):\n", len(report.Skipped))
	for _, s := range report.Skipped {
		fmt.Printf("  - %s: %s\n", s.Key, s.Reason)
	}
	if len(report.HoldoutRefused) > 0 {
		fmt.Printf("HOLDOUT-REFUSED (%d): %v  — the sealed holdout is NEVER auto-fed\n", len(report.HoldoutRefused), report.HoldoutRefused)
	}
	if len(report.Dropped) > 0 {
		fmt.Printf("DROPPED-BY-CAP (%d): %v  — raise --max-promotions to admit these (never silently dropped)\n", len(report.Dropped), report.Dropped)
	}

	if c.dryRun {
		fmt.Println("\nDRY-RUN — no file written.")
		return
	}
	if len(fresh) == 0 {
		fmt.Println("\nno new cases qualified — promoted file unchanged.")
		return
	}
	if err := eval.SavePromoted(c.out, merged); err != nil {
		fatal("discovery: write promoted file: %v", err)
	}
	fmt.Printf("\nwrote %d promoted scenario(s) to %s (additive; the frozen fixture + sealed holdout are untouched).\n", len(merged), c.out)
}

func runHoldout(baselinePath, holdoutPath string, candidates stringSlice) {
	base, err := gate.LoadBaseline(baselinePath)
	if err != nil {
		fatal("load baseline: %v", err)
	}
	hs, err := gate.LoadScorecard(holdoutPath)
	if err != nil {
		fatal("%v", err)
	}
	regOverall := base.Scorecard.Overall // regression reference = the committed baseline...
	regSrc := fmt.Sprintf("committed baseline (%s)", base.MeasuredAt)
	if len(candidates) > 0 { // ...unless a fresh regression run was passed alongside.
		cs := loadScorecards(candidates, "regression")
		regOverall = gate.Pool(cs).Overall
		regSrc = fmt.Sprintf("this build's regression run(s) x%d", len(cs))
	}
	gap := gate.HoldoutGapPoints(regOverall, hs.Overall)
	fmt.Println("== TG holdout overfitting check (docs/TESTING-AND-BENCHMARK.md §1.3) ==")
	fmt.Printf("  regression overall : %.2f  (%s)\n", regOverall, regSrc)
	fmt.Printf("  holdout overall    : %.2f  (sealed set the system may never tune to, N=%d)\n", hs.Overall, hs.N)
	fmt.Printf("  gap                : %.2f points (bar: <= %.0f)\n\n", gap, gate.HoldoutOverfitBar)
	if gap > gate.HoldoutOverfitBar {
		fmt.Printf("HOLDOUT: FAIL — regression-vs-holdout gap %.2fpt > %.0fpt: OVERFITTING signal (§1.3).\n", gap, gate.HoldoutOverfitBar)
		os.Exit(1)
	}
	fmt.Println("HOLDOUT: PASS — the sealed holdout tracks the working set (no overfitting signal).")
}

func loadScorecards(paths stringSlice, what string) []gate.Scorecard {
	var cards []gate.Scorecard
	for _, p := range paths {
		c, err := gate.LoadScorecard(p)
		if err != nil {
			fatal("%v", err)
		}
		if c.MutationCount != 0 {
			fatal("SAFETY: %s %s reports mutation_count=%d during a read-only eval — must be 0", what, p, c.MutationCount)
		}
		cards = append(cards, c)
	}
	return cards
}

func loadControls(paths stringSlice) []gate.ControlRun {
	var ctrlRuns []gate.ControlRun
	for _, p := range paths {
		cr, err := gate.LoadControlRun(p)
		if err != nil {
			fatal("%v", err)
		}
		ctrlRuns = append(ctrlRuns, cr)
	}
	return ctrlRuns
}

func emit(v gate.Verdict, asJSON bool) {
	if asJSON {
		b, _ := json.MarshalIndent(v, "", "  ")
		fmt.Println(string(b))
	}
}

func printReport(comparatorLabel string, base gate.Baseline, v gate.Verdict, comparatorRuns int) {
	fmt.Println("== TG eval gate ==")
	fmt.Printf("comparator: %s — overall %.2f (pooled over %d run(s))\n", comparatorLabel, base.Scorecard.Overall, comparatorRuns)
	fmt.Printf("candidate:  pooled over %d run(s)\n\n", v.Runs)
	fmt.Printf("  %-24s %8s %8s %8s %8s  %s\n", "dimension", "base", "cand", "Δ", "max-drop", "verdict")
	fmt.Printf("  %-24s %8s %8s %8s %8s  %s\n", "------------------------", "----", "----", "----", "--------", "-------")
	for _, d := range v.Dims {
		fmt.Printf("  %-24s %8.2f %8.2f %+8.2f %8.2f  %s\n", d.Dim, d.Baseline, d.Candidate, d.Delta, -d.MaxDrop, pf(d.Pass))
	}
	fmt.Printf("  %-24s %8.2f %8.2f %+8.2f %8.2f  %s\n", "OVERALL", v.OverallBaseline, v.OverallCandidate, v.OverallDelta, -v.OverallMaxDrop, pf(v.OverallPass))
	if v.ControlN > 0 {
		fmt.Printf("\n  negative controls: %d checked, %d violation(s) %v  %s\n", v.ControlN, len(v.ControlViolations), v.ControlViolations, pf(v.ControlPass))
	}
	fmt.Println()
	if v.Pass {
		fmt.Println("GATE: PASS — candidate holds or beats the comparator within the mechanical bars.")
		return
	}
	fmt.Println("GATE: FAIL")
	for _, r := range v.Reasons {
		fmt.Printf("  - %s\n", r)
	}
}

func pf(b bool) string {
	if b {
		return "PASS"
	}
	return "FAIL"
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "evalgate: "+format+"\n", a...)
	os.Exit(2)
}
