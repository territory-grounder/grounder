package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// TIER NAMES ARE NOT MODEL IDENTITY (TG-356).
//
// A litellm `model_name` is an alias. Several can point at one upstream model, and on this estate several
// do — measured 2026-08-06 against the running gateway:
//
//	primary            -> openai/opus-cc
//	fast               -> openai/opus-cc
//	opus-cc            -> openai/opus-cc
//	judge              -> deepseek/deepseek-v4-pro
//	fallback-deepseek  -> deepseek/deepseek-v4-pro
//
// That matters wherever TG asserts two tiers are INDEPENDENT. The frontier cross-check refuses to arm when
// the frontier tier equals the local judge tier — the right intent — but it compares the alias STRINGS, so
// `judge` vs `fallback-deepseek` reads as two different models and is one. Arming on that pair would give
// the judge itself as its own independent anchor, silently, which is the exact blind spot the cross-check
// exists to close.
//
// This resolves an alias to the upstream model the gateway will actually call, so independence can be
// asserted on identity rather than on spelling.

// modelInfoResponse is litellm's GET /v1/model/info shape. Only the two fields TG needs are decoded; the
// endpoint carries per-model cost and provider metadata TG has no business reading.
type modelInfoResponse struct {
	Data []struct {
		ModelName     string `json:"model_name"`
		LitellmParams struct {
			Model string `json:"model"`
		} `json:"litellm_params"`
	} `json:"data"`
}

// ResolveTiers maps each requested alias to the upstream model the gateway would call.
//
// A tier the gateway does not serve is OMITTED rather than mapped to "" — an empty string would compare
// equal to another unresolvable tier and manufacture a false "same model" verdict, which for an
// independence check fails in the dangerous direction (refusing a legitimate anchor is annoying; admitting
// a self-grading one is the defect).
func (g *Gateway) ResolveTiers(ctx context.Context, tiers ...string) (map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(g.BaseURL, "/")+"/v1/model/info", nil)
	if err != nil {
		return nil, fmt.Errorf("model: build model-info request: %w", err)
	}
	key, err := g.APIKeyRef.Resolve()
	if err != nil {
		return nil, fmt.Errorf("model: resolve gateway key: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)

	hc := g.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("model: model-info: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("model: model-info: gateway status %d", resp.StatusCode)
	}
	var out modelInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("model: decode model-info: %w", err)
	}

	want := make(map[string]bool, len(tiers))
	for _, t := range tiers {
		if t = strings.TrimSpace(t); t != "" {
			want[t] = true
		}
	}
	res := make(map[string]string, len(want))
	for _, m := range out.Data {
		if want[m.ModelName] && m.LitellmParams.Model != "" {
			res[m.ModelName] = m.LitellmParams.Model
		}
	}
	return res, nil
}

// SameUpstreamModel reports whether two tier aliases resolve to the SAME upstream model.
//
// The third return says whether the question was actually ANSWERED. A caller must not read a false `same`
// as proof of independence when the gateway could not be reached or did not serve one of the tiers — that
// is an unverified claim, not a negative one, and the difference is the whole point of this function.
func (g *Gateway) SameUpstreamModel(ctx context.Context, a, b string) (same bool, resolved bool, err error) {
	m, err := g.ResolveTiers(ctx, a, b)
	if err != nil {
		return false, false, err
	}
	ra, oka := m[a]
	rb, okb := m[b]
	if !oka || !okb {
		return false, false, nil
	}
	return ra == rb, true, nil
}
