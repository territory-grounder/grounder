package deploy

// TG-91 postmortem (2026-08-14): the Slurp'it compose block shipped
// `TG_SLURPIT_TOKEN_REF: ${TG_SLURPIT_TOKEN_REF:-env:SLURPIT_TOKEN}` — a secret-shaped default on a
// source whose URL default is EMPTY (dark by default). On the live triage plane, which runs
// `secret policy=enforce` with a bao: backend, that env:-scheme default was a boot-refusing
// violation for a feature the box does not run, and the plane was down ~2 minutes until the box
// .env overrode it. CI could not see it: the enforce posture is a deployment property.
//
// The rule this pins, mechanically: for every TG_<X>_URL whose compose default is empty (an
// OPTIONAL, dark-by-default source), the sibling TG_<X>_TOKEN_REF default must be empty too. The
// ref may exist only when the source is armed — an operator setting the URL sets the ref beside
// it, in the deployment's own scheme vocabulary. Always-on secrets (session key, admin token…)
// keep their env: defaults deliberately: every real deployment overrides them, and a generic dev
// boot needs SOME resolvable default.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

var (
	urlDefaultRe = regexp.MustCompile(`^\s*(TG_[A-Z0-9_]+)_URL:\s*\$\{(TG_[A-Z0-9_]+)_URL:-\}\s*$`)
	refLineRe    = regexp.MustCompile(`^\s*(TG_[A-Z0-9_]+)_TOKEN_REF:\s*\$\{(TG_[A-Z0-9_]+)_TOKEN_REF:-([^}]*)\}\s*$`)
)

func TestDarkSourcesCarryNoSecretShapedDefaults(t *testing.T) {
	b, err := os.ReadFile("docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(b), "\n")

	darkSources := map[string]bool{} // base name (TG_<X>) whose _URL default is empty
	refDefaults := map[string]string{}
	for _, ln := range lines {
		if m := urlDefaultRe.FindStringSubmatch(ln); m != nil && m[1] == m[2] {
			darkSources[m[1]] = true
		}
		if m := refLineRe.FindStringSubmatch(ln); m != nil && m[1] == m[2] {
			// Last write wins per service block; any non-empty default anywhere is the hazard,
			// so record the worst (non-empty over empty).
			if cur, ok := refDefaults[m[1]]; !ok || cur == "" {
				refDefaults[m[1]] = m[3]
			}
		}
	}
	if len(darkSources) == 0 {
		t.Fatal("found NO dark-by-default sources at all — the compose shape moved and this " +
			"guard is scanning nothing; an oracle that cannot see its subject proves nothing")
	}

	for base := range darkSources {
		if def, ok := refDefaults[base]; ok && def != "" {
			t.Errorf("%s_URL defaults to empty (dark source) but %s_TOKEN_REF defaults to %q — "+
				"a dark source must not ship a secret-shaped ref default: on an enforcing "+
				"deployment it is a boot-refusing violation for a feature the box does not run "+
				"(the TG-91 outage shape). Make the ref default empty; the operator sets it when "+
				"arming the source.", base, base, def)
		}
	}
}
