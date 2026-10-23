package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/pmezard/go-difflib/difflib"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/harnessid"
)

// componentNames is the fixed order every report uses — the order the parts are assembled in.
var componentNames = []string{"prose", "tool_catalog", "opclass_catalog", "guidance"}

func component(p harnessid.Parts, name string) (string, bool) {
	switch name {
	case "prose":
		return p.Prose, true
	case "tool_catalog":
		return p.ToolCatalog, true
	case "opclass_catalog":
		return p.OpClassCatalog, true
	case "guidance":
		return p.Guidance, true
	}
	return "", false
}

func hashOf(h harnessid.Hashes, name string) string {
	switch name {
	case "prose":
		return h.Prose
	case "tool_catalog":
		return h.ToolCatalog
	case "opclass_catalog":
		return h.OpClassCatalog
	}
	return h.Guidance
}

func builds(b []string) string {
	if len(b) == 0 {
		return "(no build recorded)"
	}
	out := make([]string, 0, len(b))
	for _, s := range b {
		if s == "" {
			s = "unknown"
		} else if len(s) > 12 {
			s = s[:12]
		}
		out = append(out, s)
	}
	return strings.Join(out, ",")
}

// renderList is the `-list` table: one line per configuration, oldest first.
func renderList(rows []db.HarnessRenderSummary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-14s %-12s %-20s %8s %9s  %s\n", "FINGERPRINT", "PREAMBLE", "FIRST_SEEN(UTC)", "BYTES", "SESSIONS", "BUILDS")
	for _, r := range rows {
		fmt.Fprintf(&b, "%-14s %-12s %-20s %8d %9d  %s\n", harnessid.Short(r.HarnessFP), r.PromptVersion, r.FirstSeen.UTC().Format(time.RFC3339), r.Bytes, r.Sessions, builds(r.Builds))
	}
	if len(rows) == 0 {
		b.WriteString("(no configuration recorded yet — the first session under the TG-600 build writes one)\n")
	}
	fmt.Fprintf(&b, "%d configuration(s)\n", len(rows))
	return b.String()
}

func header(label string, r db.HarnessRenderRow) string {
	return fmt.Sprintf("%s %s  %s  first_seen %s  sessions %d  builds %s  bytes %d\n", label, r.HarnessFP, r.PromptVersion,
		r.FirstSeen.UTC().Format(time.RFC3339), r.Sessions, builds(r.Builds), r.Parts.Bytes())
}

// renderShow prints one configuration: its header, then either one component or the assembled system
// half exactly as the model received it.
func renderShow(r db.HarnessRenderRow, only string) (string, error) {
	var b strings.Builder
	b.WriteString(header("configuration", r))
	for _, n := range componentNames {
		b.WriteString(fmt.Sprintf("  %-16s %s\n", n+":", hashOf(r.Hashes, n)[:12]))
	}
	b.WriteString("\n")
	if only == "" {
		b.WriteString(harnessid.Assemble(r.Parts))
		return b.String(), nil
	}
	text, ok := component(r.Parts, only)
	if !ok {
		return "", fmt.Errorf("unknown component %q (one of %s)", only, strings.Join(componentNames, ", "))
	}
	b.WriteString(text)
	return b.String(), nil
}

// renderDiff compares two configurations per component: a line per component saying identical or
// DIFFERS, then a unified diff (3 lines of context) for each component that differs. only narrows to one
// component. The second result is the number of components that differ.
func renderDiff(a, b db.HarnessRenderRow, only string) (string, int, error) {
	if only != "" {
		if _, ok := component(a.Parts, only); !ok {
			return "", 0, fmt.Errorf("unknown component %q (one of %s)", only, strings.Join(componentNames, ", "))
		}
	}
	var out strings.Builder
	out.WriteString(header("A", a))
	out.WriteString(header("B", b))
	if a.PromptVersion != b.PromptVersion {
		fmt.Fprintf(&out, "preamble version: %s -> %s\n", a.PromptVersion, b.PromptVersion)
	}
	differs := 0
	var diffs strings.Builder
	for _, n := range componentNames {
		if only != "" && n != only {
			continue
		}
		at, _ := component(a.Parts, n)
		bt, _ := component(b.Parts, n)
		if at == bt {
			fmt.Fprintf(&out, "  %-16s identical\n", n+":")
			continue
		}
		differs++
		ud := difflib.UnifiedDiff{A: difflib.SplitLines(at), B: difflib.SplitLines(bt), FromFile: "A/" + n, ToFile: "B/" + n, Context: 3}
		text, err := difflib.GetUnifiedDiffString(ud)
		if err != nil {
			return "", 0, fmt.Errorf("diff %s: %w", n, err)
		}
		plus, minus := 0, 0
		for _, l := range strings.Split(text, "\n") {
			switch {
			case strings.HasPrefix(l, "+++") || strings.HasPrefix(l, "---"):
			case strings.HasPrefix(l, "+"):
				plus++
			case strings.HasPrefix(l, "-"):
				minus++
			}
		}
		fmt.Fprintf(&out, "  %-16s DIFFERS (+%d -%d lines)\n", n+":", plus, minus)
		diffs.WriteString("\n")
		diffs.WriteString(text)
	}
	if differs == 0 {
		out.WriteString("IDENTICAL: the compared components are byte-equal")
		if only == "" && a.HarnessFP != b.HarnessFP {
			out.WriteString(" (the fingerprints differ only in the preamble version)")
		}
		out.WriteString("\n")
	}
	out.WriteString(diffs.String())
	return out.String(), differs, nil
}
