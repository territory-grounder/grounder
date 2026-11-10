package wikicompile

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// THE DECISIONS DIGEST — what TG decided, and what it DECLINED to do.
//
// The governance ledger is hash-chained, complete, and unreadable at scale: 8,570 rows in production.
// The console's ledger view shows it row by row, which answers "what happened at seq 8412" and nothing
// else. The question an operator actually has is the shape of the whole thing — how often TG acts, how
// often it holds, and what holding costs.
//
// WITHHELD IS THE POINT. Every declined action is on the ledger and invisible outside it: production
// carries 1,616 withheld decisions, 1,315 of them a POLL_PAUSE classification holding an action for a
// human and 143 an outright actuation refusal. A surface that only reports what TG DID would describe
// less than a fifth of its governance behaviour.
//
// THE NUMBER THIS PAGE EXISTS TO SURFACE is none of those: it is
// `human:poll-obsolete:subject-recovered`, 956 in production. That is 956 occasions where TG paused for
// a human vote and the incident resolved itself before anyone voted. It is not a failure — standing down
// on a recovered subject is correct, and the vote was genuinely not needed. But it is the honest cost of
// the polling posture, and nothing anywhere reports it. An operator deciding whether to widen autonomy
// should see it beside the refusals rather than have to reconstruct it from a hash chain.

// DecisionTally is one decision kind's counts, already aggregated by the caller.
type DecisionTally struct {
	Decision string
	Total    int
	Withheld int
	Newest   time.Time
}

// DecisionInputs is the digest's whole world.
type DecisionInputs struct {
	Tallies []DecisionTally
	// LedgerTotal is every row in the chain, including kinds the tallies do not name. It is carried so
	// the page can state its own coverage rather than imply the tallies are the whole ledger.
	LedgerTotal int
}

// DecisionsSlug is the fixed slug of the digest.
const DecisionsSlug = "governance-decisions"

// maxDecisionRows bounds the rendered table; the counts above it are always the full figures.
const maxDecisionRows = 40

// obsoletePollPrefix is the decision family recording a poll that expired because the subject recovered
// on its own. Matched by PREFIX because the ledger's decision strings carry a reason suffix.
const obsoletePollPrefix = "human:poll-obsolete"

// CompileDecisions renders the digest. Pure and clock-free, like every compiler here.
func CompileDecisions(in DecisionInputs) Article {
	var b strings.Builder
	w := func(s string) { b.WriteString(s); b.WriteByte('\n') }

	tallies := make([]DecisionTally, len(in.Tallies))
	copy(tallies, in.Tallies)
	sort.Slice(tallies, func(i, j int) bool {
		if tallies[i].Total != tallies[j].Total {
			return tallies[i].Total > tallies[j].Total
		}
		return tallies[i].Decision < tallies[j].Decision
	})

	named, withheld, obsolete := 0, 0, 0
	for _, t := range tallies {
		named += t.Total
		withheld += t.Withheld
		if strings.HasPrefix(t.Decision, obsoletePollPrefix) {
			obsolete += t.Total
		}
	}

	w("# Governance decisions")
	w("")
	w("Every governed decision TG has recorded, aggregated from the hash-chained ledger. Nothing here is " +
		"authored; the ledger is the record and this is its shape.")
	w("")

	if len(tallies) == 0 {
		w("## Nothing recorded")
		w("")
		w("The governance ledger holds no decisions. That is a statement about the ledger, not about the " +
			"estate: it means nothing has been governed yet, not that nothing has happened.")
		return Article{
			Slug: DecisionsSlug, Title: "Governance decisions", Kind: "article", Body: b.String(),
			Meta: map[string]string{"decisions": "0", "withheld": "0"},
		}
	}

	// ── Acted vs held ──────────────────────────────────────────────────────────
	w("## Acted, or held")
	w("")
	w(fmt.Sprintf("**%d decision(s)** across %d kind(s).", named, len(tallies)))
	if in.LedgerTotal > named {
		w("")
		w(fmt.Sprintf("The chain holds %d rows in total; %d carry a decision kind and are counted here. "+
			"The remainder are rows this digest does not name.", in.LedgerTotal, named))
	}
	w("")
	pct := func(n int) string {
		if named == 0 {
			return "—"
		}
		return fmt.Sprintf("%.0f%%", float64(n)*100/float64(named))
	}
	w(fmt.Sprintf("- **%d (%s) WITHHELD** — TG declined to act, or held the action for a human", withheld, pct(withheld)))
	w(fmt.Sprintf("- **%d (%s)** proceeded", named-withheld, pct(named-withheld)))
	w("")
	w("A withheld decision is not a failure. It is the governance working: an action that did not meet " +
		"its bar, or one whose risk band requires a person. What it is NOT is visible anywhere else — the " +
		"ledger records every one and no surface reports the shape.")
	w("")

	// ── The cost of waiting ────────────────────────────────────────────────────
	//
	// The one figure that argues about posture rather than describing it.
	w("## What waiting cost")
	w("")
	if obsolete == 0 {
		w("No poll expired because its subject recovered on its own. Either nothing has been polled long " +
			"enough to go obsolete, or every poll was answered while it still mattered.")
	} else {
		w(fmt.Sprintf("**%d poll(s) went obsolete because the subject recovered before anyone voted.**", obsolete))
		w("")
		w("Standing down on a recovered subject is CORRECT — the vote genuinely was not needed, and TG " +
			"changing nothing is the right outcome. This is not a defect count.")
		w("")
		w("It is the honest cost of the polling posture, and it belongs beside the refusals rather than " +
			"reconstructed from a hash chain: it measures how often a human was asked for something that " +
			"stopped mattering while they were being asked.")
	}
	w("")

	// ── The breakdown ──────────────────────────────────────────────────────────
	w("## By decision")
	w("")
	w("| decision | total | withheld | most recent |")
	w("|---|---|---|---|")
	shown := tallies
	if len(shown) > maxDecisionRows {
		shown = shown[:maxDecisionRows]
	}
	for _, t := range shown {
		when := "—"
		if !t.Newest.IsZero() {
			when = t.Newest.UTC().Format("2006-01-02 15:04")
		}
		w(fmt.Sprintf("| %s | %d | %d | %s |", mdCell(t.Decision), t.Total, t.Withheld, when))
	}
	if len(tallies) > len(shown) {
		w("")
		w(fmt.Sprintf("Showing the %d most frequent of %d decision kinds.", len(shown), len(tallies)))
	}

	return Article{
		Slug:  DecisionsSlug,
		Title: "Governance decisions",
		Kind:  "article",
		Body:  b.String(),
		Meta: map[string]string{
			"decisions":      fmt.Sprint(named),
			"withheld":       fmt.Sprint(withheld),
			"kinds":          fmt.Sprint(len(tallies)),
			"obsolete_polls": fmt.Sprint(obsolete),
		},
	}
}
