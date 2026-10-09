package judge

// JudgeParams is the ONE canonical judge sampling configuration every evaluator surface constructs from a
// single place (DefaultParams) — the eval harness, the eval gate, tools/rejudge, the durable
// temporal/skilljudge cron, and the Python shadowbench judge. Divergent judges (different model, different
// sampling) produce incomparable scores; a single deterministic JudgeParams is the fix (OpenAI Evals 3.4;
// the one-judge principle).
//
// Determinism: Temperature is 0 — the judge adjudicates a fixed record, so a deterministic verdict is the
// correct posture (re-judging the same session should not wander). Seed pins the sampler where the backend
// honors it. NB (deploy/litellm-config.yaml): the current "primary" tier is kimi-k3, a thinking-only model
// that REJECTS temperature != 1, so LiteLLM strips the sampling params for that tier and kimi uses its
// server default — Temperature/Seed are the canonical INTENT that takes effect on any tier that honors
// them, not a claim that kimi runs at 0. Keeping the intent single-sourced means a tier swap needs no
// per-caller edit. The values are carried in rubric.json (the one source) so Go and Python read the same.
type JudgeParams struct {
	Model       string  `json:"model"`       // gateway model tier (component alias); LiteLLM maps it down the fallback ladder
	Temperature float64 `json:"temperature"` // 0 = deterministic judging (the one-judge invariant)
	Seed        int64   `json:"seed"`        // fixed sampler seed where the backend honors it
}

// DefaultParams is the canonical judge configuration, sourced from the embedded rubric.json so there is a
// single value both the Go callers and the Python shadowbench judge honor. Every Go judge caller selects
// the model via DefaultParams().Model rather than a private "primary" literal.
func DefaultParams() JudgeParams { return rubric.Params }
