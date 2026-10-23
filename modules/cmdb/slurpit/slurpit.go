// Package slurpit is a read-only Slurp'it network-discovery estate source (TG-91).
//
// Slurp'it is a network-device inventory + config-discovery platform: it logs into the estate's switches,
// routers and firewalls and records their inventory (hostname, site, brand, OS) plus planned-vs-actual
// config. This module reads that inventory over Slurp'it's REST API and contributes estate edges — each
// discovered device becomes a graph node via its SITE membership, and (best-effort) a dependency edge to its
// upstream parent — seeding the causal estate graph the prediction gate reasons over alongside NetBox, PVE
// and LibreNMS. It is DISCOVERED reality rather than a hand-declared edge, but it is a periodic config scrape
// (planned-vs-actual drift) rather than a live control-plane probe, so its edges carry SourceSlurpit at 0.82
// — between declared (0.85) and learned (<=0.75), above the 0.80 ground-truth cutoff (see core/estate).
//
// ★ PLAIN HTTP, NOT TLS. Slurp'it is served over CLEARTEXT HTTP (the estate instance answers on port 80, no
// certificate). A scheme-less base URL therefore resolves to http://, NEVER https:// — see normalizeBaseURL.
// Assuming TLS would dial a cleartext port and fail with a handshake error that reads as a certificate fault,
// sending an operator to fix a problem that does not exist. This is the inverse of the NetBox/PVE modules,
// which serve HTTPS and refuse to be worked around onto http.
//
// The transport is an injectable Doer so the oracle drives the real code path against a fake Slurp'it, and
// the API token is a secret reference resolved per request, never a literal (INV-13). The read is bounded per
// invocation (INV-08): a fixed page size and a hard page cap, so an oversized instance cannot mint an
// unbounded estate in one refresh.
//
// Provenance: [O] INV-08/INV-13, spec/008 (extension — TG-91 is a future estate source beyond the day-1
// fleet). API shape grounded in the Slurp'it Python SDK (gitlab.com/slurpit.io/slurpit_sdk): GET /api/devices
// with offset/limit pagination and a Bearer token; device fields hostname/fqdn/site/parent.
package slurpit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/estate"
)

// SourceType is the vendor slug this source serves.
const SourceType = "slurpit"

// devicesPath is the ONE endpoint this source reads — the device inventory. It is a constant rather than a
// literal at each call site so the estate refresh (Edges) and the console's TEST button (SelfTest) cannot
// drift onto different endpoints: a probe that exercised a different path from the reader would certify a
// permission the reader never uses. Grounded in the Slurp'it SDK's deviceapi.get_devices (GET {base}/api/devices).
const devicesPath = "/api/devices"

// pageLimit and maxPages are the INV-08 per-invocation bound on the device surface Slurp'it turns into estate
// nodes. get_devices pages by offset/limit (SDK default limit 1000); this reader walks pages of pageLimit and
// STOPS after maxPages, so a single refresh materialises at most pageLimit*maxPages devices no matter how
// large — or how compromised — the instance is. The product (20,000) sits far above the ~700-node estate yet
// is finite: the bound exists to fail closed on an unbounded response, not to truncate a real estate.
const (
	pageLimit = 500
	maxPages  = 40
)

// Doer is the minimal HTTP contract; *http.Client satisfies it, and tests inject a fake Slurp'it.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// EstateSource reads Slurp'it device inventory and contributes estate edges. It is ALSO its own console
// probe (core/selftest.Tester, see selftest.go) — one object, one read path, so the probe cannot certify an
// endpoint the refresh loop does not use. Construct with New.
type EstateSource struct {
	baseURL  string
	tokenRef config.SecretRef
	http     Doer
	expected []string // alerts a cascade along a discovered dependency edge is expected to fire
}

// Option configures an EstateSource.
type Option func(*EstateSource)

// WithHTTPClient injects the HTTP transport (a fake in tests, *http.Client in production).
func WithHTTPClient(d Doer) Option { return func(s *EstateSource) { s.http = d } }

// WithExpectedAlerts stamps the given cascade alerts on every emitted dependency edge, so the verifier's
// "partial" branch has per-edge content (the estate carries per-edge expected alerts).
func WithExpectedAlerts(alerts ...string) Option {
	return func(s *EstateSource) { s.expected = alerts }
}

// New builds a Slurp'it estate source for a base URL and an API-token secret reference (e.g.
// "env:TG_SLURPIT_TOKEN"). The base URL is normalized to encode the plain-HTTP fact (normalizeBaseURL): a
// scheme-less host resolves to http://, never https://.
func New(baseURL string, tokenRef config.SecretRef, opts ...Option) *EstateSource {
	s := &EstateSource{baseURL: normalizeBaseURL(baseURL), tokenRef: tokenRef, http: http.DefaultClient}
	for _, o := range opts {
		o(s)
	}
	return s
}

// normalizeBaseURL encodes the PLAIN-HTTP FACT (TG-91). Slurp'it is served over cleartext HTTP; a base URL
// with no scheme resolves to http://, NEVER https://, because assuming TLS would dial a cleartext port and
// fail with a handshake error that reads as a certificate fault. An explicit http:// is kept; an explicit
// https:// is kept too (an operator MAY front Slurp'it with a TLS proxy — that is their deliberate act, the
// selftest names it if the handshake then fails), but the DEFAULT assumption is never TLS.
func normalizeBaseURL(raw string) string {
	s := strings.TrimRight(strings.TrimSpace(raw), "/")
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") {
		return "http://" + s
	}
	return s
}

// Source implements estate.EdgeSource.
func (s *EstateSource) Source() estate.Source { return estate.SourceSlurpit }

// slurpitDevice is the subset of a Slurp'it device record this source reads. Grounded in the SDK's Device
// model: `hostname` is the device identity, `fqdn` its domain-qualified form (the fallback identity),
// `site` its site membership, and `parent` its upstream device reference. Every other field the SDK carries
// (brand/device_os/serial/snmp_*/…) is inventory metadata the edge-centric estate graph has no node slot for,
// so it is deliberately not decoded.
type slurpitDevice struct {
	Hostname string `json:"hostname"`
	Fqdn     string `json:"fqdn"`
	Site     string `json:"site"`
	Parent   string `json:"parent"`
}

// do issues an authenticated GET against the Slurp'it REST API. Slurp'it uses a `Bearer <api_key>`
// Authorization scheme (SDK baseapi.py); the token is resolved from its secret reference at call time
// (INV-13). A non-2xx response is an error carrying the status in our own frame (statusFromDoError reads it).
func (s *EstateSource) do(ctx context.Context, path string) ([]byte, error) {
	token, err := s.tokenRef.Resolve()
	if err != nil {
		return nil, fmt.Errorf("slurpit: resolve token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("slurpit: GET %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// fetchDevices reads the device inventory, BOUNDED (INV-08). It walks pages of pageLimit following offset and
// stops on the first short page (fewer than pageLimit rows = the last page) or after maxPages, whichever comes
// first. The response is a bare JSON array of device objects (the SDK's get_devices → list[Device] shape); a
// body that is not one is a loud parse error, not a silent empty inventory.
func (s *EstateSource) fetchDevices(ctx context.Context) ([]slurpitDevice, error) {
	var all []slurpitDevice
	for page := 0; page < maxPages; page++ {
		offset := page * pageLimit
		path := fmt.Sprintf("%s?offset=%d&limit=%d", devicesPath, offset, pageLimit)
		body, err := s.do(ctx, path)
		if err != nil {
			return nil, err
		}
		var batch []slurpitDevice
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, fmt.Errorf("slurpit: malformed devices response: %w", err)
		}
		all = append(all, batch...)
		if len(batch) < pageLimit {
			return all, nil // short page — the inventory ends here
		}
	}
	// Hit the INV-08 page cap on an instance larger than pageLimit*maxPages: return the bounded prefix rather
	// than pulling unbounded. A refresh that contributes a bounded subset is safer than one that never returns.
	return all, nil
}

// compile-time proof the topology reader satisfies the estate edge-source seam.
var _ estate.EdgeSource = (*EstateSource)(nil)
