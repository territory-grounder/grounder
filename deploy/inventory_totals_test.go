package deploy

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// ★ THE INVENTORY'S HEADER MUST AGREE WITH ITS OWN TABLE (TG-243).
//
// docs/PREDECESSOR-MECHANISM-INVENTORY.md states exact totals ("19 PORTED / 22 PARTIAL / …") in prose, and
// the table beneath it is edited row by row as mechanisms land. Nothing reconciled the two, so the header
// drifted twice: TG-243 was filed for it on 2026-08-01, the rows were corrected, the header was not, and by
// 2026-08-06 it disagreed again — PORTED 19 vs 18, PARTIAL 22 vs 19, denominator 84 vs 81.
//
// A manual re-count fixes it once. This makes it stay fixed, which is the only version that survives the
// next correction.
//
// THREE THINGS MAKE A NAIVE COUNT WRONG, and each of them cost me a discarded number before this test
// existed:
//
//  1. Not every `| MECH-` row has a status. Eight live in the predecessor-DEFECTS table, which has no TG
//     status column at all. Counting "rows starting with | MECH-" is wrong by 8 before it starts.
//  2. Status cells carry parentheticals INSIDE the bold — `**ABSENT (corrected 2026-08-01, TG-238)**` — so
//     a strict `**STATUS**` match silently drops them, and a loose keyword search picks up statuses
//     mentioned in the prose of an unrelated cell.
//  3. `MECH-118` appears in BOTH tables, so "count the ids" and "count the rows" disagree.
//
// The parse below is therefore: first BOLDED span on the row that BEGINS with a status keyword. A row with
// no such span is treated as status-less and excluded, which is exactly right for the defects table.
const inventoryPath = "../docs/PREDECESSOR-MECHANISM-INVENTORY.md"

var (
	statusKeyword = regexp.MustCompile(`^(DIVERGENT-BY-DESIGN|DO-NOT-PORT|NEW IN TG|PORTED|PARTIAL|ABSENT|DIVERGENT|GAP)\b`)
	boldSpan      = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	// The header sentence this test holds honest.
	headerTotals = regexp.MustCompile(`(\d+) PORTED / (\d+) PARTIAL / (\d+) ABSENT / (\d+) DIVERGENT of (\d+) portable`)
)

// portableStatuses are the statuses that count toward the "of N portable" denominator. DO-NOT-PORT,
// NEW IN TG and GAP are deliberately excluded: they are not mechanisms TG failed to port.
var portableStatuses = map[string]bool{
	"PORTED": true, "PARTIAL": true, "ABSENT": true, "DIVERGENT": true, "DIVERGENT-BY-DESIGN": true,
}

func countInventory(t *testing.T) (map[string]int, int) {
	t.Helper()
	b, err := os.ReadFile(inventoryPath)
	if err != nil {
		t.Fatalf("read %s: %v", inventoryPath, err)
	}
	counts, portable, rows := map[string]int{}, 0, 0
	for _, line := range strings.Split(string(b), "\n") {
		s := strings.TrimSpace(line)
		if !strings.HasPrefix(s, "| MECH-") {
			continue
		}
		rows++
		// THE STATUS IS THE START OF THE FINAL CELL, and nothing else counts. An earlier version of this
		// took the first bolded span anywhere on the row, which a bolded status word in an unrelated cell's
		// PROSE satisfies — I proved that against this very file by adding "**ABSENT**" to a defects-table
		// row and watching the count move. Anchoring to the last cell is what makes the strictness real
		// rather than described.
		cells := strings.Split(strings.Trim(maskCodeSpans(s), "|"), "|")
		last := strings.TrimSpace(cells[len(cells)-1])
		m := boldSpan.FindStringSubmatch(last)
		if m == nil || !strings.HasPrefix(strings.TrimSpace(last), "**") {
			continue
		}
		if k := statusKeyword.FindString(strings.TrimSpace(m[1])); k != "" {
			counts[k]++
			if portableStatuses[k] {
				portable++
			}
		}
	}
	if rows == 0 {
		t.Fatal("no MECH rows parsed — the assertions below would be vacuous, which is the failure mode " +
			"this whole file exists to prevent")
	}
	return counts, portable
}

func TestInventoryHeaderTotalsMatchTheTable(t *testing.T) {
	b, err := os.ReadFile(inventoryPath)
	if err != nil {
		t.Fatalf("read %s: %v", inventoryPath, err)
	}
	m := headerTotals.FindStringSubmatch(string(b))
	if m == nil {
		t.Fatalf("no %q sentence found in %s — if the header's wording changed, this gate stops checking "+
			"anything and must be re-pointed rather than deleted", headerTotals.String(), inventoryPath)
	}

	counts, portable := countInventory(t)
	got := map[string]int{
		"PORTED":    counts["PORTED"],
		"PARTIAL":   counts["PARTIAL"],
		"ABSENT":    counts["ABSENT"],
		"DIVERGENT": counts["DIVERGENT"] + counts["DIVERGENT-BY-DESIGN"],
	}
	want := map[string]int{"PORTED": atoi(t, m[1]), "PARTIAL": atoi(t, m[2]), "ABSENT": atoi(t, m[3]), "DIVERGENT": atoi(t, m[4])}

	for _, k := range []string{"PORTED", "PARTIAL", "ABSENT", "DIVERGENT"} {
		if got[k] != want[k] {
			t.Errorf("%s: header says %d, the table has %d. Update the header sentence — a document that "+
				"states a total its own rows do not support is how TG-243 happened twice.", k, want[k], got[k])
		}
	}
	if p := atoi(t, m[5]); p != portable {
		t.Errorf("portable denominator: header says %d, the table has %d (PORTED+PARTIAL+ABSENT+DIVERGENT, "+
			"excluding DO-NOT-PORT/NEW IN TG/GAP)", p, portable)
	}
}

// The parse must not silently classify nothing. If a future edit changes the status formatting, this fails
// LOUDLY rather than reporting a tidy 0/0/0/0 that happens to match a zeroed header.
func TestTheInventoryParseIsNotVacuous(t *testing.T) {
	counts, portable := countInventory(t)
	if portable < 50 {
		t.Fatalf("only %d portable rows classified (counts %v). The inventory has ~80; a collapse this large "+
			"means the status formatting changed and this gate is now measuring nothing.", portable, counts)
	}
	// The defects table has no status column, so status-less rows are EXPECTED — but if every row were
	// status-less the number above would already have failed. This pins the other direction: the parse must
	// not be so loose that it classifies rows in the defects table too.
	total := 0
	for _, v := range counts {
		total += v
	}
	if total >= 95 {
		t.Errorf("classified %d rows, but 8 of the 95 MECH rows are in the predecessor-defects table and "+
			"carry NO status column. Classifying them means the keyword search is matching prose.", total)
	}
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return n
}

// maskCodeSpans replaces the contents of `inline code` with a placeholder before the row is split on "|".
//
// Without it, a status cell whose prose quotes a regex — MECH-114 cites `search_query|search_document|
// asymmetric` — is torn into fragments by the split, its status lands in the middle, and the "final cell"
// is a sentence tail. That row then goes uncounted and the totals are quietly one short, which is the
// precise failure mode this gate exists to catch, reproduced inside the gate itself.
func maskCodeSpans(row string) string {
	var b strings.Builder
	inCode := false
	for _, r := range row {
		switch {
		case r == '`':
			inCode = !inCode
			b.WriteRune(r)
		case inCode && r == '|':
			b.WriteRune('\u2506') // a pipe that is DATA, not a column separator
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
