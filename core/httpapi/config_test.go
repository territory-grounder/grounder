package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/cpconfig"
)

type fakeConfig struct {
	vals []cpconfig.Value
	err  error
}

func (f fakeConfig) Resolve(context.Context) ([]cpconfig.Value, error) { return f.vals, f.err }

func TestConfigHandler(t *testing.T) {
	vals := []cpconfig.Value{
		{Key: cpconfig.Key{Name: "safety.mutation_enabled", Description: "master switch", Law: true, ConsoleWritable: false}, Value: "off", Source: cpconfig.SourceLaw},
		{Key: cpconfig.Key{Name: "gateway.litellm_url", Description: "gw url", Law: false, ConsoleWritable: true}, Value: "http://litellm:4000", Source: cpconfig.SourceEnv},
	}
	d := Deps{Config: fakeConfig{vals: vals}}

	r := httptest.NewRequest(http.MethodGet, "/v1/config", nil)
	w := httptest.NewRecorder()
	d.configHandler(w, r, auth.Principal{SourceID: "operator:kyriakos"})
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var page ConfigPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Config) != 2 {
		t.Fatalf("want 2 knobs, got %d", len(page.Config))
	}
	law := page.Config[0]
	if law.Name != "safety.mutation_enabled" || !law.Law || law.ConsoleWritable || law.Source != "law" || law.Value != "off" {
		t.Fatalf("law knob wrong: %+v", law)
	}
	env := page.Config[1]
	if env.Law || !env.ConsoleWritable || env.Source != "env" {
		t.Fatalf("env knob wrong: %+v", env)
	}

	// nil resolver → 503 fail-closed
	w2 := httptest.NewRecorder()
	(Deps{}).configHandler(w2, httptest.NewRequest(http.MethodGet, "/v1/config", nil), auth.Principal{SourceID: "operator:k"})
	if w2.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil resolver must be 503, got %d", w2.Code)
	}

	// store error → 503
	w3 := httptest.NewRecorder()
	(Deps{Config: fakeConfig{err: context.DeadlineExceeded}}).configHandler(w3, httptest.NewRequest(http.MethodGet, "/v1/config", nil), auth.Principal{SourceID: "operator:k"})
	if w3.Code != http.StatusServiceUnavailable {
		t.Fatalf("resolve error must be 503, got %d", w3.Code)
	}

	// non-GET → 405
	w4 := httptest.NewRecorder()
	d.configHandler(w4, httptest.NewRequest(http.MethodPost, "/v1/config", nil), auth.Principal{SourceID: "operator:k"})
	if w4.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST must be 405, got %d", w4.Code)
	}
}
