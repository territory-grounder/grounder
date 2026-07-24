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
var parityTargets = []struct {
	service string // docker-compose.yml service name
	goFile  string // repo-relative path to that service's main.go
}{
	{service: "worker", goFile: "cmd/worker/main.go"},
	{service: "grounder", goFile: "cmd/grounder/main.go"},
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
}

// osEnvSelectors are the stdlib env accessors (os.Getenv / os.LookupEnv); KEY is the first arg.
var osEnvSelectors = map[string]bool{"Getenv": true, "LookupEnv": true}

// envKeyRE is the shape of an environment variable name. Anything the AST hands us that is not
// UPPER_SNAKE (e.g. an "env:LITELLM_MASTER_KEY" default VALUE, or a lowercase closure arg) is discarded.
var envKeyRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// allowlist: env keys a binary reads that are INTENTIONALLY not forwarded by compose. Keyed by service.
// Keep this tiny and justify every entry — each one is a hole in the guard. It is currently EMPTY: every
// key either binary reads is forwarded by its compose service. Add an entry ONLY for a var that is read
// solely in tests, or one whose in-code default is the sole intended prod value (never operator-set).
var parityAllowlist = map[string]map[string]bool{
	// The retired TG_MUTATION_ENABLED knob (the former Phase-2 mutation-enable lever) has been ABSORBED into
	// the mode chokepoint (spec/015 REQ-1520): the worker no longer reads it, so there is nothing to allowlist.
	// Enabling actuation is now an operator-authorized, audited mode transition (policy.ModeController), never
	// an env flag — so the single most safety-critical control is no longer a boot-time environment knob.
	"worker":   {},
	"grounder": {},
}

func TestComposeEnvParity(t *testing.T) {
	root := repoRoot(t)
	composeEnv := composeEnvByService(t, filepath.Join(root, "deploy", "docker-compose.yml"))

	for _, tgt := range parityTargets {
		reads := goEnvKeys(t, filepath.Join(root, tgt.goFile))
		// Guard-of-the-guard: if the AST extraction silently returned nothing, the parse broke and the
		// whole check would vacuously pass. Both binaries read many keys; require a sane floor.
		if len(reads) < 5 {
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
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	keys := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		if !isEnvAccessor(call.Fun) {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
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
	return keys
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
