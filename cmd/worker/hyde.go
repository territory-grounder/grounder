package main

import (
	"context"
	"strings"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/knowledge"
)

// hydeCompleter is the one slice of the model gateway HyDE needs — a single completion. Abstracted so the HyDE
// seam is unit-testable off a fake instead of a live gateway. *model.Gateway satisfies it.
type hydeCompleter interface {
	Complete(ctx context.Context, user, modelName string, msgs []model.Message) (string, error)
}

// hydeSystem instructs a fast model to write a plausible RESOLUTION (not a recommendation) — the hypothetical
// DOCUMENT HyDE embeds. Short + concrete so it lands near a real precedent's resolution in document space.
const hydeSystem = "You write a SHORT, plausible resolution for an infrastructure alert, in the voice a past incident's fix would read: two or three sentences, the concrete actions and the component involved, no preamble and no caveats. This is a retrieval aid, not a recommendation — write the most likely fix even if uncertain."

// hydeHypothetical builds the FusedRetriever.Hypothetical seam (TG-214 HyDE), or nil when unarmed
// (TG_RETRIEVE_HYDE unset ⇒ the raw query is embedded, byte-identical). Armed, it asks a FAST model
// (TG_HYDE_MODEL, default "fast") for a hypothetical resolution of the incident which the semantic channel
// embeds as a document to match precedent RESOLUTIONS rather than symptom queries. A generation error yields
// "" and the FusedRetriever falls back to the raw query, so HyDE never fails a retrieval. It is a MODEL CALL in
// the retrieval path (latency + gateway load), hence OFF by default and armed only by explicit operator choice.
func hydeHypothetical(gw hydeCompleter, getenv func(string, string) string) func(context.Context, knowledge.Query) string {
	if !truthy(getenv("TG_RETRIEVE_HYDE", "")) || gw == nil {
		return nil
	}
	hydeModel := strings.TrimSpace(getenv("TG_HYDE_MODEL", "fast"))
	if hydeModel == "" {
		// Compose forwards TG_HYDE_MODEL as ${TG_HYDE_MODEL:-}, which makes the variable PRESENT-but-empty
		// in the container: os.LookupEnv reports it set, so the compiled default above never applied and
		// every armed HyDE completion POSTed model:"" — a doomed LiteLLM 400, ~10 per deep retrieval,
		// swallowed into silent non-HyDE degradation (TG-530). Blank means unconfigured, not "no model".
		hydeModel = "fast"
	}
	return func(ctx context.Context, q knowledge.Query) string {
		prompt := hydePrompt(q)
		if prompt == "" {
			return ""
		}
		out, err := gw.Complete(ctx, "hyde", hydeModel, []model.Message{
			{Role: "system", Content: hydeSystem},
			{Role: "user", Content: prompt},
		})
		if err != nil {
			return "" // degrade: the FusedRetriever embeds the raw query for this one retrieval
		}
		return strings.TrimSpace(out)
	}
}

// hydePrompt renders the incident for the HyDE model as DATA (its identity), never as an instruction. Empty
// when the incident carries neither an alert rule nor a summary to hypothesize a resolution from.
func hydePrompt(q knowledge.Query) string {
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
	b.WriteString("\nWrite the likely resolution:")
	return b.String()
}
