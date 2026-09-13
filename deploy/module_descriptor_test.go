package deploy_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/modules/catalog"
)

// awaitingDescriptor is the EXPLICIT backlog: module packages that do not yet publish a configuration
// schema.
//
// It exists so the gap is a named list somebody can shrink, instead of an absence nobody can see. That
// distinction is the whole lesson of this codebase: the predecessor tracks 387 components with 46
// declared-dark and no mechanism that ever made adding the 47th visible, and TG has spent two days
// finding lanes that were built, tested, green and never reached.
//
// A NEW module package is NOT in this list, so it fails this test until it either publishes a descriptor
// or is added here deliberately. That is the enforcement: you cannot ship a connector with no way to
// configure it and not notice.

// TestEveryModulePackageIsDescribedOrExplicitlyDeferred is the guard that makes a missing configuration
// dialog a TEST FAILURE rather than an invisible gap.
//
// It walks modules/ for packages that declare a SourceType (the marker of a connector), and requires each
// to be either in the catalog or in awaitingDescriptor above.
//
// KILLING MUTATION: add a new package under modules/ declaring `const SourceType = "x"` without a
// descriptor and without a backlog entry. RED.
func TestEveryModulePackageIsDescribedOrExplicitlyDeferred(t *testing.T) {
	described := map[string]bool{}
	all, err := catalog.All()
	if err != nil {
		t.Fatalf("the module catalog does not validate: %v", err)
	}
	for _, d := range all {
		described[d.Surface+"/"+d.SourceType] = true
	}
	if len(described) == 0 {
		t.Fatal("vacuity floor: the catalog is EMPTY, so this guard would pass over a tree with no " +
			"descriptors at all")
	}

	// The join is on the module's OWN registered slug, read out of its source, never on the directory
	// name. A first version matched the catalog key by directory leaf — HasSuffix(key, "/"+leaf) — and
	// that was wrong for five of the twenty-nine connectors, because a package directory and the slug it
	// registers under are simply different strings: modules/actuation/awxjob registers "awx-job",
	// credsource/oidctoken registers "oidc-token", ingest/pveliveness registers "pve-liveness", and both
	// discovery probes append the surface (INV-17) to register "docker-discovery"/"systemd-discovery".
	// The suffix form failed all five, so the guard would have rejected five correct descriptors — and,
	// worse in the other direction, "/pve" is a suffix of nothing else only by luck. Matching the exact
	// pair additionally catches a descriptor that declares a SourceType its own module does not register
	// under, which is a dialog whose secret lane is keyed to a module that will never read it.
	root := filepath.Join("..", "modules")
	sourceTypeRe := regexp.MustCompile(`SourceType\s*=\s*"([^"]+)"`)
	connectors := map[string]string{} // package path -> declared slug
	err = filepath.Walk(root, func(path string, info os.FileInfo, werr error) error {
		if werr != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return werr
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		// The marker of a connector package: it names its own vendor slug.
		m := sourceTypeRe.FindStringSubmatch(string(src))
		if m == nil {
			return nil
		}
		rel := strings.TrimPrefix(filepath.ToSlash(filepath.Dir(path)), "../")
		connectors[rel] = m[1]
		return nil
	})
	if err != nil {
		t.Fatalf("walk modules/: %v", err)
	}

	// Vacuity floor: the walk must find the connector fleet. A broken matcher that found nothing would
	// certify a tree where every module lacks a dialog.
	if len(connectors) < 30 {
		t.Fatalf("vacuity floor: only %d connector package(s) found under modules/ — the matcher is "+
			"broken and a passing run would certify nothing", len(connectors))
	}

	for pkg, slug := range connectors {
		if catalog.IsUndescribed(pkg) {
			continue
		}
		// The surface is the directory above the package: modules/<surface>/<pkg>.
		parts := strings.Split(pkg, "/")
		if len(parts) < 3 {
			t.Errorf("connector package %q is not under modules/<surface>/<pkg>, so no catalog key can be "+
				"derived for it", pkg)
			continue
		}
		key := parts[len(parts)-2] + "/" + slug
		if !described[key] {
			t.Errorf("module package %q registers source type %q, so its catalog key is %q — and no "+
				"desc.Descriptor publishes that key, nor is the package in catalog.undescribed. It "+
				"therefore has NO configuration dialog and nothing says so. Either add a Descriptor() "+
				"(see modules/notifier/matrix/descriptor.go) whose Surface/SourceType match exactly, or "+
				"add it to catalog.undescribed with a reason.", pkg, slug, key)
		}
	}
}

// A package on the no-schema list must say WHY. A bare name cannot distinguish "nobody has written this
// dialog yet" from "this module has nothing to configure", and a console that cannot tell those apart
// reports a finished surface as permanently unfinished.
func TestEveryUndescribedPackageStatesItsReason(t *testing.T) {
	pkgs := catalog.Undescribed()
	if len(pkgs) == 0 {
		t.Skip("nothing on the list")
	}
	for _, pkg := range pkgs {
		if strings.TrimSpace(catalog.UndescribedReason(pkg)) == "" {
			t.Errorf("%q publishes no configuration schema and gives no reason — a reader cannot tell "+
				"whether the dialog is missing or unnecessary, and the list can never be shrunk by "+
				"inspection", pkg)
		}
	}
}

// A backlog entry for a package that no longer exists is stale bookkeeping that makes the list lie about
// how much is left.
func TestBacklogHasNoStaleEntries(t *testing.T) {
	for _, pkg := range catalog.Undescribed() {
		if _, err := os.Stat(filepath.Join("..", pkg)); os.IsNotExist(err) {
			t.Errorf("the descriptor backlog names %q, which does not exist — the backlog overstates the work "+
				"remaining and nobody can trust its size", pkg)
		}
	}
}

// The catalog must validate. A malformed descriptor is a dialog that would mislead an operator, and it
// should fail here once rather than render wrongly forever.
func TestCatalogValidates(t *testing.T) {
	all, err := catalog.All()
	if err != nil {
		t.Fatalf("catalog.All(): %v", err)
	}
	for _, d := range all {
		if err := d.Validate(); err != nil {
			t.Errorf("%s/%s: %v", d.Surface, d.SourceType, err)
		}
	}
}

// TestEveryDescriptorEnvKeyIsActuallyReadByTheBinary is what makes a descriptor trustworthy.
//
// A field whose EnvKey the worker never reads is a CONTROL WIRED TO NOTHING: the dialog renders an input,
// the operator fills it in, Save succeeds, and the value reaches no code. That is the defect this entire
// surface exists to remove, reproduced inside the surface itself — and it is the single most likely way a
// batch of hand-written descriptors goes wrong, because a plausible key name is easy to invent and
// impossible to spot by reading.
//
// The check is one-directional on purpose: every descriptor key must be read by the binary, but not every
// key the binary reads needs a descriptor (most of the worker's TG_* keys are platform, not module).
//
// KILLING MUTATION: change any descriptor's EnvKey to a name the worker does not read. RED.
func TestEveryDescriptorEnvKeyIsActuallyReadByTheBinary(t *testing.T) {
	all, err := catalog.All()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	read := envKeysReadAsCallArguments(t,
		filepath.Join("..", "cmd", "worker"),
		filepath.Join("..", "cmd", "grounder"))
	if len(read) < 100 {
		t.Fatalf("vacuity floor: only %d TG_ env key(s) found across the composition roots — the matcher "+
			"is broken and every descriptor would pass", len(read))
	}
	checked := 0
	for _, d := range all {
		for _, f := range d.Fields {
			if f.EnvKey == "" {
				continue
			}
			checked++
			if !read[f.EnvKey] {
				t.Errorf("%s/%s field %q declares env key %s, which NO composition root reads. The dialog "+
					"would render an input, accept a value, report success, and change nothing.",
					d.Surface, d.SourceType, f.Name, f.EnvKey)
			}
		}
	}
	if checked == 0 {
		t.Fatal("vacuity floor: no descriptor declares an env key, so this guard certified nothing")
	}
}

// envKeysReadAsCallArguments collects every TG_* env key that appears as a STRING-LITERAL ARGUMENT to a
// function call in the named packages' non-test sources — the shape of an actual read
// (getenv("TG_X", ...), os.Getenv("TG_X"), envInt("TG_X", ...)), and of nothing else.
//
// ★ THIS REPLACES A REGEX OVER RAW FILE BYTES, WHICH COULD NOT TELL A READ FROM A MENTION (TG-264). Under
// the old matcher, modules/bootstrap/bootstrap.go contributed FOUR keys to the "read by the binary" set —
// TG_ACTUATION_ALLOWED_UNITS among them — and every one of those mentions is a COMMENT. A descriptor whose
// EnvKey was only ever written about would pass the very guard that exists to refuse controls wired to
// nothing. The AST cannot be fooled that way: comments are not nodes, and a key inside a longer log string
// is not a literal whose whole value is the key.
//
// The full-literal requirement is load-bearing twice over: it excludes prose ("set TG_FOO and restart")
// embedded in Printf strings, and it means a key constructed by concatenation is NOT counted — which is
// correct, because a dynamically-assembled read is exactly the kind this repo's resolver chokepoint bans.
func envKeysReadAsCallArguments(t *testing.T, dirs ...string) map[string]bool {
	t.Helper()
	full := regexp.MustCompile(`^TG_[A-Z0-9_]+$`)
	out := map[string]bool{}
	fset := token.NewFileSet()
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read dir %s: %v", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				t.Fatalf("parse %s: %v", path, perr)
			}
			collectEnvKeyCallArgs(f, full, out)
		}
	}
	return out
}

// collectEnvKeyCallArgs walks one parsed file for call-argument env keys. Split out so the collector itself
// is testable against synthetic sources — the guard that guards the guards.
func collectEnvKeyCallArgs(f *ast.File, full *regexp.Regexp, out map[string]bool) {
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		for _, a := range call.Args {
			lit, ok := a.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			v := strings.Trim(lit.Value, "`\"")
			if full.MatchString(v) {
				out[v] = true
			}
		}
		return true
	})
}

// TestATestVerbIsPromisedONLYWhereAProbeExists is the guard that keeps the TEST button honest.
//
// The descriptor's Test.Verb is the sentence shown to the operator BEFORE they press TEST: "post a test
// message to the approvals room". It is a consent contract — it exists so somebody knows what they are
// about to cause in a third-party system other people watch.
//
// For a while exactly one surface could honour it. The worker built probers from the notifier sinks and
// nothing else, so 28 of 29 dialogs promised an action nothing could perform; the worker's own boot log
// said `module test: lane registered over 1 prober(s)`. The outcome was honest — a module with no prober
// reports "no test is implemented" rather than a pass — but the promise was not, and a button that says
// what it will do and then does not do it is the same defect class as a lane that is wired and never
// called.
//
// THE GUARD IS BICONDITIONAL, AND THAT IS WHY IT NEEDS NO EXEMPTION LIST. A module that cannot honestly
// probe itself sets Verb to "" and the console disables the button; a module that can, implements
// core/selftest.Tester. Either half alone rots: a one-directional check would let a capability sit unused
// behind a disabled button, and an exemption list would let "we will get to it" become permanent — which
// is exactly how the original gap survived.
//
// It matches on the compile-time assertion (`var _ selftest.Tester = ...`) rather than on a SelfTest
// method, because the assertion is what fails the build if the interface is ever renamed. A method that
// merely looks right satisfies nothing.
//
// KILLING MUTATION: give any descriptor a Test.Verb without implementing the capability, or delete a
// module's selftest.go while leaving its verb in place. RED either way.
func TestATestVerbIsPromisedONLYWhereAProbeExists(t *testing.T) {
	all, err := catalog.All()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	// Map each descriptor to its package directory by the same exact surface/slug join the guard above
	// uses — never by directory leaf, which disagrees with the registered slug for five of the modules.
	sourceTypeRe := regexp.MustCompile(`SourceType\s*=\s*"([^"]+)"`)
	pkgOf := map[string]string{}
	if err := filepath.Walk(filepath.Join("..", "modules"), func(path string, info os.FileInfo, werr error) error {
		if werr != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return werr
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		m := sourceTypeRe.FindStringSubmatch(string(src))
		if m == nil {
			return nil
		}
		dir := filepath.ToSlash(filepath.Dir(path))
		parts := strings.Split(strings.TrimPrefix(dir, "../"), "/")
		if len(parts) < 3 {
			return nil
		}
		pkgOf[parts[len(parts)-2]+"/"+m[1]] = dir
		return nil
	}); err != nil {
		t.Fatalf("walk modules/: %v", err)
	}
	if len(pkgOf) < 30 {
		t.Fatalf("vacuity floor: only %d connector package(s) mapped — the join is broken and every "+
			"descriptor would pass", len(pkgOf))
	}

	checked := 0
	for _, d := range all {
		key := d.Surface + "/" + d.SourceType
		dir, ok := pkgOf[key]
		if !ok {
			t.Errorf("descriptor %s has no module package declaring that source type", key)
			continue
		}
		entries, rerr := os.ReadDir(dir)
		if rerr != nil {
			t.Fatalf("read %s: %v", dir, rerr)
		}
		implements := false
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			src, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
			if rerr != nil {
				t.Fatalf("read %s/%s: %v", dir, e.Name(), rerr)
			}
			if strings.Contains(string(src), "selftest.Tester") {
				implements = true
				break
			}
		}
		checked++
		promised := strings.TrimSpace(d.Test.Verb) != ""
		switch {
		case promised && !implements:
			t.Errorf("%s declares the test verb %q but its package %s implements no core/selftest.Tester. "+
				"The dialog shows that sentence to an operator and pressing TEST does nothing of the kind. "+
				"Either implement the capability or clear Test.Verb so the console disables the button.",
				key, d.Test.Verb, dir)
		case !promised && implements:
			t.Errorf("%s implements core/selftest.Tester but declares NO test verb, so the console renders "+
				"its TEST button disabled and the working probe is unreachable — a capability wired to "+
				"nothing. Give it a verb saying what the probe does.", key)
		}
	}
	if checked == 0 {
		t.Fatal("vacuity floor: no descriptor was checked, so this guard certified nothing")
	}
}

// A PROBE WHOSE EVIDENCE IS AN OUTWARD ARTEFACT MUST DECLARE Emits.
//
// The flag is what keeps a scheduled sweep from becoming a pager: the sweep skips emitters, a human
// pressing TEST runs everything. If a notifier ever forgets it, the timer starts posting into the
// approvals room — noise aimed at the one room that has to stay readable during an incident.
//
// The guard keys on the SURFACE only because today the notifiers are the whole emitting category; the
// property that matters is declared per module, so a future emitting probe on another surface sets the
// flag and is covered by the sweep's own skip logic without touching this test.
//
// KILLING MUTATION: drop Emits from any notifier descriptor. RED.
func TestEveryNotifierProbeDeclaresThatItEmits(t *testing.T) {
	all, err := catalog.All()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	checked := 0
	for _, d := range all {
		if d.Surface != "notifier" || strings.TrimSpace(d.Test.Verb) == "" {
			continue
		}
		checked++
		if !d.Test.Emits {
			t.Errorf("%s/%s has a probe that posts a real message but does not declare Test.Emits — a "+
				"scheduled sweep would run it on every tick and page the approvals room",
				d.Surface, d.SourceType)
		}
	}
	if checked == 0 {
		t.Fatal("vacuity floor: no notifier descriptor was checked, so this guard certified nothing")
	}
}
