package db

// TWO MIGRATIONS CANNOT SHARE A NUMBER (TG-321 / TG-346).
//
// On 2026-08-06 two OPEN merge requests each added a migration numbered 0061:
//
//	fix/tg346-snapshot-plane-discriminator   0061_estate_snapshot_plane
//	fix/tg321-credit-requires-an-execution   0061_graduation_credit_requires_execution
//
// Neither author saw the other, because both had checked `main` — where 0060 was the highest — and not the
// thirteen unmerged branches. The runner keys schema_migrations on the FULL FILENAME and sorts lexically, so
// both would have applied in a defined order; the damage is not a lost migration but a directory whose
// version sequence no reader can order, and a rename hazard: changing a file's name makes it a NEW version
// that re-applies on a database where the old name already ran.
//
// This guard cannot see unmerged branches either. What it CAN do is refuse the collision the moment they
// meet on main, with a message that says which two files and what to do — instead of a schema whose "current
// version" is a question with two answers.
//
// AND IT FOUND ONE ALREADY SHIPPED, on the first run: 0058_exec_class_decision (TG-169) and
// 0058_triage_decision_latency (TG-205). Both are applied in production — verified against
// schema_migrations on dc1tg01, which holds both version rows — so the same class of mistake reached
// main before, twice merged, unnoticed. That is the evidence this guard is worth its size.

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// alreadyShipped grandfathers the ONE collision that is already applied in production.
//
// NOT RENUMBERED, DELIBERATELY, and this is the important half. schema_migrations keys on the filename, so
// renaming an applied migration makes it a NEW version that re-applies on every existing database. Neither
// 0058 is idempotent — one CREATEs a table, the other ALTERs one — so a rename would break the migration
// chain on the deployed box to tidy a directory listing. The collision is inert where it is; the guard's
// job is to stop the NEXT one, not to force a dangerous edit to settle the last.
var alreadyShipped = map[string]string{
	"0058": "0058_exec_class_decision (TG-169) and 0058_triage_decision_latency (TG-205) are BOTH applied " +
		"in production (both rows present in schema_migrations on dc1tg01). Renaming an applied " +
		"migration re-applies it, and neither is idempotent — leave them.",
}

// KILLING MUTATION: copy any migration to a file sharing another's number prefix. RED, naming both.
func TestNoTwoMigrationsShareANumber(t *testing.T) {
	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	byNumber := map[string][]string{}
	ups := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		ups++
		num, _, ok := strings.Cut(name, "_")
		if !ok || num == "" {
			t.Errorf("migration %q has no NNNN_ prefix — the ordering the runner relies on is lexical, so an "+
				"unnumbered file sorts unpredictably against the rest", name)
			continue
		}
		byNumber[num] = append(byNumber[num], name)
	}

	// VACUITY FLOOR: an empty or unreadable directory would make every assertion above pass silently, which
	// is the same shape of defect this file exists to catch.
	if ups < 20 {
		t.Fatalf("found only %d .up.sql migration(s) — the directory read is broken and this guard is "+
			"examining almost nothing", ups)
	}

	var nums []string
	for n := range byNumber {
		nums = append(nums, n)
	}
	sort.Strings(nums)
	for _, n := range nums {
		if len(byNumber[n]) > 1 {
			sort.Strings(byNumber[n])
			if why, grandfathered := alreadyShipped[n]; grandfathered {
				t.Logf("migration number %s collides and is GRANDFATHERED: %s", n, why)
				continue
			}
			t.Errorf("migration number %s is claimed by %d files: %v.\n"+
				"schema_migrations keys on the full FILENAME, so both apply — but the schema's version "+
				"becomes a question with two answers, and renumbering either one later turns it into a NEW "+
				"version that re-applies on every database where the old name already ran. Renumber the "+
				"newer one to the next free slot and make it idempotent (DROP … IF EXISTS before CREATE).",
				n, len(byNumber[n]), byNumber[n])
		}
	}
}

// Every .up.sql must have its .down.sql. A missing down is not caught by the runner (it only embeds *.up.sql)
// and is discovered when someone needs to revert.
func TestEveryMigrationHasADownFile(t *testing.T) {
	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	have := map[string]bool{}
	for _, e := range entries {
		have[e.Name()] = true
	}
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		checked++
		down := strings.TrimSuffix(name, ".up.sql") + ".down.sql"
		if !have[down] {
			t.Errorf("%s has no %s. The runner embeds only *.up.sql, so a missing down file is invisible "+
				"until someone needs to revert and finds there is nothing to run",
				filepath.Base(name), filepath.Base(down))
		}
	}
	if checked < 20 {
		t.Fatalf("checked only %d migration(s) — the directory read is broken", checked)
	}
}

// A grandfathered entry must name a number that ACTUALLY collides. A stale exemption silently permits a
// future collision on that number while making the list look considered.
func TestNoStaleGrandfatheredCollision(t *testing.T) {
	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	count := map[string]int{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		if num, _, ok := strings.Cut(e.Name(), "_"); ok {
			count[num]++
		}
	}
	for n, why := range alreadyShipped {
		if count[n] < 2 {
			t.Errorf("alreadyShipped grandfathers %s, which no longer collides (%d file(s)). A stale "+
				"exemption pre-permits the next collision on that number.\nReason on record: %s",
				n, count[n], why)
		}
		if strings.TrimSpace(why) == "" {
			t.Errorf("the %s exemption has an empty reason — an unexplained exemption reads the same as a "+
				"forgotten one", n)
		}
	}
}
