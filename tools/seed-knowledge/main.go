// Command seed-knowledge extracts the predecessor claude-gateway `incident_knowledge` corpus into
// Territory Grounder's precedent-retrieval JSON (core/knowledge.Incident), so KNOWN incident shapes stop
// classifying as ood-novel (the novelty gate's autonomy converges) and precedent retrieval becomes real
// (TG-125).
//
// # The #1 correctness requirement — provable-same-slug
//
// The novelty gate is knowledge.Count(host, alertRule) — an eqFold(trim+case-fold) match on BOTH host and
// rule. The classifier calls it with host = the action target (the LibreNMS device.hostname) and
// alertRule = env.AlertRule = librenms.SlugifyRule(rawRuleName). If a seeded row's stored alert_rule is not
// EXACTLY that slug, the corpus populates but Count stays 0 — a silent no-op. We defeat that BY
// CONSTRUCTION: this tool slugs every rule through the SAME exported librenms.SlugifyRule the live ingester
// uses, and core/knowledge/seed_roundtrip_test.go re-proves it against the committed file.
//
// # Read-only, native, deterministic
//
// The predecessor DB is opened mode=ro (never written). Output is deterministic: dedup + sort by
// external_ref via knowledge.MergeCorpus, exactly the canonical order the worker's own lessons merge
// produces — so the committed file is byte-stable and the worker's first rewrite is a no-op diff.
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	knowledge "github.com/territory-grounder/grounder/core/knowledge"
	screen "github.com/territory-grounder/grounder/core/screen"
	librenms "github.com/territory-grounder/grounder/modules/ingest/librenms"

	_ "modernc.org/sqlite"
)

const defaultDB = "/home/tg/gitlab/products/cubeos/claude-context/gateway.db"

// selectKeyed is the extract query. It is a STATIC string (no fmt.Sprintf, no string concatenation with a
// variable) — INV-03 parameterized/no-string-SQL clean. It keeps only KEYED rows (host + rule populated)
// that are NOT bi-temporally invalidated. `valid_until IS NULL OR valid_until > datetime('now')` is the
// predecessor's OWN validity predicate (scripts/kb-semantic-search.py): a FUTURE valid_until is a still-open
// suppression window (KEEP — these carry the highest Tier-0 stand-down value), a PAST one is superseded
// knowledge (DROP — never resurrect it). The non-LibreNMS host drop happens in Go (documented below).
const selectKeyed = `
SELECT id,
       alert_rule,
       hostname,
       COALESCE(root_cause, ''),
       COALESCE(resolution, ''),
       COALESCE(tags, '')
FROM incident_knowledge
WHERE TRIM(alert_rule) <> ''
  AND TRIM(hostname) <> ''
  AND (valid_until IS NULL OR valid_until > datetime('now'))
ORDER BY id`

const summaryMaxRunes = 500

func main() {
	out := flag.String("out", "", "path to write the deterministic corpus JSON array (required)")
	db := flag.String("db", defaultDB, "path to the POPULATED predecessor gateway.db (opened read-only)")
	phase := flag.Int("phase", 1, "seed phase label (informational; the filter is the same every phase)")
	flag.Parse()
	if *out == "" {
		fmt.Fprintln(os.Stderr, "seed-knowledge: --out is required")
		os.Exit(2)
	}

	incs, st, err := extract(*db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "seed-knowledge: %v\n", err)
		os.Exit(1)
	}

	// MergeCorpus(nil, incs) dedups by external_ref (unique here) and sorts by external_ref — the EXACT
	// canonical order the worker's lessons merge produces, so the committed file is byte-stable.
	corpus := knowledge.MergeCorpus(nil, incs)

	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "seed-knowledge: create %s: %v\n", *out, err)
		os.Exit(1)
	}
	if werr := knowledge.WriteCorpus(f, corpus); werr != nil {
		f.Close()
		fmt.Fprintf(os.Stderr, "seed-knowledge: write corpus: %v\n", werr)
		os.Exit(1)
	}
	if cerr := f.Close(); cerr != nil {
		fmt.Fprintf(os.Stderr, "seed-knowledge: close %s: %v\n", *out, cerr)
		os.Exit(1)
	}

	fmt.Printf("seed-knowledge phase %d → %s\n", *phase, *out)
	fmt.Printf("  scanned (keyed, non-invalidated): %d\n", st.scanned)
	fmt.Printf("  dropped non-LibreNMS host:         %d  (%s)\n", st.droppedHost, strings.Join(st.droppedHosts, ", "))
	fmt.Printf("  canonicalized rule (drift fixed):  %d\n", st.canonicalized)
	fmt.Printf("  rows with a secret redaction:      %d\n", st.redactedRows)
	fmt.Printf("  emitted incidents:                 %d\n", len(corpus))
	fmt.Printf("  wildcard (hostname='*') rows:      %d\n", st.wildcard)
	fmt.Printf("  guinea-pig golden rows present:    %d/%d\n", st.golden, len(goldenPairs))
	for _, g := range goldenPairs {
		mark := "MISSING"
		if st.goldenSeen[g.key()] {
			mark = "ok"
		}
		fmt.Printf("    [%s] %s / %s\n", mark, g.host, g.rule)
	}
	for _, w := range st.warnings {
		fmt.Printf("  WARN: %s\n", w)
	}
}

type stats struct {
	scanned       int
	droppedHost   int
	droppedHosts  []string
	canonicalized int
	redactedRows  int
	wildcard      int
	golden        int
	goldenSeen    map[string]bool
	warnings      []string
}

// goldenPair is a (host, current-live rawRule) the extract MUST de-novel — asserted here and, load-bearing,
// in core/knowledge/seed_roundtrip_test.go. rawRule is the CURRENT LIVE rule name (period-less for the
// up/down family — see canonicalizeRule), i.e. what the live LibreNMS ingester actually posts today.
type goldenPair struct{ host, rule string }

func (g goldenPair) key() string {
	return strings.ToLower(strings.TrimSpace(g.host)) + "\x00" + librenms.SlugifyRule(g.rule)
}

var goldenPairs = []goldenPair{
	{"dc1librespeed01", "Space on / is >= 90% and < 95% in use"},
	{"dc1librespeed01", "Service up/down"},
	{"dc1librespeed01", "Device Down (SNMP unreachable)"},
	{"dc1librespeed01", "Devices up/down"},
	{"dc1myspeed01", "Devices up/down"},
}

func extract(dbPath string) ([]knowledge.Incident, stats, error) {
	st := stats{goldenSeen: map[string]bool{}}

	// mode=ro: read-only open. immutable is NOT set (the file could be touched by another reader), but ro
	// guarantees this process never writes to the predecessor DB.
	conn, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return nil, st, fmt.Errorf("open %s: %w", dbPath, err)
	}
	defer conn.Close()

	rows, err := conn.Query(selectKeyed)
	if err != nil {
		return nil, st, fmt.Errorf("query incident_knowledge: %w", err)
	}
	defer rows.Close()

	seenGoldenSlug := map[string]bool{}
	for _, g := range goldenPairs {
		seenGoldenSlug[g.key()] = false
	}

	var out []knowledge.Incident
	for rows.Next() {
		var id int64
		var rawRule, host, rootCause, resolution, tags string
		if err := rows.Scan(&id, &rawRule, &host, &rootCause, &resolution, &tags); err != nil {
			return nil, st, fmt.Errorf("scan row: %w", err)
		}
		st.scanned++

		if reason, drop := nonLibreNMSHost(host); drop {
			st.droppedHost++
			st.droppedHosts = append(st.droppedHosts, fmt.Sprintf("%s(%s)", host, reason))
			continue
		}

		canon := canonicalizeRule(rawRule)
		if canon != strings.TrimSpace(rawRule) {
			st.canonicalized++
		}
		slug := librenms.SlugifyRule(canon)
		if slug == "" {
			// A rule that slugs to empty could never match the live ingester's env.AlertRule (which would
			// itself be rejected at ingest). Skip and warn rather than emit an unmatchable row.
			st.warnings = append(st.warnings, fmt.Sprintf("id %d rule %q slugged to empty — skipped", id, rawRule))
			continue
		}

		// Redact the FULL text first, THEN clip — clipping a raw secret could bisect it and leave a partial
		// credential the redactor's shape rules no longer match. Redact replaces each secret with a short
		// [REDACTED:<kind>] marker, so clipping the redacted text can never expose one.
		summaryRed, sRed := screen.Redact(rootCause)
		resRed, rRed := screen.Redact(resolution)
		summary := clip(summaryRed, summaryMaxRunes)
		res := clip(resRed, summaryMaxRunes)
		tagList, tRed := redactTags(tags)
		if len(sRed) > 0 || len(rRed) > 0 || tRed {
			st.redactedRows++
		}

		inc := knowledge.Incident{
			ExternalRef: fmt.Sprintf("pred-ik-%d", id),
			Host:        host,
			AlertRule:   slug,
			Site:        deriveSite(host),
			Summary:     summary,
			Resolution:  res,
			Tags:        tagList,
		}
		out = append(out, inc)

		if strings.TrimSpace(host) == "*" {
			st.wildcard++
		}
		gk := strings.ToLower(strings.TrimSpace(host)) + "\x00" + slug
		if _, isGolden := seenGoldenSlug[gk]; isGolden && !st.goldenSeen[gk] {
			st.goldenSeen[gk] = true
			st.golden++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, st, fmt.Errorf("iterate rows: %w", err)
	}

	for _, g := range goldenPairs {
		if !st.goldenSeen[g.key()] {
			st.warnings = append(st.warnings, fmt.Sprintf("guinea-pig golden pair NOT seeded: %s / %s", g.host, g.rule))
		}
	}
	sort.Strings(st.droppedHosts)
	return out, st, nil
}

// nonLibreNMSHost reports whether a host will NEVER be the target of a LibreNMS (host,rule) alert, so its
// rows cannot de-novel a LibreNMS incident and are dropped. The wildcard host "*" and real estate devices
// (dc1*/grskg0*) are KEPT. The two origins here are Prometheus/k8s-origin hosts (Alertmanager posts
// them, not LibreNMS); their host-agnostic shapes are carried instead by the hostname="*" wildcard rows.
func nonLibreNMSHost(host string) (string, bool) {
	switch {
	case host == "my-awx-web":
		return "awx k8s pod origin", true
	case strings.HasPrefix(host, "prometheus-"):
		return "prometheus k8s-origin pod", true
	default:
		return "", false
	}
}

// canonicalizeRule maps a predecessor historical rule string to the CURRENT LIVE LibreNMS rule name BEFORE
// slugging (Caveat A). It is a small, EVIDENCE-BASED collapse table — not a blanket "strip trailing dot",
// because some live rule names legitimately end in '.' (e.g. "Device Down! Due to no ICMP response.").
//
// Evidence (predecessor incident_knowledge.created_at recency):
//   - " - Critical Alert." suffix: appears ONLY on 2026-04-03; the base "Device Down! Due to no ICMP
//     response." runs through 2026-07-13 (current) → strip the suffix, keep the base verbatim.
//   - "Devices up/down." / "Service up/down." / "Port status up/down." / "Device rebooted.": the dotted
//     forms appear ONLY on 2026-04-03; the period-less forms run through 2026-07-19 and match the live rule
//     catalog (eval/corpus.json, the /api/v0/rules mock, tools_test.go) → strip the trailing dot.
func canonicalizeRule(raw string) string {
	r := strings.TrimSpace(raw)
	if strings.HasSuffix(r, " - Critical Alert.") {
		r = strings.TrimSpace(strings.TrimSuffix(r, " - Critical Alert."))
	}
	switch r {
	case "Devices up/down.":
		return "Devices up/down"
	case "Service up/down.":
		return "Service up/down"
	case "Port status up/down.":
		return "Port status up/down"
	case "Device rebooted.":
		return "Device rebooted"
	}
	return r
}

// deriveSite is COSMETIC (knowledge.Count ignores site; only the lexical retriever's tiebreak reads it).
// dc1*→"nl", grskg0*(dc2/dc3)→"gr", wildcard/other→"".
func deriveSite(host string) string {
	switch {
	case strings.HasPrefix(host, "dc1"):
		return "nl"
	case strings.HasPrefix(host, "grskg0"):
		return "gr"
	default:
		return ""
	}
}

// redactTags splits the predecessor comma-joined tags, redacts each (an LLM session summary can structurally
// name a secret), trims, and drops empties. Returns the cleaned list and whether any tag was redacted.
func redactTags(tags string) ([]string, bool) {
	var out []string
	redacted := false
	for _, t := range strings.Split(tags, ",") {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		red, kinds := screen.Redact(t)
		if len(kinds) > 0 {
			redacted = true
		}
		red = strings.TrimSpace(red)
		if red != "" {
			out = append(out, red)
		}
	}
	if len(out) == 0 {
		return nil, redacted
	}
	return out, redacted
}

// clip truncates to at most n runes (rune-safe, never mid-codepoint), appending an ellipsis when it cut.
// Redaction runs BEFORE clip, so a truncation can never bisect a secret (already a short [REDACTED:…] marker).
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}
