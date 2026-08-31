package main

// A SAFETY SEAM'S YIELD MUST NOT BE OBSERVABLE ONLY WHEN AN OPTIONAL EXPORTER IS CONFIGURED.
//
// suppression.tier1 reported UNOBSERVED on dc1tg01 for the whole life of the register, and the reason
// was structural rather than incidental: its ONLY wiringYield.ObserveTotals call sat three levels inside
// the observability export loop —
//
//	if TG_OBSERVABILITY_EXPORT_INTERVAL != ""   // empty in production
//	  if len(exporters) > 0                     // needs an enabled exporter module
//	    for range t.C
//
// so nothing ever called it. UNOBSERVED was the register's honest answer: this seam could be admitting
// everything, or suppressing everything, and no report would know. The suppression gate is the Tier-1
// learning chain (TG-219) — a safety control — and "is it suppressing everything?" is precisely the
// question the offered/produced pair exists to answer.
//
// This is the same defect as the register's own (!994), one layer in: the OBSERVATION of a control placed
// behind an off-by-default flag. Fixing the register's exposition without fixing this would have left the
// gauge visible and permanently reading unobserved, which looks like coverage and is not.
//
// The observation now runs on the unconditional yield cadence next to suppGate. ObserveTotals SETS
// cumulative totals rather than adding, so the export loop can keep calling it too and the two cadences
// cannot double-count.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// enclosingConditions walks upward from a line, tracking brace depth, and returns the conditions the line
// is nested inside. Used to assert that an observation is NOT buried under a config gate.
func enclosingConditions(src string, lineNo int) []string {
	lines := strings.Split(src, "\n")
	if lineNo < 1 || lineNo > len(lines) {
		return nil
	}
	var out []string
	depth := 0
	for i := lineNo - 1; i >= 0; i-- {
		ln := lines[i]
		depth += strings.Count(ln, "}") - strings.Count(ln, "{")
		if depth < 0 {
			if t := strings.TrimSpace(ln); t != "" && !strings.HasPrefix(t, "//") {
				out = append(out, t)
			}
			depth = 0
		}
	}
	return out
}

func TestSuppressionYieldIsObservedOutsideTheExporterGate(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	src := string(raw)

	// Every call that feeds the suppression seam's yield pair.
	call := regexp.MustCompile(`(?m)^.*wiring\.SeamSuppression.*$`)
	var observeLines []int
	for i, ln := range strings.Split(src, "\n") {
		if !call.MatchString(ln) {
			continue
		}
		if !strings.Contains(ln, "ObserveTotals") && !strings.Contains(ln, "Observe(") {
			continue
		}
		observeLines = append(observeLines, i+1)
	}
	if len(observeLines) == 0 {
		t.Fatal("no wiringYield observation of wiring.SeamSuppression found in main.go. Either the seam " +
			"was renamed or the observation was deleted — this guard compared NOTHING and must not pass. " +
			"An unobserved safety seam reports UNOBSERVED forever, which reads like coverage and is not.")
	}

	// CHECK WHERE IT IS CALLED, NOT WHERE IT IS WRITTEN.
	//
	// The first version of this guard asked whether the ObserveTotals LINE was gated. That is the wrong
	// question and its own killing mutation showed it: hoisting the observation into a helper declared at
	// the top of main(), then calling that helper only from inside the exporter loop, leaves the literal
	// ungated and the seam just as unobservable. So resolve the entry point first — if the observation
	// lives inside a named func literal, the thing that must be ungated is a CALL to it.
	entryPoints := map[int]string{} // line -> the identifier that must be called, "" for an inline call
	helperDecl := regexp.MustCompile(`^\s*(\w+)\s*:=\s*func\(`)
	for _, ln := range observeLines {
		name := ""
		for _, c := range enclosingConditions(src, ln) {
			if m := helperDecl.FindStringSubmatch(c); m != nil {
				name = m[1]
				break
			}
		}
		entryPoints[ln] = name
	}

	ungated := func(line int) bool {
		for _, c := range enclosingConditions(src, line) {
			if strings.Contains(c, "TG_OBSERVABILITY_EXPORT_INTERVAL") || strings.Contains(c, "len(exporters)") {
				return false
			}
		}
		return true
	}

	var reachable int
	srcLines := strings.Split(src, "\n")
	for ln, helper := range entryPoints {
		if helper == "" {
			if ungated(ln) {
				reachable++
			}
			continue
		}
		// A helper: find its call sites and require at least one outside the exporter gate.
		callRe := regexp.MustCompile(`\b` + regexp.QuoteMeta(helper) + `\(\)`)
		for i, cand := range srcLines {
			if i+1 == ln || !callRe.MatchString(cand) {
				continue
			}
			if strings.Contains(strings.TrimSpace(cand), ":= func(") {
				continue // the declaration itself is not a call
			}
			if ungated(i + 1) {
				reachable++
			}
		}
	}
	if reachable == 0 {
		t.Errorf("every observation of suppression.tier1 (%d call site(s)) is nested inside the "+
			"observability exporter gate.\n"+
			"TG_OBSERVABILITY_EXPORT_INTERVAL is empty by default, so in a normal deployment nothing ever "+
			"calls it and the seam reports UNOBSERVED forever — the register's honest 'this could be "+
			"producing nothing and I would not know'. A safety seam's yield must be observed on an "+
			"unconditional cadence.", len(observeLines))
	}
}

// The helper's own control: it must actually detect nesting, or the test above passes by measuring
// nothing. Two synthetic sources, one gated and one not.
func TestEnclosingConditionsDetectsTheExporterGate(t *testing.T) {
	gated := `func main() {
	if iv := getenv("TG_OBSERVABILITY_EXPORT_INTERVAL", ""); iv != "" {
		if len(exporters) > 0 {
			observe()
		}
	}
}`
	conds := enclosingConditions(gated, 4)
	var sawGate bool
	for _, c := range conds {
		if strings.Contains(c, "TG_OBSERVABILITY_EXPORT_INTERVAL") {
			sawGate = true
		}
	}
	if !sawGate {
		t.Fatalf("the walker missed the exporter gate it exists to find; conditions were %v", conds)
	}

	// The negative control. The walker legitimately reports the enclosing FUNCTION as a scope, so the
	// property that matters is not "no conditions" but "no exporter gate" — assert exactly that, or this
	// control fails for a reason that has nothing to do with what the guard checks.
	plain := `func main() {
	observe()
}`
	for _, c := range enclosingConditions(plain, 2) {
		if strings.Contains(c, "TG_OBSERVABILITY_EXPORT_INTERVAL") || strings.Contains(c, "len(exporters)") {
			t.Fatalf("an ungated call was reported as sitting inside the exporter gate (%q) — the walker "+
				"would then mark every call site gated and the guard above could never pass", c)
		}
	}
}
