package deploy

// TG-455: every entry in litellm_settings.callbacks of the REAL config must RESOLVE — a docker-compose mount
// puts the module at /etc/litellm/<module>.py, that source file exists, and it defines <instance> at column 0.
//
// Why a guard exists at all: LiteLLM boots fine whether or not a callback resolves, so a rename/move of the
// callback file (deploy/litellm-forward-user.py), an edit/delete of its docker-compose volume line, or a typo
// in the config reference silently drops the hook — for TG-319 that means the user->caller forwarding reverts
// to caller:"" with NO build- or boot-time signal. This is a pure-Go test on purpose: it runs under
// `go test ./deploy/...` in the unconditional build-test CI job (no `changes:` gate, no -run filter), reads
// the REAL deploy/ files, and cannot skip (no python / PyYAML dependency). An earlier draft ran the check as a
// python validator gated behind the litellm-config job's `changes:[deploy/litellm-config.yaml]` filter and a
// PyYAML-skipping Go exec — both CI-UNREACHABLE for the exact rename scenario this protects (TG-455 review).

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// callbackMounts maps container module name -> repo-relative source, from the litellm service's
// /etc/litellm/*.py volume mounts. Handles BOTH short-form ("src:dst:opts") and long-form ({source,target})
// volume entries, so a future compose-syntax change cannot make a real mount invisible (review #5).
func callbackMounts(t *testing.T, composePath string) map[string]string {
	t.Helper()
	var doc struct {
		Services struct {
			Litellm struct {
				Volumes []yaml.Node `yaml:"volumes"`
			} `yaml:"litellm"`
		} `yaml:"services"`
	}
	b, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatalf("read %s: %v", composePath, err)
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse %s: %v", composePath, err)
	}
	out := map[string]string{}
	add := func(src, dst string) {
		if strings.HasPrefix(dst, "/etc/litellm/") && strings.HasSuffix(dst, ".py") {
			out[strings.TrimSuffix(filepath.Base(dst), ".py")] = src
		}
	}
	for _, v := range doc.Services.Litellm.Volumes {
		if v.Kind == yaml.ScalarNode {
			parts := strings.Split(v.Value, ":")
			if len(parts) >= 2 {
				add(parts[0], parts[1])
			}
			continue
		}
		var lf struct {
			Source string `yaml:"source"`
			Target string `yaml:"target"`
		}
		if err := v.Decode(&lf); err == nil {
			add(lf.Source, lf.Target)
		}
	}
	return out
}

func callbacksOf(t *testing.T, configPath string) []string {
	t.Helper()
	var cfg struct {
		LitellmSettings struct {
			Callbacks []string `yaml:"callbacks"`
		} `yaml:"litellm_settings"`
	}
	b, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read %s: %v", configPath, err)
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("parse %s: %v", configPath, err)
	}
	return cfg.LitellmSettings.Callbacks
}

// resolveCallbackErrs is the check. deployDir holds litellm-config.yaml + docker-compose.yml + the mounted
// sources. It returns one message per unresolved callback (empty ⇒ all resolve).
func resolveCallbackErrs(t *testing.T, deployDir string) []string {
	t.Helper()
	mounts := callbackMounts(t, filepath.Join(deployDir, "docker-compose.yml"))
	var errs []string
	for _, cb := range callbacksOf(t, filepath.Join(deployDir, "litellm-config.yaml")) {
		dot := strings.LastIndex(cb, ".")
		if dot <= 0 || dot == len(cb)-1 {
			errs = append(errs, "callback "+cb+": not a 'module.instance' string")
			continue
		}
		mod, inst := cb[:dot], cb[dot+1:]
		src, ok := mounts[mod]
		if !ok {
			errs = append(errs, "callback "+cb+": no docker-compose litellm mount provides /etc/litellm/"+mod+".py")
			continue
		}
		body, err := os.ReadFile(filepath.Join(deployDir, strings.TrimPrefix(src, "./")))
		if err != nil {
			errs = append(errs, "callback "+cb+": mounted source "+src+" not readable: "+err.Error())
			continue
		}
		if !regexp.MustCompile(`(?m)^`+regexp.QuoteMeta(inst)+`\s*=`).Match(body) {
			errs = append(errs, "callback "+cb+": "+inst+" not defined at top level of "+src)
		}
	}
	return errs
}

// THE ARMED GUARD: the real deploy/ config's callbacks must all resolve. This is what fails the build on a
// rename/move/drop. "." is the deploy package dir at test time.
func TestRealConfigCallbacksResolve(t *testing.T) {
	if len(callbacksOf(t, "litellm-config.yaml")) == 0 {
		t.Skip("the real config declares no callbacks — nothing to guard (not a failure; the guard is dormant)")
	}
	if errs := resolveCallbackErrs(t, "."); len(errs) > 0 {
		t.Fatalf("a litellm callback no longer resolves — a rename/move/drop silently disables it "+
			"(e.g. TG-319 caller forwarding reverting to caller:\"\"):\n  %s", strings.Join(errs, "\n  "))
	}
}

// THE ORACLE + KILLING MUTATION: the check must actually CATCH a broken callback and PASS a resolving one.
// Gut resolveCallbackErrs (return nil) and the two "must be caught" arms go green — a guard that cannot fail.
func TestCallbackCheckCatchesBrokenCallback(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("docker-compose.yml", "services:\n  litellm:\n    volumes:\n      - ./forward.py:/etc/litellm/forward_user.py:ro\n")
	write("forward.py", "forward_user_instance = object()\n")

	write("litellm-config.yaml", "litellm_settings:\n  callbacks: [\"forward_user.not_defined_here\"]\n")
	if errs := resolveCallbackErrs(t, dir); len(errs) == 0 {
		t.Fatal("a callback whose instance is not defined in the mounted module must be caught")
	}
	write("litellm-config.yaml", "litellm_settings:\n  callbacks: [\"nowhere.some_instance\"]\n")
	if errs := resolveCallbackErrs(t, dir); len(errs) == 0 {
		t.Fatal("a callback whose module is not mounted into /etc/litellm must be caught")
	}
	// The control against over-refusal: a genuinely resolving callback must pass.
	write("litellm-config.yaml", "litellm_settings:\n  callbacks: [\"forward_user.forward_user_instance\"]\n")
	if errs := resolveCallbackErrs(t, dir); len(errs) != 0 {
		t.Fatalf("a resolving callback must not error: %v", errs)
	}
}
