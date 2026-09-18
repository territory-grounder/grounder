package manifest

import (
	"errors"
	"github.com/territory-grounder/grounder/core/fuzzcorpus"
	"testing"
)

// mutateActionField changes EXACTLY ONE field of a per sel and reports the field name and whether the value
// actually changed. It deep-copies Params first so the caller's Action is never touched — the fuzz compares
// the mutated identity against the original. Every field of Action is folded into the content hash
// (canonicalJSON marshals all of them); the fuzz asserts each is therefore reflected in the id.
func mutateActionField(a Action, sel uint8, newVal, key string) (Action, string, bool) {
	p := make(map[string]string, len(a.Params)+1)
	for k, v := range a.Params {
		p[k] = v
	}
	a.Params = p
	switch sel % 5 {
	case 0:
		old := a.Target
		a.Target = newVal
		return a, "Target", a.Target != old
	case 1:
		old := a.OpClass
		a.OpClass = newVal
		return a, "OpClass", a.OpClass != old
	case 2:
		old := a.Op
		a.Op = newVal
		return a, "Op", a.Op != old
	case 3:
		old, had := a.Params[key]
		a.Params[key] = newVal
		return a, "Params[" + key + "]", !had || a.Params[key] != old
	default: // case 4
		old := a.Reversible
		a.Reversible = !a.Reversible // a flip always changes the value
		return a, "Reversible", a.Reversible != old
	}
}

// FuzzActionID drives the action content-hash — the INV-07 identity threaded unchanged through
// predict → approve → execute → verify, and the value Assert / the manifest chain trust to mean "the same
// action" (TG-5 Phase 4). manifest_test.go pins determinism and per-field sensitivity on ONE sampleAction
// with a fixed mutation list; this generalizes both to arbitrary content and every field. Three properties:
//
//   - TOTAL: ID() never errors for a valid Action — canonicalization must not have inputs it cannot encode
//     (a canonicalize error on the hot path would fail an action that should have an identity).
//   - DETERMINISTIC: the same action (and an independently built equal action with a fresh Params map) yield
//     the SAME id — else the identity threaded across the lifecycle would drift between stages.
//   - SENSITIVE (no COLLISION): a real change to ANY field changes the id. A collision is the dangerous
//     direction — it would let an attacker present a DIFFERENT action that Assert accepts as the approved one
//     (INV-07 substitution). The mirror property — a no-op must NOT change the id — catches non-determinism.
//
// The fuzzer fails only on: a valid action whose ID drifts on equal input, a collision (changed field, same
// id), a phantom change (no-op, different id), or an INVALID-UTF-8 action that does not fail closed with
// ErrNonCanonicalAction (TG-528 — json.Marshal would collapse distinct invalid bytes to U+FFFD and share an
// id, so ID() refuses instead). Runs the seed corpus in CI; drives wide with `go test -fuzz=FuzzActionID ./core/manifest`.
func FuzzActionID(f *testing.F) {
	// seeds: target, opClass, op, paramKey, paramVal, reversible, fieldSel, newVal
	f.Add("web01", "restart-service", "restart", "graceful", "true", true, uint8(0), "web02")
	f.Add("", "", "", "", "", false, uint8(4), "")                                                // all-empty; flip Reversible
	f.Add("host-a", "kubectl-get", "get", "namespace", "default", false, uint8(3), "kube-system") // mutate a param value
	f.Add(`a"b`, "op<class>", "o&p", "k\ty", "v\nw", true, uint8(1), "other")                     // JSON metacharacters / escaping stress
	f.Add("t", "c", "o", "k", "v", false, uint8(2), "o")                                          // no-op: newVal equals Op
	f.Add("同じ", "クラス", "操作", "キー", "値", true, uint8(0), "同じ")                                     // multibyte, no-op on Target

	for _, h := range fuzzcorpus.Strings() {
		f.Add(h, "restart-service", h, "unit", h, true, uint8(0), h) // the shared §3.2 battery on the content fields
	}
	f.Fuzz(func(t *testing.T, target, opClass, op, pk, pv string, rev bool, fieldSel uint8, newVal string) {
		a := Action{Target: target, OpClass: opClass, Op: op, Params: map[string]string{pk: pv}, Reversible: rev}

		id0, err := a.ID()

		// FAIL CLOSED on invalid UTF-8 (TG-528): json.Marshal collapses every invalid byte to U+FFFD, which
		// would make two distinct invalid-byte actions share an id. ID() refuses instead — no id, so the
		// action can never be gated or sealed. Real actions come from ParseProposal's json decode and are
		// always valid UTF-8; this asserts the in-memory-bypass case is CLOSED, not merely skipped.
		if !canonicalUTF8(a) {
			if !errors.Is(err, ErrNonCanonicalAction) {
				t.Fatalf("an invalid-UTF-8 action must fail closed with ErrNonCanonicalAction, got id=%q err=%v", id0, err)
			}
			return
		}

		// Valid-UTF-8 domain: ID is total, deterministic, injective.
		if err != nil {
			t.Fatalf("Action.ID must be total for a valid Action: %v\naction=%+v", err, a)
		}
		if id0 == "" {
			t.Fatalf("Action.ID returned an empty id with no error: %+v", a)
		}

		// determinism — twice on the same value, and across an independently built equal action (fresh map).
		if id0b, _ := a.ID(); id0b != id0 {
			t.Fatalf("Action.ID not deterministic on the same value: %q vs %q", id0, id0b)
		}
		equal := Action{Target: target, OpClass: opClass, Op: op, Params: map[string]string{pk: pv}, Reversible: rev}
		if idEq, _ := equal.ID(); idEq != id0 {
			t.Fatalf("Action.ID not stable across an EQUAL action (fresh Params map): %q vs %q\n%+v", id0, idEq, a)
		}

		// sensitivity — a real change to any field must change the identity; a no-op must not. A mutation that
		// introduces invalid UTF-8 (via newVal) must itself fail closed.
		m, field, changed := mutateActionField(a, fieldSel, newVal, pk)
		id1, err1 := m.ID()
		if !canonicalUTF8(m) {
			if !errors.Is(err1, ErrNonCanonicalAction) {
				t.Fatalf("a mutation to invalid UTF-8 (%s) must fail closed, got id=%q err=%v", field, id1, err1)
			}
			return
		}
		if err1 != nil {
			t.Fatalf("mutated valid Action.ID errored: %v", err1)
		}
		if changed && id1 == id0 {
			t.Fatalf("COLLISION (INV-07): changed %s but action_id is UNCHANGED — a different action shares the identity Assert trusts:\n base=%+v\n  mut=%+v\n   id=%s", field, a, m, id0)
		}
		if !changed && id1 != id0 {
			t.Fatalf("PHANTOM change: a no-op on %s altered the action_id (non-determinism): %q -> %q", field, id0, id1)
		}
	})
}
