package main

// Drills for the TG-536 restamp-authorization wiring. Each arm is a regression guard: revert the wiring
// (or weaken a refusal) and the matching test goes red. The chain arm doubles as the tamper oracle: a
// hand-edited entry breaks the prev_hash walk.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
)

func readLedger(t *testing.T, root string) []audit.LedgerEntry {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, restampLedgerRel))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []audit.LedgerEntry
	for _, ln := range strings.Split(string(b), "\n") {
		if ln == "" {
			continue
		}
		var e audit.LedgerEntry
		if err := json.Unmarshal([]byte(ln), &e); err != nil {
			t.Fatalf("unparseable ledger line: %v", err)
		}
		out = append(out, e)
	}
	return out
}

func TestRestampWithoutActorIsRefusedAndAppendsNothing(t *testing.T) {
	root := t.TempDir()
	t.Setenv(restampActorEnv, "")
	_, err := authorizeRestamps(root, map[string][]string{"007-x": {"a.go"}}, map[string]bool{"007-x": true}, false)
	if err == nil || !strings.Contains(err.Error(), restampActorEnv) {
		t.Fatalf("an actorless restamp must be refused naming %s, got %v", restampActorEnv, err)
	}
	if got := readLedger(t, root); got != nil {
		t.Fatalf("a refused restamp must append NOTHING, got %d entr(ies)", len(got))
	}
}

func TestUnknownActorIsRefused(t *testing.T) {
	root := t.TempDir()
	t.Setenv(restampActorEnv, "somebody-else")
	_, err := authorizeRestamps(root, map[string][]string{"007-x": {"a.go"}}, map[string]bool{"007-x": true}, false)
	if err == nil {
		t.Fatal("an unpermitted actor must be refused")
	}
	if got := readLedger(t, root); got != nil {
		t.Fatalf("a refused restamp must append NOTHING, got %d entr(ies)", len(got))
	}
}

func TestSpecNotUpdatedIsRefusedBeforeAnyAppend(t *testing.T) {
	// Two specs, one satisfying the same-diff rule and one not: the denial must leave ZERO entries —
	// including none for the spec that WOULD have passed (no authorized-but-never-exercised records).
	root := t.TempDir()
	t.Setenv(restampActorEnv, "autonomous-session")
	moved := map[string][]string{"007-ok": {"a.go"}, "012-stale": {"b.go"}}
	_, err := authorizeRestamps(root, moved, map[string]bool{"007-ok": true}, false)
	if err == nil {
		t.Fatal("a hash move whose owning spec did not move must be refused")
	}
	if got := readLedger(t, root); got != nil {
		t.Fatalf("preflight must run before ANY append; got %d entr(ies)", len(got))
	}
}

func TestAuthorizedRestampAppendsChainedAttributedEntries(t *testing.T) {
	root := t.TempDir()
	t.Setenv(restampActorEnv, "autonomous-session")
	n, err := authorizeRestamps(root, map[string][]string{"007-x": {"tools/specvalidate/main.go"}},
		map[string]bool{"007-x": true}, false)
	if err != nil || n != 1 {
		t.Fatalf("want 1 authorized approval, got n=%d err=%v", n, err)
	}
	// A second run must CONTINUE the chain, not fork a new one from seq 1.
	n, err = authorizeRestamps(root, map[string][]string{"014-y": {"core/skillstore/welch.go"}},
		map[string]bool{"014-y": true}, false)
	if err != nil || n != 1 {
		t.Fatalf("second restamp: n=%d err=%v", n, err)
	}
	got := readLedger(t, root)
	if len(got) != 2 {
		t.Fatalf("want 2 chained entries, got %d", len(got))
	}
	if got[0].Seq != 1 || got[1].Seq != 2 || got[1].PrevHash != got[0].Hash {
		t.Fatalf("entries must chain (seq 1→2, prev_hash links): %+v", got)
	}
	for _, e := range got {
		if e.Decision != "lockstep:restamp" || !strings.Contains(e.Reason, "role=autonomous-session") {
			t.Fatalf("entry must be attributed lockstep:restamp with the actor role, got %+v", e)
		}
	}
	if !strings.Contains(got[0].Reason, "spec=007-x") || !strings.Contains(got[0].Reason, "tools/specvalidate/main.go") {
		t.Fatalf("the record must reconstruct spec+paths from the ledger alone, got %q", got[0].Reason)
	}
}

func TestHandEditedTailIsRefusedNotRestarted(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "spec"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, restampLedgerRel), []byte("{\"garbage\": true}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(restampActorEnv, "owner")
	_, err := authorizeRestamps(root, map[string][]string{"007-x": {"a.go"}}, map[string]bool{"007-x": true}, false)
	// The full-chain walk (which now runs first) catches this as a broken chain; the tail-shape read is
	// the second line of defense. Either refusal is correct — a fork from seq 1 is the only wrong outcome.
	if err == nil || (!strings.Contains(err.Error(), "refused, not restarted") && !strings.Contains(err.Error(), "chain broken")) {
		t.Fatalf("an unreadable chain tail must refuse, never silently fork from seq 1; got %v", err)
	}
	if got := readLedger(t, root); len(got) > 1 {
		t.Fatalf("nothing may be appended after a refused tail, got %d entr(ies)", len(got))
	}
}

func TestAllowUnchangedSpecStandsInForTheSameDiffAttestation(t *testing.T) {
	// The explicit, history-visible flag is the documented exceptional path; it must still produce a
	// fully attributed, chained record — never a silent bypass of the ledger.
	root := t.TempDir()
	t.Setenv(restampActorEnv, "owner")
	n, err := authorizeRestamps(root, map[string][]string{"007-x": {"a.go"}}, nil, true)
	if err != nil || n != 1 {
		t.Fatalf("flagged restamp must be authorized AND ledgered: n=%d err=%v", n, err)
	}
	if got := readLedger(t, root); len(got) != 1 {
		t.Fatalf("want 1 entry, got %d", len(got))
	}
}

func TestTamperedMidChainEntryIsDetected(t *testing.T) {
	// A PLAUSIBLE tamper: edit an earlier entry's Reason, leave its seq/hash bytes alone. The tail-shape
	// read cannot see it; the full walk must (review finding 2026-08-25 — the doc claimed the walk, the
	// code checked only the tail).
	root := t.TempDir()
	t.Setenv(restampActorEnv, "owner")
	for i, spec := range []string{"007-a", "012-b"} {
		if _, err := authorizeRestamps(root, map[string][]string{spec: {"f.go"}}, map[string]bool{spec: true}, false); err != nil {
			t.Fatalf("restamp %d: %v", i, err)
		}
	}
	path := filepath.Join(root, restampLedgerRel)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(b), "spec=007-a", "spec=007-EVIL", 1)
	if tampered == string(b) {
		t.Fatal("tamper did not apply")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyRestampLedger(root); err == nil || !strings.Contains(err.Error(), "tampered") {
		t.Fatalf("a content-edited mid-chain entry must break the walk, got %v", err)
	}
	// And the next restamp must REFUSE to extend the tampered chain.
	if _, err := authorizeRestamps(root, map[string][]string{"014-c": {"g.go"}}, map[string]bool{"014-c": true}, false); err == nil {
		t.Fatal("appending to a tampered chain must refuse")
	}
}

// TestLockstepRestampWiresTheLedger is the CALL-SITE killing test (review finding 2026-08-25, HIGH): it
// runs the real binary end-to-end so deleting the authorizeRestamps call in lockstep() — which every unit
// test above survives — goes red here: the manifest would move with no ledger entry.
func TestLockstepRestampWiresTheLedger(t *testing.T) {
	repo := repoRoot()
	bin := filepath.Join(t.TempDir(), "specvalidate")
	build := exec.Command("go", "build", "-o", bin, "./tools/specvalidate")
	build.Dir = repo
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	fixture := t.TempDir()
	if err := os.WriteFile(filepath.Join(fixture, "go.mod"), []byte("module fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(fixture, "spec", "007-x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "governed.go"), []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lock := `{"files":[{"path":"governed.go","spec":"007-x","sha256":"` + strings.Repeat("0", 64) + `"}]}`
	if err := os.WriteFile(filepath.Join(fixture, "spec", ".lockstep.lock"), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}

	run := exec.Command(bin, "lockstep", "--restamp", "--allow-unchanged-spec")
	run.Dir = fixture
	run.Env = append(os.Environ(), restampActorEnv+"=autonomous-session")
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("restamp run failed: %v\n%s", err, out)
	}
	entries := readLedger(t, fixture)
	if len(entries) != 1 || entries[0].Decision != "lockstep:restamp" {
		t.Fatalf("the CLI restamp must append exactly one ledger entry via the wired call site, got %+v\n%s", entries, out)
	}
	if !strings.Contains(entries[0].Reason, "allow-unchanged-spec") {
		t.Fatalf("the exceptional-path attestation must be in the ledger Reason, got %q", entries[0].Reason)
	}
	relocked, err := os.ReadFile(filepath.Join(fixture, "spec", ".lockstep.lock"))
	if err != nil || strings.Contains(string(relocked), strings.Repeat("0", 64)) {
		t.Fatalf("the manifest hash must have moved alongside the ledger entry (err=%v)", err)
	}

	// Actorless: the SAME end-to-end path must refuse and leave the manifest untouched.
	if err := os.WriteFile(filepath.Join(fixture, "spec", ".lockstep.lock"), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}
	run2 := exec.Command(bin, "lockstep", "--restamp", "--allow-unchanged-spec")
	run2.Dir = fixture
	run2.Env = append(os.Environ(), restampActorEnv+"=")
	if out2, err := run2.CombinedOutput(); err == nil {
		t.Fatalf("an actorless CLI restamp must exit non-zero, got:\n%s", out2)
	}
	if after, _ := os.ReadFile(filepath.Join(fixture, "spec", ".lockstep.lock")); !strings.Contains(string(after), strings.Repeat("0", 64)) {
		t.Fatal("a refused CLI restamp must leave the manifest untouched")
	}
}
