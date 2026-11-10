package configwrite

// ORACLES FOR THE OPERATOR-DRIVEN REWRAP LANE (TG-163). The activity is a plain method, so its
// single-writer semantics, its fail-closed direction and its reporting are testable without a Temporal
// server. The DURABLE half — a half-rewrapped store staying readable, and the re-put race — lives in
// core/db/sealed_rewrap_test.go against a real Postgres, because that is where those properties actually
// live.

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/seal"
)

// memRewrap is an in-memory sealed store that models the ONE thing that matters here: the conditional
// swap. RewrapDEK lands only when the caller's "old" bytes still match what is stored.
type memRewrap struct {
	rows      []WrappedDEKRow
	updates   int
	failOn    string
	forceMiss string // this name's conditional update always misses (a re-put underneath the run)
}

func (m *memRewrap) ListWrappedDEKs(_ context.Context, afterName string) ([]WrappedDEKRow, error) {
	var out []WrappedDEKRow
	for _, r := range m.rows {
		if r.Name > afterName {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *memRewrap) RewrapDEK(_ context.Context, name string, oldWrapped, _, newWrapped, newNonce []byte) (bool, error) {
	if name == m.failOn {
		return false, errors.New("store down")
	}
	for i, r := range m.rows {
		if r.Name != name {
			continue
		}
		if name == m.forceMiss || string(r.WrappedDEK) != string(oldWrapped) {
			return false, nil
		}
		m.rows[i].WrappedDEK, m.rows[i].DEKNonce = newWrapped, newNonce
		m.updates++
		return true, nil
	}
	return false, nil
}

// bumpWrapper is a minimal seal.DEKWrapper whose wrapped DEK is "vault:v<N>:<dek>" — enough to exercise
// the version census and the rewrap's own verification.
type bumpWrapper struct{ version int }

func (b *bumpWrapper) WrapDEK(_ string, dek []byte) ([]byte, []byte, error) {
	return []byte("vault:v" + strconv.Itoa(b.version) + ":" + string(dek)), nil, nil
}

func (b *bumpWrapper) UnwrapDEK(_ string, wrapped, _ []byte) ([]byte, error) {
	s := string(wrapped)
	i := strings.LastIndexByte(s, ':')
	if i < 0 || seal.KeyVersion(wrapped) == 0 {
		return nil, seal.ErrOpenFailed
	}
	return []byte(s[i+1:]), nil
}

func (b *bumpWrapper) RewrapDEK(name string, wrapped, nonce []byte) ([]byte, []byte, error) {
	dek, err := b.UnwrapDEK(name, wrapped, nonce)
	if err != nil {
		return nil, nil, err
	}
	return b.WrapDEK(name, dek)
}

func rewrapRig(rows []WrappedDEKRow, version int) (*Activities, *memRewrap, *bumpWrapper) {
	st := &memRewrap{rows: rows}
	w := &bumpWrapper{version: version}
	s, _ := seal.NewSealer(w)
	acts, _, _, _ := rig()
	acts.D.Rewrap = st
	acts.D.Sealer = s
	return acts, st, w
}

func rowsAtV1(names ...string) []WrappedDEKRow {
	out := make([]WrappedDEKRow, 0, len(names))
	for _, n := range names {
		out = append(out, WrappedDEKRow{Name: n, WrappedDEK: []byte("vault:v1:dek-" + n)})
	}
	return out
}

// The lane must LEDGER the decision to re-key before it moves a single row — the same discipline every
// other governed write in this package keeps.
func TestRewrapLedgersTheDecisionBeforeAnyRowMoves(t *testing.T) {
	acts, st, w := rewrapRig(rowsAtV1("a", "b", "c"), 1)
	w.version = 2
	out, err := acts.RewrapSecretsActivity(context.Background(),
		RewrapRequest{Rationale: "retiring transit key v1", Operator: "kyriakos"})
	if err != nil {
		t.Fatalf("rewrap: %v", err)
	}
	if out.LedgerSeq == 0 {
		t.Fatalf("the run committed no ledger entry — re-keying the whole secret store is a governance " +
			"decision, and an unrecorded one cannot be audited afterwards")
	}
	if out.Scanned != 3 || out.Rewrapped != 3 || st.updates != 3 {
		t.Fatalf("scanned=%d rewrapped=%d storeUpdates=%d, want 3/3/3", out.Scanned, out.Rewrapped, st.updates)
	}
	if out.Versions["v2"] != 3 {
		t.Fatalf("version census = %v, want v2=3 — the census is what tells an operator whether the old "+
			"key version may be retired, so a wrong one is worse than none", out.Versions)
	}
}

// FAIL CLOSED. A worker with no seal backend must REFUSE, not report a serene success over a store it never
// touched — that report is what would get an operator to retire a key version still holding the estate up.
//
// KILLING MUTATION (executed 2026-08-04): change the guard to `if a.D.Rewrap == nil` so a nil Sealer falls
// through. RED — and the observed failure is worth recording exactly, because it is NOT the one the comment
// above would lead you to expect:
//
//	panic: runtime error: invalid memory address or nil pointer dereference
//	  core/seal.(*Sealer).RewrapDEK ... temporal/configwrite.(*Activities).rewrapRows
//
// Without this guard the activity does not report a false all-clear — it panics the worker mid-walk, AFTER
// the ledger entry for the run is already committed. So the guard earns its place twice over: it keeps the
// honest refusal honest, and it keeps a deployment with no seal backend from taking a Temporal activity
// down on an operator's first rewrap.
func TestRewrapRefusesWhenTheWorkerHasNoSealBackend(t *testing.T) {
	acts, _, _ := rewrapRig(rowsAtV1("a"), 1)
	acts.D.Sealer = nil
	out, err := acts.RewrapSecretsActivity(context.Background(),
		RewrapRequest{Rationale: "why", Operator: "op"})
	if err == nil {
		t.Fatalf("a rewrap with NO sealer reported success (%+v) — it must fail closed, because "+
			"'0 rows re-keyed' and 'I cannot re-key' are opposite answers to 'can I retire the old key?'", out)
	}
	if !errors.Is(err, ErrRewrapUnavailable) {
		t.Fatalf("want ErrRewrapUnavailable, got %v", err)
	}
}

// Every governed write in this lane states why. A re-key of the whole secret store is no exception.
func TestRewrapRequiresARationale(t *testing.T) {
	acts, _, _ := rewrapRig(rowsAtV1("a"), 1)
	if _, err := acts.RewrapSecretsActivity(context.Background(), RewrapRequest{Operator: "op"}); !errors.Is(err, ErrRationaleRequired) {
		t.Fatalf("want ErrRationaleRequired, got %v", err)
	}
}

// THE VACUITY FLOOR ON THE REPORT ITSELF. A run over an empty store legitimately does nothing — and must
// SAY so, because "rewrap complete" over zero rows is precisely the sentence that precedes an operator
// retiring a key version that half the estate still depends on.
func TestARewrapOverAnEmptyStoreSaysItProvedNothing(t *testing.T) {
	acts, _, _ := rewrapRig(nil, 2)
	out, err := acts.RewrapSecretsActivity(context.Background(),
		RewrapRequest{Rationale: "post-rotation sweep", Operator: "op"})
	if err != nil {
		t.Fatalf("rewrap: %v", err)
	}
	if out.Scanned != 0 || out.Rewrapped != 0 {
		t.Fatalf("expected an empty run, got %+v", out)
	}
	if !strings.Contains(out.Note, "NOTHING") {
		t.Fatalf("an empty run reported %q — it must state outright that it proved nothing about which "+
			"key versions are in use, or it reads as an all-clear", out.Note)
	}
}

// A store spread across TWO key versions must say so in words, not just in a map nobody reads.
func TestAMixedVersionStoreWarnsAgainstRetiringTheOldKey(t *testing.T) {
	rows := rowsAtV1("a", "b")
	rows = append(rows, WrappedDEKRow{Name: "c", WrappedDEK: []byte("vault:v2:dek-c")})
	acts, st, w := rewrapRig(rows, 2)
	st.forceMiss = "a" // re-put underneath the run, so it stays where the writer left it
	w.version = 2
	out, err := acts.RewrapSecretsActivity(context.Background(),
		RewrapRequest{Rationale: "sweep", Operator: "op"})
	if err != nil {
		t.Fatalf("rewrap: %v", err)
	}
	if out.Skipped != 1 {
		t.Fatalf("skipped=%d, want 1 — a row re-put underneath the run is not an error, but it must be "+
			"counted separately from one this run actually moved", out.Skipped)
	}
	// Now force a genuinely mixed census and check the warning.
	mixed := RewrapOutcome{Scanned: 3, Rewrapped: 2, Versions: map[string]int{"v1": 1, "v2": 2}}
	if note := rewrapNote(mixed); !strings.Contains(note, "do NOT raise min_decryption_version") {
		t.Fatalf("a store on TWO key versions reported %q — without that warning the next step an "+
			"operator takes destroys every row still on the old version", note)
	}
}

// A bounded run must not let its report imply anything about rows it never looked at.
func TestAPartialRunSaysItIsPartial(t *testing.T) {
	acts, _, w := rewrapRig(rowsAtV1("a", "b", "c", "d"), 1)
	w.version = 2
	out, err := acts.RewrapSecretsActivity(context.Background(),
		RewrapRequest{Rationale: "batch one", Operator: "op", Limit: 2})
	if err != nil {
		t.Fatalf("rewrap: %v", err)
	}
	if out.Scanned != 2 || !out.Partial {
		t.Fatalf("scanned=%d partial=%v, want 2/true", out.Scanned, out.Partial)
	}
	if out.LastName != "b" {
		t.Fatalf("resume cursor = %q, want \"b\" — a wrong cursor either re-does work or silently strands "+
			"rows on the old key version forever", out.LastName)
	}
	if !strings.Contains(out.Note, "PARTIAL") {
		t.Fatalf("a bounded run reported %q — it must not read as a complete account of the store", out.Note)
	}
}

// A row that cannot be re-keyed must name ITSELF. The run discovered a pre-existing fault; losing which
// credential it was turns a precise finding into "something is wrong somewhere".
func TestARewrapFailureNamesTheRow(t *testing.T) {
	acts, st, w := rewrapRig(rowsAtV1("alpha", "beta"), 1)
	w.version = 2
	st.failOn = "beta"
	_, err := acts.RewrapSecretsActivity(context.Background(),
		RewrapRequest{Rationale: "sweep", Operator: "op"})
	if err == nil {
		t.Fatalf("a store write failure was swallowed")
	}
	if !strings.Contains(err.Error(), "beta") {
		t.Fatalf("the error %q does not name the row that failed — the operator needs to know WHICH "+
			"credential is stuck, because that row is a fault this run found rather than caused", err)
	}
	// THE RESUME CURSOR MUST NOT SKIP THE ROW THAT FAILED.
	//
	// KILLING MUTATION (executed 2026-08-05): move `out.LastName = row.Name` back to the TOP of the loop
	// body, where it started. RED here — the error advertises `after="beta"`, and an operator who follows
	// that advice resumes PAST the only row that did not get re-keyed. It is then stranded on the old key
	// version forever, and the next run reports a clean single-version census that is simply false. That
	// census is what the operator uses to decide it is safe to retire the old key — so this off-by-one ends
	// in exactly the destruction the whole lane exists to prevent.
	if !strings.Contains(err.Error(), `after="alpha"`) {
		t.Fatalf("the failure reported %q — the resume cursor must name the last COMPLETED row (\"alpha\"), "+
			"never the row that failed, or resuming strands that credential on the old key version", err)
	}
	if !strings.Contains(err.Error(), "1 rewrapped") {
		t.Fatalf("the failure %q does not say how far the run got — this lane is built to be interrupted, "+
			"and an interruption that loses its own progress is not resumable", err)
	}
}
