// Package deploy hosts the compose/env parity guard. It is a test-only package (no production code
// ships from here) whose single job is to fail CI when a binary reads an environment variable that
// deploy/docker-compose.yml never forwards to that binary's service.
//
// WHY THIS EXISTS (the recurring prod-gap class it kills): a Go binary reads TG_FOO via the getenv
// family, but the compose `environment:` block for its service never passes TG_FOO. Tests stay green
// (nothing exercises compose), CI ships, and in prod the feature silently runs on its in-code default.
// This has shipped three times (LibreNMS deployments on the grounder, per-site LibreNMS tokens, the
// TG_SKILL_* flywheel surface). This guard turns that latent gap into a deterministic CI failure.
//
// It is pure-stdlib and does only file reads (no network, no DB), so it is CI-runnable and deterministic.
// It lives in a NON-governed path (deploy/) on purpose: the two binaries are lockstep-governed
// (spec/012 governs cmd/worker/main.go), so the guard must not sit under a governed path or a spec
// re-stamp would be required just to add a test.
package deploy

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// service <-> binary source. Each service's compose `environment:` block MUST forward every env key its
// binary reads. These are the only two services that run a TG binary; the rest are off-the-shelf images.
type parityTarget struct {
	service string // docker-compose.yml service name
	goFile  string // repo-relative path to that service's composition-root file
	minKeys int    // vacuity floor for THIS file (0 ⇒ the default main.go-sized floor of 5)
}

var parityTargets = []parityTarget{
	{service: "worker", goFile: "cmd/worker/main.go"},
	{service: "grounder", goFile: "cmd/grounder/main.go"},
	// TG-160. The worker's composition root is no longer one file: cmd/worker/egress.go reads
	// TG_EGRESS_ALLOW / TG_EGRESS_MODE and the guard could not see them, so the exact gap this file exists
	// to kill was reachable again — an operator sets TG_EGRESS_ALLOW in .env, compose never forwards it,
	// the allowlist silently stays as-derived, and everything looks configured. Any future boot-time knob
	// that lives outside main.go belongs here for the same reason.
	// Two knobs, so the floor is 2: a floor of 5 would fail a correct file, and lowering the SHARED floor
	// to 2 would weaken the guard-of-the-guard on the two main.go files, where a parse that returned three
	// keys really would be broken. The floor is per-file for that reason.
	{service: "worker", goFile: "cmd/worker/egress.go", minKeys: 2},
	// The TG-81 b3 verdict-provenance wiring reads its two keys outside main.go (the TG-501 ratchet);
	// parity must still prove compose forwards them.
	{service: "worker", goFile: "cmd/worker/verdictsig_wire.go", minKeys: 2},
	// The TG-80 P2-6 kill-terminal flip is read in the runner's activity file, outside main.go —
	// parity must still prove compose forwards it, or arming the flip on the box would be a no-op.
	{service: "worker", goFile: "temporal/runner/screen_kill.go", minKeys: 1},
	// TG-86 slice 1c. The estate-doc GROUNDING coverage job is a THIRD worker composition-root file outside
	// main.go: cmd/worker/estate_doc_coverage.go reads TG_ESTATE_DOCS_DIR and TG_ESTATE_DOC_CORPUS via getenv
	// at boot. Until this entry, the parity guard never parsed that file, so those two reads were invisible —
	// and the compose worker service forwarded NEITHER, leaving the arming oracle (the tg_estate_doc_files
	// gauge) stuck on a blank default: the exact latent prod-gap this guard exists to turn into a CI failure.
	// Two keys, so the floor is 2 (mirrors egress.go above; a floor of 5 would fail a correct file).
	{service: "worker", goFile: "cmd/worker/estate_doc_coverage.go", minKeys: 2},
	// spec/016 T-016-5: the authn-compose arming flip is read in its wire file (the TG-501 ratchet) —
	// parity must still prove compose forwards it, or arming the 4d2 gate on the box would be a no-op.
	{service: "worker", goFile: "cmd/worker/authn_compose_wiring.go", minKeys: 1},
	// spec/023 T-023-9: the k8s audit reader's allowlist is read in its wire file — parity must prove
	// compose forwards it, or declaring control planes in .env would silently register nothing.
	{service: "worker", goFile: "cmd/worker/k8saudit_wiring.go", minKeys: 1},
	// The SECRET-SUBSTRATE bootstrap moved out of main.go (TG-501 ratchet): its six OpenBao keys and the
	// three Vaultwarden ones are the process's path TO every other secret. If compose stops forwarding
	// one, the worker silently falls back a rank (mTLS → approle → token) or leaves vw: unregistered.
	{service: "worker", goFile: "cmd/worker/credential_delivery_wiring.go", minKeys: 6},
	{service: "worker", goFile: "cmd/worker/vaultwarden_wiring.go", minKeys: 3},
	// spec/024 T-024-5: the passbolt homelab backend's three keys, same rationale as the two above.
	{service: "worker", goFile: "cmd/worker/passbolt_wiring.go", minKeys: 3},
}

// goEnvHelpers are the bare-identifier env accessors defined in the two binaries: worker uses getenv +
// the typed wrappers (truthyEnv/envInt/envFloat/envDuration, each of which takes the env KEY as its first
// arg); grounder uses a local `get` closure. The KEY is always the first argument. Restricted to these
// two files, no other function shares these names, and the UPPER_SNAKE shape filter (envKeyRE) rejects any
// stray non-env call that slipped through.
var goEnvHelpers = map[string]bool{
	"getenv":      true, // worker: getenv(k, def)
	"get":         true, // grounder: local get(k, def) closure
	"truthyEnv":   true, // worker: truthyEnv(k)
	"envInt":      true, // worker: envInt(k, def)
	"envFloat":    true, // worker: envFloat(k, def)
	"envDuration": true, // worker: envDuration(k, def)
	// PLANE-SCOPED reads (TG-153) are env reads like any other, and the guard could not see them: a
	// plane-scoped key an operator sets in .env that compose never forwards fails exactly the same way
	// as an unscoped one — the process reports the feature dark while looking configured.
	"planeEnv": true, // worker: planeEnv(k, def) — the credential-plane-scoped reader
}

// osEnvSelectors are the stdlib env accessors (os.Getenv / os.LookupEnv); KEY is the first arg.
var osEnvSelectors = map[string]bool{"Getenv": true, "LookupEnv": true}

// envKeyRE is the shape of an environment variable name. Anything the AST hands us that is not
// UPPER_SNAKE (e.g. an "env:LITELLM_MASTER_KEY" default VALUE, or a lowercase closure arg) is discarded.
var envKeyRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// parityDynamicKeyFiles names the composition-root files whose env reads legitimately use a KEY THAT IS
// NOT A LITERAL, each with the reason. Every entry is a hole in the extractor's vision, so the list stays
// short and each reason has to say why the key cannot be a literal there.
//
// The alternative — silently skipping every unresolvable read, which is what this walk did until
// 2026-08-23 — makes the guard cover less than it claims with nothing saying so. A named exemption is
// reviewable; a silent skip is not.
// Entries are consumed for BOTH the declared parityTargets and the DISCOVERED wire-file sweep (every
// cmd/{worker,grounder} file that reads env) — a 2026-08-25 review call to delete the two loop-variable
// entries as "dead" was refuted by the gate itself: removing them red the sweep on exactly those files.
var parityDynamicKeyFiles = map[string]string{
	// Both iterate a KEY LIST (the plane key slices, the SSH-key ref enumerator, the secret-entry
	// enumerator): the key is the loop variable, and the lists themselves are Go source this guard cannot
	// read as compose keys anyway. The keys they visit are declared as literals where the lists are built.
	"cmd/worker/credential_plane.go":    "iterates the plane key lists; the key is a loop variable",
	"cmd/worker/main.go":                "the secret-entry and SSH-key-ref enumerators take the key as a parameter, and two readers use a cross-package constant (journal/k8saudit KnownHostsEnv, forwarded as TG_JOURNAL_KNOWN_HOSTS)",
	"cmd/worker/pve_liveness_config.go": "iterates its own key list; the key is a loop variable",
	"cmd/grounder/main.go":              "the `get` resolver closure takes the key as a parameter — every caller passes a literal, which this walk sees at those call sites",
}

// relOrSelf renders a path relative to the repo root when possible, so allowlist keys are stable across
// machines and worktrees.
func relOrSelf(path string) string {
	for _, marker := range []string{"/cmd/", "/deploy/", "/core/"} {
		if i := strings.LastIndex(path, marker); i >= 0 {
			return strings.TrimPrefix(path[i:], "/")
		}
	}
	return path
}

// allowlist: env keys a binary reads that are INTENTIONALLY not forwarded by compose. Keyed by service.
// Keep this tiny and justify every entry — each one is a hole in the guard. It is currently EMPTY: every
// key either binary reads is forwarded by its compose service. Add an entry ONLY for a var that is read
// solely in tests, or one whose in-code default is the sole intended prod value (never operator-set).
var parityAllowlist = map[string]map[string]bool{
	// The retired TG_MUTATION_ENABLED knob (the former Phase-2 mutation-enable lever) has been ABSORBED into
	// the mode chokepoint (spec/015 REQ-1520): the worker no longer reads it, so there is nothing to allowlist.
	// Enabling actuation is now an operator-authorized, audited mode transition (policy.ModeController), never
	// an env flag — so the single most safety-critical control is no longer a boot-time environment knob.
	// HOME is a SYSTEM variable, not a TG knob: cmd/worker/observation_probe_loop.go reads it only as the
	// fallback base for the probe SSH key path, and it goes through the resolver rather than os.Getenv
	// because this package forbids reads that bypass the console-config layer. Compose must not declare it —
	// the container runtime provides it, and pinning it here would override the image's own value.
	"worker":   {"HOME": true},
	"grounder": {},
}

// discoverWireFiles finds every cmd/worker composition-root wire file that reads an env key, so a NEW
// one is covered the moment it lands.
//
// ★ WHY THIS EXISTS. parityTargets is a hand-maintained list, and on 2026-08-23 four wire files were added
// in one sitting — each one had to be REMEMBERED here, and the fifth would simply have been invisible: its
// keys would go unchecked, an operator would set them in .env, compose would never forward them, and the
// feature would report itself dark while looking configured. That is the exact defect this whole file
// exists to catch, reachable again through the list that catches it. The TG-501 ratchet pushes new wiring
// OUT of main.go and into these files continuously, so the list was guaranteed to fall behind.
//
// Discovery closes it: the guard now derives its own targets from the tree. A wire file with no env read
// contributes nothing (no floor to fail); one that reads keys is checked like any other target.
func discoverWireFiles(t *testing.T, root string) []parityTarget {
	t.Helper()
	var out []parityTarget
	// BOTH composition roots. The worker is where the ratchet pushes wiring today, but the grounder has
	// wire files too (dyndb_wire.go) and the same trap applies there — a guard that watched only one
	// binary would be exactly half a guard.
	binaries := map[string]string{"worker": "worker", "grounder": "grounder"}
	for service, dir := range binaries {
		// EVERY non-test .go file in the composition root, not just the *_wire.go / *_wiring.go ones.
		//
		// ★ WHY THE WIDER GLOB (2026-08-23, peer review). The two wire patterns are the FILES THE RATCHET
		// CREATES, not the surface that reads env: cmd/worker already holds env-reading files named neither
		// way (egress.go, estate_doc_coverage.go), and each had to be hand-registered — the hand-maintenance
		// this discovery exists to end. A knob added tomorrow to a file called anything else would ship
		// unforwarded exactly the way TG_GITOPSMR_ARM did, with this guard green while it happened. A gate
		// whose subject list is narrower than its stated scope is the recurring shape here, and it always
		// reads as passing.
		//
		// If widening ever surfaces a key deliberately NOT compose-forwarded (host-local, test-only), the
		// honest fix is a named parityAllowlist entry with a reason — an exclusion someone wrote down
		// survives review; a glob that silently misses a file does not.
		for _, pat := range []string{"*.go"} {
			matches, err := filepath.Glob(filepath.Join(root, "cmd", dir, pat))
			if err != nil {
				t.Fatalf("glob %s: %v", pat, err)
			}
			for _, m := range matches {
				if strings.HasSuffix(m, "_test.go") {
					continue
				}
				rel, err := filepath.Rel(root, m)
				if err != nil {
					t.Fatalf("rel %s: %v", m, err)
				}
				out = append(out, parityTarget{service: service, goFile: filepath.ToSlash(rel), minKeys: 1})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].goFile < out[j].goFile })
	return out
}

func TestComposeEnvParity(t *testing.T) {
	root := repoRoot(t)
	composeEnv := composeEnvByService(t, filepath.Join(root, "deploy", "docker-compose.yml"))

	// The hand-declared targets (the two main.go composition roots and the non-wire-named files) plus
	// every discovered wire file. Discovered entries whose file reads NO env key are dropped rather than
	// failing their floor — a wire file is allowed to take no configuration.
	targets := append([]parityTarget(nil), parityTargets...)
	declared := map[string]bool{}
	for _, tgt := range parityTargets {
		declared[tgt.goFile] = true
	}
	discovered := 0
	for _, tgt := range discoverWireFiles(t, root) {
		if declared[tgt.goFile] {
			continue
		}
		if len(goEnvKeys(t, filepath.Join(root, tgt.goFile))) == 0 {
			continue // a wire file that takes no configuration has nothing to forward
		}
		targets = append(targets, tgt)
		discovered++
	}
	// GUARD-OF-THE-GUARD, the same shape as the per-file key floor below: if discovery returns nothing,
	// the glob broke or the naming convention moved, and this whole mechanism would cover nothing while
	// still reporting PASS. The tree has carried wire files that read env since 2026-08 and the ratchet
	// keeps adding them, so zero is a broken discovery, never a real result.
	if discovered == 0 {
		t.Fatal("compose env parity: discovery found NO wire file that reads an env key — the glob or the " +
			"*_wiring.go / *_wire.go convention moved, and every new composition-root file is now invisible " +
			"to this guard (which is the defect it exists to catch)")
	}
	t.Logf("compose env parity: %d declared target(s) + %d discovered wire file(s) that read env", len(parityTargets), discovered)

	for _, tgt := range targets {
		reads := goEnvKeys(t, filepath.Join(root, tgt.goFile))
		// Guard-of-the-guard: if the AST extraction silently returned nothing, the parse broke and the
		// whole check would vacuously pass. Both main.go files read many keys; a smaller composition-root
		// file declares its own floor, which must still be > 0.
		floor := tgt.minKeys
		if floor == 0 {
			floor = 5
		}
		if len(reads) < floor {
			t.Fatalf("%s: extracted only %d env keys from %s — extraction is broken, not a real result",
				tgt.service, len(reads), tgt.goFile)
		}
		passed := composeEnv[tgt.service]
		if len(passed) == 0 {
			t.Fatalf("%s: compose service has no environment block (or it was not parsed) — cannot verify parity", tgt.service)
		}

		var missing []string
		for k := range reads {
			if passed[k] || parityAllowlist[tgt.service][k] {
				continue
			}
			missing = append(missing, k)
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("compose env parity: the %q binary (%s) reads %d env key(s) the %q compose service never forwards.\n"+
				"Add each to the %q service's environment block in deploy/docker-compose.yml as `KEY: ${KEY:-}` "+
				"(preserve any non-empty in-code default as `${KEY:-default}`), or exempt it in parityAllowlist with a reason.\n"+
				"MISSING (%d):\n  %s",
				tgt.service, tgt.goFile, len(missing), tgt.service, tgt.service, len(missing), strings.Join(missing, "\n  "))
		}
	}
}

// goEnvKeys parses a Go source file and returns every string-literal env KEY passed as the first argument
// to a getenv-family accessor. It walks the AST (comments are not part of the AST, so commented-out reads
// are correctly ignored) and matches both bare idents (goEnvHelpers) and os.Getenv/os.LookupEnv selectors.
// Nested calls like config.SecretRef(getenv("TG_X_REF", "env:Y")) are handled: ast.Inspect visits the
// inner getenv CallExpr, we take its first arg ("TG_X_REF"), and the "env:Y" default is rejected by shape.
func goEnvKeys(t *testing.T, path string) map[string]bool {
	keys, unresolved := goEnvKeysAndUnresolved(t, path)
	if reason, ok := parityDynamicKeyFiles[filepath.ToSlash(relOrSelf(path))]; ok && len(unresolved) > 0 {
		t.Logf("%s: %d env read(s) with a dynamic key, allowed — %s", path, len(unresolved), reason)
		unresolved = nil
	}
	if len(unresolved) > 0 {
		t.Errorf("%s: %d env read(s) whose KEY this extractor cannot resolve (a constant or variable, not a "+
			"string literal): %v.\nThe parity check therefore does NOT cover those keys — it would report PASS "+
			"while they went unforwarded, which is the defect this file exists to catch. Either pass the key as "+
			"a literal at the call site, or teach this walk to resolve the constant.", path, len(unresolved), unresolved)
	}
	return keys
}

func goEnvKeysAndUnresolved(t *testing.T, path string) (map[string]bool, []string) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	// Same-package constants: `const bootConfigDisableEnv = "TG_CONFIG_IGNORE_STORE"` is as much a key
	// declaration as a literal at the call site, and refusing to resolve it would push authors toward
	// duplicating the string rather than naming it once.
	consts := map[string]string{}
	for _, d := range file.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, sp := range gd.Specs {
			vs, ok := sp.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, nm := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if v, err := strconv.Unquote(lit.Value); err == nil {
						consts[nm.Name] = v
					}
				}
			}
		}
	}

	keys := map[string]bool{}
	var unresolved []string
	// The accessor DEFINITIONS forward their own key parameter (`func planeEnv(k, def string) { return
	// getenv(real, def) }`), which is plumbing, not a key read — flagging those as unresolvable would bury
	// the real cases in noise from the very helpers this walk recognises.
	inAccessorDef := false
	ast.Inspect(file, func(n ast.Node) bool {
		if fn, ok := n.(*ast.FuncDecl); ok {
			inAccessorDef = fn.Name != nil && goEnvHelpers[fn.Name.Name]
		}
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		if !isEnvAccessor(call.Fun) {
			return true
		}
		// A same-package constant resolves like a literal. A RESOLVED constant whose value fails the
		// env-key shape is REPORTED, not skipped — the 2026-08-25 review found this branch silently
		// dropping exactly the class (resolved-but-malformed) that the unresolved-reporting below was
		// added to surface: the same silent-skip defect, one branch over.
		if id, ok := call.Args[0].(*ast.Ident); ok {
			if v, found := consts[id.Name]; found {
				if envKeyRE.MatchString(v) {
					keys[v] = true
				} else if !inAccessorDef {
					unresolved = append(unresolved, fset.Position(call.Pos()).String()+
						" (constant "+id.Name+" resolved to a non-env-shaped value)")
				}
				return true
			}
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			// AN ENV READ THIS EXTRACTOR CANNOT RESOLVE — record it rather than skipping.
			//
			// ★ THE SILENT-SKIP BLIND SPOT (2026-08-23). A key passed as a CONSTANT or a variable —
			// getenv(k8saudit.KnownHostsEnv, "") in a wire file, os.Getenv(bootConfigDisableEnv) in
			// boot_config.go — parses fine and is simply not a BasicLit, so this walk used to drop it on the
			// floor. The file then contributed fewer keys than it reads and the guard reported PASS over the
			// difference: exactly the "covers less than it claims" shape this whole file exists to catch,
			// one level down inside the checker itself. Unresolvable reads are now surfaced to the caller,
			// which fails naming the file — the honest answer being "I cannot see this one", never silence.
			if !inAccessorDef {
				unresolved = append(unresolved, fset.Position(call.Pos()).String())
			}
			return true
		}
		key, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		if envKeyRE.MatchString(key) {
			keys[key] = true
		}
		return true
	})
	return keys, unresolved
}

// isEnvAccessor reports whether a call target is one of the recognized env accessors: a bare helper ident,
// or an os.Getenv / os.LookupEnv selector.
func isEnvAccessor(fun ast.Expr) bool {
	switch f := fun.(type) {
	case *ast.Ident:
		return goEnvHelpers[f.Name]
	case *ast.SelectorExpr:
		x, ok := f.X.(*ast.Ident)
		return ok && x.Name == "os" && osEnvSelectors[f.Sel.Name]
	}
	return false
}

var (
	// a top-level service header: exactly two leading spaces, then `name:`.
	composeServiceRE = regexp.MustCompile(`^  ([A-Za-z0-9_-]+):\s*(#.*)?$`)
	// an env key inside an `environment:` block: exactly six leading spaces, then `KEY:`.
	composeEnvKeyRE = regexp.MustCompile(`^      ([A-Za-z_][A-Za-z0-9_]*):`)
)

// composeEnvByService reads docker-compose.yml as text and returns, per service, the set of keys declared
// in that service's `environment:` block. Deliberately a small indentation state machine rather than a
// YAML dependency: keys sit at 6 spaces under an `environment:` at 4 spaces under a service at 2 spaces.
func composeEnvByService(t *testing.T, path string) map[string]map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	out := map[string]map[string]bool{}
	service := ""
	inEnv := false
	for _, line := range strings.Split(string(raw), "\n") {
		if m := composeServiceRE.FindStringSubmatch(line); m != nil {
			service = m[1]
			out[service] = map[string]bool{}
			inEnv = false
			continue
		}
		if service == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		trimmed := strings.TrimSpace(line)
		// A 4-space section header opens or closes the environment block for this service.
		if indent == 4 && strings.HasSuffix(trimmed, ":") {
			inEnv = trimmed == "environment:"
			continue
		}
		if inEnv {
			if m := composeEnvKeyRE.FindStringSubmatch(line); m != nil {
				out[service][m[1]] = true
			}
			continue
		}
	}
	return out
}

// repoRoot walks up from the test's working directory (the package dir) to the module root (go.mod).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find go.mod above %s", dir)
		}
		dir = parent
	}
}

// TestPlaneScopedKeysReachAPlaneThatCanReadThem is the check TestComposeEnvParity structurally cannot
// make, and the reason it cannot is worth stating: parity asks "does SOME service forward this key",
// which is the right question for an unscoped knob and the WRONG one for a plane-scoped one. A key on
// actuationPlaneEnvKeys is withheld by planeEnv on a triage-plane process — so forwarding it only to a
// service running plane=triage delivers a variable that process is designed to ignore, while the plane
// that needs it never receives it. Both halves look fine from either end.
//
// ★ MEASURED, 2026-08-23. TG_GITOPSMR_ALLOWLIST and TG_GITOPSMR_PROPOSE_MAP are actuation-plane keys and
// were forwarded to `worker` (which runs plane=triage on the live deployment) but not to worker-actuate;
// TG_GITOPSMR_ARM was forwarded to `worker` alone. So `truthyEnv(ARM) && gitopsHasFieldRules(allowlist)`
// was unsatisfiable in EITHER process, whatever an operator set — the gitops-mr lane could not be armed
// at all, behind a boot line that read "DARK (no actuator)" and was indistinguishable from "not armed
// yet". An arm attempt on the documented procedure would have burned a live window twice over.
//
// The assertion is deliberately narrow: it checks the PAIRING (a plane-scoped key must reach at least
// one service whose declared plane can read it) rather than teaching this file the whole plane model.
// That is checkable from the compose file plus the key list, and it fails on exactly the class above.
//
// ★ WHAT THIS CHECK STILL CANNOT SEE, stated rather than left for someone to discover as a surprise:
// worker-actuate carries `profiles: ["split-planes"]`, so it runs only when that profile is activated
// (--profile split-planes / COMPOSE_PROFILES). "Forwarded to worker-actuate" therefore means the key
// reaches a RUNNING process only on a deployment that activates the profile — one that never does
// satisfies this check while the key reaches nothing. Teaching the guard which profiles a deployment
// activates is the plane-model complexity this check exists to avoid, so the boundary is named here
// instead. On the live estate the profile IS active (worker-actuate-1 is up), so the pairing holds
// today; that is a fact about the deployment, not something this file can assert.
func TestPlaneScopedKeysReachAPlaneThatCanReadThem(t *testing.T) {
	root := repoRoot(t)
	actuationKeys := planeKeyList(t, filepath.Join(root, "cmd", "worker", "credential_plane.go"), "actuationPlaneEnvKeys")
	if len(actuationKeys) < 5 {
		t.Fatalf("parsed %d actuation-plane key(s) — the parse broke, and this check would pass vacuously", len(actuationKeys))
	}
	composeEnv := composeEnvByService(t, filepath.Join(root, "deploy", "docker-compose.yml"))
	planes := composePlanesByService(t, filepath.Join(root, "deploy", "docker-compose.yml"))

	// The services whose plane STATICALLY holds actuation — a literal `actuation` or `both`, never a
	// ${VAR:-both} form.
	//
	// ★ THE OVERRIDABLE-PLANE TRAP, found by this check's own killing mutation. `worker` declares
	// TG_CREDENTIAL_PLANE: ${TG_CREDENTIAL_PLANE:-both}, and the first version of this test read that
	// default as "worker can hold actuation" — which made the check PASS on exactly the state it was built
	// to catch, because the live deployment sets TG_CREDENTIAL_PLANE=triage in .env and that worker runs
	// plane=triage. A compose default is what the deployment runs UNLESS an operator overrides it, and this
	// one is overridden; treating it as a guarantee is how a guard reports green over the defect. So an
	// operator-overridable plane counts as UNKNOWN here, and only a statically-declared actuation plane
	// satisfies the pairing.
	var canRead []string
	for svc, p := range planes {
		if strings.Contains(p, "${") {
			continue // operator-overridable ⇒ cannot be relied on to hold actuation
		}
		if strings.Contains(p, "actuation") || strings.Contains(p, "both") {
			canRead = append(canRead, svc)
		}
	}
	if len(canRead) == 0 {
		// The likeliest cause is that worker-actuate's `TG_CREDENTIAL_PLANE: actuation` was parameterised
		// into a ${VAR:-actuation} form. That is not a small edit: it makes this whole check unsatisfiable
		// (no service statically holds actuation, so every actuation key reads as unpaired), which is why it
		// fails LOUDLY here rather than degrading to a green over an empty candidate set.
		t.Fatal("no compose service declares a plane that STATICALLY holds actuation — either the parse broke, " +
			"or worker-actuate's TG_CREDENTIAL_PLANE was parameterised (a ${VAR:-...} form counts as UNKNOWN " +
			"here, see above), or this deployment genuinely has no actuation process. In every case this check " +
			"can no longer mean anything and must not report PASS")
	}
	sort.Strings(canRead)

	for _, k := range actuationKeys {
		reachable := false
		for _, svc := range canRead {
			if composeEnv[svc][k] {
				reachable = true
				break
			}
		}
		if !reachable {
			t.Errorf("actuation-plane key %s is forwarded to NO service whose plane can read it (services that "+
				"can: %v). planeEnv withholds it on a triage-plane process, so wherever it is forwarded today it "+
				"is either ignored or absent — the feature it configures cannot be armed in ANY process, behind a "+
				"boot line that reads like it simply was not configured.", k, canRead)
		}
	}
}

// planeKeyList extracts the string literals of a named []string var from a Go source file. It is a text
// parse on purpose: this package must not import cmd/worker (a main package).
func planeKeyList(t *testing.T, path, varName string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	s := string(b)
	i := strings.Index(s, "var "+varName+" = []string{")
	if i < 0 {
		t.Fatalf("%s not found in %s — the plane key lists moved and this check is now blind", varName, path)
	}
	end := strings.Index(s[i:], "\n}")
	if end < 0 {
		t.Fatalf("%s is not terminated as expected", varName)
	}
	var out []string
	for _, m := range regexp.MustCompile(`"(TG_[A-Z0-9_]+)"`).FindAllStringSubmatch(s[i:i+end], -1) {
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}

// composePlanesByService reads each service's declared TG_CREDENTIAL_PLANE value (raw, including a
// ${VAR:-default} form — the default is what the deployment runs unless an operator overrides it).
func composePlanesByService(t *testing.T, path string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read compose: %v", err)
	}
	out := map[string]string{}
	svc := ""
	for _, line := range strings.Split(string(b), "\n") {
		if m := composeServiceRE.FindStringSubmatch(line); m != nil {
			svc = m[1]
			continue
		}
		if svc == "" {
			continue
		}
		if m := regexp.MustCompile(`^      TG_CREDENTIAL_PLANE:\s*(.+?)\s*$`).FindStringSubmatch(line); m != nil {
			out[svc] = m[1]
		}
	}
	return out
}

// TestGoEnvKeysReportsResolvedButMalformedConstant pins the 2026-08-25 review finding: a same-package
// constant that RESOLVES but whose value fails the env-key shape must land in `unresolved`, never be
// silently passed over — that is the same silent-skip defect the unresolved-reporting was added to close,
// one branch over. The well-shaped constant in the same fixture is the mutation control: reverting the
// fix to "always report" breaks the first assertion, reverting to "always skip" breaks the second.
func TestGoEnvKeysReportsResolvedButMalformedConstant(t *testing.T) {
	dir := t.TempDir()
	src := `package p

import "os"

const goodKey = "TG_GOOD_KEY"
const badKey = "tg_lowercase_key"

func f() {
	_ = os.Getenv(goodKey)
	_ = os.Getenv(badKey)
}
`
	path := filepath.Join(dir, "fixture.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	keys, unresolved := goEnvKeysAndUnresolved(t, path)
	if !keys["TG_GOOD_KEY"] {
		t.Fatalf("the well-shaped constant must resolve to its key; got %v", keys)
	}
	if len(unresolved) != 1 || !strings.Contains(unresolved[0], "badKey") {
		t.Fatalf("a resolved-but-malformed constant must be REPORTED as unresolved, got %v", unresolved)
	}
}
