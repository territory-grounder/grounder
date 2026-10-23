// Package netbox is the loadable NetBox CMDB module (spec/008 REQ-810, T-008-14).
//
// It implements adapters/cmdb.CMDB: it resolves devices, VMs, IP addresses, VLANs, and interfaces by id
// and is the authoritative entity-resolution source every ingested payload is re-read against before
// dispatch (INV-05 — the payload is a claim, the NetBox record is the fact). It also exposes an entity's
// changelog to the triage context. The HTTP transport is injectable (a Doer) so the oracle drives the real
// code path against a fake NetBox. The API token is a secret reference, resolved per request, never a
// literal (INV-13).
//
// Provenance: [O] INV-05/INV-13, spec/008.
package netbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	cmdb "github.com/territory-grounder/grounder/adapters/cmdb"
	"github.com/territory-grounder/grounder/core/config"
)

// SourceType is the vendor slug this module serves.
const SourceType = "netbox"

// Doer is the minimal HTTP contract; *http.Client satisfies it, and tests inject a fake NetBox.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Module is the NetBox CMDB adapter. Construct with New.
type Module struct {
	baseURL  string
	tokenRef config.SecretRef
	http     Doer
}

// Option configures a Module.
type Option func(*Module)

// WithHTTPClient injects the HTTP transport (a fake in tests, *http.Client in production).
func WithHTTPClient(d Doer) Option { return func(m *Module) { m.http = d } }

// New builds a NetBox module for a base URL and an API-token secret reference (e.g. "env:NETBOX_TOKEN").
func New(baseURL string, tokenRef config.SecretRef, opts ...Option) *Module {
	m := &Module{baseURL: strings.TrimRight(baseURL, "/"), tokenRef: tokenRef, http: http.DefaultClient}
	for _, o := range opts {
		o(m)
	}
	return m
}

// SourceType implements adapters/cmdb.CMDB.
func (m *Module) SourceType() string { return SourceType }

// compile-time proof the module satisfies the stable cmdb interface.
var _ cmdb.CMDB = (*Module)(nil)

// do issues an authenticated GET against the NetBox REST API. NetBox uses a "Token <token>" scheme; the
// token is resolved from its secret reference at call time (INV-13). A non-2xx response is an error.
func (m *Module) do(ctx context.Context, path string) ([]byte, error) {
	token, err := m.tokenRef.Resolve()
	if err != nil {
		return nil, fmt.Errorf("netbox: resolve token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Token "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("netbox: GET %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// Resolve re-reads the authoritative entity of the given kind by id. The claimed fields of an ingested
// payload are reconciled against this record before dispatch (INV-05).
func (m *Module) Resolve(ctx context.Context, kind, id string) (cmdb.Entity, error) {
	if id == "" {
		return cmdb.Entity{}, fmt.Errorf("netbox: empty entity id")
	}
	ep, ok := endpointFor(kind)
	if !ok {
		return cmdb.Entity{}, fmt.Errorf("netbox: unsupported entity kind %q", kind)
	}
	body, err := m.do(ctx, ep+id+"/")
	if err != nil {
		return cmdb.Entity{}, err
	}
	return toEntity(kind, id, body)
}

// Changelog returns the recorded object-changes for an entity, exposing its history to the triage context.
func (m *Module) Changelog(ctx context.Context, id string) ([]Change, error) {
	if id == "" {
		return nil, fmt.Errorf("netbox: empty entity id")
	}
	body, err := m.do(ctx, "/api/extras/object-changes/?changed_object_id="+id)
	if err != nil {
		return nil, err
	}
	var page struct {
		Results []Change `json:"results"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, fmt.Errorf("netbox: malformed changelog: %w", err)
	}
	return page.Results, nil
}
