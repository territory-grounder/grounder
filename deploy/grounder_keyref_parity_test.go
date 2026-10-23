package deploy

import (
	"strings"
	"testing"
)

// TG-249 item 7. `grounder --check` is the DEPLOY GATE: it resolves every SSH key reference before a
// rollout proceeds. core/preflight.SSHKeyRefsFromEnv collects refs from FIVE environment sources (the
// ticket says four), and deploy/docker-compose.yml forwarded exactly one of them to the grounder service.
//
// So the gate resolved a quarter of what it claims to check and reported green on the rest.
//
// Measured on the live grounder container 2026-08-06, which is worse than the ticket recorded:
//
//	TG_ACTUATION_SSH_KEY        <absent>
//	TG_SYSLOGNG_DEPLOYMENTS     <absent>
//	TG_HOSTDIAG_DEPLOYMENTS     <absent>
//	TG_CREDENTIAL_NATIVE_RULES  <absent>
//	TG_AWX_CRED_REF_MAP         <absent>   ← the one the credential engine actually resolves through
//
// None of the four reaches the running process, so today the preflight resolves ZERO references and still
// passes. A gate that checks nothing is worse than no gate: it certifies.

// keyRefEnvSources are the FIVE env vars core/preflight.SSHKeyRefsFromEnv reads. Listed here rather than
// imported so this test fails when the two DRIFT — which is the whole defect — rather than tracking the
// implementation silently.
var keyRefEnvSources = []string{
	"TG_ACTUATION_SSH_KEY", // 1. the actuation identity
	// 2. AWX credential map — and per that function's own note, "THE ONE THAT MATTERS MOST (TG-306)": the
	// credential engine resolves most-specific-wins, AWX registers at precedence 20 against native
	// hostdiag's 100, so the AWX rule WINS and the key handed out comes from this map. TG-249 records this
	// finding as "3 of the 4 sources"; there are five, and the missing one is the one that decides.
	// TestTheKeyRefSourceListMatchesPreflight found it on its first run.
	"TG_AWX_CRED_REF_MAP",
	"TG_SYSLOGNG_DEPLOYMENTS",    // 3. syslog-ng device-log reads (field 4 of each row)
	"TG_HOSTDIAG_DEPLOYMENTS",    // 4. host-diagnostics reads (field 4)
	"TG_CREDENTIAL_NATIVE_RULES", // 5. native credential rules (field 5, ssh/netconf schemes only)
}

// KILLING MUTATION: remove any of the three added forwards from the grounder service block. RED.
func TestTheGrounderSeesEveryKeyRefSourceItsPreflightReads(t *testing.T) {
	block, ok := composeServiceBlock(composeFile(t), "grounder")
	if !ok {
		t.Fatal("no grounder service in deploy/docker-compose.yml")
	}
	for _, k := range keyRefEnvSources {
		if !strings.Contains(block, k+":") {
			t.Errorf("the grounder service does not forward %s, but core/preflight.SSHKeyRefsFromEnv reads "+
				"it. `grounder --check` would resolve the refs it can see and report green on a deployment "+
				"whose other key references were never validated.", k)
		}
	}
}

// THE SOURCE OF TRUTH MUST NOT DRIFT. If preflight grows a fifth source, this list — and therefore the
// compose block — has to grow with it, or the gate silently narrows again.
//
// KILLING MUTATION: add a fifth `add(...)` env source to SSHKeyRefsFromEnv without updating compose. RED.
func TestTheKeyRefSourceListMatchesPreflight(t *testing.T) {
	src, err := readRepoFile("../core/preflight/secrets.go")
	if err != nil {
		t.Fatalf("read core/preflight/secrets.go: %v", err)
	}
	fn := funcBody(src, "func SSHKeyRefsFromEnv(")
	if fn == "" {
		t.Fatal("SSHKeyRefsFromEnv is gone from core/preflight — this guard has no subject")
	}
	// Every TG_ env var the function reads must be in the list above.
	for _, name := range envVarsRead(fn) {
		if !containsStr(keyRefEnvSources, name) {
			t.Errorf("SSHKeyRefsFromEnv reads %s, which keyRefEnvSources does not list — so nothing checks "+
				"that the grounder is given it, and `grounder --check` would quietly stop covering that "+
				"source", name)
		}
	}
	// ..and every var in the list must actually be read, or this test guards a var nobody uses and the
	// compose block accretes dead entries.
	read := envVarsRead(fn)
	for _, want := range keyRefEnvSources {
		if !containsStr(read, want) {
			t.Errorf("keyRefEnvSources lists %s but SSHKeyRefsFromEnv does not read it — the list has drifted "+
				"the other way and this guard is asserting something stale", want)
		}
	}
	// Vacuity floor: the extractor must find SOMETHING, or both loops above pass over an empty set.
	if len(read) == 0 {
		t.Fatal("no env vars extracted from SSHKeyRefsFromEnv — the extractor is broken and both checks " +
			"above are vacuous")
	}
}

// composeServiceBlock returns one top-level service's YAML block.
func composeServiceBlock(compose, name string) (string, bool) {
	marker := "\n  " + name + ":\n"
	i := strings.Index(compose, marker)
	if i < 0 {
		return "", false
	}
	rest := compose[i+1:]
	var out []string
	for n, l := range strings.Split(rest, "\n") {
		// A new service starts at exactly two spaces of indent followed by a name and a colon.
		if n > 0 && strings.HasPrefix(l, "  ") && !strings.HasPrefix(l, "   ") && strings.HasSuffix(strings.TrimSpace(l), ":") {
			break
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n"), true
}

// funcBody returns a Go function's text from its signature to the next top-level closing brace.
func funcBody(src, sig string) string {
	i := strings.Index(src, sig)
	if i < 0 {
		return ""
	}
	rest := src[i:]
	if j := strings.Index(rest, "\n}\n"); j > 0 {
		return rest[:j]
	}
	return rest
}

// envVarsRead extracts the TG_-prefixed names passed to get(...) inside a function body.
func envVarsRead(body string) []string {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(body, `get("`)[1:] {
		if k := strings.Index(part, `"`); k > 0 {
			name := part[:k]
			if strings.HasPrefix(name, "TG_") && !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	return out
}

func containsStr(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// composeFile reads deploy/docker-compose.yml. It is the single definition for this package: an
// identical helper was added independently in egress_posture_test.go on a parallel branch, and the two
// collided the moment both merged ("composeFile redeclared in this block"). Two green pipelines, each
// correct against the main it was cut from, produced a red main together. Add package-level helpers here,
// not beside their first caller.
func composeFile(t *testing.T) string {
	t.Helper()
	s, err := readRepoFile("docker-compose.yml")
	if err != nil {
		t.Fatalf("read docker-compose.yml: %v", err)
	}
	return s
}
