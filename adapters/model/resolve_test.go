package model

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

// The real litellm /v1/model/info shape, with the aliasing this estate actually has (measured 2026-08-06).
const modelInfoFixture = `{"data":[
 {"model_name":"primary","litellm_params":{"model":"openai/opus-cc"}},
 {"model_name":"fast","litellm_params":{"model":"openai/opus-cc"}},
 {"model_name":"judge","litellm_params":{"model":"deepseek/deepseek-v4-pro"}},
 {"model_name":"fallback-deepseek","litellm_params":{"model":"deepseek/deepseek-v4-pro"}},
 {"model_name":"fallback-mistral","litellm_params":{"model":"mistral/mistral-large-latest"}}
]}`

func gatewayServing(t *testing.T, status int, body string) (*Gateway, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/model/info" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Setenv("TEST_GW_KEY", "k")
	return &Gateway{BaseURL: srv.URL, APIKeyRef: config.SecretRef("env:TEST_GW_KEY"), HTTP: srv.Client()}, srv.Close
}

// ★ TWO NAMES, ONE MODEL (TG-356).
//
// The frontier cross-check refuses to arm when the frontier tier "equals" the local judge tier — comparing
// alias STRINGS. `judge` and `fallback-deepseek` are different strings and the same upstream model, so that
// pair passes a name check and arms the judge as its own independent anchor.
func TestTwoAliasesOfOneModelAreNotIndependent(t *testing.T) {
	g, done := gatewayServing(t, 200, modelInfoFixture)
	defer done()

	same, resolved, err := g.SameUpstreamModel(context.Background(), "judge", "fallback-deepseek")
	if err != nil {
		t.Fatalf("SameUpstreamModel: %v", err)
	}
	if !resolved {
		t.Fatal("the gateway served both tiers but the check reported unresolved")
	}
	if !same {
		t.Error("judge and fallback-deepseek both resolve to deepseek/deepseek-v4-pro and were reported as " +
			"DIFFERENT models. A name comparison passes this pair, which is exactly how a self-grading " +
			"anchor gets armed past a guard that reports OK.")
	}
}

func TestGenuinelyDifferentVendorsAreIndependent(t *testing.T) {
	g, done := gatewayServing(t, 200, modelInfoFixture)
	defer done()
	same, resolved, err := g.SameUpstreamModel(context.Background(), "fallback-mistral", "primary")
	if err != nil || !resolved {
		t.Fatalf("resolve failed: resolved=%v err=%v", resolved, err)
	}
	if same {
		t.Error("mistral/mistral-large-latest and openai/opus-cc were reported as the same model — a check " +
			"this blunt would refuse every legitimate anchor and the cross-check could never arm")
	}
}

// AN UNANSWERABLE QUESTION MUST NOT READ AS "INDEPENDENT". A tier the gateway does not serve resolves to
// nothing; returning same=false with resolved=true would let a caller treat "I could not check" as "I
// checked and they differ", which fails in the dangerous direction.
func TestAnUnservedTierReportsUnresolvedNotIndependent(t *testing.T) {
	g, done := gatewayServing(t, 200, modelInfoFixture)
	defer done()
	same, resolved, err := g.SameUpstreamModel(context.Background(), "fallback-mistral", "a-tier-that-does-not-exist")
	if err != nil {
		t.Fatalf("SameUpstreamModel: %v", err)
	}
	if resolved {
		t.Error("an unserved tier reported RESOLVED. The caller would then read same=false as proof of " +
			"independence, when the question was never answered.")
	}
	if same {
		t.Error("an unresolved pair reported same=true")
	}
}

// A gateway error is an error, never a quiet "independent".
func TestAGatewayErrorIsNotAnIndependenceVerdict(t *testing.T) {
	g, done := gatewayServing(t, 500, `{}`)
	defer done()
	if _, resolved, err := g.SameUpstreamModel(context.Background(), "judge", "primary"); err == nil || resolved {
		t.Errorf("a 500 from the gateway produced resolved=%v err=%v; it must surface as an error so the "+
			"caller logs UNVERIFIED rather than arming on a fabricated verdict", resolved, err)
	}
}

// Omission, not empty-string mapping: two unresolvable tiers must not both map to "" and compare equal.
func TestUnresolvableTiersAreOmittedNotBlank(t *testing.T) {
	g, done := gatewayServing(t, 200, modelInfoFixture)
	defer done()
	m, err := g.ResolveTiers(context.Background(), "ghost-a", "ghost-b")
	if err != nil {
		t.Fatalf("ResolveTiers: %v", err)
	}
	if len(m) != 0 {
		t.Errorf("unserved tiers produced %v; they must be OMITTED. Mapping them to \"\" makes two unknown "+
			"tiers compare EQUAL and manufactures a false same-model verdict.", m)
	}
}
