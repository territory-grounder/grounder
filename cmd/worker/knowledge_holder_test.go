package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

// silent discards the loader's log lines; these oracles assert the RETURNED standing, not the prose.
func silent(string, ...any) {}

// THE PROPERTY. An unreadable corpus must yield NO holder, so the composition root takes its else arm:
// deps.PriorIncidents stays nil, novelty reports UNKNOWN, and the operator gets the WARNING.
//
// The composition root called knowledge.NewHolder(loadCorpus()) directly, and NewHolder replaces a nil
// retriever with an EMPTY one so Retrieve never dereferences nil. Correct for the Holder; catastrophic
// here, because it launders "could not be read" into "is empty" — the distinction loadKnowledgeCorpus's
// own doc says must never be masked behind an empty-but-armed retriever.
//
// KILLING MUTATION: in newKnowledgeHolder, replace the nil return with knowledge.NewHolder(r). RED.
func TestNewKnowledgeHolderIsNilOnAnUnreadableCorpus(t *testing.T) {
	dir := t.TempDir()
	corpus := filepath.Join(dir, "corpus.json")
	// A torn write: valid file, invalid JSON. This is the realistic production failure — the worker
	// itself writes this file (novelty writeback, lessons merge, decay prune).
	if err := os.WriteFile(corpus, []byte(`[{"external_ref":"a"`), 0o600); err != nil {
		t.Fatal(err)
	}
	if h := newKnowledgeHolder("", corpus, silent); h != nil {
		t.Fatal("a torn corpus produced an armed holder. deps.PriorIncidents is then wired and answers " +
			"(0, true) — count zero, positively KNOWN — for EVERY (host, alert_rule) in the estate, so " +
			"runner.novelIncident marks every incident novel and forces the first-sight-human poll " +
			"fleet-wide, while the WARNING written to announce the disabled gate cannot print")
	}
}

// The wholly-absent corpus with no seed is the other nil case: nothing to rank, and nothing to claim.
func TestNewKnowledgeHolderIsNilWhenThereIsNoCorpusAtAll(t *testing.T) {
	dir := t.TempDir()
	if h := newKnowledgeHolder("", filepath.Join(dir, "absent.json"), silent); h != nil {
		t.Fatal("an absent corpus with no seed produced an armed holder — the novelty gate would report " +
			"'never seen' with confidence about an estate it has no record of")
	}
}

// The counterweight, and the reason this cannot simply return nil more often: a corpus that loads must
// produce a WORKING holder, and a genuinely empty-but-READABLE corpus is a legitimate armed state.
func TestNewKnowledgeHolderArmsOnAReadableCorpus(t *testing.T) {
	dir := t.TempDir()
	corpus := filepath.Join(dir, "corpus.json")
	if err := os.WriteFile(corpus, []byte(`[
	  {"external_ref":"INC-1","host":"web01","alert_rule":"Device-Down","resolution":"restarted the guest"}
	]`), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newKnowledgeHolder("", corpus, silent)
	if h == nil {
		t.Fatal("a readable corpus produced NO holder — the retrieval plane and the novelty gate are both " +
			"dark over a corpus that parsed fine")
	}
	if got := h.Count("web01", "Device-Down"); got != 1 {
		t.Fatalf("the loaded corpus does not answer for its own row: Count = %d, want 1", got)
	}
	// A key with no precedent must read 0 over a LOADED corpus — that is the real novelty signal, and it
	// is exactly the answer the unreadable case must not be able to give.
	if got := h.Count("web02", "Device-Down"); got != 0 {
		t.Fatalf("an unseen key reported %d prior incidents", got)
	}

	// A readable file holding an empty array is an honestly empty corpus: armed, and answering 0.
	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, []byte(`[]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if h := newKnowledgeHolder("", empty, silent); h == nil {
		t.Fatal("an empty-but-READABLE corpus produced no holder; empty is a fact, unreadable is an outage, " +
			"and collapsing them in the other direction is the same defect mirrored")
	}
}

// A fresh box — maintained corpus not yet written, seed present — must stay ARMED from the seed. This is
// the case the seed/maintained split exists for, and a nil-return that swallowed it would silently
// disable novelty on every new deployment.
func TestNewKnowledgeHolderArmsFromTheSeedOnAFreshBox(t *testing.T) {
	dir := t.TempDir()
	seed := filepath.Join(dir, "corpus.seed.json")
	if err := os.WriteFile(seed, []byte(`[
	  {"external_ref":"SEED-1","host":"web01","alert_rule":"Device-Down","resolution":"seeded"}
	]`), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newKnowledgeHolder(seed, filepath.Join(dir, "not-written-yet.json"), silent)
	if h == nil {
		t.Fatal("a fresh box with a seed produced no holder — the novelty gate would be dark on every new " +
			"deployment, which is precisely what the seed/maintained split exists to prevent")
	}
	if got := h.Count("web01", "Device-Down"); got != 1 {
		t.Fatalf("the seed is not visible to the novelty gate: Count = %d, want 1", got)
	}
}

// TestCompositionRootDoesNotBypassTheKnowledgeHolderGuard closes the hole the unit tests above cannot see.
//
// Those tests exercise newKnowledgeHolder directly, so reverting the composition root to
// knowledge.NewHolder(loadCorpus()) leaves them GREEN while restoring the defect in full. That was
// confirmed by running the mutation: it survived every unit oracle. What has to be pinned is that the
// root goes through the guard.
//
// knowledge.NewHolder is not wrong — it is correct for its own contract, and NewHolder(nil) returning an
// empty retriever is what keeps Retrieve from dereferencing nil. It is wrong HERE, at the one call site
// that decides whether the novelty gate is armed, because at that site a nil retriever means "the corpus
// could not be read" and an empty one means "the estate has no precedent". Only one of those is safe to
// act on.
//
// KILLING MUTATION: replace newKnowledgeHolder(...) with knowledge.NewHolder(loadCorpus()) in main.go. RED.
func TestCompositionRootDoesNotBypassTheKnowledgeHolderGuard(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var direct []int
	guarded, totalCalls := false, 0
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		totalCalls++
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			if x, ok := fn.X.(*ast.Ident); ok && x.Name == "knowledge" && fn.Sel.Name == "NewHolder" {
				direct = append(direct, fset.Position(call.Pos()).Line)
			}
		case *ast.Ident:
			if fn.Name == "newKnowledgeHolder" {
				guarded = true
			}
		}
		return true
	})

	// Vacuity floor: a walk that found nothing certifies nothing.
	if totalCalls < 10 {
		t.Fatalf("vacuity floor: the AST walk found only %d call sites in main.go — the matcher is broken", totalCalls)
	}
	if !guarded {
		t.Error("cmd/worker/main.go never calls newKnowledgeHolder — the corpus standing is decided " +
			"somewhere that cannot distinguish an unreadable corpus from an empty one")
	}
	for _, line := range direct {
		t.Errorf("cmd/worker/main.go:%d calls knowledge.NewHolder directly. NewHolder replaces a nil "+
			"retriever with an EMPTY one, which launders 'the corpus could not be read' into 'the estate "+
			"has no precedent': deps.PriorIncidents is then wired and answers (0, true) for every "+
			"(host, alert_rule), so every incident reads as novel and polls a human, fleet-wide. Use "+
			"newKnowledgeHolder.", line)
	}
}
