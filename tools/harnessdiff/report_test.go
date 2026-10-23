package main

import (
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/harnessid"
)

func row(guidance, tools, build string, sessions int) db.HarnessRenderRow {
	p := harnessid.Parts{Prose: "PROTOCOL\n{{TOOL_CATALOG}}\n{{OPCLASS_CATALOG}}\n", ToolCatalog: tools, OpClassCatalog: "- restart-service: params.unit\n", Guidance: guidance}
	r := db.HarnessRenderRow{Hashes: p.Hashes(), FirstSeen: time.Date(2026, 10, 22, 22, 46, 0, 0, time.UTC), Builds: []string{build}, Sessions: sessions}
	r.Record = harnessid.Record{HarnessFP: harnessid.Fingerprint(p, "preamble/5"), PromptVersion: "preamble/5", BuildSHA: build, Parts: p}
	return r
}

// A diff names ONLY the components that moved, with a hunk for each, and says IDENTICAL when nothing did.
func TestRenderDiffNamesOnlyTheComponentsThatMoved(t *testing.T) {
	a := row("## Guidance A\n- prefer a service restart\n", "- read_file: reads a file\n", "9dcc409a0000", 12)
	b := row("## Guidance A\n- prefer a guest reboot\n", "- read_file: reads a file\n", "1f9ab2cd0000", 3)
	text, n, err := renderDiff(a, b, "")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("exactly one component moved, got %d:\n%s", n, text)
	}
	for _, want := range []string{"prose:           identical", "tool_catalog:    identical", "opclass_catalog: identical", "guidance:        DIFFERS (+1 -1 lines)", "-- prefer a service restart", "+- prefer a guest reboot", "sessions 12", "sessions 3", "builds 9dcc409a0000", "A " + a.HarnessFP, "B " + b.HarnessFP} {
		if !strings.Contains(text, want) {
			t.Fatalf("diff must contain %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "A/prose") || strings.Contains(text, "A/tool_catalog") {
		t.Fatalf("no hunk for an identical component:\n%s", text)
	}
	same, n, err := renderDiff(a, a, "")
	if err != nil || n != 0 || !strings.Contains(same, "IDENTICAL") {
		t.Fatalf("identical rows: n=%d err=%v\n%s", n, err, same)
	}
	narrowed, n, err := renderDiff(a, b, "tool_catalog")
	if err != nil || n != 0 || strings.Contains(narrowed, "guidance:") {
		t.Fatalf("-component narrows the comparison: n=%d err=%v\n%s", n, err, narrowed)
	}
	if _, _, err := renderDiff(a, b, "nope"); err == nil {
		t.Fatal("an unknown component must be refused")
	}
}

// -show prints the assembled system half byte for byte (or one component), under a header that names the
// configuration, its lineage and its component hashes.
func TestRenderShowPrintsTheExactSystemHalf(t *testing.T) {
	r := row("## Guidance A\n", "- read_file: reads a file\n", "", 0)
	text, err := renderShow(r, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(text, harnessid.Assemble(r.Parts)) {
		t.Fatalf("the body must be the assembled system half verbatim:\n%s", text)
	}
	if !strings.Contains(text, "builds unknown") || !strings.Contains(text, "guidance:        "+r.Hashes.Guidance[:12]) {
		t.Fatalf("header must name an unrecorded build as unknown and carry the component hashes:\n%s", text)
	}
	one, err := renderShow(r, "guidance")
	if err != nil || !strings.HasSuffix(one, "## Guidance A\n") || strings.Contains(one, "PROTOCOL") {
		t.Fatalf("-component prints that component only: %v\n%s", err, one)
	}
	if _, err := renderShow(r, "bogus"); err == nil {
		t.Fatal("an unknown component must be refused")
	}
}

func TestRenderListIsOneLinePerConfigurationOldestFirst(t *testing.T) {
	rows := []db.HarnessRenderSummary{
		{HarnessFP: strings.Repeat("a", 64), PromptVersion: "preamble/5", FirstSeen: time.Date(2026, 10, 22, 22, 46, 0, 0, time.UTC), Bytes: 20480, Builds: []string{"9dcc409a0000"}, Sessions: 12},
		{HarnessFP: strings.Repeat("b", 64), PromptVersion: "preamble/5", FirstSeen: time.Date(2026, 10, 24, 1, 0, 0, 0, time.UTC), Bytes: 20512, Builds: []string{"", "1f9ab2cd0000"}, Sessions: 0},
	}
	text := renderList(rows)
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[1], "aaaaaaaaaaaa") || !strings.HasPrefix(lines[2], "bbbbbbbbbbbb") || lines[3] != "2 configuration(s)" {
		t.Fatalf("unexpected list:\n%s", text)
	}
	if !strings.Contains(lines[2], "unknown,1f9ab2cd0000") || !strings.Contains(lines[1], "       12") {
		t.Fatalf("builds and session counts must render:\n%s", text)
	}
	if empty := renderList(nil); !strings.Contains(empty, "no configuration recorded yet") || !strings.Contains(empty, "0 configuration(s)") {
		t.Fatalf("an empty list must say so, with its zero denominator:\n%s", empty)
	}
}

func TestReadOnlyDSNGuardsBothForms(t *testing.T) {
	u, applied, err := readOnlyDSN("postgres://u:p@h:5432/d?sslmode=disable")
	if err != nil || !applied || !strings.Contains(u, "default_transaction_read_only") {
		t.Fatalf("url form: %q %v %v", u, applied, err)
	}
	if _, applied, _ := readOnlyDSN("postgres://u:p@h/d?options=-c%20x=y"); applied {
		t.Fatal("an operator-set options must be left alone")
	}
	kv, applied, _ := readOnlyDSN("host=h dbname=d")
	if !applied || !strings.Contains(kv, "options='-c default_transaction_read_only=on'") {
		t.Fatalf("kv form: %q", kv)
	}
}
