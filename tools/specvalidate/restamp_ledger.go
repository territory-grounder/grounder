package main

// The wiring that makes core/governance.AuthorizeRestamp REAL for the CLI restamp path (TG-536, decided
// 2026-08-25 under the owner-approved graduation plan). Before `lockstep --restamp` rewrites
// spec/.lockstep.lock, every hash move is authorized through AuthorizeRestamp and appended — RBAC-
// attributed and hash-chained — to the IN-REPO restamp ledger spec/.restamp-ledger.jsonl, which travels
// in the SAME MR as the moved hashes. That answers REQ-703's two open questions concretely:
//
//   - WHERE a developer-box restamp gets its actor role: TG_RESTAMP_ACTOR, required, validated against
//     the compiled authority below (the two roles this repo actually operates under). Absent or unknown
//     actor: the restamp is refused and the manifest is NOT written.
//   - WHICH ledger it appends to: the in-repo chained JSONL. Each entry carries seq/prev_hash/hash via
//     core/audit's chain, so a hand-edited or deleted record breaks the walk (verified by the drill in
//     restamp_ledger_test.go), and a manifest change with no matching entry is visible in any review of
//     the same diff. This is deliberately NOT the production DB ledger — a dev-box tool must not need DB
//     credentials to be governed, and an out-of-repo record would be invisible to the MR that moves the
//     hash. One control, reviewable where the change is reviewed.
//
// Fail-closed order: ALL approvals are preflighted (authority + same-diff rule + shape) before ANY entry
// is appended, so a denial leaves neither ledger entries nor a rewritten manifest behind.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/governance"
)

// restampLedgerRel is the in-repo, append-only, hash-chained record of authorized re-stamps.
const restampLedgerRel = "spec/.restamp-ledger.jsonl"

// restampActorEnv names the role performing the restamp; required (REQ-703: RBAC-attributed, never
// anonymous). The value is recorded verbatim in every ledger entry's reason.
const restampActorEnv = "TG_RESTAMP_ACTOR"

// compiledRestampAuthority is the RBAC seam's dev-box policy: the two roles this repository operates
// under may authorize a re-stamp of any spec. "owner" is @ncpjfuzl (CODEOWNERS); "autonomous-session"
// is the standing Law-Change grant (AGENTS.md § Standing trailer authority). Anything else denies —
// including the empty string, so an unset actor can never slip through as a permitted role.
type compiledRestampAuthority struct{}

func (compiledRestampAuthority) MayRestamp(actorRole, owningSpec string) bool {
	if owningSpec == "" {
		return false
	}
	switch actorRole {
	case "owner", "autonomous-session":
		return true
	}
	return false
}

// restampFileSink appends each authorized entry as one JSON line. Sync before returning: a restamp is a
// governance record first and a convenience second — losing the record while keeping the moved hash is
// the exact host-local-edit shape REQ-703 forbids.
type restampFileSink struct{ f *os.File }

func (s restampFileSink) Persist(e audit.LedgerEntry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := s.f.Write(append(b, '\n')); err != nil {
		return err
	}
	return s.f.Sync()
}

// verifyRestampLedger walks the WHOLE in-repo chain — seq monotonicity, prev-hash linkage, and a content
// re-hash of every entry (audit.VerifyChain) — so a plausible-looking edit to ANY historical line is
// detected, not just a mangled tail. Returns (entries, nil) on a clean chain; (0, err) on absence-of-file
// is NOT an error (0 entries is an honest empty ledger, distinguishable because err is nil).
// Wired into `lockstep --check` so CI re-walks the chain on every run (review finding 2026-08-25: the
// tail-shape check alone let a content-tampered mid-chain entry pass silently).
func verifyRestampLedger(root string) (int, error) {
	path := filepath.Join(root, restampLedgerRel)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()
	var entries []audit.LedgerEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	line := 0
	for sc.Scan() {
		line++
		l := sc.Text()
		if l == "" {
			continue
		}
		var e audit.LedgerEntry
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			return 0, fmt.Errorf("restamp ledger line %d unparseable: %w", line, err)
		}
		entries = append(entries, e)
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	if err := audit.VerifyChain(entries); err != nil {
		return 0, fmt.Errorf("restamp ledger %s: %w", restampLedgerRel, err)
	}
	return len(entries), nil
}

// loadRestampLedgerTail returns (lastSeq, lastHash) of the chained ledger, or (0, "") when the file does
// not exist yet (the chain starts at seq 1). A present-but-unparseable tail is an error, never a silent
// restart from seq 1 — forking the chain is what tampering looks like.
func loadRestampLedgerTail(path string) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, "", nil
		}
		return 0, "", err
	}
	defer f.Close()
	var last string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		if l := sc.Text(); l != "" {
			last = l
		}
	}
	if err := sc.Err(); err != nil {
		return 0, "", err
	}
	if last == "" {
		return 0, "", nil
	}
	var e audit.LedgerEntry
	if err := json.Unmarshal([]byte(last), &e); err != nil || e.Seq == 0 || e.Hash == "" {
		return 0, "", fmt.Errorf("restamp ledger tail unreadable (%s): a forked or hand-edited chain is "+
			"refused, not restarted", path)
	}
	return e.Seq, e.Hash, nil
}

// authorizeRestamps authorizes every pending hash move (grouped by owning spec) through
// governance.AuthorizeRestamp against the in-repo chained ledger. movedBySpec maps owning spec ->
// changed paths; specsTouched is the same-diff spec set (nil when --allow-unchanged-spec, whose explicit,
// history-visible use stands in for the same-diff attestation exactly as the existing refusal text
// documents). ANY denial returns before a single entry is appended.
func authorizeRestamps(root string, movedBySpec map[string][]string, specsTouched map[string]bool, allowUnchanged bool) (int, error) {
	if len(movedBySpec) == 0 {
		return 0, nil
	}
	actor := os.Getenv(restampActorEnv)
	if actor == "" {
		return 0, fmt.Errorf("%s is not set — a re-stamp must be RBAC-attributed (REQ-703); export "+
			"%s=owner (or autonomous-session, under the standing Law-Change grant) and re-run", restampActorEnv, restampActorEnv)
	}
	authority := compiledRestampAuthority{}
	specs := make([]string, 0, len(movedBySpec))
	for s := range movedBySpec {
		specs = append(specs, s)
	}
	sort.Strings(specs)

	approvals := make([]governance.RestampApproval, 0, len(specs))
	for _, spec := range specs {
		appr := governance.RestampApproval{
			ActorRole:    actor,
			OwningSpec:   spec,
			ChangedPaths: movedBySpec[spec],
			SpecUpdated:  allowUnchanged || (specsTouched != nil && specsTouched[spec]),
		}
		if allowUnchanged {
			// The exceptional path must be reconstructable from the ledger ALONE — never from command
			// history (review finding 2026-08-25).
			appr.Note = "allow-unchanged-spec (operator attestation stood in for the same-diff rule)"
		}
		// Preflight the exact conditions AuthorizeRestamp enforces, so a denial anywhere leaves zero
		// entries appended (no authorized-but-never-exercised records).
		if !appr.SpecUpdated || !authority.MayRestamp(appr.ActorRole, appr.OwningSpec) {
			return 0, fmt.Errorf("re-stamp of %s DENIED for actor %q (spec updated in diff: %v) — %w",
				spec, actor, appr.SpecUpdated, governance.ErrUnauthorizedRestamp)
		}
		approvals = append(approvals, appr)
	}

	// Refuse to EXTEND a tampered chain: the full walk runs before any append, not just a tail-shape read.
	if _, err := verifyRestampLedger(root); err != nil {
		return 0, err
	}
	path := filepath.Join(root, restampLedgerRel)
	seq, hash, err := loadRestampLedgerTail(path)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var led *audit.Ledger
	if seq == 0 {
		led = audit.NewLedger()
	} else {
		led = audit.NewLedgerFromTail(seq, hash)
	}
	led = led.WithSink(restampFileSink{f})
	for _, appr := range approvals {
		if err := governance.AuthorizeRestamp(authority, appr, led); err != nil {
			return 0, fmt.Errorf("re-stamp of %s: %w", appr.OwningSpec, err)
		}
	}
	return len(approvals), nil
}
