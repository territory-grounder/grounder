package gitopsmr

// TG-122 slice 1 oracles: the gitops-mr actuator's SAFETY structure — the mode gate, the repo allowlist, the
// op-class confused-deputy binding, the secret-value guard, the empty/≠1-edit refusals, and the exactly-once
// open — exercised via a fake Renderer + fake Opener (the concrete hclwrite/kyaml renderer + the live GitLab
// two-REST-call client are follow-ons within slice 1; this pins the safety leaf first). Mirrors awxjob_test.

import (
	"context"
	"errors"
	"testing"

	"github.com/territory-grounder/grounder/core/safety"
)

type fakeRenderer struct {
	files map[string][]byte
	err   error
}

func (f fakeRenderer) Render(RepoPolicy, []FieldEdit) (map[string][]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.files, nil
}

type fakeOpener struct {
	calls  int
	opened OpenedMR
	err    error
}

func (f *fakeOpener) OpenMR(_ context.Context, _ RepoPolicy, branch, _, _ string, _ map[string][]byte) (OpenedMR, error) {
	f.calls++
	if f.err != nil {
		return OpenedMR{}, f.err
	}
	o := f.opened
	o.Branch = branch
	return o, nil
}

func allowlist() RepoAllowlist {
	return RepoAllowlist{"7": {
		BaseURL: "https://gitlab.example", ProjectPath: "infrastructure/example/production",
		TargetBranch: "main", BranchPrefix: "tg/", OpClass: "helm-values-set",
		FieldRules: []FieldRule{{RuleID: "replicas", File: "k8s/_core/foo/main.tf", Selector: "helm_release.foo.set.replicas"}},
	}}
}

func okSpec() ProposeSpec {
	return ProposeSpec{RepoID: "7", OpClass: "helm-values-set",
		Edits: []FieldEdit{{FieldRuleID: "replicas", NewValue: "3"}}, Rationale: "scale up for load"}
}

func mustEncode(t *testing.T, spec ProposeSpec) []byte {
	t.Helper()
	_, stdin, err := EncodePropose(spec)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return stdin
}

// an actuator whose mode gate PERMITS actuation (test-only chokepoint) with a real renderer/opener.
func actuating(t *testing.T, rend Renderer, op Opener) *Actuator {
	t.Helper()
	a, err := New(Config{Opener: op, Renderer: rend, Allowlist: allowlist(), ModeGate: safety.NewActuatingChokepoint()})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return a
}

// TestExecOpensExactlyOneMRAndReturnsTheHandle: a well-formed propose renders + opens (the Opener performs the
// two-REST-call open exactly once) and returns the async MR handle; no success is declared inline.
func TestExecOpensExactlyOneMRAndReturnsTheHandle(t *testing.T) {
	op := &fakeOpener{opened: OpenedMR{Handle: "7!42", RepoID: "7", IID: 42}}
	rend := fakeRenderer{files: map[string][]byte{"k8s/_core/foo/main.tf": []byte("replicas = 3\n")}}
	res, err := actuating(t, rend, op).Exec(context.Background(), []string{ProposeVerb}, mustEncode(t, okSpec()))
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if op.calls != 1 {
		t.Fatalf("opener called %d times, want exactly 1 (open then STOP)", op.calls)
	}
	opened, err := DecodeOpened(res.Stdout)
	if err != nil {
		t.Fatalf("decode handle: %v", err)
	}
	if opened.Handle != "7!42" {
		t.Fatalf("handle=%q, want 7!42", opened.Handle)
	}
}

// TestExecStampsRepoIdentityOntoTheHandle: the REAL Opener yields only iid/branch/url — it does NOT hold the
// allowlist key — so the Actuator must stamp RepoID + Handle=<repoID>!<iid> from the spec after the open. This
// fake returns the real httpOpener's shape (iid only, EMPTY RepoID/Handle), so the assertion fails if the
// stamping is removed — the oracle the pre-baked {RepoID,Handle} fixture in the test above cannot give.
//
// KILLING MUTATION: delete the `opened.RepoID = …` / `opened.Handle = MakeHandle(…)` lines in Exec → RepoID and
// Handle come back empty → RED.
func TestExecStampsRepoIdentityOntoTheHandle(t *testing.T) {
	op := &fakeOpener{opened: OpenedMR{IID: 42}} // real-opener shape: iid only, no repo identity
	rend := fakeRenderer{files: map[string][]byte{"k8s/_core/foo/main.tf": []byte("replicas = 3\n")}}
	res, err := actuating(t, rend, op).Exec(context.Background(), []string{ProposeVerb}, mustEncode(t, okSpec()))
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	opened, err := DecodeOpened(res.Stdout)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if opened.RepoID != "7" || opened.Handle != "7!42" {
		t.Fatalf("the Actuator must stamp repo identity from the spec: RepoID=%q Handle=%q, want 7 / 7!42", opened.RepoID, opened.Handle)
	}
	// ...and the deferred-verify poller can split back exactly what the actuator produced.
	if repo, iid, err := SplitHandle(opened.Handle); err != nil || repo != "7" || iid != 42 {
		t.Fatalf("stamped handle not splittable by the poller: repo=%q iid=%d err=%v", repo, iid, err)
	}
}

// TestExecFailsClosedWithoutAGate: a nil mode gate is a read-only actuator — Exec refuses before any open
// (mutation OFF). The Opener is never called.
func TestExecFailsClosedWithoutAGate(t *testing.T) {
	op := &fakeOpener{}
	a, err := New(Config{Opener: op, Renderer: fakeRenderer{files: map[string][]byte{"f": []byte("x")}}, Allowlist: allowlist()})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if !a.ReadOnly() {
		t.Fatal("a nil-gate actuator must be ReadOnly")
	}
	if _, err := a.Exec(context.Background(), []string{ProposeVerb}, mustEncode(t, okSpec())); !errors.Is(err, ErrOpenGateClosed) {
		t.Fatalf("no-gate Exec err=%v, want ErrOpenGateClosed", err)
	}
	if op.calls != 0 {
		t.Fatalf("the opener must NOT be called when the gate is closed, got %d", op.calls)
	}
}

func TestExecRefusals(t *testing.T) {
	goodRend := fakeRenderer{files: map[string][]byte{"k8s/_core/foo/main.tf": []byte("replicas = 3\n")}}
	cases := []struct {
		name string
		rend Renderer
		argv []string
		spec ProposeSpec
		want error
	}{
		{"non-propose argv", goodRend, []string{"rm", "-rf"}, okSpec(), ErrNotProposeArgv},
		{"non-allowlisted repo", goodRend, []string{ProposeVerb}, func() ProposeSpec { s := okSpec(); s.RepoID = "999"; return s }(), ErrRepoNotAllowlisted},
		{"op-class mismatch", goodRend, []string{ProposeVerb}, func() ProposeSpec { s := okSpec(); s.OpClass = "some-other-class"; return s }(), ErrOpClassMismatch},
		{"secret value in rendered patch", fakeRenderer{files: map[string][]byte{"k8s/x.tf": []byte("token = \"glpat-AAAABBBBCCCCDDDDEEEE\"\n")}}, []string{ProposeVerb}, okSpec(), ErrSecretInPatch},
		{"renderer produced no file", fakeRenderer{files: map[string][]byte{}}, []string{ProposeVerb}, okSpec(), ErrNoEdits},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			op := &fakeOpener{opened: OpenedMR{Handle: "7!1"}}
			// build stdin directly (some cases carry a spec EncodePropose would reject, e.g. mismatch is fine;
			// all okSpec-derived specs encode cleanly here).
			_, stdin, err := EncodePropose(tc.spec)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			_, err = actuating(t, tc.rend, op).Exec(context.Background(), tc.argv, stdin)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Exec err=%v, want %v", err, tc.want)
			}
			if op.calls != 0 {
				t.Fatalf("a refused propose must NOT open an MR, got %d open(s)", op.calls)
			}
		})
	}
}

// TestEncodeProposeRefusesEmpty: the encoder fails closed on an empty repo or zero edits (an empty MR is never
// opened).
func TestEncodeProposeRefusesEmpty(t *testing.T) {
	if _, _, err := EncodePropose(ProposeSpec{OpClass: "x", Edits: []FieldEdit{{FieldRuleID: "r", NewValue: "1"}}}); !errors.Is(err, ErrBadProposeSpec) {
		t.Fatalf("empty repo_id err=%v, want ErrBadProposeSpec", err)
	}
	if _, _, err := EncodePropose(ProposeSpec{RepoID: "7", OpClass: "x"}); !errors.Is(err, ErrNoEdits) {
		t.Fatalf("zero edits err=%v, want ErrNoEdits", err)
	}
}

// TestSecretGuardCatchesRationale: the guard scans the rationale too, not only the rendered files.
func TestSecretGuardCatchesRationale(t *testing.T) {
	op := &fakeOpener{opened: OpenedMR{Handle: "7!2"}}
	rend := fakeRenderer{files: map[string][]byte{"k8s/x.tf": []byte("replicas = 3\n")}}
	spec := okSpec()
	spec.Rationale = "rotating creds, new token glpat-AAAABBBBCCCCDDDDEEEE oops"
	if _, err := actuating(t, rend, op).Exec(context.Background(), []string{ProposeVerb}, mustEncode(t, spec)); !errors.Is(err, ErrSecretInPatch) {
		t.Fatalf("a secret in the rationale must refuse; err=%v, want ErrSecretInPatch", err)
	}
	if op.calls != 0 {
		t.Fatalf("a secret refusal must NOT open an MR, got %d", op.calls)
	}
}
