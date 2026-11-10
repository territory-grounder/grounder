package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"

	"github.com/territory-grounder/grounder/core/httpapi"
	tg "github.com/territory-grounder/grounder/temporal"
	"github.com/territory-grounder/grounder/temporal/policytrace"
)

// policyTraceBackend implements httpapi.PolicyTracer by starting the worker's READ-ONLY trace workflow — the
// grounder never evaluates policy itself. A grounder-side engine would be a SECOND policy that could disagree
// with the live decision the interceptor makes, and a packet-tracer that lies is worse than none; so the
// answer comes from the ONE engine, reached over the SAME Temporal channel opClassWrite/modeTransition use.
//
// It mirrors opClassWriteBackend: ExecuteWorkflow + run.Get. The trace writes no audit row and actuates
// nothing. Unlike a ratify, a trace has NO idempotency need, so each call gets a UNIQUE workflow id (a random
// nonce beside the readable host/op-class prefix): two operators tracing the same (host, op-class) at once must
// each get their own run, not have the second collide with the first's still-open run.
type policyTraceBackend struct{ tc client.Client }

func (b policyTraceBackend) Trace(ctx context.Context, req httpapi.PolicyTraceRequest) (httpapi.PolicyTraceResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	run, err := b.tc.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        fmt.Sprintf("tg/policytrace/%s/%s/%s", traceIDPart(req.Host), traceIDPart(req.OpClass), traceNonce()),
		TaskQueue: tg.TaskQueueRunner,
		// Each trace id is unique (the nonce above), so no two runs share an id; ALLOW_DUPLICATE is retained
		// only as defence in the vanishing case that two nonces coincide.
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
	}, policytrace.PolicyTraceWorkflow, policytrace.Request{
		OpClass:     req.OpClass,
		Argv:        req.Argv,
		Host:        req.Host,
		Resource:    req.Resource,
		Groups:      req.Groups,
		DeviceClass: req.DeviceClass,
		Territory:   req.Territory,
		Reversible:  req.Reversible,
		Confidence:  req.Confidence,
		Band:        req.Band,
		Mode:        req.Mode,
	})
	if err != nil {
		return httpapi.PolicyTraceResult{}, err
	}
	var res policytrace.Result
	if err := run.Get(ctx, &res); err != nil {
		return httpapi.PolicyTraceResult{}, err
	}
	out := httpapi.PolicyTraceResult{
		Verdict:               res.Verdict,
		MatchedRuleID:         res.MatchedRuleID,
		ComposedBand:          res.ComposedBand,
		ApproveBy:             res.ApproveBy,
		Mode:                  res.Mode,
		Reason:                res.Reason,
		NeverAutoFloor:        res.NeverAutoFloor,
		BundleVersion:         res.BundleVersion,
		RateGovernorSimulated: res.RateGovernorSimulated,
	}
	for _, r := range res.MatchedRules {
		out.MatchedRules = append(out.MatchedRules, httpapi.PolicyTraceMatchedRule{RuleID: r.RuleID, Verdict: r.Verdict})
	}
	return out, nil
}

// traceIDPart keeps the workflow id well-formed when a dimension is blank — a bare "tg/policytrace//" would
// still be a valid id, but "_" reads clearly in the Temporal UI as "no value supplied for this dimension".
func traceIDPart(s string) string {
	if strings.TrimSpace(s) == "" {
		return "_"
	}
	return s
}

// traceNonce returns a short random hex suffix so each trace workflow id is unique (48 bits of entropy). On the
// vanishing chance crypto/rand fails, it returns a constant — the id stays well-formed and the only cost is the
// concurrent-duplicate edge reappearing for that one call (the pre-nonce behaviour), never a broken id.
func traceNonce() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "x"
	}
	return hex.EncodeToString(b[:])
}

var _ httpapi.PolicyTracer = policyTraceBackend{}
