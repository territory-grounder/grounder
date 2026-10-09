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
	"time"

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

// judgeAll re-judges every session with the current rubric and returns the parsed scores (a session whose
// judge call or score parse fails is reported and skipped — the same per-session fail-soft as the file path).
func judgeAll(ctx context.Context, gw *model.Gateway, modelName string, sessions []eval.Session) []eval.Score {
	scores := make([]eval.Score, 0, len(sessions))
	for _, s := range sessions {
		out, jerr := gw.Complete(ctx, "rejudge", modelName, []model.Message{{Role: "user", Content: judge.Prompt(mapSession(s))}})
		if jerr != nil {
			fmt.Fprintln(os.Stderr, "judge", s.Ref, jerr)
			continue
		}
		sc, perr := eval.ParseScore(s.Ref, out)
		if perr != nil {
			fmt.Fprintln(os.Stderr, "parse-score", s.Ref, perr)
			continue
		}
		scores = append(scores, sc)
	}
	return scores
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
	// TG-314: the estate snapshot that lets the OFFLINE scorecard carry estate_grounded, measured against the
	// SAME fixed fixture for candidate and baseline so a skill's effect is isolated from estate drift. The
	// harness ships eval/estate_fixture.json to both worktrees; a missing/unreadable one leaves the axis N/A.
	estatePath := flag.String("estate", "eval/estate_fixture.json", "estate snapshot fixture for the estate_grounded axis; missing/empty ⇒ the axis stays N/A")
	// TG-527 slice A2 — DB-replay mode: score a window of LIVE session_triage rows (trajectory included)
	// instead of captured files. -dsn selects the mode; the row→eval.Session mapping is dbload.go.
	dbDSN := flag.String("dsn", "", "DB-replay mode: a session_triage DSN (read-only role is enough); replaces the file arguments")
	dbSince := flag.Duration("since", 24*time.Hour, "DB-replay mode: how far back to read session_triage rows")
	dbLimit := flag.Int("limit", 200, "DB-replay mode: max rows, newest first")
	dbOut := flag.String("out", "session_triage.rejudge.json", "DB-replay mode: scorecard output path")
	flag.Parse()
	files := flag.Args()
	// Load the estate snapshot ONCE (same fixture for every input). LoadEstateGraph returns a nil graph on any
	// read/parse failure, so estate_grounded then stays honestly N/A exactly as before this shipped.
	estateGraph, eerr := eval.LoadEstateGraph(*estatePath)
	if eerr != nil {
		fmt.Fprintf(os.Stderr, "estate fixture %q: %v — estate_grounded stays N/A\n", *estatePath, eerr)
	}
	if *gwURL == "" || (len(files) == 0 && *dbDSN == "") {
		fmt.Fprintln(os.Stderr, "usage: rejudge -gateway URL sessions.run1.json [sessions.run2.json ...]\n       rejudge -gateway URL -dsn postgres://... [-since 24h] [-limit 200] [-out file]")
		os.Exit(2)
	}
	gw := model.NewGateway(*gwURL, config.SecretRef("env:LITELLM_MASTER_KEY"))
	ctx := context.Background()
	rc := 0
	if *dbDSN != "" {
		sessions, err := loadSessionsFromDB(ctx, *dbDSN, time.Now().Add(-*dbSince), *dbLimit)
		if err != nil {
			fmt.Fprintln(os.Stderr, "db-replay:", err)
			os.Exit(1)
		}
		withTraj := 0
		for _, s := range sessions {
			if len(s.Trajectory) > 0 {
				withTraj++
			}
		}
		// Say what the window can and cannot score BEFORE spending judge calls: a window of pre-0104 rows
		// scores trajectory_grounded N/A everywhere, and that is a fact the operator should read, not infer.
		fmt.Printf("db-replay: %d session_triage rows since %s; %d carry a persisted trajectory (the rest predate migration 0104 and read N/A on that axis)\n",
			len(sessions), dbSince.String(), withTraj)
		scores := judgeAll(ctx, gw, *modelName, sessions)
		card := eval.Aggregate(sessions, scores, estateGraph)
		if err := os.WriteFile(*dbOut, eval.ScorecardJSON(card), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "write", *dbOut, err)
			os.Exit(1)
		}
		fmt.Printf("db-replay: n=%d scored=%d overall=%.3f prop=%.3f dims=%v -> %s\n",
			card.N, len(scores), card.Overall, card.ProposalRate, card.DimMeans, *dbOut)
		os.Exit(0)
	}
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
		card := eval.Aggregate(sessions, scores, estateGraph)
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
