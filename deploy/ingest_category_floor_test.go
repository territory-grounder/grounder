package deploy

// EVERY INGEST SOURCE MUST DECIDE, IN WRITING, WHETHER IT DERIVES AN ALERT CATEGORY (TG-241, MECH-305).
//
// core/risk/classifier.go forces a POLL_PAUSE when the alert category is high-risk — maintenance,
// security-incident, or deployment — because "a containment (ban/shun/isolate) ENDS in an infra change by
// definition, so a human acks it even when each individual op reads as reversible". That driver reads
// env.Labels["category"].
//
// A source that sets no category makes that driver STRUCTURALLY UNREACHABLE for every incident it
// produces. The safety input does not read "unknown"; it reads NOT HIGH RISK. That is a safety signal
// failing in the reassuring direction, and it failed silently on three of four sources until MECH-305.
//
// WHAT THIS GUARD IS AND IS NOT. It does not demand every source derive a category — TG-241's own scope
// was narrowed, correctly, and the reasoning is worth preserving: the high-risk set is facts about the
// OPERATOR'S calendar and change process, not about a device being down. LibreNMS and pve-liveness
// genuinely do not know whether a maintenance window is open, and inventing a category there would be a
// guess wearing a safety control's clothes.
//
// What it demands is that the decision be RECORDED. A source with no category and no stated reason is
// indistinguishable from one where somebody forgot, and the difference is exactly what nobody could see
// before. This is the "vacuity floor that fails if a new ingest module is added without one" that TG-241
// asked for and that was never built.
//
// It is written against the SOURCE TREE rather than the registry on purpose: a module that is not yet
// registered still has to declare its position, so the decision is made when the code is written rather
// than when someone remembers to wire it.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// categoryNotDerived records the ingest modules that deliberately publish no alert category, WITH the
// reason. Adding an entry here is a deliberate act; leaving a module out of both this map and the
// category-setting set is what fails.
var categoryNotDerived = map[string]string{
	"librenms": "a device being down says nothing about whether a maintenance window is open or a " +
		"deployment is in flight; the high-risk categories are facts about the operator's calendar, and " +
		"deriving one from an availability alert would be a guess wearing a safety control's clothes",
	"pveliveness": "same as librenms — guest liveness is an availability fact, not a change-process one",
	"otlp": "an OTLP log record carries a severity, not a change-process fact; whether a maintenance window " +
		"is open is nowhere in the telemetry, and deriving a high-risk category from a log line would be a " +
		"guess wearing a safety control's clothes — the severity threshold is the only classification this " +
		"adapter honestly makes",
}

// TestEveryIngestModuleDeclaresItsCategoryPosition is the floor.
func TestEveryIngestModuleDeclaresItsCategoryPosition(t *testing.T) {
	root := filepath.Join("..", "modules", "ingest")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read %s: %v — this guard cannot assert anything about a tree it cannot walk, and must "+
			"fail rather than pass vacuously", root, err)
	}

	var modules []string
	for _, e := range entries {
		if e.IsDir() {
			modules = append(modules, e.Name())
		}
	}
	// VACUITY FLOOR ON THE FLOOR ITSELF. With an empty or truncated enumeration every assertion below is
	// trivially satisfied, and this file would report health over a tree it never read.
	if len(modules) < 4 {
		t.Fatalf("found only %d ingest module(s) under %s: %v — the deployment has more than that, so "+
			"this guard is asserting over almost nothing", len(modules), root, modules)
	}

	var derives, silent []string
	for _, m := range modules {
		if setsCategory(t, filepath.Join(root, m)) {
			derives = append(derives, m)
			continue
		}
		if reason, ok := categoryNotDerived[m]; ok {
			if strings.TrimSpace(reason) == "" {
				t.Errorf("ingest module %q is listed as not deriving a category with an EMPTY reason. "+
					"An unexplained exemption is the same as a forgotten one to everybody who reads it later.", m)
			}
			silent = append(silent, m)
			continue
		}
		t.Errorf("ingest module %q sets no alert category and is not recorded in categoryNotDerived.\n"+
			"Every incident it produces reaches core/risk with the high-risk-category driver STRUCTURALLY "+
			"UNREACHABLE — the safety input reads 'not high risk', not 'unknown'. Either derive a category "+
			"from a stated per-source rule, or add %q to categoryNotDerived with the reason it cannot.", m, m)
	}

	// Both sides must be non-empty, or the guard has drifted into asserting one thing about everything.
	if len(derives) == 0 {
		t.Error("NO ingest module derives a category. The poll-forcing high-risk driver is then unreachable " +
			"on every source in the deployment, which is the exact state TG-241 was opened on.")
	}
	if len(silent) == 0 {
		t.Log("note: every ingest module now derives a category; categoryNotDerived is empty in effect")
	}
	t.Logf("category coverage: %d derive %v · %d deliberately do not %v", len(derives), derives, len(silent), silent)
}

// A recorded exemption must name a module that EXISTS. A stale entry silently exempts nothing while
// making the list look considered — and would let a real module be added under a name that was already
// spelled wrong here.
func TestNoStaleCategoryExemptions(t *testing.T) {
	root := filepath.Join("..", "modules", "ingest")
	for m := range categoryNotDerived {
		if _, err := os.Stat(filepath.Join(root, m)); err != nil {
			t.Errorf("categoryNotDerived names %q, which is not a module directory. The exemption list "+
				"overstates what has been considered, and a future module of that name would be exempted "+
				"without anyone deciding.", m)
		}
	}
}

// setsCategory reports whether a module's non-test source publishes an alert category label.
func setsCategory(t *testing.T, dir string) bool {
	t.Helper()
	var found bool
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		for _, line := range strings.Split(string(b), "\n") {
			// Comment lines are PROSE, not code. A module whose comment merely discusses the category —
			// crowdsec.go explains at length why librenms deliberately does not set one — must not be
			// counted as setting it. This repo has already had a guard pass on its own explanation.
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if strings.Contains(line, `"category"`) {
				found = true
				return nil
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return found
}

// PROSE IS NOT CODE. crowdsec.go explains at length why librenms deliberately sets no category, and that
// explanation contains the quoted word this guard scans for. A module could equally carry a comment
// discussing the category while setting none — and would then be counted as covered.
//
// No module in the tree exercises that today, so the comment filter above is defensive rather than
// load-bearing, and a defensive clause nothing tests is a clause that gets deleted as dead. This drives it
// directly: a file whose ONLY occurrence is inside a comment must NOT count as setting a category.
func TestACommentDiscussingTheCategoryIsNotSettingOne(t *testing.T) {
	dir := t.TempDir()
	prose := "package x\n\n" +
		"// This module deliberately does NOT set \"category\": a device being down says nothing about\n" +
		"// whether a maintenance window is open.\n" +
		"const SourceType = \"x\"\n"
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(prose), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if setsCategory(t, dir) {
		t.Error("a module was counted as deriving a category because its COMMENT mentions one. The " +
			"high-risk driver stays structurally unreachable for that source while this guard reports it " +
			"covered — a guard passing on its own explanation.")
	}

	// The positive control, so this test cannot pass against a helper that always returns false.
	code := "package x\n\nfunc f() map[string]string { return map[string]string{\"category\": \"security-incident\"} }\n"
	if err := os.WriteFile(filepath.Join(dir, "y.go"), []byte(code), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if !setsCategory(t, dir) {
		t.Error("a module that really does set a category was NOT detected — the helper answers false to " +
			"everything and every other assertion in this file is vacuous")
	}
}
