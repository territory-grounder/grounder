package wikicompile

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// PER-RULE PAGES — the family with no predecessor counterpart.
//
// `wiki-compile.py` can only key on hostname: its compile_host_pages is the whole of its per-entity
// output, and nothing in it answers "what does THIS ALERT mean across the estate?". That is not an
// oversight in the predecessor so much as a limit of what it had — its knowledge rows are host-scoped.
// TG's spine records (host, alert_rule, outcome, op_class, mutated, confirmed_clear) on every session, so
// the cross-host question is answerable here and is the more useful one for a recurring alert: an
// operator meeting `device-down` for the ninth time wants to know how it ended the other eight times, on
// which machines, and which resolution actually held.
//
// FOLDED BY RULE FAMILY, NOT BY STRING. The same condition arrives under different names from different
// sources — production carries `Device-Down`, `Devices-up/down`, `Device-Down-SNMP-unreachable` and
// `Device-Down-Due-to-no-ICMP-response.` for one fault class. Keying on the raw string would split that
// family four ways and hide the recurrence that makes the page worth reading. The canonical name is
// resolved by the caller (core/knowledge.CanonicalRule, the family authority) and arrives here already
// folded, so this package stays dependency-free; the RAW names ride along and are rendered, because an
// operator searching for the string they saw in the alert must still find the page.

// RuleSession is one triage session as the rule compiler needs it.
type RuleSession struct {
	ExternalRef string
	Host        string
	// Rule is the CANONICAL family name, resolved by the caller.
	Rule string
	// RawRule is the source vocabulary's name — rendered so the page is findable by what the alert said.
	RawRule        string
	Outcome        string
	OpClass        string
	Mutated        bool
	ConfirmedClear bool
	CreatedAt      time.Time
}

// RuleInputs is the compiler's whole world: already-fetched, already-canonicalized sessions.
type RuleInputs struct {
	Sessions []RuleSession
	// Truncated is true when the read hit its bound, so the page can say its counts are a floor.
	Truncated bool
}

const (
	ruleSlugPrefix = "rule-"
	// maxRuleHosts / maxRuleResolutions bound what one page RENDERS; the counts above them are always the
	// full number the compiler saw.
	maxRuleHosts       = 40
	maxRuleResolutions = 25
)

// CompileRules renders one page per rule family and returns the families it refused, with reasons.
func CompileRules(in RuleInputs) ([]Article, []Skip) {
	byRule := map[string][]RuleSession{}
	for _, s := range in.Sessions {
		r := strings.TrimSpace(s.Rule)
		if r == "" {
			continue // a session with no rule cannot be grouped; it is not a refusal, it is not a rule
		}
		byRule[r] = append(byRule[r], s)
	}

	names := make([]string, 0, len(byRule))
	for r := range byRule {
		names = append(names, r)
	}
	sort.Strings(names)

	articles := make([]Article, 0, len(names))
	var skips []Skip
	for _, rule := range names {
		slug, ok := SafeSlug(ruleSlugPrefix, rule)
		if !ok {
			skips = append(skips, Skip{Kind: "rule",
				Host:   rule, // the Skip's subject; for rule pages that is the family name
				Reason: "rule name is not a safe page identifier — refused rather than rewritten, because two rules rewritten to one slug would merge unrelated fault classes onto a single page",
			})
			continue
		}
		sess := byRule[rule]
		// Newest first, deterministically: a tie on the instant falls back to the ref so two sessions
		// recorded in the same second cannot reorder between compiles.
		sort.Slice(sess, func(i, j int) bool {
			if !sess[i].CreatedAt.Equal(sess[j].CreatedAt) {
				return sess[i].CreatedAt.After(sess[j].CreatedAt)
			}
			return sess[i].ExternalRef < sess[j].ExternalRef
		})
		articles = append(articles, Article{
			Slug:  slug,
			Title: rule,
			Kind:  "article",
			Body:  ruleBody(rule, sess, in.Truncated),
			Meta:  ruleMeta(rule, sess),
		})
	}
	sort.Slice(skips, func(i, j int) bool { return skips[i].Host < skips[j].Host })
	return articles, skips
}

func ruleMeta(rule string, sess []RuleSession) map[string]string {
	hosts, raws := map[string]bool{}, map[string]bool{}
	healed := 0
	for _, s := range sess {
		if s.Host != "" {
			hosts[s.Host] = true
		}
		if s.RawRule != "" {
			raws[s.RawRule] = true
		}
		if s.Mutated && s.ConfirmedClear {
			healed++
		}
	}
	return map[string]string{
		"rule":            rule,
		"sessions":        fmt.Sprint(len(sess)),
		"hosts":           fmt.Sprint(len(hosts)),
		"source_names":    fmt.Sprint(len(raws)),
		"confirmed_heals": fmt.Sprint(healed),
	}
}

func ruleBody(rule string, sess []RuleSession, truncated bool) string {
	var b strings.Builder
	w := func(s string) { b.WriteString(s); b.WriteByte('\n') }

	hostSet, rawSet := map[string]int{}, map[string]int{}
	byOutcome := map[string]int{}
	type resolution struct {
		opClass string
		host    string
		ref     string
		when    time.Time
	}
	var confirmed []resolution
	actedUnconfirmed, proposedOnly, stoodDown := 0, 0, 0

	for _, s := range sess {
		if s.Host != "" {
			hostSet[s.Host]++
		}
		if s.RawRule != "" {
			rawSet[s.RawRule]++
		}
		byOutcome[firstNonEmpty(s.Outcome, "(none recorded)")]++
		switch {
		case s.Mutated && s.ConfirmedClear:
			confirmed = append(confirmed, resolution{s.OpClass, s.Host, s.ExternalRef, s.CreatedAt})
		case s.Mutated:
			actedUnconfirmed++
		case strings.HasPrefix(s.Outcome, "proposed"):
			proposedOnly++
		default:
			stoodDown++
		}
	}

	w("# " + rule)
	w("")
	w("Every line below is derived from TG's own triage spine. Nothing here is authored.")
	w("")

	// ── The headline an operator came for ──────────────────────────────────────
	w("## How often, and where")
	w("")
	w(fmt.Sprintf("**%d session(s) across %d host(s).**", len(sess), len(hostSet)))
	if truncated {
		w("")
		w("The read that produced this page was bounded, so every count here is a FLOOR — this rule has " +
			"fired at least this often, possibly more.")
	}
	w("")
	if len(hostSet) == 0 {
		w("No session for this rule recorded a host, so the estate spread is unknown.")
	} else {
		hosts := make([]string, 0, len(hostSet))
		for h := range hostSet {
			hosts = append(hosts, h)
		}
		sort.Slice(hosts, func(i, j int) bool {
			if hostSet[hosts[i]] != hostSet[hosts[j]] {
				return hostSet[hosts[i]] > hostSet[hosts[j]]
			}
			return hosts[i] < hosts[j]
		})
		shown := hosts
		if len(shown) > maxRuleHosts {
			shown = shown[:maxRuleHosts]
		}
		w("| host | times |")
		w("|---|---|")
		for _, h := range shown {
			w(fmt.Sprintf("| %s | %d |", mdCell(h), hostSet[h]))
		}
		if len(hosts) > len(shown) {
			w("")
			w(fmt.Sprintf("Showing %d of %d hosts.", len(shown), len(hosts)))
		}
	}
	w("")

	// ── The vocabulary spread — why this page exists at all ────────────────────
	w("## What this alert is called")
	w("")
	switch len(rawSet) {
	case 0:
		w("No source name was recorded for any session of this rule.")
	case 1:
		for r := range rawSet {
			w(fmt.Sprintf("One source name: `%s`.", mdCell(r)))
		}
	default:
		w(fmt.Sprintf("**%d different source names denote this same condition**, folded into one family "+
			"here. Keying on the raw string would split this page %d ways and hide the recurrence:", len(rawSet), len(rawSet)))
		w("")
		raws := make([]string, 0, len(rawSet))
		for r := range rawSet {
			raws = append(raws, r)
		}
		sort.Strings(raws)
		for _, r := range raws {
			w(fmt.Sprintf("- `%s` — %d session(s)", mdCell(r), rawSet[r]))
		}
	}
	w("")

	// ── How it ends ────────────────────────────────────────────────────────────
	w("## How it ends")
	w("")
	w(fmt.Sprintf("- **%d** acted on and confirmed clear", len(confirmed)))
	w(fmt.Sprintf("- **%d** acted on, clear NOT confirmed — TG changed something and could not verify it held", actedUnconfirmed))
	w(fmt.Sprintf("- **%d** proposed but never carried out", proposedOnly))
	w(fmt.Sprintf("- **%d** stood down without a proposal", stoodDown))
	w("")
	w("| recorded outcome | sessions |")
	w("|---|---|")
	outs := make([]string, 0, len(byOutcome))
	for o := range byOutcome {
		outs = append(outs, o)
	}
	sort.Slice(outs, func(i, j int) bool {
		if byOutcome[outs[i]] != byOutcome[outs[j]] {
			return byOutcome[outs[i]] > byOutcome[outs[j]]
		}
		return outs[i] < outs[j]
	})
	for _, o := range outs {
		w(fmt.Sprintf("| %s | %d |", mdCell(o), byOutcome[o]))
	}
	w("")

	// ── What actually held ─────────────────────────────────────────────────────
	//
	// The one section worth acting on. A proposal that was never carried out is not evidence; a heal that
	// was never confirmed clear is not evidence either. Only a confirmed-clean resolution earns this list.
	w("## What actually held")
	w("")
	if len(confirmed) == 0 {
		w("**Nothing yet.** No session for this rule was both acted on AND confirmed clear, so there is no " +
			"resolution here that is known to have worked. That is not the same as saying nothing works — " +
			"it means the spine has not recorded one holding.")
	} else {
		sort.Slice(confirmed, func(i, j int) bool {
			if !confirmed[i].when.Equal(confirmed[j].when) {
				return confirmed[i].when.After(confirmed[j].when)
			}
			return confirmed[i].ref < confirmed[j].ref
		})
		w(fmt.Sprintf("**%d confirmed-clean resolution(s)** — acted on, and the original condition verified "+
			"cleared afterwards:", len(confirmed)))
		w("")
		w("| when | host | op-class | session |")
		w("|---|---|---|---|")
		shown := confirmed
		if len(shown) > maxRuleResolutions {
			shown = shown[:maxRuleResolutions]
		}
		for _, c := range shown {
			w(fmt.Sprintf("| %s | %s | %s | %s |",
				c.when.UTC().Format("2006-01-02 15:04"), mdCell(c.host),
				mdCell(firstNonEmpty(c.opClass, "—")), mdCell(c.ref)))
		}
		if len(confirmed) > len(shown) {
			w("")
			w(fmt.Sprintf("Showing %d of %d.", len(shown), len(confirmed)))
		}
	}

	return b.String()
}
