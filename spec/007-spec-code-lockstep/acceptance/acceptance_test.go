package acceptance

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/governance"
	"github.com/territory-grounder/grounder/tools/specvalidate/lockstep"
)

// repoRoot is the repo root relative to this acceptance package.
const repoRoot = "../../.."

type lockEntry struct {
	Path   string `json:"path"`
	Spec   string `json:"spec"`
	SHA256 string `json:"sha256"`
}

func loadLock() []lockEntry {
	b, err := os.ReadFile(filepath.Join(repoRoot, "spec", ".lockstep.lock"))
	if err != nil {
		panic(fmt.Sprintf("read .lockstep.lock: %v", err))
	}
	var lf struct {
		Files []lockEntry `json:"files"`
	}
	if err := json.Unmarshal(b, &lf); err != nil {
		panic(fmt.Sprintf("parse .lockstep.lock: %v", err))
	}
	return lf.Files
}

// roleAuthority is an in-memory RBAC authority: a role may re-stamp iff it is in the permitted set.
type roleAuthority struct{ permitted map[string]bool }

func (a roleAuthority) MayRestamp(actorRole, _ string) bool { return a.permitted[actorRole] }

func TestSpecCodeLockstepAcceptance(t *testing.T) {
	suite := godog.TestSuite{
		Name:                "spec/007 spec-code-lockstep",
		ScenarioInitializer: initializeScenario,
		Options:             &godog.Options{Format: "pretty", Paths: []string{"."}, Tags: "~@pending", Strict: true, TestingT: t},
	}
	if suite.Run() != 0 {
		t.Fatal("spec/007 acceptance scenarios failed")
	}
}

type world struct {
	authority governance.RestampAuthority
	appr      governance.RestampApproval
	ledger    *audit.Ledger
	err       error
}

func initializeScenario(sc *godog.ScenarioContext) {
	w := &world{}

	// REQ-703 scenario 1 — an authorized, audited approval.
	sc.Step(`^a manifest re-stamp raised inside an RBAC-gated approval by a spec-owner role that updates the owning spec$`, func() error {
		w.ledger = audit.NewLedger()
		w.authority = roleAuthority{permitted: map[string]bool{"spec-owner": true}}
		w.appr = governance.RestampApproval{
			ActorRole:    "spec-owner",
			OwningSpec:   "007-spec-code-lockstep",
			ChangedPaths: []string{"core/governance/lockstep_restamp.go"},
			SpecUpdated:  true,
		}
		return nil
	})
	sc.Step(`^the re-stamp approval is applied$`, func() error {
		w.err = governance.AuthorizeRestamp(w.authority, w.appr, w.ledger)
		return nil
	})
	sc.Step(`^the recorded content hashes are accepted and an immutable re-stamp record is appended to the governance ledger$`, func() error {
		if w.err != nil {
			return fmt.Errorf("an authorized approval must be accepted, got %v", w.err)
		}
		if w.ledger.Len() != 1 {
			return fmt.Errorf("an authorized re-stamp must append exactly one ledger record, got %d", w.ledger.Len())
		}
		if w.ledger.Verify() != nil {
			return fmt.Errorf("the re-stamp record must be on a verifiable tamper-evident chain: %v", w.ledger.Verify())
		}
		return nil
	})

	// REQ-703 scenario 2 — an edit outside the approval flow (no permitted role, no ledger record).
	sc.Step(`^a manifest edited outside the RBAC-gated approval flow with no ledger record$`, func() error {
		w.ledger = audit.NewLedger()
		// The policy permits only the spec-owner role; a host-local edit carries no such authorization.
		w.authority = roleAuthority{permitted: map[string]bool{"spec-owner": true}}
		w.appr = governance.RestampApproval{
			ActorRole:    "host-local-edit", // not a permitted role → outside the approval flow
			OwningSpec:   "007-spec-code-lockstep",
			ChangedPaths: []string{"core/governance/lockstep_restamp.go"},
			SpecUpdated:  true,
		}
		return nil
	})
	sc.Step(`^the lockstep check runs in continuous integration$`, func() error {
		w.err = governance.AuthorizeRestamp(w.authority, w.appr, w.ledger)
		return nil
	})
	sc.Step(`^the re-stamp is rejected as spec drift and no ledger record exists for it$`, func() error {
		if !errors.Is(w.err, governance.ErrUnauthorizedRestamp) {
			return fmt.Errorf("an unauthorized re-stamp must be rejected as spec drift, got %v", w.err)
		}
		if w.ledger.Len() != 0 {
			return fmt.Errorf("a rejected re-stamp must leave NO ledger record, got %d", w.ledger.Len())
		}
		return nil
	})

	// ---- REQ-701/702/704: the content-aware hash mechanism (drives lockstep.HashSemantic + the real lock) ----
	lw := &lockstepWorld{}

	// pickGoverned loads a known governed Go file and its recorded hash from the real .lockstep.lock.
	pickGoverned := func() error {
		for _, e := range loadLock() {
			if e.Path == "core/safety/safety.go" {
				src, err := os.ReadFile(filepath.Join(repoRoot, e.Path))
				if err != nil {
					return err
				}
				lw.path, lw.src, lw.recorded, lw.spec = e.Path, src, e.SHA256, e.Spec
				return nil
			}
		}
		return fmt.Errorf("core/safety/safety.go is not in the lockstep manifest")
	}
	sc.Step(`^a governed safety-critical file bound to its owning spec$`, pickGoverned)
	sc.Step(`^a governed file whose stamped hash is recorded in the manifest$`, pickGoverned)
	sc.Step(`^a governed Go file whose stamped hash is recorded in the manifest$`, pickGoverned)

	sc.Step(`^the lockstep manifest is stamped$`, func() error {
		lw.computed = lockstep.HashSemantic(lw.path, lw.src)
		return nil
	})
	sc.Step(`^the manifest records a content hash for the file bound to that owning spec$`, func() error {
		if lw.recorded == "" || lw.spec == "" {
			return fmt.Errorf("the manifest must record a hash + owning spec, got hash=%q spec=%q", lw.recorded, lw.spec)
		}
		if lw.computed != lw.recorded {
			return fmt.Errorf("the recorded hash must equal the content hash of the bound file")
		}
		return nil
	})

	// REQ-702: a governed file changed without its spec ⇒ drift (failing check).
	sc.Step(`^the file's executable content changes but its owning spec does not$`, func() error {
		lw.changed = lockstep.HashSemantic(lw.path, append([]byte("package safety\nvar injected = 1\n"), lw.src...))
		return nil
	})
	sc.Step(`^the lockstep check reports spec drift and exits with a failing status$`, func() error {
		if lw.changed == lw.recorded {
			return fmt.Errorf("a semantic content change must diverge from the recorded hash (drift)")
		}
		return nil
	})

	// REQ-702 coverage invariant: no governed safety-critical file is excluded from the manifest.
	sc.Step(`^the set of governed safety-critical files for the classifier, prediction gate, verifier, suppression chain, actuation interceptor, ledger, and schema$`, func() error {
		lw.mandatory = []string{
			"core/risk/classifier.go", "core/predict/gate.go", "core/verify/verdict.go",
			"core/suppression/chain.go", "core/actuate/interceptor.go", "core/audit/ledger.go", "core/schema/version.go",
		}
		return nil
	})
	sc.Step(`^the coverage invariant runs against the lockstep manifest$`, func() error {
		inLock := map[string]string{}
		for _, e := range loadLock() {
			inLock[e.Path] = e.Spec
		}
		for _, m := range lw.mandatory {
			if inLock[m] == "" {
				lw.missing = append(lw.missing, m)
			}
		}
		return nil
	})
	sc.Step(`^every governed safety-critical file is present in the manifest bound to an existing spec$`, func() error {
		if len(lw.missing) > 0 {
			return fmt.Errorf("governed safety-critical files missing from the manifest: %v", lw.missing)
		}
		return nil
	})

	// REQ-704: a comment-only edit is NOT drift; a semantic token change IS drift (comment-insensitive hash).
	sc.Step(`^only comments and formatting change while the executable tokens are unchanged$`, func() error {
		commented := append([]byte("// a fresh cosmetic comment\n\n\t"), lw.src...)
		lw.commentEdit = lockstep.HashSemantic(lw.path, commented)
		return nil
	})
	sc.Step(`^the recomputed comment-insensitive hash equals the stamped hash and no drift is reported$`, func() error {
		if lw.commentEdit != lockstep.HashSemantic(lw.path, lw.src) {
			return fmt.Errorf("a comment/format-only edit must NOT change the semantic hash (REQ-704)")
		}
		return nil
	})
	sc.Step(`^an executable token in the file changes$`, func() error {
		swapped := []byte("package safety_renamed" + string(lw.src[len("package safety"):]))
		lw.tokenEdit = lockstep.HashSemantic(lw.path, swapped)
		return nil
	})
	sc.Step(`^the recomputed comment-insensitive hash differs from the stamped hash and drift is reported$`, func() error {
		if lw.tokenEdit == lockstep.HashSemantic(lw.path, lw.src) {
			return fmt.Errorf("changing an executable token MUST change the semantic hash (REQ-704)")
		}
		return nil
	})
}

type lockstepWorld struct {
	path, spec, recorded string
	src                  []byte
	computed, changed    string
	commentEdit          string
	tokenEdit            string
	mandatory            []string
	missing              []string
}
