package judge

// JudgeParams is the ONE canonical judge sampling configuration every evaluator surface constructs from a
// single place (DefaultParams) — the eval harness, the eval gate, tools/rejudge, the durable
// temporal/skilljudge cron, and the Python shadowbench judge. Divergent judges (different model, different
// sampling) produce incomparable scores; a single deterministic JudgeParams is the fix (OpenAI Evals 3.4;
// the one-judge principle).
//
// Determinism: Temperature is 0 — the judge adjudicates a fixed record, so a deterministic verdict is the
// correct posture (re-judging the same session should not wander). Seed pins the sampler where the backend
// honors it. Temperature/Seed are the canonical INTENT that takes effect on any tier that honors them (a
// backend that strips sampling params runs its own default; LiteLLM does that per tier), not a claim about one
// backend. Keeping the intent single-sourced means a tier swap needs no per-caller edit. The values are carried
// in rubric.json (the one source) so Go and Python read the same.
//
// Model is the "judge" gateway alias (TG-584; owner ruling 2026-08-31: judge = Opus, chain-free) — a tier
// DISTINCT from the agent's "primary"/"fast". Until 2026-10-23 it was "primary": the judge shared the agent's
// brain and scored its output bimodally (1-or-5), 23 % of live sessions identical on all five dimensions; the
// same sessions under "judge" were graded, 4 % identical (eval/history/2026-10-22-judgecal-tier-ab). Changing
// the tier changes the MEASUREMENT: the rubric version is bumped with it (TG-194 — one version per pool), so
// every pooled statistic re-anchors on new-version samples instead of mixing two judges.
type JudgeParams struct {
	Model       string  `json:"model"`       // gateway model tier (component alias); LiteLLM maps it down the fallback ladder
	Temperature float64 `json:"temperature"` // 0 = deterministic judging (the one-judge invariant)
	Seed        int64   `json:"seed"`        // fixed sampler seed where the backend honors it
}

// DefaultParams is the canonical judge configuration, sourced from the embedded rubric.json so there is a
// single value both the Go callers and the Python shadowbench judge honor. Every Go judge caller selects
// the model via DefaultParams().Model rather than a private "primary" literal.
func DefaultParams() JudgeParams { return rubric.Params }
