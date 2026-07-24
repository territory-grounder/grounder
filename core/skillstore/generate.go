package skillstore

import (
	"context"
	"fmt"
	"strings"
)

// The candidate generator (spec/014 REQ-1312) — GENERATE-ONLY, the predecessor GEPA invariant promoted
// to law: model output only ever becomes DRAFT ROWS carrying their rationale and source; a draft has no
// effect on composition until it passes the offline admission gate (REQ-1307) and a statistical trial
// (REQ-1308). No generator output becomes control flow (INV-08).

// Completer is the model surface (the LiteLLM gateway in production, scripted in tests).
type Completer interface {
	Complete(ctx context.Context, user, model string, prompt string) (string, error)
}

// GenTrigger is the typed signal that fires generation: a judge dimension's rolling mean fell below its
// threshold for a skill (the eval-failure source) or a resolved incident exposed a missing procedure
// (the lesson source). The trigger is DATA about measurements, never model output.
type GenTrigger struct {
	SkillName string
	Dimension string
	Mean      float64
	Threshold float64
	Window    int    // sessions in the rolling window
	Source    string // "flywheel:eval-failure:<runid>" | "flywheel:lesson:<external_ref>"
}

// lenses are the mutation directions — each candidate rewrites toward a different shape, so the trial
// compares genuinely different hypotheses rather than three paraphrases.
var lenses = []struct{ name, instruction string }{
	{"concise", "Rewrite it MORE CONCISELY: cut every sentence that does not change what the agent does. Keep every tool name and every hard rule."},
	{"detailed", "Rewrite it with ONE worked micro-example inline (a single alert walked through the steps). Keep it under the size limit."},
	{"decision-first", "Rewrite it DECISION-FIRST: lead with the decision rules, then the evidence-gathering steps that feed them. Keep every tool name and every hard rule."},
}

// GenerateCandidates asks the model for one rewrite per lens and lands each as a validated DRAFT row.
// Dedup (against the current body and each other), the 8 KiB cap, and predicate preservation are
// enforced here; ValidateDraft (pinned refusal included) runs at the store. Returns the created drafts;
// per-lens failures degrade (a generator error never blocks anything else).
func GenerateCandidates(ctx context.Context, st interface {
	Store
	CreateVersion(context.Context, Version) (Version, error)
}, model Completer, trig GenTrigger) ([]Version, error) {
	sk, err := st.GetSkill(ctx, trig.SkillName)
	if err != nil {
		return nil, err
	}
	if sk.Pinned {
		return nil, fmt.Errorf("%w: %s", ErrPinnedSkill, sk.Name)
	}
	cur, ok, err := st.ProductionVersion(ctx, trig.SkillName)
	if err != nil || !ok {
		return nil, fmt.Errorf("no production version to improve for %s: %v", trig.SkillName, err)
	}

	rationale := fmt.Sprintf("generated: %s mean %.2f fell below %.2f over %d sessions (%s)",
		trig.Dimension, trig.Mean, trig.Threshold, trig.Window, trig.Source)
	seen := map[string]bool{ContentHash(cur.Body, cur.AppliesWhen): true}
	var out []Version
	for i, lens := range lenses {
		prompt := "You are improving ONE skill prompt for an SRE triage agent. The skill guides behavior; it can never grant permissions (enforcement is machine-checked elsewhere).\n\n" +
			"The agent's judged sessions score weakly on the dimension \"" + trig.Dimension + "\".\n" +
			lens.instruction + "\n\nReply with ONLY the rewritten skill body (markdown, no fences, no commentary). Current body:\n\n" + cur.Body
		raw, cerr := model.Complete(ctx, "skill-generator", "primary", prompt)
		if cerr != nil {
			continue
		}
		body := strings.TrimSpace(raw)
		if len(body) == 0 || len(body) > 8192 {
			continue
		}
		h := ContentHash(body, cur.AppliesWhen)
		if seen[h] {
			continue // a paraphrase of the current body or an earlier lens adds no hypothesis
		}
		seen[h] = true
		v, cerr := st.CreateVersion(ctx, Version{
			SkillName: trig.SkillName, Version: fmt.Sprintf("%s-cand%d", cur.Version, i+1),
			Body: body, AppliesWhen: cur.AppliesWhen, ContentHash: h,
			Author: "flywheel", Source: trig.Source,
			Rationale:       "[draft] " + rationale + " — lens: " + lens.name,
			ParentVersionID: cur.ID,
		})
		if cerr != nil {
			continue
		}
		out = append(out, v)
	}
	return out, nil
}
