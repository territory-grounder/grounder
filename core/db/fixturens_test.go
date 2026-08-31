package db

// A PER-PROCESS FIXTURE NAMESPACE, BECAUSE THE GOLDEN FIXTURES COLLIDE (TG-311/312/318/327).
//
// core/db's golden fixtures seed LITERAL ids — `gold-axis-1`, `gold-axis-esc-organic`, `goldhist-host-a` —
// into the ONE shared Postgres named by TG_TEST_DSN. This box runs several agent worktrees at once, so two
// `go test` processes routinely seed the same rows at the same time. Two things then go wrong, and the
// second is the worse one:
//
//  1. INSERT collides:  duplicate key value violates unique constraint "session_triage_pkey" (SQLSTATE 23505)
//  2. each fixture's cleanup runs `DELETE ... WHERE external_ref LIKE 'gold-axis-%'`, which deletes the
//     OTHER run's rows mid-test — so a test can also fail an assertion (`MutatedCount = 1, want >= 2`)
//     having never seen an error at all.
//
// The failing test NAMES change from run to run, which is the signature of a race rather than a defect in
// any one test. Reproduced deterministically on 2026-08-05 by running two `go test ./core/db/` processes
// concurrently against the shared fixture: both exited 1, on different tests, with 23505 on the fixed ids.
//
// WHY THIS MATTERS MORE THAN A FLAKE USUALLY DOES. `make all` is the mandated local gate and these tests
// also run in CI, so a green or a red here is evidence about a change. When the result depends on whether
// another agent happened to be testing at that moment, EVERY verdict this package issues is weakened —
// including the verdicts that gate actuation safety. And on this project every failed pipeline emails the
// owner, so the cost of a spurious red is paid by a human every time.
//
// THE FIX: give each test PROCESS its own namespace and put it at the FRONT of every fixture id, so that
//   - inserts cannot collide (the ids differ between runs), and
//   - cleanup can be scoped with a single `LIKE <ns>-%`, which by construction cannot reach another run.
//
// Prefix rather than suffix is load-bearing: a suffix would leave the existing `LIKE 'gold-axis-%'`
// cleanups matching every run's rows, which fixes the insert collision while leaving the destructive
// delete — the subtler half — fully intact.

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
)

// fixtureNS is unique per test BINARY (one value per process, computed once). Randomness rather than the
// pid alone: pids are recycled, and two worktrees can be handed the same pid minutes apart while rows from
// the earlier run are still present after a crash that skipped cleanup.
var fixtureNS = func() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Deliberately NOT a silent fallback to a constant: a constant namespace re-creates the exact
		// collision this file exists to remove, and it would do so invisibly.
		panic(fmt.Sprintf("fixture namespace: crypto/rand unavailable (%v) — refusing to run with a "+
			"shared namespace, which is the defect TG-318 describes", err))
	}
	return fmt.Sprintf("ns%s%d", hex.EncodeToString(b[:]), os.Getpid())
}()

// gx namespaces a fixture id. `gx("gold-axis-1")` -> `nsdeadbeef1234-gold-axis-1`.
//
// Use it for EVERY seeded identifier a test inserts and every value it later asserts on. A half-namespaced
// fixture is worse than none: the namespaced rows survive a concurrent run's cleanup while the bare ones do
// not, so the test fails on a partially-deleted fixture, which reads as a logic bug.
func gx(id string) string { return fixtureNS + "-" + id }

// gxLike is the ONE cleanup pattern a test needs: everything this process seeded, and nothing any other
// process did. Pass it as a query ARGUMENT (`... LIKE $1`), never concatenated into the SQL text — the
// repo forbids string-built SQL, and that rule does not have a test-code exemption.
func gxLike() string { return fixtureNS + "-%" }
