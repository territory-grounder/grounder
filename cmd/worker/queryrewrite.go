package main

import (
	"context"
	"strings"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/knowledge"
)

// queryRewriteCompleter is the one slice of the model gateway the query-rewrite seam needs — a single
// completion. Abstracted so the seam is unit-testable off a fake. *model.Gateway satisfies it.
type queryRewriteCompleter interface {
	Complete(ctx context.Context, user, modelName string, msgs []model.Message) (string, error)
}

// queryRewriteSystem instructs a fast model to rewrite the alert into a crisp retrieval query: the concrete
// fault + component, vendor terms normalized, no ticket boilerplate. It rewrites the free-text SUMMARY only —
// the typed retrieval fields (host, rule, site, tags) are structural facts the model must not invent.
const queryRewriteSystem = "You rewrite an infrastructure alert into a SHORT search query for finding past similar incidents: name the concrete fault and the component in plain, normalized terms (expand shorthand, drop ticket IDs / timestamps / boilerplate), one line, no preamble and no quotes. Keep it faithful to the alert — do not invent hosts, services, or causes."

// queryRewrite builds the knowledge.QueryRewriter seam (TG-50 query-rewrite), or nil when unarmed
// (TG_RETRIEVE_QUERY_REWRITE unset ⇒ the raw query is used, byte-identical). Armed, it asks a FAST model
// (TG_QUERY_REWRITE_MODEL, default "fast") to reformulate the incident's SUMMARY into a retrieval query; the
// whole fused+multiquery+graph+rerank stack then runs on the rewritten query. A generation error or an empty
// rewrite returns the original Query unchanged, so the rewrite never fails a retrieval. It is a MODEL CALL in
// the retrieval path (latency + gateway load), hence OFF by default and armed only by explicit operator choice.
func queryRewrite(gw queryRewriteCompleter, getenv func(string, string) string) knowledge.QueryRewriter {
	if !truthy(getenv("TG_RETRIEVE_QUERY_REWRITE", "")) || gw == nil {
		return nil
	}
	rewriteModel := strings.TrimSpace(getenv("TG_QUERY_REWRITE_MODEL", "fast"))
	if rewriteModel == "" {
		// Same present-but-empty compose trap as TG_HYDE_MODEL (hyde.go) — one doomed model:"" call per
		// retrieval, silently degraded to the unrewritten query (TG-530). Blank means unconfigured.
		rewriteModel = "fast"
	}
	return func(ctx context.Context, q knowledge.Query) knowledge.Query {
		prompt := queryRewritePrompt(q)
		if prompt == "" {
			return q
		}
		out, err := gw.Complete(ctx, "query-rewrite", rewriteModel, []model.Message{
			{Role: "system", Content: queryRewriteSystem},
			{Role: "user", Content: prompt},
		})
		if err != nil {
			return q // degrade: retrieve on the original query for this one retrieval
		}
		rewritten := strings.TrimSpace(out)
		if rewritten == "" {
			return q
		}
		// Replace the free-text summary with the rewrite; keep the typed fields the retriever scores on.
		nq := q
		nq.Summary = rewritten
		return nq
	}
}

// queryRewritePrompt renders the incident for the rewrite model as DATA (its identity), never as an
// instruction. Empty when the incident carries neither an alert rule nor a summary to rewrite from.
func queryRewritePrompt(q knowledge.Query) string {
	rule, host, summary := strings.TrimSpace(q.AlertRule), strings.TrimSpace(q.Host), strings.TrimSpace(q.Summary)
	if rule == "" && summary == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("Alert: ")
	b.WriteString(rule)
	if host != "" {
		b.WriteString(" on host ")
		b.WriteString(host)
	}
	if summary != "" {
		b.WriteString("\nSummary: ")
		b.WriteString(summary)
	}
	b.WriteString("\nRewrite as a search query:")
	return b.String()
}
