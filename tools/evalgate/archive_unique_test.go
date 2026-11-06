package main

import (
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"github.com/territory-grounder/grounder/eval/gate"
)

// TG-587: two runs of the same candidate on the same day are TWO records. This drives the SAME function main()
// calls (archiveKeyDir composes the key AND claims the directory), so re-inlining the plain keyed path at the
// call site has nowhere to hide: the first verdict is byte-identical after the second write, the second lands
// under the first free zero-padded ordinal, a third follows, and the names sort [original, -r02, -r03].
func TestArchiveNeverOverwritesAnEarlierRecordOfTheSameKey(t *testing.T) {
	root := t.TempDir()
	first, err := archiveKeyDir(root, "2026-10-22", "change", "b6fbcfb0cf37deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(first) != "2026-10-22-change-b6fbcfb0cf37" {
		t.Fatalf("the first record must take the plain keyed name (12-char sha), got %s", first)
	}
	if err := writeArchive(first, gate.Baseline{}, gate.Scorecard{}, gate.Verdict{Pass: false, Outcome: "fail"}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(first, "verdict.json"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := archiveKeyDir(root, "2026-10-22", "change", "b6fbcfb0cf37deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	if second != first+"-r02" {
		t.Fatalf("a second run of the same key must get the -r02 ordinal, got %s", second)
	}
	if err := writeArchive(second, gate.Baseline{}, gate.Scorecard{}, gate.Verdict{Pass: true, Outcome: "pass"}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(first, "verdict.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("the first record changed under the second write:\n%s\n---\n%s", before, after)
	}
	third, err := archiveKeyDir(root, "2026-10-22", "change", "b6fbcfb0cf37deadbeef")
	if err != nil || third != first+"-r03" {
		t.Fatalf("a third run must get -r03, got %s (%v)", third, err)
	}
	entries, _ := os.ReadDir(root)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := []string{"2026-10-22-change-b6fbcfb0cf37", "2026-10-22-change-b6fbcfb0cf37-r02", "2026-10-22-change-b6fbcfb0cf37-r03"}
	if !sort.StringsAreSorted(names) || len(names) != 3 || names[0] != want[0] || names[1] != want[1] || names[2] != want[2] {
		t.Fatalf("expected %v in name order, got %v", want, names)
	}
	// Zero-padding keeps lexical order through -r99: the 10th run must sort AFTER the 9th (release-gate `sort | tail -1`).
	for n := 4; n <= 10; n++ {
		if _, err := archiveKeyDir(root, "2026-10-22", "change", "b6fbcfb0cf37deadbeef"); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ = os.ReadDir(root)
	last := entries[len(entries)-1].Name()
	if last != "2026-10-22-change-b6fbcfb0cf37-r10" {
		t.Fatalf("the newest record must sort last, got %s", last)
	}
}

// The claim is exclusive: N racing runs on one key get N DISTINCT directories (os.Mkdir is the atomic
// test-and-claim; a Stat-then-create would let two of them share one).
func TestArchiveClaimIsExclusiveUnderConcurrency(t *testing.T) {
	root := t.TempDir()
	const n = 24
	var wg sync.WaitGroup
	got := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], errs[i] = archiveKeyDir(root, "2026-10-22", "change", "cafe0000cafe")
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for i := range got {
		if errs[i] != nil {
			t.Fatalf("run %d: %v", i, errs[i])
		}
		if seen[got[i]] {
			t.Fatalf("two racing runs claimed the same record directory %s", got[i])
		}
		seen[got[i]] = true
	}
	if len(seen) != n {
		t.Fatalf("want %d distinct records, got %d", n, len(seen))
	}
}

// A claimed directory whose record cannot be written is released again, so a half-written record never becomes
// the "latest" one the release gate reads.
func TestFailedArchiveWriteReleasesTheClaimedDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions; the write cannot be made to fail this way")
	}
	root := t.TempDir()
	dir, err := archiveKeyDir(root, "2026-10-22", "change", "cafe0000cafe")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil { // claimed, but unwritable
		t.Fatal(err)
	}
	werr := writeArchiveOrDiscard(dir, gate.Baseline{}, gate.Scorecard{}, gate.Verdict{Outcome: "pass", Pass: true})
	if werr == nil {
		t.Fatal("writing into an unwritable claim must fail")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("a failed write must release the claimed directory, stat err=%v", err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("no record may remain after a failed write, found %d", len(entries))
	}
}
