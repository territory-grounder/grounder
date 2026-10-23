package faultinjector

import (
	"context"
	"strings"
	"testing"
	"time"
)

// log-fill is the realistic disk-pressure fault: it grows an OPERATOR-DECLARED application log until root
// usage enters the alerting band, and its restore TRUNCATES that log. It exists because the only
// disk-pressure fault TG has ever seen is disk-fill's `fallocate`d image at a path nothing but the harness
// owns — benchmark instrumentation TG must never learn to delete — so 100% of the disk-pressure corpus is
// unhealable by construction and the owner-authorized reclaim capability has nothing honest to prove itself
// against.
//
// The oracles below are about the SAFETY BOUNDARY, because that is where this class can do real harm: its
// restore truncates a file, so the set of files it may ever target is the whole security argument. The
// reclaim red-team's finding drives it — `journalctl --vacuum-*` was rejected outright because journald
// carries the sudo lines the actor-attribution engine reads and (on the control-plane host) every TG
// container's own logs, and the tg-actuator-guard trail is the last-line control's only record. A fault whose
// undo truncates those would destroy the evidence TG's safety controls depend on.

// mustRefuse is the INDEPENDENT statement of what may never be a log-fill target, written down here rather
// than read from the shipped list.
//
// ★ THIS EXISTS BECAUSE THE FIRST VERSION OF THIS ORACLE WAS VACUOUS AGAINST ITS OWN MUTATION CONTROL. It
// derived every case from forbiddenLogPrefixes, so deleting "/var/log/journal/" from the shipped list also
// deleted the test case that would have caught the deletion — the protection could be removed and this test
// stayed GREEN (only a sibling oracle failed, and by luck). A list-driven test proves the list is applied
// CONSISTENTLY; it cannot prove the list still CONTAINS anything in particular. Both statements are needed,
// so this file now makes both: the loop below proves consistency, and this constant proves membership.
//
// Each entry is here because something READS it as proof — journald carries the sudo lines the actor-
// attribution engine parses and, on the control-plane host, every TG container's own logs; the guard trail is
// the last-line actuation control's only record; wtmp/audit are the host's tamper-evidence.
var mustRefuse = []string{
	"/var/log/journal/system.journal",
	"/run/log/journal/system.journal",
	"/var/log/tg-actuator-guard.log",
	"/var/log/audit/audit.log",
	"/var/log/wtmp",
	"/var/log/btmp",
	"/var/log/lastlog",
}

// TestALogFillTargetMayNeverBeAnEvidenceStore is the load-bearing oracle: the closed set of refusals.
//
// Two independent halves, for the reason documented on mustRefuse above — (a) every path in the hand-written
// mustRefuse list is refused, which catches a protection being DELETED from the shipped list; and (b) every
// prefix in the shipped list is applied to both a child path and the bare prefix, which catches the matching
// logic being weakened while the list stays intact.
func TestALogFillTargetMayNeverBeAnEvidenceStore(t *testing.T) {
	// (a) membership — independent of the shipped list.
	for _, p := range mustRefuse {
		if err := ValidLogPath(p); err == nil {
			t.Errorf("ValidLogPath(%q) ACCEPTED an evidence store — a log-fill there would have its restore "+
				"truncate the record TG's own safety controls read. If this path is genuinely no longer an "+
				"evidence store, remove it from mustRefuse DELIBERATELY, with the reason.", p)
		}
	}
	// (b) consistency — the shipped list is actually applied, at both granularities.
	if len(forbiddenLogPrefixes) == 0 {
		t.Fatal("forbiddenLogPrefixes is empty — every path would be accepted and this half would pass vacuously")
	}
	for _, prefix := range forbiddenLogPrefixes {
		// Both a file directly under the prefix and the prefix itself must be refused.
		for _, candidate := range []string{prefix + "victim.log", strings.TrimSuffix(prefix, "/")} {
			if err := ValidLogPath(candidate); err == nil {
				t.Errorf("ValidLogPath(%q) ACCEPTED an evidence store — a log-fill there would have its restore "+
					"truncate the record TG's own safety controls read (journald/sudo attribution, the "+
					"actuator-guard trail, the audit log)", candidate)
			}
		}
	}
	// And the ordinary case must still be usable, or the class is unusable and the refusals prove nothing.
	if err := ValidLogPath("/var/log/tg-demo/app.log"); err != nil {
		t.Fatalf("a plain application log must be declarable, got: %v", err)
	}
}

// TestALogFillTargetMustBeAnUnambiguousAbsolutePath — the other three declaration properties. A relative path
// resolves against an unknown remote cwd; a traversal segment escapes any prefix reasoning the refusal list
// does; and shell metacharacters matter even though the injector uses fixed argv, because the same declared
// string is what an operator later pastes into a host allowfile line.
func TestALogFillTargetMustBeAnUnambiguousAbsolutePath(t *testing.T) {
	for _, bad := range []string{
		"", "   ",
		"var/log/app.log",             // relative
		"/var/log/../../etc/passwd",   // traversal
		"/var/log/journal/../app.log", // traversal that lands outside a refused prefix
		"/var/log/app log.log",        // whitespace
		"/var/log/$(id).log",          // command substitution shape
		"/var/log/a;rm -rf /.log",     // separator
		"/var/log/*.log",              // glob
	} {
		if err := ValidLogPath(bad); err == nil {
			t.Errorf("ValidLogPath(%q) accepted an unsafe or ambiguous path", bad)
		}
	}
}

// TestTheUndoTruncatesAndNeverRemoves — the repair must restore the log's SIZE without destroying the inode
// the running service holds open. An `rm` would leave the writer appending to an unlinked file nobody can
// read: a worse estate than the fault, and one no verifier would notice.
func TestTheUndoTruncatesAndNeverRemoves(t *testing.T) {
	argv, host, err := UndoArgv(Outstanding{ID: 7, Host: "guest01", Class: ClassLogFill,
		FaultRef: "/var/log/tg-demo/app.log"})
	if err != nil {
		t.Fatalf("UndoArgv: %v", err)
	}
	if host != "guest01" {
		t.Errorf("the log-fill undo must run on the GUEST (it stays up throughout), got host %q", host)
	}
	joined := strings.Join(argv, " ")
	if argv[0] != "truncate" {
		t.Errorf("the undo must TRUNCATE, got %q — removing the file orphans the writer's open inode", joined)
	}
	if strings.Contains(joined, "rm ") {
		t.Errorf("the undo must never remove the operator's log file, got %q", joined)
	}
	if !strings.Contains(joined, "/var/log/tg-demo/app.log") {
		t.Errorf("the undo must name the DECLARED path, got %q", joined)
	}
}

// TestTheUndoRefusesAnUnsafeLedgerPath — FaultRef is read back from the LEDGER, so it is untrusted input at
// repair time: a row written by an older binary, or edited, must not be able to make the repair path truncate
// an evidence store. A repair that cannot be proven safe is refused, which quarantines the host rather than
// doing damage in the name of cleanup.
func TestTheUndoRefusesAnUnsafeLedgerPath(t *testing.T) {
	for _, ref := range []string{"/var/log/journal/system.journal", "/var/log/tg-actuator-guard.log", "relative.log", ""} {
		if _, _, err := UndoArgv(Outstanding{ID: 9, Host: "guest01", Class: ClassLogFill, FaultRef: ref}); err == nil {
			t.Errorf("UndoArgv accepted an unsafe ledger fault_ref %q — the repair would truncate it", ref)
		}
	}
}

// TestInjectRefusesAnUnsafePathEvenWhenTheLoaderIsBypassed — InjectLogFill is EXPORTED and takes a path, so a
// future caller that skips the pool loader must still be unable to grow (and thereby mark for truncation) an
// evidence store. A destructive default has to be unreachable from every entry point, not just today's.
func TestInjectRefusesAnUnsafePathEvenWhenTheLoaderIsBypassed(t *testing.T) {
	r := newFake()
	if _, err := InjectLogFill(context.Background(), r, "guest01", "/var/log/journal/system.journal"); err == nil {
		t.Fatal("InjectLogFill accepted an evidence-store path when called directly")
	}
	if len(r.calls) != 0 {
		t.Errorf("the refusal must happen BEFORE any remote command runs, got %v", r.calls)
	}
}

// TestVerifyLogTruncatedFailsClosedOnAnUnknownExit is the inherited fail-open lesson, re-proven at the new
// seam. SSHRunner turns a non-zero remote exit into (out, code, nil); the ssh client exits 255 when it cannot
// connect and a context kill yields -1. Reading "not 0" as proof would report an UNREACHABLE guest — exactly
// the state where the fill is most likely still present — as repaired, close the obligation PERMANENTLY, and
// release the quarantine so the planner could stack another fault on a guest still at 91%. That is the bug
// that stranded two guests at 97% and the reason this package exists.
func TestVerifyLogTruncatedFailsClosedOnAnUnknownExit(t *testing.T) {
	path := "/var/log/tg-demo/app.log"
	cases := []struct {
		code      int
		wantOK    bool
		wantErr   bool
		statement string
	}{
		{0, true, false, "empty or absent is the only proof of truncation"},
		{1, false, false, "the file still has bytes — not repaired, but a known answer"},
		{255, false, true, "unreachable must never read as repaired"},
		{-1, false, true, "a timed-out check must never read as repaired"},
	}
	for _, c := range cases {
		r := newFake()
		r.code["test"] = c.code
		ok, err := VerifyLogTruncated(context.Background(), r, "guest01", path)
		if ok != c.wantOK || (err != nil) != c.wantErr {
			t.Errorf("exit %d: got (ok=%v err=%v), want (ok=%v err=%v) — %s", c.code, ok, err != nil, c.wantOK, c.wantErr, c.statement)
		}
	}
}

// TestLogFillIsDeclaredDetectionOnlyUntilAReclaimVerbExists — the P3-before-P4 order, asserted rather than
// commented. Declaring a pairing to a verb that does not exist is the false-coverage failure opcover exists to
// prevent, and it is exactly what disk-fill's entry was corrected for. When the truncate/rotate reclaim verb
// ships, IT claims this pairing — and this oracle is what forces that to be a deliberate edit.
func TestLogFillIsDeclaredDetectionOnlyUntilAReclaimVerbExists(t *testing.T) {
	if got := ClassLogFill.Provokes(); len(got) != 0 {
		t.Fatalf("log-fill declares it provokes %v, but no reclaim op-class is registered — a pairing to a "+
			"nonexistent verb credits coverage that can never be exercised. If the verb now exists, update "+
			"this oracle deliberately along with the pairing.", got)
	}
	if !ClassLogFill.OwesRestore() {
		t.Error("log-fill leaves a grown file on the estate — it MUST owe a restore or the fault strands")
	}
}

// TestAGuestWithNoUsableLogIsNotEVENSELECTED — the planner arm, not the leaf refusal.
//
// FOUND LIVE, WITHIN THE HOUR OF SHIPPING: the planner picked dc1wallos01 for log-fill although that
// guest declares no LogPath. The leaf refused and the obligation closed honestly ("injection aborted BEFORE
// any effect ... provably nothing was broken"), so the estate was never touched — but the cycle's injection
// slot was already spent, and the aborted row lands in the benchmark population. The comment on ClassLogFill
// claimed "an undeclared LogPath makes the guest ineligible"; that was true at the LEAF and false at the
// PLANNER, and only a planner-level oracle can tell those apart.
//
// It drives Decide over the CLOSED enumeration of guests rather than one hand-picked case, so a guest whose
// path is malformed or points into an evidence store is covered by the same assertion — those are exactly as
// unusable as an empty one, and a check for `== ""` would pass this test while still selecting them.
func TestAGuestWithNoUsableLogIsNotEvenSelected(t *testing.T) {
	// VMID ORDER IS LOAD-BEARING, and it took two failed controls to establish that. PlanNext does NOT sweep
	// the pool in slice order: it sorts a copy by VMID (`sort.SliceStable(order, ... VMID < VMID)`) and starts
	// at `Injected % n`. The first draft gave the eligible guest VMID "101" — which sorts FIRST — so with a
	// deleted eligibility arm the planner still reached it before any ineligible candidate, and BOTH mutation
	// controls (delete the arm; weaken it to `LogPath == ""`) stayed GREEN. Reordering the slice changed
	// nothing, for the same reason. An oracle that cannot fail is the one thing an oracle must never be.
	//
	// The eligible guest now carries the HIGHEST VMID, so the sweep visits every ineligible SHAPE before it:
	// undeclared, evidence-store, relative, and shell-metacharacter. A missing filter selects one of those
	// and the failure names it.
	pool := []PoolGuest{
		{VMID: "101", Name: "undeclared", Node: "n1"},
		{VMID: "102", Name: "evidence-store", Node: "n1", LogPath: "/var/log/journal/system.journal"},
		{VMID: "103", Name: "relative", Node: "n1", LogPath: "var/log/app.log"},
		{VMID: "104", Name: "metachars", Node: "n1", LogPath: "/var/log/$(id).log"},
		{VMID: "999", Name: "declared", Node: "n1", LogPath: "/var/log/tg-demo/app.log"},
	}
	st := State{
		Now:       time.Now(),
		Pool:      pool,
		Allowlist: map[string]bool{},
		Status:    map[string]string{},
		Limits:    Limits{MaxDown: 5, MaxBusy: 5},
	}
	for _, g := range pool {
		st.Allowlist[g.Name] = true
		st.Status[g.VMID] = "running"
	}
	// Every guest is free and allowlisted; only ELIGIBILITY can decide which one is picked.
	d := PlanNext(st, []Class{ClassLogFill})
	if !d.Act {
		t.Fatalf("no guest selected for log-fill although %q declares a usable log — the class is unreachable: %s",
			"declared", d.Reason)
	}
	if d.Guest.Name != "declared" {
		t.Fatalf("planner selected %q for log-fill, whose declared log path %q is unusable — the guest must be "+
			"skipped at PLANNING, not refused at the effect leaf after the injection slot is already spent",
			d.Guest.Name, d.Guest.LogPath)
	}
}
