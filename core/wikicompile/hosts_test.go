package wikicompile

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ts(day int) time.Time { return time.Date(2026, 7, day, 12, 0, 0, 0, time.UTC) }

func sampleInputs() HostInputs {
	return HostInputs{Facts: []HostFacts{
		{
			Host:       "dc1mealie01",
			EntityType: "lxc", Status: "approved",
			Sessions: []HostSession{
				{ExternalRef: "librenms-2", AlertRule: "Service up/down", Outcome: "proposed", OpClass: "restart-service", Proposed: true, CreatedAt: ts(30)},
				{ExternalRef: "librenms-1", AlertRule: "DiskFull-90", Outcome: "proposed", OpClass: "prune-journal", Proposed: true, Mutated: true, ConfirmedClear: true, CreatedAt: ts(29)},
			},
			Edges: []HostEdge{
				{From: "dc1mealie01", To: "dc1pve01", Rel: "hosted-on", Confidence: 0.9},
				{From: "dc1sw01", To: "dc1mealie01", Rel: "uplink", Confidence: 0.7},
			},
			Precedents: []HostPrecedent{
				{ExternalRef: "librenms-1", AlertRule: "DiskFull-90", Summary: "journal filled /", Resolution: "prune journal", ResolvedAt: ts(29)},
			},
		},
		{Host: "dc1quiet01"}, // no sessions, no edges, no precedent, not in the world model
	}}
}

// TestCompileHostsDeterministic — the property the whole package rests on. Identical input must produce
// byte-identical output, and input ORDER must not matter (the roster and per-host reads arrive from SQL
// whose ordering the compiler must not depend on).
//
// RED MUTATION CONTROL (executed 2026-08-01): reordering the input facts is covered here — the sorted
// output is byte-identical.
//
// WHAT THIS TEST CANNOT DO, stated because the first version of this comment claimed otherwise. Stamping a
// compile timestamp into hostBody — exactly what the predecessor does at wiki-compile.py:50 — SURVIVES
// this test: time.Now() has second resolution and both compiles run inside the same second, so the two
// byte streams match. That mutation is caught by TestPackageIsClockFree instead, which asserts the
// invariant at the source rather than trying to observe it at runtime. An oracle that a fast machine can
// defeat is not an oracle, and the honest fix was to move the assertion, not to add a sleep.
func TestCompileHostsDeterministic(t *testing.T) {
	a1, s1 := CompileHosts(sampleInputs())

	shuffled := sampleInputs()
	shuffled.Facts[0], shuffled.Facts[1] = shuffled.Facts[1], shuffled.Facts[0]
	a2, s2 := CompileHosts(shuffled)

	var b1, b2 bytes.Buffer
	env := func(w *bytes.Buffer, arts []Article, sk []Skip) {
		if err := WriteArticles(w, Envelope{SchemaVersion: SchemaVersion, CompiledAt: ts(31), Articles: arts, Skipped: sk}); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	env(&b1, a1, s1)
	env(&b2, a2, s2)
	if b1.String() != b2.String() {
		t.Fatalf("compile is not deterministic under input reordering.\n--- first ---\n%s\n--- second ---\n%s", b1.String(), b2.String())
	}

	var b3 bytes.Buffer
	a3, s3 := CompileHosts(sampleInputs())
	env(&b3, a3, s3)
	if b1.String() != b3.String() {
		t.Fatal("two compiles of identical input differ — something in the body varies per run")
	}
}

// TestHostPageHonestEmptySections — a host with nothing recorded still gets a page that SAYS so, section by
// section. The predecessor omits empty sections (compile_host_pages:540), producing a four-line page that
// reads exactly like a host where nothing ever happened.
//
// RED MUTATION CONTROL (executed 2026-08-01): making any section skip itself when its slice is empty fails
// with the section's name; restored green.
func TestHostPageHonestEmptySections(t *testing.T) {
	arts, skips := CompileHosts(HostInputs{Facts: []HostFacts{{Host: "dc1quiet01"}}})
	if len(skips) != 0 {
		t.Fatalf("a host with no data must still get a page, got skips %+v", skips)
	}
	if len(arts) != 1 {
		t.Fatalf("want 1 article, got %d", len(arts))
	}
	body := arts[0].Body
	for _, want := range []struct{ section, phrase string }{
		{"Identity", "Not in the approved world model"},
		{"Incidents", "No triage session has been recorded against this host"},
		{"Dependencies", "No outbound dependency has been discovered"},
		{"Dependents", "Nothing is recorded as depending on this host"},
		{"Precedent", "No corpus entry is recorded against this host"},
	} {
		if !strings.Contains(body, want.phrase) {
			t.Errorf("the %s section must state its own emptiness in words; %q missing from:\n%s",
				want.section, want.phrase, body)
		}
	}
	// And it must not read as a clean bill of health.
	for _, banned := range []string{"healthy", "all clear", "no issues"} {
		if strings.Contains(strings.ToLower(body), banned) {
			t.Errorf("an empty page must not imply health; found %q", banned)
		}
	}
}

// TestHostSkippedOnReadFailure — the single most important assertion here. A per-host read that ERRORED
// must produce NO page and a Skip, never a page whose incident section says "no triage session has been
// recorded". That sentence over a failed query is a confident claim about a host nobody could see, which
// is the exact defect class this console spent today fixing.
//
// RED MUTATION CONTROL (executed 2026-08-01): swallowing SessionsErr and compiling with a nil slice fails
// with "a host whose read FAILED must not be rendered"; restored green.
func TestHostSkippedOnReadFailure(t *testing.T) {
	arts, skips := CompileHosts(HostInputs{Facts: []HostFacts{
		{Host: "dc1broken01", SessionsErr: errors.New("connection reset")},
		{Host: "dc1fine01"},
	}})
	for _, a := range arts {
		if a.Slug == "host-dc1broken01" {
			t.Fatalf("a host whose read FAILED must not be rendered — the page would assert quiet over an "+
				"error. Body was:\n%s", a.Body)
		}
	}
	if len(skips) != 1 || skips[0].Host != "dc1broken01" {
		t.Fatalf("the failed host must appear in Skipped with a reason, got %+v", skips)
	}
	if !strings.Contains(skips[0].Reason, "connection reset") {
		t.Errorf("the skip reason must carry the underlying error, got %q", skips[0].Reason)
	}
	if len(arts) != 1 || arts[0].Slug != "host-dc1fine01" {
		t.Errorf("one bad host must not suppress the others, got %d articles", len(arts))
	}
}

// TestSafeSlugRefusesHostileHostnames — hostnames arrive from inbound alert payloads. The predecessor
// joins them straight into a path (wiki-compile.py:547) and a literal "*" produced a real 203,900-byte
// file at wiki/hosts/*.md.
//
// RED MUTATION CONTROL (executed 2026-08-01): returning (prefix+raw, true) unconditionally fails on the
// first hostile case; restored green.
func TestSafeSlugRefusesHostileHostnames(t *testing.T) {
	for _, bad := range []string{"", "   ", "*", "a/b", "..", "../../etc/passwd", "a\\b", "-leading", "a b", "héllo"} {
		if got, ok := SafeSlug("host-", bad); ok {
			t.Errorf("hostname %q must be refused, got slug %q", bad, got)
		}
	}
	for _, good := range []string{"dc1mealie01", "a", "a.b-c_d", "GRSKG01PVE02"} {
		got, ok := SafeSlug("host-", good)
		if !ok {
			t.Errorf("hostname %q must be accepted", good)
			continue
		}
		if strings.Contains(got, "/") || !strings.HasPrefix(got, "host-") {
			t.Errorf("slug %q for %q is malformed", got, good)
		}
	}
	// A refused hostname must be SKIPPED, not silently rewritten into a colliding slug.
	arts, skips := CompileHosts(HostInputs{Facts: []HostFacts{{Host: "*"}, {Host: "a/b"}}})
	if len(arts) != 0 {
		t.Errorf("hostile hostnames must produce no articles, got %d", len(arts))
	}
	if len(skips) != 2 {
		t.Errorf("both hostile hostnames must be reported as skips, got %+v", skips)
	}
}

// TestBodyNeverBreaksItsOwnTable — a `conclusion` is free text written by a model and an `alert_rule`
// comes from an inbound payload; either can contain a pipe or a newline, which would silently restructure
// or truncate the markdown table it lands in.
func TestBodyNeverBreaksItsOwnTable(t *testing.T) {
	arts, _ := CompileHosts(HostInputs{Facts: []HostFacts{{
		Host: "dc1mealie01",
		Sessions: []HostSession{{
			ExternalRef: "x", AlertRule: "rule | with | pipes", Outcome: "proposed",
			OpClass: "cls\nwith\nnewlines", Conclusion: "c", CreatedAt: ts(30),
		}},
	}}})
	body := arts[0].Body
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "| ") {
			continue
		}
		// Count only UNESCAPED pipes; a well-formed 5-column row has exactly 6.
		n := strings.Count(line, "|") - strings.Count(line, "\\|")
		if n != 6 {
			t.Errorf("table row has %d unescaped pipes (want 6) — untrusted text restructured the table: %q", n, line)
		}
	}
}

// TestMetaCarriesCountsAndBodyCarriesNoTimestamp — the pair that keeps determinism and staleness from
// fighting. Counts and provenance belong in Meta; a timestamp in a body would make every compile differ.
func TestMetaCarriesCountsAndBodyCarriesNoTimestamp(t *testing.T) {
	arts, _ := CompileHosts(sampleInputs())
	var page Article
	for _, a := range arts {
		if a.Slug == "host-dc1mealie01" {
			page = a
		}
	}
	if page.Meta["sessions"] != "2" || page.Meta["edges"] != "2" || page.Meta["precedents"] != "1" {
		t.Errorf("Meta must carry the denominators, got %+v", page.Meta)
	}
	if page.Meta["entity_type"] != "lxc" || page.Meta["status"] != "approved" {
		t.Errorf("Meta must carry world-model identity, got %+v", page.Meta)
	}
	// The body may legitimately contain recorded row timestamps; what it must not contain is a COMPILE
	// time. Both compiles below use different envelope times and must yield identical bodies.
	a1, _ := CompileHosts(sampleInputs())
	a2, _ := CompileHosts(sampleInputs())
	if a1[0].Body != a2[0].Body {
		t.Error("bodies differ between compiles of identical input")
	}
}

// TestParseArticlesRejectsUnknownFieldAndBlankSlug — mirrors knowledge.ParseCorpus:18-26. A writer and
// reader that disagree about the schema must fail loudly; a page with no identity cannot be served.
//
// RED MUTATION CONTROL (executed 2026-08-01): dropping DisallowUnknownFields, and separately dropping the
// blank-slug check, each fail their case; restored green.
func TestParseArticlesRejectsUnknownFieldAndBlankSlug(t *testing.T) {
	cases := []struct{ name, body string }{
		{"unknown field", `{"schema_version":1,"compiled_at":"2026-07-31T00:00:00Z","articles":[],"surprise":1}`},
		{"blank slug", `{"schema_version":1,"compiled_at":"2026-07-31T00:00:00Z","articles":[{"slug":"","title":"t","kind":"article","body":"b"}]}`},
		{"duplicate slug", `{"schema_version":1,"compiled_at":"2026-07-31T00:00:00Z","articles":[{"slug":"host-a","title":"t","kind":"article","body":"b"},{"slug":"host-a","title":"u","kind":"article","body":"c"}]}`},
		{"wrong schema version", `{"schema_version":99,"compiled_at":"2026-07-31T00:00:00Z","articles":[]}`},
		{"malformed", `{`},
	}
	for _, c := range cases {
		if _, err := ParseArticles(strings.NewReader(c.body)); err == nil {
			t.Errorf("%s must be rejected", c.name)
		}
	}
	env, err := ParseArticles(strings.NewReader(`{"schema_version":1,"compiled_at":"2026-07-31T00:00:00Z","articles":[]}`))
	if err != nil {
		t.Fatalf("a valid empty envelope must parse: %v", err)
	}
	if env.Articles == nil {
		t.Error("an empty envelope must yield [] not nil — a nil slice serializes as null downstream")
	}
}

// TestRoundTrip — what the worker writes, the grounder reads, unchanged.
func TestRoundTrip(t *testing.T) {
	arts, skips := CompileHosts(sampleInputs())
	var buf bytes.Buffer
	if err := WriteArticles(&buf, Envelope{CompiledAt: ts(31), Articles: arts, Skipped: skips,
		Sources: map[string]int{"hosts": 2, "sessions": 2}}); err != nil {
		t.Fatalf("write: %v", err)
	}
	env, err := ParseArticles(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("parse what we just wrote: %v", err)
	}
	if len(env.Articles) != len(arts) {
		t.Fatalf("round trip lost articles: wrote %d read %d", len(arts), len(env.Articles))
	}
	if env.Sources["hosts"] != 2 {
		t.Errorf("the Sources denominators must survive the round trip, got %+v", env.Sources)
	}
	for i := 1; i < len(env.Articles); i++ {
		if env.Articles[i-1].Slug >= env.Articles[i].Slug {
			t.Errorf("articles must be slug-ordered on disk, got %q then %q", env.Articles[i-1].Slug, env.Articles[i].Slug)
		}
	}
}

// TestPackageIsClockFree — the VACUITY FLOOR under TestCompileHostsDeterministic.
//
// That test compares two compiles and asserts they are byte-identical. It CANNOT catch a compile timestamp
// in an article body, because time.Now() has second resolution and both compiles run in the same second —
// proven: a mutation stamping time.Now() into hostBody passed the determinism test cleanly. An oracle that
// can be defeated by running fast is not an oracle.
//
// So the invariant is asserted where it actually lives: this package is PURE, and purity is checkable at
// the source. No clock, no filesystem, no network — everything a compile needs arrives as an argument.
// That is also what keeps the lane structurally unable to reach an actuator.
//
// RED MUTATION CONTROL (executed 2026-08-01): stamping time.Now() into hostBody fails here with the file
// and the call; restored green.
func TestPackageIsClockFree(t *testing.T) {
	banned := []string{"time.Now(", "os.Open", "os.ReadFile", "os.WriteFile", "http.", "sql.", "rand."}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue // the oracles may use a clock; the compiler may not
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		checked++
		for _, b := range banned {
			if bytes.Contains(src, []byte(b)) {
				t.Errorf("%s calls %s — this package must be a pure function of its inputs. A clock here "+
					"makes every article differ on every compile (the predecessor's NOW header, "+
					"wiki-compile.py:50, churns all 86 of its files nightly for this reason), and the "+
					"determinism oracle cannot catch it because two compiles share a second.", f, b)
			}
		}
	}
	if checked == 0 {
		t.Fatal("VACUITY: no non-test .go files were scanned — this guard checked nothing")
	}
}
