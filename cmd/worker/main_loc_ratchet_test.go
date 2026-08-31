package main

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

// mainLOCRatchet is the ceiling on main()'s line count. TG-501 (the god-file split) only moves if the
// number can't silently grow back: the 2026-08-19 extraction burst took main() from 7,162 to 6,498 lines
// and three feature merges then regrew it to 6,078 by 2026-08-22 — every new wiring block landing back
// in main() instead of a builder. This pin ratchets DOWN: an extraction MR lowers it to the new count;
// a feature that grows main() past it fails here and must land its wiring in a phase builder or a
// wire*() file (the house pattern — 16 wire* functions already exist).
const mainLOCRatchet = 6096

// mainFuncLines counts the lines of func main() in main.go — from its signature to the first `}` at
// column zero after it.
func mainFuncLines(t *testing.T) int {
	t.Helper()
	f, err := os.Open("main.go")
	if err != nil {
		t.Fatalf("open main.go: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	start, n := 0, 0
	for sc.Scan() {
		n++
		line := sc.Text()
		if start == 0 {
			if strings.HasPrefix(line, "func main() {") {
				start = n
			}
			continue
		}
		if line == "}" {
			return n - start + 1
		}
	}
	t.Fatal("could not find func main() { … } in main.go — the ratchet would be vacuous")
	return 0
}

func TestMainLOCRatchetOnlyGoesDown(t *testing.T) {
	got := mainFuncLines(t)
	if got < 1000 {
		t.Fatalf("main() measured at %d lines — implausible for the composition root; the scanner broke (vacuity floor)", got)
	}
	if got > mainLOCRatchet {
		t.Fatalf("main() is %d lines, above the TG-501 ratchet of %d — new wiring landed back in the god-file. "+
			"Move it into a phase builder or a wire*() file (cmd/worker/*_wiring.go); the ratchet only lowers", got, mainLOCRatchet)
	}
	t.Logf("main() = %d lines (ratchet %d; lower the pin when an extraction lands)", got, mainLOCRatchet)
}
