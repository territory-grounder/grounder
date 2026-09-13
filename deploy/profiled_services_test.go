package deploy

// A SERVICE BEHIND A COMPOSE PROFILE THE DEPLOY NEVER NAMES IS NEVER REDEPLOYED (TG-326).
//
// Compose skips profiled services unless the profile is named on the command line. It does so SILENTLY —
// no warning, no non-zero exit — so `docker compose up -d` succeeds, the deploy job reports success, the
// pipeline goes green, and `docker ps` shows the service Up. Every signal an operator has says the estate
// is running main. Only comparing image tags container-by-container says otherwise.
//
// Measured on dc1tg01, 2026-08-05:
//
//	worker         -> grounder/worker:98a32024   (up 52 minutes)
//	worker-actuate -> grounder/worker:85770c56   (up 5 hours)
//
// Same image, different commits, because worker-actuate is declared `profiles: ["split-planes"]` and the
// AWX deploy playbook runs a plain `up -d`. That is not a one-off miss, it is the steady state — and it
// lands on the plane with the MOST dangerous authority, the one that mutates the estate. A fix to the
// interceptor chain or the chokepoint can be merged, deployed, reported green, and still not be running
// where it matters most.
//
// This guard makes the set of never-deployed services explicit and refuses to let it grow quietly.

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// knownUndeployedProfiles are profiles no automated deploy path names, recorded WITH the reason rather
// than left to be rediscovered. This is a baseline, not an amnesty: the test fails if the set grows.
//
// Removing an entry requires fixing the deploy, not editing this map.
// EMPTY IS THE CORRECT STATE, and it is load-bearing that it stays checkable rather than becoming a
// habit. `on-box-sidecar` (TG-413) lived here while the model sidecar was staged but not cut over; the
// entry's own instruction was "remove this when the cutover lands". It landed and was verified live on
// dc1tg01 on 2026-08-07, the profile came off all three sidecar services, and the rot check below is
// what forced this edit — deleting the profile alone turned the map into a claim about a profile that no
// longer exists, and the test said so.
var knownUndeployedProfiles = map[string]string{
	// TG-420 slice 1: the litellm->provider egress proxy (tg-egress-proxy) ships UNARMED and OBSERVE-only,
	// so it is deliberately named by NO deploy path — a bare `up -d` must NOT start it. Arming observe is an
	// owner-gated, deploy-time act (add `egress-proxy` to COMPOSE_PROFILES in the box .env AND set
	// TG_LITELLM_HTTPS_PROXY; see deploy/.env.example and deploy/host/tg-egress-proxy-observe-drill.sh).
	// Unlike the split-planes gap this guard was filed about, "not redeployed" is the CORRECT state here:
	// the proxy carries no state and no traffic until armed. REMOVE this entry when slice-2 enforcement is
	// armed and the deploy names the profile, so the proxy is then redeployed like any other service.
	"egress-proxy": "TG-420 slice 1 — egress proxy is owner-armed, observe-only; intentionally not deployed " +
		"by default. Remove when slice-2 enforcement lands and the deploy names the profile.",
}

func composeProfiles(t *testing.T, path string) map[string][]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc struct {
		Services map[string]struct {
			Profiles []string `yaml:"profiles"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	out := map[string][]string{}
	for name, svc := range doc.Services {
		for _, p := range svc.Profiles {
			out[p] = append(out[p], name)
		}
	}
	return out
}

func TestEveryComposeProfileIsNamedByADeployPath(t *testing.T) {
	ci, err := os.ReadFile("../.gitlab-ci.yml")
	if err != nil {
		t.Fatalf("read .gitlab-ci.yml: %v", err)
	}
	// `--profile <name>` as it would appear in any deploy invocation — but COMMENT LINES DO NOT COUNT.
	//
	// The first version of this guard scanned the raw file, and its own killing mutation exposed the flaw:
	// removing the real `--profile tls` from the deploy command left the guard GREEN, because the comment
	// above that command explaining the flag still contained the string. A guard satisfied by prose about
	// a control, rather than by the control, is precisely the failure mode this repo keeps rediscovering.
	named := map[string]bool{}
	profileRe := regexp.MustCompile(`--profile\s+([a-zA-Z0-9_-]+)`)
	for _, line := range strings.Split(string(ci), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		for _, m := range profileRe.FindAllStringSubmatch(line, -1) {
			named[m[1]] = true
		}
	}

	// THE SECOND, STRONGER DEPLOY PATH: COMPOSE_PROFILES in the environment.
	//
	// `--profile` only covers the one invocation someone remembered to edit. COMPOSE_PROFILES covers every
	// path compose is ever driven through — the AWX playbook's plain `up -d`, a hand-run `docker compose
	// restart`, a fresh clone — because compose reads it from the environment. deploy/.env.example is the
	// template every deployment's .env is copied from, so a profile named there is named everywhere.
	//
	// This is what closed the split-planes gap: worker-actuate sat 97 commits behind, unmetered and
	// unscraped, because the playbook is not in this repo and could not be edited from here.
	envExample, err := os.ReadFile(".env.example")
	if err != nil {
		t.Fatalf("read deploy/.env.example: %v", err)
	}
	var sawProfilesVar bool
	for _, line := range strings.Split(string(envExample), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || !strings.HasPrefix(trimmed, "COMPOSE_PROFILES=") {
			continue
		}
		sawProfilesVar = true
		for _, p := range strings.FieldsFunc(strings.TrimPrefix(trimmed, "COMPOSE_PROFILES="),
			func(r rune) bool { return r == ',' || r == ' ' }) {
			named[p] = true
		}
	}
	if !sawProfilesVar {
		t.Error("deploy/.env.example declares no COMPOSE_PROFILES line. It is the template every " +
			"deployment's .env is copied from; without it a fresh deployment silently runs without the " +
			"profiled services, which is how worker-actuate ended up 97 commits behind.")
	}

	profiles := map[string][]string{}
	var scanned int
	for _, f := range []string{"docker-compose.yml", "claude-proxy/compose.yml"} {
		for p, svcs := range composeProfiles(t, f) {
			profiles[p] = append(profiles[p], svcs...)
			scanned++
		}
	}
	// Vacuity floor: if nothing in either compose file uses profiles any more, this guard is asserting
	// nothing. Say so rather than passing.
	if scanned == 0 {
		t.Fatal("no compose service declares a profile — this guard scanned NOTHING and would have passed. " +
			"If profiles were genuinely removed, delete this test deliberately; do not let it go quiet.")
	}

	var unnamed []string
	for p := range profiles {
		if named[p] {
			continue
		}
		if _, known := knownUndeployedProfiles[p]; known {
			continue
		}
		unnamed = append(unnamed, p)
	}
	sort.Strings(unnamed)
	for _, p := range unnamed {
		sort.Strings(profiles[p])
		t.Errorf("compose profile %q (services: %s) is named by NO deploy path in .gitlab-ci.yml.\n"+
			"Compose skips profiled services silently, so these will keep whatever image and config they "+
			"were last started with while the deploy reports success. Either name the profile in the "+
			"deploy invocation, or record it in knownUndeployedProfiles with the reason and a ticket.",
			p, strings.Join(profiles[p], ", "))
	}

	// The baseline must not rot either: an entry naming a profile that no longer exists is dead weight
	// that makes the map look like it is doing more work than it is.
	for p := range knownUndeployedProfiles {
		if _, exists := profiles[p]; !exists {
			t.Errorf("knownUndeployedProfiles lists %q but no compose service declares it — remove the "+
				"stale entry so the baseline reflects the real gap", p)
		}
	}
}
