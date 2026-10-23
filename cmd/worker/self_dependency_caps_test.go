package main

import (
	"testing"

	"github.com/territory-grounder/grounder/core/estate"
)

// TG-394 slice 2 — TG's OTHER dependency capabilities (secrets/model/tracker/notifier) are declared as endpoint
// URLs, resolved to their host and covered under the same {capability} concentration metric.

func TestSelfDepHostOfURL(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"https://openbao:8200", "openbao"},
		{"http://litellm:4000", "litellm"},
		{"https://tracker.example.net:443/api", "tracker.example.net"},
		{"matrix:8448", "matrix"}, // bare host:port, no scheme
		{"", ""},
		{"   ", ""},
	} {
		if got := selfDepHostOfURL(c.in); got != c.want {
			t.Errorf("selfDepHostOfURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSelfDepCapabilities_SkipsUnsetEndpoints(t *testing.T) {
	env := map[string]string{
		"TG_OPENBAO_ADDR": "https://openbao:8200",
		"TG_LITELLM_URL":  "http://litellm:4000",
		// tracker + notifier endpoints deliberately unset
	}
	getenv := func(k, d string) string {
		if v, ok := env[k]; ok {
			return v
		}
		return d
	}
	byName := map[string][]string{}
	for _, c := range selfDepCapabilities(getenv, []string{"dc1*"}) {
		byName[c.Name] = c.Globs
	}
	if g, ok := byName[selfDepCapabilityJournal]; !ok || len(g) != 1 || g[0] != "dc1*" {
		t.Errorf("journal globs = %v (ok=%v), want [dc1*]", g, ok)
	}
	if g := byName[selfDepCapabilitySecrets]; len(g) != 1 || g[0] != "openbao" {
		t.Errorf("secrets globs = %v, want [openbao]", g)
	}
	if g := byName[selfDepCapabilityModel]; len(g) != 1 || g[0] != "litellm" {
		t.Errorf("model globs = %v, want [litellm]", g)
	}
	if _, ok := byName[selfDepCapabilityTracker]; ok {
		t.Error("tracker present despite TG_YOUTRACK_URL unset — an unset endpoint must contribute NOTHING (no phantom coverage)")
	}
	if _, ok := byName[selfDepCapabilityNotifier]; ok {
		t.Error("notifier present despite both channel URLs unset")
	}
}

// The model gateway is dialed even when TG_LITELLM_URL is unset (main.go defaults it to the compose gateway), so
// the concentration metric must count the model dependency in that case too — an implicit-var deploy must NOT read
// as "TG has no model dependency". The opt-in endpoints (secrets/tracker/notifier) still contribute nothing when
// unset. Killing mutation: revert the TG_LITELLM_URL default to "" and the model capability vanishes here.
func TestSelfDepCapabilities_ModelDefaultsToGateway(t *testing.T) {
	// EVERY endpoint var unset — only the always-dialed model gateway should surface.
	getenv := func(_, d string) string { return d }
	byName := map[string][]string{}
	for _, c := range selfDepCapabilities(getenv, nil) {
		byName[c.Name] = c.Globs
	}
	if g := byName[selfDepCapabilityModel]; len(g) != 1 || g[0] != "litellm" {
		t.Errorf("model globs = %v, want [litellm] from the default gateway when TG_LITELLM_URL is unset", g)
	}
	for _, optIn := range []string{selfDepCapabilitySecrets, selfDepCapabilityTracker, selfDepCapabilityNotifier} {
		if _, ok := byName[optIn]; ok {
			t.Errorf("%s present despite its endpoint being unset — only the model gateway defaults", optIn)
		}
	}
}

func TestSelfDepCapabilities_NotifierBothChannels(t *testing.T) {
	env := map[string]string{
		"TG_MATRIX_HOMESERVER": "https://matrix:8448",
		"TG_MATTERMOST_URL":    "https://mattermost:8065",
	}
	getenv := func(k, d string) string {
		if v, ok := env[k]; ok {
			return v
		}
		return d
	}
	for _, c := range selfDepCapabilities(getenv, nil) {
		if c.Name == selfDepCapabilityNotifier {
			if len(c.Globs) != 2 || c.Globs[0] != "matrix" || c.Globs[1] != "mattermost" {
				t.Errorf("notifier globs = %v, want [matrix mattermost] (sorted, both channels)", c.Globs)
			}
			return
		}
	}
	t.Error("notifier capability missing despite both channel URLs set")
}

func TestSelfDepConcentrationMultiJob_EmitsPerCapability(t *testing.T) {
	// The multi-reader emits each capability's samples under its own {capability} label. journal (dep-*) has 3
	// hosts on one parent → a concentration; secrets (dep-a) has 1 → the coverage pair only. Both must appear.
	holder := estate.NewHolder(gtestGraph(t))
	caps := []selfDepCapability{
		{Name: selfDepCapabilityJournal, Globs: []string{"dep-*"}},
		{Name: selfDepCapabilitySecrets, Globs: []string{"dep-a"}},
	}
	samples := startSelfDepConcentrationMultiJob(holder, caps)()
	if s, ok := findSample(samples, "tg_self_dependency_concentration", map[string]string{"capability": selfDepCapabilityJournal, "parent": "pve03"}); !ok || s.Value != 3 {
		t.Errorf("want journal concentration{pve03}=3, got %+v (ok=%v)", s, ok)
	}
	if s, ok := findSample(samples, "tg_self_dependency_globs_declared", map[string]string{"capability": selfDepCapabilitySecrets}); !ok || s.Value != 1 {
		t.Errorf("want secrets globs_declared=1, got %+v (ok=%v)", s, ok)
	}
	if _, ok := findSample(samples, "tg_self_dependency_concentration", map[string]string{"capability": selfDepCapabilitySecrets}); ok {
		t.Error("secrets (single dependency host) must have NO concentration series — one host cannot be concentrated")
	}
}

// The nil-holder / empty-caps reader must be a safe no-op (parity with the single-capability job).
func TestSelfDepConcentrationMultiJob_NilSafe(t *testing.T) {
	if got := startSelfDepConcentrationMultiJob(nil, nil)(); got != nil {
		t.Errorf("nil holder reader emitted %v, want nil", got)
	}
	if got := startSelfDepConcentrationMultiJob(estate.NewHolder(gtestGraph(t)), nil)(); got != nil {
		t.Errorf("empty-caps reader emitted %v, want nil", got)
	}
}
