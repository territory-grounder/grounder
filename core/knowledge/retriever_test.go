package knowledge

import (
	"bytes"
	"strings"
	"testing"
)

func corpus() []Incident {
	return []Incident{
		{ExternalRef: "TG-100", Host: "web01", AlertRule: "NginxDown", Site: "nl", Summary: "nginx worker crashed under load", Resolution: "restart nginx", Tags: []string{"web", "restart"}},
		{ExternalRef: "TG-101", Host: "web02", AlertRule: "NginxDown", Site: "nl", Summary: "nginx oom killed", Resolution: "raise memory limit", Tags: []string{"web", "memory"}},
		{ExternalRef: "TG-102", Host: "db01", AlertRule: "DiskFull", Site: "gr", Summary: "postgres wal filled the disk", Resolution: "prune wal archives", Tags: []string{"db", "disk"}},
	}
}

// The same-alert-rule precedent ranks first; an exact host match adds to it; an unrelated incident is not
// retrieved at all.
func TestRetrieveRanksByRelevance(t *testing.T) {
	r := NewLexicalRetriever(corpus())
	hits := r.Retrieve(Query{Host: "web01", AlertRule: "NginxDown", Site: "nl", Summary: "nginx crashed", Tags: []string{"web"}}, 5)
	if len(hits) != 2 { // both NginxDown incidents; DiskFull shares nothing → excluded
		t.Fatalf("expected 2 relevant hits, got %d: %+v", len(hits), hits)
	}
	// TG-100 (same host web01 + same rule + tag) must outrank TG-101 (same rule + tag, different host).
	if hits[0].Incident.ExternalRef != "TG-100" {
		t.Fatalf("the same-host same-rule precedent must rank first, got %s", hits[0].Incident.ExternalRef)
	}
	if hits[0].Score <= hits[1].Score {
		t.Fatalf("scores must be ordered: %.2f !> %.2f", hits[0].Score, hits[1].Score)
	}
	// a query matching nothing retrieves nothing
	if got := r.Retrieve(Query{Host: "zzz", AlertRule: "UnheardOf"}, 5); len(got) != 0 {
		t.Fatalf("an unmatched query must retrieve nothing, got %+v", got)
	}
	// k bounds the result
	if got := r.Retrieve(Query{AlertRule: "NginxDown"}, 1); len(got) != 1 {
		t.Fatalf("k must bound results, got %d", len(got))
	}
}

// The rendered context frames precedent as DATA and carries the resolutions.
func TestContextRendersPrecedent(t *testing.T) {
	r := NewLexicalRetriever(corpus())
	ctx := Context(r.Retrieve(Query{AlertRule: "NginxDown", Host: "web01"}, 5))
	if !strings.Contains(ctx, "not instructions") {
		t.Fatal("the precedent block must frame itself as data, not instructions")
	}
	if !strings.Contains(ctx, "restart nginx") {
		t.Fatalf("the resolution must be carried into the context, got:\n%s", ctx)
	}
	if Context(nil) != "" {
		t.Fatal("empty hits must render an empty context")
	}
}

// The holder swaps the corpus atomically at runtime, so a reload takes effect without rebuilding callers.
func TestHolderReloadSwapsCorpus(t *testing.T) {
	h := NewHolder(NewLexicalRetriever(corpus()))
	if got := h.Retrieve(Query{AlertRule: "DiskFull"}, 5); len(got) != 1 || got[0].Incident.ExternalRef != "TG-102" {
		t.Fatalf("initial corpus must retrieve TG-102, got %+v", got)
	}
	// reload with a corpus that has a NEW precedent for DiskFull.
	h.Set(NewLexicalRetriever([]Incident{{ExternalRef: "TG-900", AlertRule: "DiskFull", Host: "db02", Resolution: "extend the LV"}}))
	got := h.Retrieve(Query{AlertRule: "DiskFull"}, 5)
	if len(got) != 1 || got[0].Incident.ExternalRef != "TG-900" {
		t.Fatalf("after reload, the new corpus must be retrieved, got %+v", got)
	}
	// nil init never panics; nil Set is ignored.
	empty := NewHolder(nil)
	empty.Set(nil)
	if len(empty.Retrieve(Query{AlertRule: "X"}, 5)) != 0 {
		t.Fatal("an empty holder retrieves nothing")
	}
}

// MergeCorpus dedups by ExternalRef (new wins), orders deterministically, and round-trips through
// WriteCorpus→ParseCorpus — the write-side of the lessons→corpus loop.
func TestMergeAndWriteCorpus(t *testing.T) {
	existing := []Incident{
		{ExternalRef: "TG-1", AlertRule: "NginxDown", Resolution: "old fix"},
		{ExternalRef: "TG-2", AlertRule: "DiskFull", Resolution: "prune"},
	}
	added := []Incident{
		{ExternalRef: "TG-1", AlertRule: "NginxDown", Resolution: "NEW fix"}, // updates TG-1
		{ExternalRef: "TG-3", AlertRule: "OOM", Resolution: "raise limit"},   // new
		{ExternalRef: "", AlertRule: "X"},                                    // no ref → dropped
	}
	merged := MergeCorpus(existing, added)
	if len(merged) != 3 {
		t.Fatalf("expected 3 deduped incidents (ref-less dropped), got %d: %+v", len(merged), merged)
	}
	if merged[0].ExternalRef != "TG-1" || merged[0].Resolution != "NEW fix" {
		t.Fatalf("the newer record must win for TG-1, got %+v", merged[0])
	}
	// round-trip through the serializer + parser.
	var buf bytes.Buffer
	if err := WriteCorpus(&buf, merged); err != nil {
		t.Fatal(err)
	}
	back, err := ParseCorpus(&buf)
	if err != nil {
		t.Fatalf("the written corpus must parse back: %v", err)
	}
	if len(back) != 3 || back[2].ExternalRef != "TG-3" {
		t.Fatalf("round-trip mismatch: %+v", back)
	}
}

// TestCountBySignature proves Count is the novelty gate's data source: it returns the exact (host, rule)
// prior-incident count (case-insensitive), 0 for a never-seen signature (→ the gate forces a poll), and the
// Holder delegates to the current corpus.
func TestCountBySignature(t *testing.T) {
	corp := []Incident{
		{ExternalRef: "TG-1", Host: "web01", AlertRule: "NginxDown"},
		{ExternalRef: "TG-2", Host: "web01", AlertRule: "NginxDown"},
		{ExternalRef: "TG-3", Host: "web01", AlertRule: "DiskFull"},
		{ExternalRef: "TG-4", Host: "db01", AlertRule: "NginxDown"},
	}
	r := NewLexicalRetriever(corp)
	if got := r.Count("web01", "NginxDown"); got != 2 {
		t.Errorf("Count(web01,NginxDown) = %d, want 2", got)
	}
	if got := r.Count(" WEB01 ", "nginxdown"); got != 2 { // case/space-insensitive
		t.Errorf("Count must be case-insensitive/trimmed, got %d", got)
	}
	if got := r.Count("web99", "NeverSeen"); got != 0 { // novel → 0 → forces a poll
		t.Errorf("a novel signature must count 0, got %d", got)
	}
	if got := NewHolder(r).Count("web01", "DiskFull"); got != 1 {
		t.Errorf("Holder.Count must delegate to the corpus, got %d", got)
	}
}

// TestCountWildcardFleetWide proves the predecessor's fleet-wide precedent: a corpus row whose host is "*"
// de-novels its rule on EVERY host, while an exact-host row still de-novels only its own host — and the
// broadening is INERT unless such a "*" row is deliberately authored (a concrete-host corpus never matches a
// different host, so default novelty semantics are unchanged: the writeback only ever stores concrete hosts).
func TestCountWildcardFleetWide(t *testing.T) {
	wild := NewLexicalRetriever([]Incident{{ExternalRef: "TG-STAR", Host: "*", AlertRule: "NginxDown"}})
	if got := wild.Count("anyhost", "NginxDown"); got == 0 {
		t.Fatal("a \"*\" host row must de-novel the rule fleet-wide")
	}
	if got := wild.Count("some-other-host", "NginxDown"); got == 0 {
		t.Fatal("\"*\" must match every host for the rule")
	}
	if got := wild.Count("anyhost", "OtherRule"); got != 0 {
		t.Fatalf("\"*\" must not de-novel a DIFFERENT rule, got %d", got)
	}

	// Default (no wildcard row): a concrete-host corpus stays exact — a different host remains novel, so the
	// wildcard branch is inert unless an operator authors a "*" row.
	concrete := NewLexicalRetriever([]Incident{{ExternalRef: "TG-1", Host: "web01", AlertRule: "NginxDown"}})
	if got := concrete.Count("web01", "NginxDown"); got != 1 {
		t.Fatalf("exact host must still match, got %d", got)
	}
	if got := concrete.Count("web02", "NginxDown"); got != 0 {
		t.Fatalf("a concrete-host corpus must NOT match another host (wildcard inert unless a \"*\" row exists), got %d", got)
	}
}

// TestCountRuleFamilyCanonicalization proves the novelty signature matches by canonical rule FAMILY: a
// de-novel recorded under one device-down alias counts for the same physical fault arriving under a sibling
// alias (the guest-down fault reaches TG under ~4 LibreNMS rule names), so one confirmed-clean resolution
// de-novels them all instead of ~1/N. A rule in NO family keeps EXACT matching, and the host still binds.
func TestCountRuleFamilyCanonicalization(t *testing.T) {
	// a de-novel recorded under ONE device-down alias
	r := NewLexicalRetriever([]Incident{{ExternalRef: "TG-1", Host: "pve01", AlertRule: "Device-Down-Due-to-no-ICMP-response."}})
	// a SIBLING alias of the same family, same host → matches (non-novel)
	for _, alias := range []string{"Device-Down-SNMP-unreachable", "Devices-up/down", "device-down-due-to-no-icmp-response.", "HostDown"} {
		if got := r.Count("pve01", alias); got != 1 {
			t.Fatalf("cross-alias %q must match the device-down family (count 1), got %d", alias, got)
		}
	}
	// TargetDown is DELIBERATELY NOT in the family (a single scrape target/exporter down ≠ whole host down) —
	// grouping it would suppress the first-sight human poll for a genuine host-down, so it must stay novel.
	if got := r.Count("pve01", "TargetDown"); got != 0 {
		t.Fatalf("TargetDown must NOT collapse into device-down (distinct fault), got %d", got)
	}
	// host still binds: same family, different host → novel
	if got := r.Count("pve02", "Device-Down-SNMP-unreachable"); got != 0 {
		t.Fatalf("a family match on a DIFFERENT host must not count, got %d", got)
	}
	// a rule in NO family keeps EXACT matching — two distinct non-family rules do not collapse
	nf := NewLexicalRetriever([]Incident{{ExternalRef: "TG-2", Host: "web01", AlertRule: "NginxConfigReloadFailed"}})
	if got := nf.Count("web01", "NginxDown"); got != 0 {
		t.Fatalf("non-family rules must NOT collapse (exact match preserved), got %d", got)
	}
	if got := nf.Count("web01", "NginxConfigReloadFailed"); got != 1 {
		t.Fatalf("a non-family rule must still match itself exactly, got %d", got)
	}
}

// TestRuleFamilyMapWellFormed proves the embedded loadable family map parses and canonicalizes as intended,
// so a malformed edit fails the build/test rather than silently disabling family matching.
func TestRuleFamilyMapWellFormed(t *testing.T) {
	if canonicalRule("Device-Down-SNMP-unreachable") != canonicalRule("Device-Down-Due-to-no-ICMP-response.") {
		t.Fatal("device-down aliases must share one canonical family signature")
	}
	if canonicalRule("SomeUnmappedRule") != "someunmappedrule" {
		t.Fatal("an unmapped rule must canonicalize to its own trimmed/lower-cased identity")
	}
}
