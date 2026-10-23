package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/config"
)

// litellmModels serves the models read surface (REQ-514) by asking the LiteLLM gateway's control API
// for its model inventory (`/model/info`) with the master key resolved per request and discarded —
// the key never reaches the browser; the caller gets the gateway's verbatim JSON or fail-closed 503.
type litellmModels struct {
	baseURL string
	keyRef  config.SecretRef
	client  *http.Client
}

func newLitellmModels(baseURL string, keyRef config.SecretRef) litellmModels {
	return litellmModels{baseURL: baseURL, keyRef: keyRef, client: &http.Client{Timeout: 10 * time.Second}}
}

func (m litellmModels) ModelsUsage(ctx context.Context, _ auth.Principal) ([]byte, error) {
	key, err := m.keyRef.Resolve()
	if err != nil {
		return nil, fmt.Errorf("models: gateway key %s not resolvable: %w", m.keyRef, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.baseURL+"/model/info", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("models: gateway unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models: gateway answered %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	return body, nil
}
