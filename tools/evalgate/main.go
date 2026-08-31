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
//
// Exit status: 0 PASS · 1 regression FAIL · 3 INCONCLUSIVE (the run declares it did not measure a capability
// the gate bars on — it certifies nothing, so it blocks exactly like a FAIL) · 2 integrity/usage error.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/eval"
	"github.com/territory-grounder/grounder/eval/gate"
)

// writeArchive persists one gate verdict as an append-only quality-record entry: the comparator, the
// pooled candidate card, and the verdict, each as indented JSON. The directory is the record's identity
// (<date>-<mode>-<sha>); writing over an existing entry is allowed (same MR re-running its gate) but
// entries are never deleted — eval/history is the committed trail the gitignored scorecards never gave.
func writeArchive(dir string, comparator gate.Baseline, pooled gate.Scorecard, v gate.Verdict) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	write := func(name string, val any) error {
		b, err := json.MarshalIndent(val, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, name), append(b, '\n'), 0o644)
	}
	if err := write("comparator.json", comparator); err != nil {
		return err
	}
	if err := write("scorecard.json", pooled); err != nil {
		return err
	}
	return write("verdict.json", v)
}

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
	baseGitSHA := flag.String("base-git-sha", "", "change mode: the RESOLVED sha the base arm (origin/main) was measured at — recorded on the archived comparator so change records are self-verifying (the candidate sha alone cannot say what the base arm actually was)")
	measuredAt := flag.String("measured-at", time.Now().UTC().Format("2006-01-02"), "date recorded on a refreshed/synthetic baseline (default today, UTC)")
	runs := flag.Int("runs", 0, "expected number of candidate runs (pooling protocol); 0 = accept whatever is given")
	rejudge := flag.Bool("rejudge", false, "change mode: the arms are a RE-JUDGE of the SAME captured sessions under two rubric versions (TG-359). The rubric is the change under test, so the arms MUST carry different rubric versions and each arm must be internally single-version. Use for a core/judge/rubric.json edit, which cannot be gated by the ordinary two-arm change gate.")
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
	proposalFloor := flag.Float64("proposal-floor", gate.DefaultThresholds().ProposalRateFloor, "absolute candidate-arm proposal_rate floor (0 disables) — a shared collapse must not pass")
	predictionFloor := flag.Float64("prediction-floor", gate.DefaultThresholds().PredictionRateFloor, "absolute candidate-arm prediction_rate floor (0 disables)")
	recallFloor := flag.Float64("recall-floor", gate.DefaultThresholds().ProposalRecallFloor, "proposal_recall floor, applied only when the corpus carries expected-propose labels (0 disables)")
	archiveDir := flag.String("archive-dir", "", "append-only quality-record dir (eval/history): on any change/trend verdict, write <dir>/<date>-<mode>-<sha>/{scorecard,comparator,verdict}.json for the MR to commit")
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
	th := gate.Thresholds{OverallDrop: *overallDrop, DimDrop: *dimDrop, SafetyDrop: *safetyDrop,
		ProposalRateFloor: *proposalFloor, PredictionRateFloor: *predictionFloor, ProposalRecallFloor: *recallFloor}

	// archive writes the append-only quality record (eval/history) — the durable, committed trail that
	// replaces "paste the PASS table into the MR". Written on PASS and FAIL alike: a red verdict is
	// evidence too, and an untracked quality record is how 12 days of collapse went unrecorded.
	archive := func(modeLabel string, comparator gate.Baseline, pooled gate.Scorecard, v gate.Verdict) {
		if *archiveDir == "" {
			return
		}
		dir := filepath.Join(*archiveDir, fmt.Sprintf("%s-%s-%s", *measuredAt, modeLabel, shortSHA(*gitSHA)))
		if err := writeArchive(dir, comparator, pooled, v); err != nil {
			fatal("archive quality record: %v", err)
		}
		fmt.Printf("\nARCHIVE: quality record written to %s — commit it with the MR.\n", dir)
	}

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
		// A RE-JUDGE comparison holds the sessions fixed and moves the rubric on purpose (TG-359), so the
		// one-rubric-per-comparison rule is inverted rather than dropped: each arm must still be internally
		// single-version, and the two must DIFFER or the comparison is vacuous.
		verifier := gate.VerifyComparable
		if *rejudge {
			verifier = gate.VerifyComparableRejudge
		}
		if probs := verifier(baseCards, cands); len(probs) > 0 {
			fatal("%s", strings.Join(probs, "\n  "))
		}
		comparator := gate.PoolToBaseline(baseCards, *measuredAt, *gitSHA)
		// Self-verifying change records (spec/026 review follow-up): the archived comparator must name the
		// BASE arm's own resolved sha, not just inherit the candidate's. Recorded in both the GitSHA field
		// and the provenance string so a future reader can re-derive the exact A/B pair from the archive.
		if *baseGitSHA != "" {
			comparator.GitSHA = *baseGitSHA
			comparator.Provenance += fmt.Sprintf(" — base arm measured at %s", *baseGitSHA)
		}
		v := gate.Compare(comparator, cands, ctrlRuns, th)
		markMissingControls(&v, ctrlRuns)
		printReport("FRESH BASE ARM (origin/main, same window)", comparator, gate.Pool(cands), v, len(baseCards))
		emit(v, *emitJSON)
		// The archived record's mode label says WHICH comparison produced it. A rubric re-judge and an
		// ordinary two-arm change gate are different evidence about different things, and a reader of
		// eval/history must not have to guess which one a directory holds. Both still match the
		// `<date>-change-<sha>` shape scripts/lint-eval-evidence.sh accepts, because a rubric re-judge IS
		// a change record — "change-rubric" only narrows it.
		archiveLabel := "change"
		if *rejudge {
			archiveLabel = "change-rubric"
		}
		archive(archiveLabel, comparator, gate.Pool(cands), v)
		// Keyed on the OUTCOME, not on the derived Pass boolean: 1 = regression, 3 = INCONCLUSIVE (the run
		// measured nothing). Both block the merge. Going through exitFor means there is exactly one place
		// where "which verdicts are green" is decided, and it is the enum — so a future edit cannot make an
		// uncertified run exit 0 by touching the boolean alone.
		if code := exitFor(v); code != 0 {
			os.Exit(code)
		}

	case "trend":
		base, err := gate.LoadBaseline(*baseline)
		if err != nil {
			fatal("load baseline: %v", err)
		}
		v := gate.Compare(base, cands, ctrlRuns, th)
		markMissingControls(&v, ctrlRuns)
		printReport(fmt.Sprintf("COMMITTED trend baseline (%s @ %s)", base.MeasuredAt, shortSHA(base.GitSHA)), base, gate.Pool(cands), v, base.Runs)
		emit(v, *emitJSON)
		archive("trend", base, gate.Pool(cands), v)
		// Self-refresh (TG-64): auto-update the committed baseline ONLY on a clean, non-regressing run, so the
		// long-horizon anchor tracks main and never goes stale. A regressing run files an issue and does NOT
		// refresh — the baseline is never lowered to hide a regression.
		if *refreshBaseline != "" {
			// The self-refresh decision (TG-64, TG-424) lives in gate.ShouldRefreshTrend so it is unit-tested
			// with fixture scorecards rather than only exercised by the ~2h on-box nightly. It refreshes on a
			// clean run, never from an INCONCLUSIVE one, and — the TG-424 fix — re-anchors past a STALE committed
			// anchor even on a regression verdict, because a stale anchor's "regression" cannot be told from
			// model/estate drift and gating on it wedges the anchor stale forever (the opus-cc->mistral stuck
			// fixed-point). The exit code below is UNCHANGED, so a stale re-anchor still exits non-zero and files
			// the issue that surfaces the delta for review — the change is un-sticking the anchor, not hiding a drop.
			refresh, reason := gate.ShouldRefreshTrend(v.Outcome, base.MeasuredAt, time.Now().UTC())
			if refresh {
				nb := gate.BuildRefreshedBaseline(cands, *gitSHA, *measuredAt, v.Outcome)
				if err := gate.WriteBaseline(*refreshBaseline, nb); err != nil {
					fatal("refresh trend baseline: %v", err)
				}
				fmt.Printf("\nTREND: baseline self-refreshed → %s (overall %.2f @ %s) — %s.\n", *refreshBaseline, nb.Scorecard.Overall, shortSHA(*gitSHA), reason)
			} else {
				fmt.Printf("\nTREND: baseline NOT refreshed — %s.\n", reason)
			}
		}
		if code := exitFor(v); code != 0 { // 1 = regression vs the committed baseline; 3 = INCONCLUSIVE (nothing measured)
			os.Exit(code)
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
	// The holdout arm gets the SAME treatment as every other arm: integrity first (a degraded 0-judged
	// holdout must abort, not read as "no overfitting"), then normalization (a pre-v2 holdout card whose
	// zero-proposal run dropped falsifiable_prediction from its mean reads ~+0.43 high, understating the
	// gap ~8.6pt on the 100-scale — an asymmetry vs the normalized regression arm).
	if probs := gate.VerifyIntegrity("holdout", []gate.Scorecard{hs}, 0); len(probs) > 0 {
		fatal("holdout arm integrity failed (rerun it):\n  - %s", strings.Join(probs, "\n  - "))
	}
	hs = gate.NormalizeScorecard(hs)
	regOverall := gate.NormalizeScorecard(base.Scorecard).Overall // regression reference = the committed baseline...
	regSrc := fmt.Sprintf("committed baseline (%s)", base.MeasuredAt)
	if len(candidates) > 0 { // ...unless a fresh regression run was passed alongside.
		cs := loadScorecards(candidates, "regression")
		if probs := gate.VerifyIntegrity("regression", cs, 0); len(probs) > 0 {
			fatal("regression arm integrity failed (rerun it):\n  - %s", strings.Join(probs, "\n  - "))
		}
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

func printReport(comparatorLabel string, base gate.Baseline, cand gate.Scorecard, v gate.Verdict, comparatorRuns int) {
	fmt.Println("== TG eval gate ==")
	fmt.Printf("comparator: %s — overall %.2f (pooled over %d run(s))\n", comparatorLabel, base.Scorecard.Overall, comparatorRuns)
	fmt.Printf("candidate:  pooled over %d run(s)\n\n", v.Runs)
	fmt.Printf("  %-24s %8s %8s %8s %8s  %s\n", "dimension", "base", "cand", "Δ", "max-drop", "verdict")
	fmt.Printf("  %-24s %8s %8s %8s %8s  %s\n", "------------------------", "----", "----", "----", "--------", "-------")
	for _, d := range v.Dims {
		verdict := pf(d.Pass)
		if d.Unresolved { // dropped past the floor but within the run's resolution — not certifiable either way (TG-409)
			verdict = "INCONC↑full"
		}
		fmt.Printf("  %-24s %8.2f %8.2f %+8.2f %8.2f  %s\n", d.Dim, d.Baseline, d.Candidate, d.Delta, -d.MaxDrop, verdict)
	}
	fmt.Printf("  %-24s %8.2f %8.2f %+8.2f %8.2f  %s\n", "OVERALL", v.OverallBaseline, v.OverallCandidate, v.OverallDelta, -v.OverallMaxDrop, pf(v.OverallPass))
	// A4 autonomy rate (docs/BENCHMARK-AXES.md): REPORTED, not gated (a legitimately-more-conservative change
	// lowers it — see gate.Scorecard.AutonomyRate). The Δ is the trend signal; the dims above are the bars.
	ar := base.Scorecard.AutonomyRate
	fmt.Printf("  %-24s %8.2f %8.2f %+8.2f %8s  %s\n", "autonomy_rate (A4)", ar, cand.AutonomyRate, cand.AutonomyRate-ar, "—", "report")
	// A5 fault-class breadth (distinct proposed op-classes): REPORTED, not gated (narrowing to fewer safer
	// classes is legitimate). The Δ is the trend signal.
	fb := base.Scorecard.FaultClassBreadth
	fmt.Printf("  %-24s %8d %8d %+8d %8s  %s\n", "fault_class_breadth (A5)", fb, cand.FaultClassBreadth, cand.FaultClassBreadth-fb, "—", "report")
	// A6a decision STEPS (mean investigation cycles): REPORTED, not gated. FEWER is better, so a NEGATIVE Δ
	// is the win (the candidate decided in fewer model round-trips). The wall-clock half of the axis (A6b) is
	// deliberately absent from the merge gate — it is gateway-dominated — and is reported by the live scorer.
	ds := base.Scorecard.MeanDecisionSteps
	fmt.Printf("  %-24s %8.2f %8.2f %+8.2f %8s  %s\n", "decision_steps (A6a,↓better)", ds, cand.MeanDecisionSteps, cand.MeanDecisionSteps-ds, "—", "report")
	// TG-491 resolution-recall (leave-one-out retrieval recall over the shipped seed corpus): REPORTED, not
	// gated — the retriever's own CI ratchet owns any regression red; the eval gate never bars on it (TG-507).
	// It rides the change-gate diff so a retrieval-quality change is VISIBLE where deploys are gated. HIGHER is
	// better; the of-findable figure (of incidents whose fix is recoverable, the fraction surfaced) is the
	// honest quality number — raw recall + ceiling are on the scorecard JSON.
	rr := base.Scorecard.ResolutionRecallOfFindable
	fmt.Printf("  %-24s %8.2f %8.2f %+8.2f %8s  %s\n", "res_recall@3 (TG-491)", rr, cand.ResolutionRecallOfFindable, cand.ResolutionRecallOfFindable-rr, "—", "report")
	// Absolute candidate-arm floors: GATED (a collapse shared by both arms cancels in every Δ above — the
	// floors are what make it impossible to pass; see the 2026-07-25 incident in eval/gate/gate.go).
	for _, r := range v.Rates {
		fmt.Printf("  %-24s %8s %8.2f %8s %8.2f  %s\n", r.Name+" (floor)", "—", r.Candidate, "—", r.Floor, pf(r.Pass))
	}
	if cand.ExpectedProposeN == 0 {
		fmt.Printf("  %-24s %8s %8s %8s %8s  %s\n", "proposal_recall (floor)", "—", "n/a", "—", "—", "corpus has no expected-propose labels")
	}
	// B4a disclosure: how much of the recall supply was served from captured fixtures (deterministic arm)
	// vs the live estate. Reported, never gated — the honesty mechanism is the disclosure itself.
	if cand.FixtureArmed > 0 {
		fmt.Printf("  %-24s %8s %8d %8s %8s  %s\n", "fixture_armed (B4a)", "—", cand.FixtureArmed, "—", "—", "recall supply served from captured fixtures (deterministic arm)")
	}
	if v.ControlN > 0 {
		fmt.Printf("\n  negative controls: %d checked, %d violation(s) %v  %s\n", v.ControlN, len(v.ControlViolations), v.ControlViolations, pf(v.ControlPass))
		// SAY WHAT IT SAW (TG-362). A bare ref cannot tell an over-eager proposal from one the agent
		// grounded on observations after refusing the summary's own claim — and on 2026-08-06 that
		// distinction was the whole finding. The conclusion is already captured; print it.
		for _, d := range v.ControlViolationDetail {
			fmt.Printf("    %s  band=%s  outcome=%q\n", d.Ref, d.Band, d.Outcome)
			if d.Conclusion != "" {
				fmt.Printf("      concluded: %s\n", d.Conclusion)
			}
		}
	}
	for _, w := range v.Warnings {
		fmt.Printf("\n  ⚠ %s\n", w)
	}
	fmt.Println()
	// Three outcomes, three headlines (TG-258). INCONCLUSIVE exists so the operator is told the difference
	// between "your change made it worse" and "this run measured nothing, so it certifies nothing" — the
	// second used to print "GATE: PASS" with the bad news demoted to a ⚠ line above it.
	switch v.Outcome {
	case gate.OutcomePass:
		fmt.Println("GATE: PASS — candidate holds or beats the comparator within the mechanical bars.")
	case gate.OutcomeInconclusive:
		fmt.Println("GATE: INCONCLUSIVE — NOT a pass: the run did not measure a capability this gate exists to bar on.")
		for _, u := range v.Unmeasured {
			fmt.Printf("  - UNMEASURED: %s\n", u)
		}
		fmt.Println("  Nothing may be certified on this run. Restore the measurement (refresh/label the corpus")
		fmt.Println("  with live action-warranted incidents) and re-run; do NOT merge on this verdict.")
	default:
		fmt.Println("GATE: FAIL")
		for _, r := range v.Reasons {
			fmt.Printf("  - %s\n", r)
		}
	}
}

// markMissingControls is the certifying invocation's half of "a bar that was SKIPPED is not a bar that was
// HELD" (TG-258). The negative-control bar — the agent must NOT propose on a benign, no-action-warranted
// incident — is one of the four documented bars, and it exists ONLY if a control arm was supplied: with no
// --controls, gate.Compare leaves ControlN 0 / ControlPass true and the run is certified having never been
// asked the question. eval/eval-gate.sh appends --controls conditionally (`[ -f "$cand_ctrl" ]`), so a
// candidate arm whose TestEvalControlsOnBox produced no file loses the bar silently — no flag, no warning,
// GATE: PASS, exit 0. Recording it as an unmeasured capability makes that run INCONCLUSIVE instead: non-zero
// exit, named in the report and in the committed eval/history record.
//
// It applies to the change and trend modes only — the two that certify — not to --verify-integrity, the
// holdout check or discovery mode, none of which apply the control bar or claim to.
func markMissingControls(v *gate.Verdict, ctrlRuns []gate.ControlRun) {
	if len(ctrlRuns) > 0 {
		return
	}
	v.MarkUnmeasured("negative controls: no --controls run was supplied, so the benign-incident bar " +
		"(the agent must NOT propose on a no-action-warranted incident) was never applied to this run")
}

// exitFor maps the verdict onto the process exit status. INCONCLUSIVE gets its OWN non-zero code so a
// caller (CI, the shell, a human reading `echo $?`) can tell "regressed" from "measured nothing" without
// parsing text — and, critically, so "measured nothing" can never be mistaken for success. Absent is
// visible; skipped is not: this is the exit-status half of making the skip impossible to miss.
//
// Honest limit: `go run ./tools/evalgate` — how eval/eval-gate.sh invokes this — COLLAPSES any non-zero
// program status to 1 (it prints "exit status 3" to stderr and exits 1 itself). So 3 is distinguishable
// only to a caller that runs the built binary. What is guaranteed on EVERY path is what the gate depends
// on: an unmeasured run exits NON-ZERO and prints the "GATE: INCONCLUSIVE — NOT a pass" headline, so the
// shell, the CI job (`RC -ne 0`) and make all treat it exactly like a FAIL.
func exitFor(v gate.Verdict) int {
	switch v.Outcome {
	case gate.OutcomePass:
		return 0
	case gate.OutcomeInconclusive:
		return 3
	default:
		return 1
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
