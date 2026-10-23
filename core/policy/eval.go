package policy

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/open-policy-agent/opa/rego"
)

// regoSource is the FIXED, audited evaluator module (REQ-1503). It is embedded at build time and is the ONLY
// Rego the engine ever compiles — there is no API on this package that accepts operator-authored Rego, so
// there is no path from operator input to evaluator logic (INV-06). Operators supply policy as rule DATA.
//
//go:embed rego/policy.rego
var regoSource string

// RegoModule exposes the fixed module source for audit and for the acceptance oracle to assert that the
// evaluator compiles this one embedded module and nothing operator-supplied.
func RegoModule() string { return regoSource }

// evaluator wraps the OPA Go SDK: the fixed module is compiled and prepared ONCE, in-process (no sidecar, no
// network — distroless-safe), and each Decide runs a prepared eval over the rule DATA (REQ-1503).
type evaluator struct {
	query rego.PreparedEvalQuery
}

// newEvaluator compiles and prepares the embedded fixed module. A compile failure is returned to the caller
// (it can only be a bug in the embedded module, never operator input) so the engine fails to construct
// rather than running with a broken evaluator.
func newEvaluator(ctx context.Context) (*evaluator, error) {
	pq, err := rego.New(
		rego.Query("data.tg.policy.result"),
		rego.Module("policy.rego", regoSource),
	).PrepareForEval(ctx)
	if err != nil {
		return nil, fmt.Errorf("policy: prepare fixed rego module: %w", err)
	}
	return &evaluator{query: pq}, nil
}

// regoResult is the decoded projection returned by data.tg.policy.result.
type regoResult struct {
	verdict    Verdict
	matchedIDs []string
	denyIDs    []string
	winningIDs []string
}

// evaluate runs the fixed deny-overrides module over the pre-matched rule data (REQ-1504). Each element of
// preparedRules carries the operator rule's id + verdict + the Go-computed `matched` boolean (matching uses
// the ONE shared object-model grammar, done in engine.go — not reimplemented in Rego). The module owns only
// the deny-overrides combination.
func (e *evaluator) evaluate(ctx context.Context, preparedRules []map[string]any) (regoResult, error) {
	input := map[string]any{"rules": preparedRules}
	rs, err := e.query.Eval(ctx, rego.EvalInput(input))
	if err != nil {
		return regoResult{}, fmt.Errorf("policy: rego eval: %w", err)
	}
	if len(rs) != 1 || len(rs[0].Expressions) != 1 {
		return regoResult{}, fmt.Errorf("policy: unexpected rego result shape (%d results)", len(rs))
	}
	m, ok := rs[0].Expressions[0].Value.(map[string]any)
	if !ok {
		return regoResult{}, fmt.Errorf("policy: rego result is not an object")
	}
	v, _ := m["verdict"].(string)
	out := regoResult{
		verdict:    Verdict(v),
		matchedIDs: toStrings(m["matched_ids"]),
		denyIDs:    toStrings(m["deny_ids"]),
		winningIDs: toStrings(m["winning_ids"]),
	}
	// Defence in depth: the module can only ever emit a closed-enum verdict, but if a future edit regressed
	// it, refuse rather than pass an unknown verdict through (fail closed, INV-09).
	if !validVerdict(out.verdict) {
		return regoResult{}, fmt.Errorf("policy: fixed module returned non-enum verdict %q", v)
	}
	return out, nil
}

func toStrings(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
