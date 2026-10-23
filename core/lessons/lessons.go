// Package lessons is Territory Grounder's teacher: it distills a RESOLVED incident's outcome into a reusable
// lesson — a knowledge.Incident the retrieval plane surfaces for future similar incidents — closing the
// outcome-labelled memory loop (observe → resolve → learn → retrieve).
//
// The load-bearing discipline: a lesson is recorded ONLY from a CONFIRMED CLEAN outcome (a mechanical
// verdict of `match` AND an orchestrator-confirmed clear). A deviation, a partial, or an unconfirmed session
// never becomes precedent, so the corpus is never poisoned with advice from a session where reality diverged
// from the model or the fix was never verified. Learning from your successes is safe; learning from your
// near-misses as if they were successes is how an autonomous system compounds its own mistakes.
//
// That discipline gates the OUTCOME. The CONTENT is gated separately and here (TG-296): every free-text field
// a lesson carries is input-screened (core/screen) on the WRITE path — neutralized and flagged, never
// rejected — so untrusted alert prose and any credential embedded in it are filtered once on the way in
// rather than on every read out. The argument for that policy is above Lesson.
package lessons

import (
	"sort"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/knowledge"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/screen"
)

// ResolvedIncident is a closed-out incident with its verified outcome — the input the teacher labels. The
// json tags let an operator-exported resolved-incident history (or, in Phase 2, the close-out path) round-trip
// through ParseResolved into the persistence hop.
type ResolvedIncident struct {
	ExternalRef    string         `json:"external_ref"`
	Host           string         `json:"host,omitempty"`
	AlertRule      string         `json:"alert_rule,omitempty"`
	Site           string         `json:"site,omitempty"`
	Summary        string         `json:"summary,omitempty"`
	Action         string         `json:"action,omitempty"`  // what was done (the ActionManifest op) — becomes the lesson's Resolution
	Verdict        safety.Verdict `json:"verdict,omitempty"` // the mechanical verdict (spec/002)
	ConfirmedClear bool           `json:"confirmed_clear"`   // an orchestrator-captured confirmation the condition actually cleared (INV-11)
	Tags           []string       `json:"tags,omitempty"`
	// ResolvedAt is the lesson's PROVENANCE timestamp — when the incident was resolved and the precedent
	// became true (spec/018, Gulli ch14). It lets the recency/decay discipline know a lesson's AGE so a stale
	// precedent can be down-weighted (HalfLifeWeight) or pruned from the corpus (Reconcile). Zero = undatable:
	// the reconciliation never ages out a lesson whose age it cannot prove (fail toward retention).
	ResolvedAt time.Time `json:"resolved_at,omitempty"`
}

// ScreenedTagPrefix namespaces the write-screen provenance tags this package attaches to a lesson whose
// untrusted text tripped the input screen ("screened:persona-shift", "screened:secret-redaction", …). It is
// exported so an operator query — and the wiki, which renders a lesson's tags — can find every corpus row
// that arrived carrying hostile content, without regex-hunting the prose for the inline marker.
const ScreenedTagPrefix = "screened:"

// WRITE-PATH CONTENT SCREEN (TG-296). Everything the outcome gate above checks is about WHETHER the session
// earned a precedent. Nothing checked WHAT was copied into it: ri.Summary — the alert's own narrative, which
// anything able to raise an alert can influence — was written into the corpus verbatim, and the only content
// filter in the system sat at RETRIEVAL (temporal/runner precedent()), re-screening every row on every read.
//
// Measured on the live corpus (670 incidents = 140 seed + 530 written by THIS function), that ordering costs
// two distinct things:
//
//  1. A confirmed-clean lesson whose summary happens to trip the screen is skipped at retrieval — forever, on
//     every read. It still de-novels its (host, rule), so it looks like a working precedent from the outside,
//     while the resolution it carries is never once shown to the agent that needed it.
//  2. A credential pasted into an alert body is persisted verbatim into the durable corpus FILE and rendered
//     in the wiki (spec/001 REQ-010, SK 6.3 — a live NetBox token was once found in a predecessor plaintext
//     log). Screening at read time cannot un-write that; only a write-time pass can.
//
// So the untrusted text is filtered ONCE, here, on the way in. This does NOT retire the retrieval screen:
// corpus rows also arrive from the shipped seed, from operator-authored exports, and from the AWX runbook
// ingest — none of which pass through this function — so that screen stays as the backstop for rows this one
// never saw. Do not delete it on the strength of this comment.
//
// NEUTRALIZE-AND-FLAG, NEVER REJECT — the choice the ticket asked to be argued, not just made.
// A screened-positive summary is stored SCRUBBED (screen.Scrub's inline [SCREENED:<category>] /
// [REDACTED:<kind>] markers are the flag an operator reads in the corpus file and the wiki) with a
// `screened:<category>` provenance tag, and the lesson is KEPT. Rejecting the record instead would:
//   - hand an alert author a denial-of-learning primitive. An injection string in the alert body would stop
//     that (host, rule) from ever being de-noveled, so the same incident POLL_PAUSEs a human forever and the
//     attacker — not the operator — chooses which incidents TG is never permitted to learn.
//   - discard the trustworthy half because of the untrusted half. The Resolution is TG's own validated
//     ActionManifest op and the outcome was confirmed clean; the summary is not even rendered in the
//     precedent block (knowledge.Context prints ref / rule / host / resolution / staleness).
//
// It is also what the screen package already does at an INPUT boundary: neutralize and flag, never drop,
// because dropping lets embedded hostile content suppress the very thing it is embedded in. Retrieval drops
// instead — correctly, because there the whole row is of unknown provenance and is optional enrichment.
//
// IDENTIFIERS ARE DELIBERATELY NOT SCRUBBED. A marker substituted into ExternalRef / Host / AlertRule would
// corrupt the exact key knowledge.Count reads — i.e. destroy the de-novel this entire path exists to record —
// so mangling an identifier is a worse outcome than the hostile string it would remove. On the live path they
// are ingest-validated anyway (core/ingest slugRe forbids whitespace, so an injection PHRASE cannot appear in
// one); for operator-authored feeds the retrieval screen still covers them.
//
// DO NOT GATE THIS WRITE ON GRADUATION. TG-153 recommended exactly that and it was WRONG (TG-296 records the
// correction): the writeback gate is BAND-INDEPENDENT on purpose, because the first-occurrence de-novel IS a
// POLL_PAUSE-band resolution, which the reconciler routes To Verify. Gate on the band, the auto-close
// decision, or the graduation state and first-occurrence de-novelling could never fire at all — the loop
// would only ever learn from incidents it had already learned from. What is added here is a CONTENT filter
// and changes nothing about which OUTCOMES qualify: a screened row is still written, still counted, still
// de-novels.

// Lesson distills a resolved incident into a knowledge.Incident, or (_, false) when the outcome is not a
// trustworthy precedent. Both gates must hold: the mechanical verdict is a clean `match` (reality matched the
// prediction) AND the condition was confirmed clear (the fix is verified, not merely asserted). An incident
// with no external_ref or no action is also not a citable lesson. The free text it carries is input-screened
// on the way in (see the TG-296 block above): neutralized and flagged, never rejected.
func Lesson(ri ResolvedIncident) (knowledge.Incident, bool) {
	if ri.Verdict != safety.VerdictMatch || !ri.ConfirmedClear {
		return knowledge.Incident{}, false
	}
	if strings.TrimSpace(ri.ExternalRef) == "" || strings.TrimSpace(ri.Action) == "" {
		return knowledge.Incident{}, false
	}
	// Both free-text fields are screened. Summary is the untrusted one; Action is TG's own manifest op, but
	// it is screened too because the operator-export feed (ParseResolved) can supply either field from
	// outside, and a rule that holds only for records this process authored is not a boundary.
	summary, summaryCats := scrubField(ri.Summary)
	resolution, resolutionCats := scrubField(ri.Action)
	return knowledge.Incident{
		ExternalRef: ri.ExternalRef,
		Host:        ri.Host,
		AlertRule:   ri.AlertRule,
		Site:        ri.Site,
		Summary:     summary,
		Resolution:  resolution,
		// Carry the provenance timestamp INTO the corpus. It was computed here, documented as the
		// lesson's provenance, and then dropped because knowledge.Incident had nowhere to put it — which
		// is why 92.5% of production retrieval cuts were settled alphabetically among same-rule rows that
		// each resolved on a different day.
		ResolvedAt: ri.ResolvedAt,
		// PROVENANCE (TG-172 item 1). This is the ONE write path with a mechanical verification behind it —
		// the two gates at the top of this function are exactly what the label claims. Stamping it here,
		// where those gates are, rather than at the merge, means the claim cannot outlive them: delete the
		// verdict check and this label becomes a lie that a test can catch.
		Source: knowledge.ProvenanceVerifiedResolution,
		Tags:   withScreenedTags(ri.Tags, summaryCats, resolutionCats),
	}, true
}

// scrubField runs one free-text field through the input screen, returning the neutralized text and the
// categories it tripped (nil for clean text, which screen.Scrub returns byte-identical).
func scrubField(text string) (string, []screen.Category) {
	scrubbed, ms := screen.Scrub(text)
	if len(ms) == 0 {
		return scrubbed, nil
	}
	cats := make([]screen.Category, 0, len(ms))
	for _, m := range ms {
		cats = append(cats, m.Category)
	}
	return scrubbed, cats
}

// withScreenedTags appends one deduplicated, sorted `screened:<category>` tag per tripped category to a COPY
// of the incident's own tags (never the caller's backing array — the ResolvedIncident may be re-read from a
// feed that is merged repeatedly, and appending in place would grow its tags on every pass). Clean text adds
// nothing, so an unscreened lesson's tags are unchanged, exactly as before.
func withScreenedTags(tags []string, cats ...[]screen.Category) []string {
	seen := make(map[string]struct{})
	var add []string
	for _, set := range cats {
		for _, c := range set {
			t := ScreenedTagPrefix + string(c)
			if _, dup := seen[t]; dup {
				continue
			}
			seen[t] = struct{}{}
			add = append(add, t)
		}
	}
	if len(add) == 0 {
		return tags
	}
	sort.Strings(add) // stable corpus output: the serialized row must not reorder between identical writes
	out := make([]string, 0, len(tags)+len(add))
	out = append(out, tags...)
	return append(out, add...)
}

// ScreenedTags returns the write-screen provenance tags a stored lesson carries — the flags Lesson attached
// because the incoming text tripped the input screen (nil for a clean row). It is the READ side of that flag:
// the composition root logs it at the moment of the corpus write, because a durable marker nobody ever reads
// is not an alert — a hostile alert body should be a visible operational event, not an artifact somebody
// notices later while scrolling the corpus file.
func ScreenedTags(inc knowledge.Incident) []string {
	var out []string
	for _, t := range inc.Tags {
		if strings.HasPrefix(t, ScreenedTagPrefix) {
			out = append(out, t)
		}
	}
	return out
}

// Distill maps a batch of resolved incidents to the lessons worth keeping — the confirmed-clean subset. It is
// the teacher's corpus-building pass: the survivors are exactly the incidents the retriever should surface.
func Distill(resolved []ResolvedIncident) []knowledge.Incident {
	out := make([]knowledge.Incident, 0, len(resolved))
	for _, ri := range resolved {
		if lesson, ok := Lesson(ri); ok {
			out = append(out, lesson)
		}
	}
	return out
}
