package credential

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

// A GUARD WHOSE CONTROL SAMPLES ITS OWN FIELDS IS A GUARD NOBODY IS CHECKING.
//
// sameIdentity decides whether two equal-specificity rules name ONE principal or an AMBIGUITY. Get it wrong
// in the permissive direction and two different principals collapse into one, and the resolver hands out a
// key for an identity the operator did not intend — which is the whole reason this function exists.
//
// The pre-existing mutation control opens with "Every field that decides WHO connects and WITH WHAT must
// break equivalence" and then varies TWO: user and key. Measured by deleting each comparison in turn and
// running the whole core/credential suite:
//
//	a.Port()        == b.Port()        -> deletable, suite GREEN
//	a.Scheme()      == b.Scheme()      -> deletable, suite GREEN
//	a.APITokenRef() == b.APITokenRef() -> deletable, suite GREEN
//	a.BecomeRef()   == b.BecomeRef()   -> caught
//
// Three of six comparisons in a safety guard could be deleted with everything green.
//
// ADDING THREE MORE CASES WOULD NOT FIX THIS. A hand-written list of six is a sample that happens to be
// complete today; the seventh field added to Bundle arrives uncovered and silent, which is exactly how the
// list got to two. So the set is DERIVED FROM THE FUNCTION ITSELF: parse sameIdentity's source, extract
// every `a.X() == b.X()` comparison, and require a mutation case for each X. A comparison added without a
// case fails this test; a case for a comparison that no longer exists fails it too.
func TestSameIdentityControlCoversEveryComparisonInTheFunction(t *testing.T) {
	compared := comparisonsInSameIdentity(t)
	if len(compared) < 4 {
		t.Fatalf("parsed only %v from sameIdentity — the extractor is broken, so this test proves nothing", compared)
	}

	// One mutation per identity accessor: a spec that differs ONLY in that field. If sameIdentity stops
	// comparing it, the two bundles compare equal and the case fails.
	base := BundleSpec{User: "root", Port: 22, Scheme: SchemeSSH, SSHKeyRef: config.SecretRef("file:/k"), Become: config.SecretRef("file:/b")}
	cases := map[string]BundleSpec{
		"User":        {User: "other", Port: 22, Scheme: SchemeSSH, SSHKeyRef: config.SecretRef("file:/k"), Become: config.SecretRef("file:/b")},
		"Port":        {User: "root", Port: 2222, Scheme: SchemeSSH, SSHKeyRef: config.SecretRef("file:/k"), Become: config.SecretRef("file:/b")},
		"Scheme":      {User: "root", Port: 22, Scheme: SchemeAPI, APITokenRef: config.SecretRef("file:/t"), Become: config.SecretRef("file:/b")},
		"SSHKeyRef":   {User: "root", Port: 22, Scheme: SchemeSSH, SSHKeyRef: config.SecretRef("file:/other"), Become: config.SecretRef("file:/b")},
		"APITokenRef": {User: "root", Port: 22, Scheme: SchemeAPI, APITokenRef: config.SecretRef("file:/t2"), Become: config.SecretRef("file:/b")},
		"BecomeRef":   {User: "root", Port: 22, Scheme: SchemeSSH, SSHKeyRef: config.SecretRef("file:/k"), Become: config.SecretRef("file:/other")},
	}

	// THE CLOSED-SET CHECK, both directions. This is the half that survives a new field being added.
	var missing, stale []string
	for _, acc := range compared {
		if _, ok := cases[acc]; !ok {
			missing = append(missing, acc)
		}
	}
	for acc := range cases {
		found := false
		for _, c := range compared {
			if c == acc {
				found = true
				break
			}
		}
		if !found {
			stale = append(stale, acc)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 {
		t.Errorf("sameIdentity compares %v with NO mutation case — a field that decides who connects is "+
			"unguarded, and deleting its comparison would leave this suite green", missing)
	}
	if len(stale) > 0 {
		t.Errorf("mutation cases %v name comparisons sameIdentity no longer makes — the control is testing "+
			"a function that has moved on", stale)
	}

	// And the behaviour: every case must actually break equivalence.
	baseB, err := NewBundle(base)
	if err != nil {
		t.Fatalf("base bundle: %v", err)
	}
	// The APITokenRef and Scheme cases need an api-scheme base to be a one-field delta from something valid;
	// compare them against an api base instead of forcing an invalid ssh+token hybrid.
	apiBase, err := NewBundle(BundleSpec{User: "root", Port: 22, Scheme: SchemeAPI, APITokenRef: config.SecretRef("file:/t"), Become: config.SecretRef("file:/b")})
	if err != nil {
		t.Fatalf("api base bundle: %v", err)
	}
	if !sameIdentity(baseB, baseB) || !sameIdentity(apiBase, apiBase) {
		t.Fatal("a bundle does not compare equal to itself — the fix never engages")
	}
	for name, spec := range cases {
		other, err := NewBundle(spec)
		if err != nil {
			t.Fatalf("%s bundle: %v", name, err)
		}
		against := baseB
		if name == "APITokenRef" {
			against = apiBase // one-field delta within the api scheme
		}
		// ★ SCHEME CANNOT BE ISOLATED, AND THAT IS A TYPE INVARIANT, NOT A HOLE HERE. NewBundle requires
		// SSHKeyRef for scheme ssh and APITokenRef for scheme api, so no valid pair differs ONLY in scheme —
		// the Scheme case necessarily varies its key ref too. Deleting the scheme comparison is therefore
		// caught by the CLOSED-SET half above (the case goes stale), not by this behavioural loop. Do not
		// "fix" that by loosening NewBundle; the guard is stronger with the invariant than without it.
		if sameIdentity(against, other) {
			t.Errorf("%s differs but compared EQUAL — two different principals collapse into one and the "+
				"resolver hands out a key for an identity the operator did not name", name)
		}
	}
}

// comparisonsInSameIdentity parses this package's own source and returns the accessor name of every
// `a.X() == b.X()` comparison inside sameIdentity. Deriving the set from the artifact — rather than from a
// list a human keeps in step — is the only version of this check that survives a seventh field.
func comparisonsInSameIdentity(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "precedence.go", nil, 0)
	if err != nil {
		t.Fatalf("parse precedence.go: %v", err)
	}
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "sameIdentity" {
			fn = fd
			break
		}
	}
	if fn == nil {
		t.Fatal("sameIdentity not found in precedence.go — it was renamed or moved, and this control is " +
			"silently testing nothing")
	}
	seen := map[string]bool{}
	ast.Inspect(fn, func(n ast.Node) bool {
		be, ok := n.(*ast.BinaryExpr)
		if !ok || be.Op != token.EQL {
			return true
		}
		l, lok := accessorName(be.X)
		r, rok := accessorName(be.Y)
		if lok && rok && l == r {
			seen[l] = true
		}
		return true
	})
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// accessorName pulls "Port" out of `a.Port()`. Anything else (a field, a call with args, a literal) is not
// an identity accessor and is ignored.
func accessorName(e ast.Expr) (string, bool) {
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	if id, ok := sel.X.(*ast.Ident); !ok || (id.Name != "a" && id.Name != "b") {
		return "", false
	}
	name := sel.Sel.Name
	if name == "Valid" || strings.HasPrefix(name, "String") {
		return "", false // not an identity field
	}
	return name, true
}
