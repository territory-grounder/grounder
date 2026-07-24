// Command rejudge re-scores already-captured eval sessions with the CURRENT core/judge rubric,
// holding the triage runs fixed. The judge is read-only over the session record (core/judge doc),
// so re-judging isolates a rubric change as the SINGLE variable in an A/B: the same captured runs
// are re-scored, and any score delta is purely the rubric — no triage nondeterminism confound.
//
// Usage (on a host that reaches the model gateway, e.g. dc1tg01):
//
//	LITELLM_MASTER_KEY=... rejudge -gateway http://localhost:4000 sessions.run1.json sessions.run2.json ...
//
// For each input it writes <file>.rejudge.json (an eval.Scorecard) and prints a one-line summary.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/eval"
)

func or(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// row mirrors one element of a captured sessions.runN.json file.
type row struct {
	Session eval.Session `json:"session"`
	Score   eval.Score   `json:"score"`
}

func mapSession(s eval.Session) judge.Session {
	return judge.Session{
		Ref: s.Ref, AlertRule: s.AlertRule, Host: s.Host, Severity: s.Severity,
		Band: s.Band, Proposed: s.Proposed, ActionID: s.ActionID, Prediction: s.Prediction,
		Predicted: s.Predicted, Evidence: s.Evidence, Conclusion: s.Conclusion,
		Decisions: s.Decisions, Outcome: s.Outcome, Mutated: s.Mutated,
	}
}

func main() {
	gwURL := flag.String("gateway", os.Getenv("TG_EVAL_GATEWAY"), "model gateway base url")
	// The judge model tier defaults to the canonical JudgeParams (the one source, shared with the eval
	// harness, the durable cron, and the Python shadowbench judge) — never a private "primary" literal.
	modelName := flag.String("model", judge.DefaultParams().Model, "judge model tier")
	flag.Parse()
	files := flag.Args()
	if *gwURL == "" || len(files) == 0 {
		fmt.Fprintln(os.Stderr, "usage: rejudge -gateway URL sessions.run1.json [sessions.run2.json ...]")
		os.Exit(2)
	}
	gw := model.NewGateway(*gwURL, config.SecretRef("env:LITELLM_MASTER_KEY"))
	ctx := context.Background()
	rc := 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			fmt.Fprintln(os.Stderr, "read", f, err)
			rc = 1
			continue
		}
		var rows []row
		if err := json.Unmarshal(raw, &rows); err != nil {
			fmt.Fprintln(os.Stderr, "parse", f, err)
			rc = 1
			continue
		}
		sessions := make([]eval.Session, 0, len(rows))
		scores := make([]eval.Score, 0, len(rows))
		for _, r := range rows {
			sessions = append(sessions, r.Session)
			out, jerr := gw.Complete(ctx, "rejudge", *modelName, []model.Message{{Role: "user", Content: judge.Prompt(mapSession(r.Session))}})
			if jerr != nil {
				fmt.Fprintln(os.Stderr, "judge", r.Session.Ref, jerr)
				continue
			}
			sc, perr := eval.ParseScore(r.Session.Ref, out)
			if perr != nil {
				fmt.Fprintln(os.Stderr, "parse-score", r.Session.Ref, perr)
				continue
			}
			scores = append(scores, sc)
			// Per-session line: old→new appropriate_band + band/outcome, for targeted rubric validation.
			fmt.Printf("  %-9s band=%-11s ab_old=%d ab_new=%d concl=%q outcome=%q\n",
				r.Session.Ref, or(r.Session.Band, "none"), r.Score.Scores["appropriate_band"], sc.Scores["appropriate_band"],
				trunc(r.Session.Conclusion, 24), trunc(r.Session.Outcome, 28))
		}
		card := eval.Aggregate(sessions, scores)
		dst := f + ".rejudge.json"
		if err := os.WriteFile(dst, eval.ScorecardJSON(card), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "write", dst, err)
			rc = 1
			continue
		}
		fmt.Printf("%s: n=%d scored=%d overall=%.3f prop=%.3f dims=%v -> %s\n",
			filepath.Base(f), card.N, len(scores), card.Overall, card.ProposalRate, card.DimMeans, filepath.Base(dst))
	}
	os.Exit(rc)
}
