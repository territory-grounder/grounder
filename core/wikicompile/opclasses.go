package wikicompile

import (
	"fmt"
	"sort"
	"strings"
)

// PER-OP-CLASS PAGES — TG's earned doctrine, made readable.
//
// The op-class catalogue is the whole point of the governed-autonomy ladder (propose -> candidate ->
// ratify -> AUTO_NOTICE -> AUTO), and it has had no operator-readable surface anywhere. An operator can
// see individual proposals and individual sessions; nothing answers "what does TG know how to do, how
// often has it done it, and did it work?".
//
// KEYED ON ACTUAL USE, NOT ON RATIFICATION — and that inversion is load-bearing.
//
// Production, measured 2026-08-01: opclass_ratified holds ZERO rows and opclass_candidate holds ONE,
// while session_triage carries SEVEN distinct op-classes over 3,202 sessions and action_execution holds
// 460 real executions. A page set keyed on the ratified catalogue would therefore render nothing at all
// — the exact failure that made #manifest and #proposals empty, and the same one the host pages avoided
// by letting a host earn its page through having been dealt with rather than through appearing in a
// graph.
//
// So an op-class earns a page by having been PROPOSED OR EXECUTED, and its ratification status becomes a
// FACT ON the page rather than the gate to it. That inversion is what makes the most important sentence
// on these pages sayable at all: TG has carried out hundreds of actions under classes that are not in
// the earned catalogue, because the earning ladder's later stages are not built yet. A surface keyed on
// ratification could never report that; it would just be blank, and blank reads as "nothing to see".

// OpClassInputs is everything the compiler needs, already fetched. Sessions are reused verbatim from the
// per-rule read — one query feeds both families, since every triage row already carries its op_class.
type OpClassInputs struct {
	Sessions []RuleSession
	// Ratified is the set of op-classes in the EARNED catalogue (opclass_ratified). Nil is a legitimate
	// state and is reported as such, not as "none ratified" — an unreadable catalogue and an empty one
	// are different facts.
	Ratified map[string]bool
	// RatifiedKnown distinguishes "the catalogue was read and is empty" from "the catalogue could not be
	// read". Without it a failed read would render as a confident "not ratified" on every page.
	RatifiedKnown bool
	// Candidates maps op-class -> candidacy status (observing / candidate / ratify_ready / ...).
	Candidates map[string]string
	// Truncated marks a bounded session read, so counts render as floors.
	Truncated bool
}

const (
	opClassSlugPrefix = "opclass-"
	maxOpClassHosts   = 40
)

// CompileOpClasses renders one page per op-class TG has actually used.
func CompileOpClasses(in OpClassInputs) ([]Article, []Skip) {
	byClass := map[string][]RuleSession{}
	for _, s := range in.Sessions {
		c := strings.TrimSpace(s.OpClass)
		if c == "" {
			continue // a session that proposed no class is not an op-class; it is an absence
		}
		byClass[c] = append(byClass[c], s)
	}

	names := make([]string, 0, len(byClass))
	for c := range byClass {
		names = append(names, c)
	}
	sort.Strings(names)

	articles := make([]Article, 0, len(names))
	var skips []Skip
	for _, class := range names {
		slug, ok := SafeSlug(opClassSlugPrefix, class)
		if !ok {
			skips = append(skips, Skip{Kind: "opclass",
				Host:   class,
				Reason: "op-class name is not a safe page identifier — refused rather than rewritten, because two classes rewritten to one slug would merge distinct capabilities onto a single page",
			})
			continue
		}
		sess := byClass[class]
		sort.Slice(sess, func(i, j int) bool {
			if !sess[i].CreatedAt.Equal(sess[j].CreatedAt) {
				return sess[i].CreatedAt.After(sess[j].CreatedAt)
			}
			return sess[i].ExternalRef < sess[j].ExternalRef
		})
		articles = append(articles, Article{
			Slug:  slug,
			Title: class,
			Kind:  "article",
			Body:  opClassBody(class, sess, in),
			Meta:  opClassMeta(class, sess, in),
		})
	}
	sort.Slice(skips, func(i, j int) bool { return skips[i].Host < skips[j].Host })
	return articles, skips
}

func opClassStats(sess []RuleSession) (executed, confirmed int, hosts map[string]int) {
	hosts = map[string]int{}
	for _, s := range sess {
		if s.Host != "" {
			hosts[s.Host]++
		}
		if s.Mutated {
			executed++
			if s.ConfirmedClear {
				confirmed++
			}
		}
	}
	return executed, confirmed, hosts
}

func opClassMeta(class string, sess []RuleSession, in OpClassInputs) map[string]string {
	executed, confirmed, hosts := opClassStats(sess)
	m := map[string]string{
		"op_class":  class,
		"sessions":  fmt.Sprint(len(sess)),
		"executed":  fmt.Sprint(executed),
		"confirmed": fmt.Sprint(confirmed),
		"hosts":     fmt.Sprint(len(hosts)),
	}
	if in.RatifiedKnown {
		m["ratified"] = fmt.Sprint(in.Ratified[class])
	}
	if st, ok := in.Candidates[class]; ok {
		m["candidacy"] = st
	}
	return m
}

func opClassBody(class string, sess []RuleSession, in OpClassInputs) string {
	var b strings.Builder
	w := func(s string) { b.WriteString(s); b.WriteByte('\n') }

	executed, confirmed, hosts := opClassStats(sess)
	proposedOnly := len(sess) - executed

	w("# " + class)
	w("")
	w("What TG has actually done under this capability, from its own audit spine. Nothing here is authored.")
	w("")

	// ── Standing: the fact this page exists to make sayable ────────────────────
	w("## Standing")
	w("")
	switch {
	case !in.RatifiedKnown:
		w("**The earned catalogue could not be read**, so this class's ratification standing is UNKNOWN — " +
			"which is not the same as unratified, and must not be read as either.")
	case in.Ratified[class]:
		w("**Ratified.** This class is in the earned catalogue: an operator approved it on the evidence, " +
			"and it may run under the bands that evidence earned.")
	default:
		w("**NOT in the earned catalogue.** TG has used this class anyway — the authored op-class registry " +
			"compiled into the binary is what permits it, and the earned catalogue (`opclass_ratified`) is " +
			"a separate, additive ladder that this class has not climbed.")
		if executed > 0 {
			w("")
			w(fmt.Sprintf("That is worth stating plainly: **%d execution(s) have already happened under a "+
				"class no operator has ratified.** Those executions were permitted by the authored registry "+
				"and gated by the mode chokepoint, so this is not an escape — but the earned ladder is not "+
				"what authorised them, and a reader who assumes ratification means \"allowed to run\" has "+
				"the relationship backwards.", executed))
		}
	}
	if st, ok := in.Candidates[class]; ok {
		w("")
		w(fmt.Sprintf("Candidacy status: **%s**.", mdCell(st)))
	} else if in.RatifiedKnown && !in.Ratified[class] {
		w("")
		w("No candidacy record either — nothing is accruing toward ratification for this class.")
	}
	w("")

	// ── The record ─────────────────────────────────────────────────────────────
	w("## The record")
	w("")
	w(fmt.Sprintf("**%d session(s)** proposed this class, across **%d host(s)**.", len(sess), len(hosts)))
	if in.Truncated {
		w("")
		w("The read behind this page was bounded, so every count is a FLOOR.")
	}
	w("")
	w(fmt.Sprintf("- **%d** carried out", executed))
	w(fmt.Sprintf("- **%d** proposed but never carried out", proposedOnly))
	switch {
	case executed == 0:
		w("- **no confirmation rate** — nothing has run, so nothing has been verified")
	case confirmed == executed:
		w(fmt.Sprintf("- **%d of %d** confirmed clear afterwards — every execution verified", confirmed, executed))
	default:
		w(fmt.Sprintf("- **%d of %d** confirmed clear afterwards; **%d** ran without a confirmed clear, "+
			"meaning TG changed something and could not verify it held", confirmed, executed, executed-confirmed))
	}
	w("")

	if len(hosts) == 0 {
		w("No session recorded a host, so the estate spread is unknown.")
	} else {
		hs := make([]string, 0, len(hosts))
		for h := range hosts {
			hs = append(hs, h)
		}
		sort.Slice(hs, func(i, j int) bool {
			if hosts[hs[i]] != hosts[hs[j]] {
				return hosts[hs[i]] > hosts[hs[j]]
			}
			return hs[i] < hs[j]
		})
		shown := hs
		if len(shown) > maxOpClassHosts {
			shown = shown[:maxOpClassHosts]
		}
		w("| host | times proposed |")
		w("|---|---|")
		for _, h := range shown {
			w(fmt.Sprintf("| %s | %d |", mdCell(h), hosts[h]))
		}
		if len(hs) > len(shown) {
			w("")
			w(fmt.Sprintf("Showing %d of %d hosts.", len(shown), len(hs)))
		}
	}
	w("")

	// ── Most recent ────────────────────────────────────────────────────────────
	w("## Most recent")
	w("")
	if len(sess) == 0 {
		w("No session recorded.") // unreachable via CompileOpClasses, kept so the section never renders bare
	} else {
		newest := sess[0]
		w(fmt.Sprintf("`%s` on `%s`, %s — %s.",
			mdCell(newest.ExternalRef), mdCell(firstNonEmpty(newest.Host, "unknown host")),
			newest.CreatedAt.UTC().Format("2006-01-02 15:04"),
			describeOutcome(newest)))
	}

	return b.String()
}

// describeOutcome renders one session's disposition in the operator's terms rather than the spine's.
func describeOutcome(s RuleSession) string {
	switch {
	case s.Mutated && s.ConfirmedClear:
		return "carried out, and the condition verified cleared"
	case s.Mutated:
		return "carried out, but the clear was never confirmed"
	default:
		return "proposed, not carried out"
	}
}
