package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TG-363. tg-syslogng-guard returned exit 42 for BOTH "your request is malformed" and "that file does not
// exist", and the caller collapsed the two into one agent-facing sentence — "the device may not log there,
// or that day has no file". So a TG-side defect (a bad argv shape, a bound out of range) would have been
// reported to the model, permanently and silently, as an observation about the ESTATE.
//
// This guard's own header calls the refused-vs-unreachable distinction load-bearing, citing TG-271/TG-300.
// The same argument applies one level in: an estate FACT and a TG BUG must not share a status.
//
// Probed live on dc1syslogng01 2026-08-06: dc1ap01 read fine through the guard, while
// dc1syslogng01 and dc1mealie01 were refused — they have no log directory, not a broken request.

func dateLogTree(t *testing.T) (base, existing string) {
	t.Helper()
	base = t.TempDir()
	dir := filepath.Join(base, "dc1ap01", "2026", "08")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	existing = filepath.Join(dir, "dc1ap01-2026-08-06.log")
	if err := os.WriteFile(existing, []byte("Aug  6 09:35:31 dc1ap01 %DOT11-6-ROAMED\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return base, existing
}

// KILLING MUTATION: return a plain error instead of errNoSuchLog for the missing-file case (the pre-fix
// state). RED.
func TestAMissingFileInsideTheBaseIsNotARefusal(t *testing.T) {
	base, _ := dateLogTree(t)
	missing := filepath.Join(base, "dc1mealie01", "2026", "08", "dc1mealie01-2026-08-06.log")

	err := checkPath(missing, base)
	if err == nil {
		t.Fatal("a nonexistent log was accepted — the read would exec tail on nothing")
	}
	if !errors.Is(err, errNoSuchLog) {
		t.Errorf("a legal request for an absent file reports as a REFUSAL (%v). The caller then tells the "+
			"agent the request was rejected, and a TG defect and an estate fact become indistinguishable", err)
	}
}

// THE SECURITY PROPERTY, and the reason containment is checked lexically FIRST. If existence were reported
// before containment, the exit status would be an oracle for whether arbitrary paths exist on the syslog
// host — a caller could probe /etc/shadow and read the answer off the exit code.
//
// KILLING MUTATION: move the EvalSymlinks(p) existence check above the lexical containment check. RED on
// both cases below.
func TestExistenceIsNeverReportedForAPathOutsideTheBase(t *testing.T) {
	base, _ := dateLogTree(t)

	// (a) An OUTSIDE path that EXISTS must be refused, never acknowledged.
	outsideExisting := filepath.Join(t.TempDir(), "real.log")
	if err := os.WriteFile(outsideExisting, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := checkPath(outsideExisting, base)
	if err == nil {
		t.Fatal("a path outside the log base was accepted")
	}
	if errors.Is(err, errNoSuchLog) {
		t.Error("an existing path outside the base reported as no-such-log — inconsistent, and the pair of " +
			"answers still discriminates")
	}
	if !strings.Contains(err.Error(), "outside") {
		t.Errorf("an outside path was not refused as outside: %v", err)
	}

	// (b) The SAME outside path, now deleted, must give a BYTE-IDENTICAL answer. Using one path is the
	// whole point: comparing two different paths could never match (the message names the path), so it
	// would prove nothing. With the path held constant, the only variable is existence — and if the answers
	// differ at all, the exit status is an oracle: probe any path, read whether it exists off the response.
	if err := os.Remove(outsideExisting); err != nil {
		t.Fatal(err)
	}
	err2 := checkPath(outsideExisting, base)
	if err2 == nil {
		t.Fatal("a nonexistent path outside the log base was accepted")
	}
	if errors.Is(err2, errNoSuchLog) {
		t.Error("a nonexistent path OUTSIDE the base reported as no-such-log — that is the existence oracle: " +
			"probe any path, read the answer off the exit status")
	}
	if err.Error() != err2.Error() {
		t.Errorf("the SAME outside path answers differently depending on whether it exists (%q vs %q) — the "+
			"pair discriminates and leaks existence", err, err2)
	}
	// ..and a traversal escape is still refused rather than probed.
	if e := checkPath(filepath.Join(base, "..", "etc", "shadow"), base); e == nil || errors.Is(e, errNoSuchLog) {
		t.Errorf("a traversal out of the base was not refused as outside: %v", e)
	}
}

// The symlink defence is unchanged: containment is RE-checked after resolution, because a symlink inside
// the tree may point anywhere.
//
// KILLING MUTATION: drop the post-EvalSymlinks containment check. RED.
func TestASymlinkInsideTheTreePointingOutIsStillRefused(t *testing.T) {
	base, _ := dateLogTree(t)
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("root:x:0:0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "dc1ap01", "2026", "08", "escape.log")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	err := checkPath(link, base)
	if err == nil {
		t.Fatal("a symlink inside the log tree pointing OUTSIDE it was accepted — this is the defence the " +
			"lexical-first reordering must not have weakened")
	}
	if errors.Is(err, errNoSuchLog) {
		t.Errorf("an escaping symlink reported as no-such-log (%v) — it exists, and reporting absence would "+
			"both lie and leak", err)
	}
}

// The permitted case must still be permitted — a reordering that refused everything would pass every test
// above.
func TestAnExistingLogInsideTheBaseIsPermitted(t *testing.T) {
	base, existing := dateLogTree(t)
	if err := checkPath(existing, base); err != nil {
		t.Fatalf("a real log inside the base was refused: %v", err)
	}
	// And through the full validate(), in the exact wire shape the caller sends.
	if err := validate([]string{"tail", "-n", "50", "--", existing}, base); err != nil {
		t.Errorf("the tail shape over a real log was refused: %v", err)
	}
	if err := validate([]string{"grep", "-F", "-m", "10", "--", "ROAMED", existing}, base); err != nil {
		t.Errorf("the grep shape over a real log was refused: %v", err)
	}
}

// A MALFORMED request over an absent file must still be a REFUSAL, not a no-such-log: the shape is checked
// before the path, so TG's own defect never hides behind an estate fact.
//
// KILLING MUTATION: check the path before the argv shape. RED.
func TestAMalformedRequestIsARefusalEvenWhenTheFileIsAlsoMissing(t *testing.T) {
	base, _ := dateLogTree(t)
	missing := filepath.Join(base, "dc1mealie01", "2026", "08", "x.log")
	for _, argv := range [][]string{
		{"tail", "-n", "99999", "--", missing},        // bound out of range
		{"tail", "-n", "50", missing},                 // missing --
		{"cat", "--", missing},                        // binary not permitted
		{"grep", "-F", "-m", "0", "--", "p", missing}, // bound below 1
	} {
		err := validate(argv, base)
		if err == nil {
			t.Errorf("validate(%v) accepted a malformed request", argv)
			continue
		}
		if errors.Is(err, errNoSuchLog) {
			t.Errorf("validate(%v) reported TG's own malformed request as an estate fact (%v) — the exact "+
				"conflation this change exists to end", argv, err)
		}
	}
}

// The two statuses must differ, or the caller cannot act on them.
func TestTheTwoExitStatusesAreDistinct(t *testing.T) {
	if noSuchLogExit == refusedExit {
		t.Fatalf("no-such-log and refused share exit %d — the caller is back to one answer for two causes",
			refusedExit)
	}
	if refusedExit != 42 {
		t.Errorf("refusedExit moved to %d; the sibling guards and every caller assume 42", refusedExit)
	}
}
