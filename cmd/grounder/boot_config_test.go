package main

// ORACLES FOR THE GROUNDER'S CONFIG RESOLUTION (TG-263).
//
// The load-bearing one is the LAST: a hand-maintained exclusion list protects nothing once the code moves
// past it. TestEveryAuthEnvKeyTheGrounderReadsIsExcluded AST-walks main.go for the env keys the
// authentication path actually consumes and fails if any is missing from authEnvKeys — so adding an LDAP
// or session knob without excluding it is a red build, not a discovery.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/modules/catalog"
)

func withOverrides(t *testing.T, m map[string]string) {
	t.Helper()
	grounderOverrides.Store(&m)
	t.Cleanup(func() { grounderOverrides.Store(nil) })
}

// KILLING MUTATION: drop the grounderOverride call from loadEnv's get closure. RED — the grounder is back
// to env-only and the two halves of TG can disagree about the configuration (TG-263's defect).
func TestTheConsoleValueBeatsTheEnvironmentInTheGrounder(t *testing.T) {
	t.Setenv("TG_LIBRENMS_DEPLOYMENTS", "env-must-lose")
	withOverrides(t, map[string]string{"TG_LIBRENMS_DEPLOYMENTS": "console-must-win"})
	if got := loadEnv().LibrenmsDeployments; got != "console-must-win" {
		t.Fatalf("got %q — the operator's saved value must win over the deployed environment", got)
	}
}

// KILLING MUTATION: leave one of the four direct os.Getenv reads in place. RED for that field — it would
// silently ignore the console layer while its neighbours honour it.
func TestTheFormerlyDirectReadsGoThroughTheResolver(t *testing.T) {
	withOverrides(t, map[string]string{
		"TG_KNOWLEDGE_FILE":      "/console/corpus.json",
		"TG_KNOWLEDGE_SEED_FILE": "/console/seed.json",
	})
	cfg := loadEnv()
	for name, got := range map[string]string{
		"KnowledgeFile":     cfg.KnowledgeFile,
		"KnowledgeSeedFile": cfg.KnowledgeSeedFile,
	} {
		if !strings.HasPrefix(got, "/console/") {
			t.Errorf("%s = %q — still read with a direct os.Getenv, bypassing the console layer", name, got)
		}
	}
}

// THE DSNs MUST NOT BE RESOLVABLE FROM THE DATABASE THEY OPEN.
// KILLING MUTATION: route RuntimeDSN/MigrationDSN through get. RED.
func TestTheDSNsAreNeverResolvedFromTheStore(t *testing.T) {
	t.Setenv("TG_RUNTIME_DSN", "postgres://env/real")
	t.Setenv("TG_MIGRATION_DSN", "postgres://env/migr")
	withOverrides(t, map[string]string{
		"TG_RUNTIME_DSN":   "postgres://console/hijack",
		"TG_MIGRATION_DSN": "postgres://console/hijack",
	})
	cfg := loadEnv()
	if cfg.RuntimeDSN != "postgres://env/real" || cfg.MigrationDSN != "postgres://env/migr" {
		t.Fatalf("a DSN resolved from the config store (runtime=%q migration=%q) — a database cannot supply "+
			"the address of the database it lives in", cfg.RuntimeDSN, cfg.MigrationDSN)
	}
}

// THE ESCALATION GUARD. KILLING MUTATION: remove TG_LDAP_URLS from authEnvKeys. RED — an operator editing
// the credsource/ldap CONNECTOR dialog would re-point the directory that authenticates the console and
// decides who may elevate.
func TestAConnectorDialogCannotRepointTheConsolesOwnAuthentication(t *testing.T) {
	t.Setenv("TG_LDAP_URLS", "ldaps://real-directory.example.test")
	t.Setenv("TG_LDAP_STARTTLS", "true")
	withOverrides(t, map[string]string{
		"TG_LDAP_URLS":     "ldaps://attacker.example.test",
		"TG_LDAP_STARTTLS": "false",
	})
	cfg := loadEnv()
	if cfg.LDAPURLs != "ldaps://real-directory.example.test" {
		t.Fatalf("LDAP URLs resolved to %q from the config store — a settings dialog just re-pointed the "+
			"console's own authentication", cfg.LDAPURLs)
	}
	// The snapshot is what a compromised/mistaken store would contain; the guard is that loadEnv refuses
	// to consult it for these keys. (installGrounderConfig refuses to STORE them; this is the second line.)
	if !cfg.LDAPStartTLS {
		t.Fatal("LDAP StartTLS was downgraded from the config store")
	}
}

// THE LIST STAYS HONEST AS THE CODE MOVES. KILLING MUTATION: add an auth env key to main.go's auth path
// without adding it to authEnvKeys. RED, naming the key.
func TestEveryAuthEnvKeyTheGrounderReadsIsExcluded(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	// Every string literal in main.go that names an authentication-shaped env key. Deliberately a PREFIX
	// sweep rather than a curated list: the point is to catch a key nobody remembered to curate.
	prefixes := []string{"TG_LDAP", "TG_SESSION", "TG_OPERATOR_", "TG_ADMIN_"}
	found := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		v := strings.Trim(lit.Value, `"`)
		for _, p := range prefixes {
			if strings.HasPrefix(v, p) {
				found[v] = true
			}
		}
		return true
	})
	if len(found) == 0 {
		t.Fatal("no auth env keys found in main.go — the sweep is broken, not the code")
	}
	var missing []string
	for k := range found {
		// TG_ADMIN_ADDR is a listen address, excluded as a BOOTSTRAP key rather than an auth one.
		if authEnvKeys[k] || bootstrapEnvKeys[k] {
			continue
		}
		missing = append(missing, k)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("%d authentication env key(s) the grounder reads are NOT excluded from console resolution — "+
			"a settings dialog could set them: %v\nAdd each to authEnvKeys in boot_config.go.", len(missing), missing)
	}
	t.Logf("all %d auth env keys read by main.go are excluded from console resolution", len(found))
}

// A companion fact worth pinning: exactly which auth keys are console-writable TODAY. If a new descriptor
// claims another, this number moves and the reviewer is told — the exclusion already holds it, but the
// SURFACE having grown is itself worth knowing.
func TestTheConsoleWritableAuthOverlapIsKnown(t *testing.T) {
	bindings, err := catalog.EnvBindings()
	if err != nil {
		t.Fatal(err)
	}
	var overlap []string
	for _, b := range bindings {
		if authEnvKeys[b.EnvKey] {
			overlap = append(overlap, b.EnvKey)
		}
	}
	sort.Strings(overlap)
	want := []string{"TG_LDAP_STARTTLS", "TG_LDAP_URLS"}
	if strings.Join(overlap, ",") != strings.Join(want, ",") {
		t.Fatalf("the console-writable ∩ authentication surface changed: got %v, want %v — the exclusion "+
			"still holds, but a descriptor now claims a different auth key and that deserves a look",
			overlap, want)
	}
}
