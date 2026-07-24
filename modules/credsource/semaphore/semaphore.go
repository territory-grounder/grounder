// Package semaphore is the native-Go Semaphore UI (semaphoreui/semaphore) credential connector
// (spec/016 T-016-8, REQ-1612, TG-88). It is a READ-ONLY INVENTORY + key-ASSIGNMENT source on the machine
// plane: it pulls Semaphore's projects → inventories → hosts/groups and maps each to a
// credential.SourceEntry whose Bundle carries SecretRef REFERENCES only — never a raw secret.
//
// WHY inventory-only (grounded in the Semaphore REST API reality, verified live against
// http://dc1semaphore01.example.net:3000, NOT an assumption): Semaphore's Key Store
// (GET /api/project/{id}/keys) holds SSH keys / login-passwords but the API is WRITE-ONLY for secret
// material — a read returns only {id,name,type} (type ∈ none|ssh|login_password), never the private key or
// password. So Semaphore is an INVENTORY + key-NAME source: the per-host connection identity lives in the
// ansible inventory TEXT (GET /api/project/{id}/inventory returns the inventory as a `inventory` string,
// type ∈ static [INI] | static-yaml [YAML] | file | terraform-workspace), and WHICH key applies is the
// inventory's ssh_key_id / become_key_id (resolved to a key NAME), or a per-host
// ansible_ssh_private_key_file / tg_secret_ref / tg_credential var — never a value. This connector resolves
// the OBJECT MODEL + connection metadata + WHICH key applies; the actual secret is dereferenced elsewhere
// (the OpenBao/Vault vault:/bao: scheme, or the native store: scheme) at USE TIME (INV-13). A host for which
// no key REFERENCE can be derived is skipped-with-record (fail closed: a blank or literal identity is never
// injected), while a transport/auth/decode failure fails the whole Sync closed.
//
// DISTROLESS-SAFE: net/http + crypto/tls with an optional private-CA cert; NO subprocess, no semaphore-cli,
// no ansible binary (INV-02). Auth is a long-lived API token supplied as a core/config.SecretRef
// (env:/file:/store:), sent as "Authorization: Bearer <token>" and NEVER logged (INV-13).
//
// Grounded in the Semaphore REST API (semaphoreui/semaphore api-docs.yml, live http on port 3000):
//   - GET /api/ping                          — unauthenticated health, returns "pong"; the safe live probe.
//   - GET /api/projects                      — array of {id,name,type,...}.
//   - GET /api/project/{id}/inventory        — array of {id,name,project_id,inventory,ssh_key_id,
//                                              become_key_id,type}; `inventory` is the ansible inventory TEXT.
//   - GET /api/project/{id}/keys             — array of {id,name,type}; NO secret material (write-only store).
// Unlike AWX, Semaphore list endpoints return PLAIN ARRAYS with no cursor/next link, so the connector never
// follows a server-supplied URL — every request URL is built from the configured base + a fixed path + an
// integer id read from the projects list. The off-host guard (resolveURL) is therefore defense-in-depth
// (parity with the AWX connector), applied to every request.
//
// Provenance: [O] INV-13/INV-05/INV-02, spec/016 (REQ-1612), TG-88.
package semaphore

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/credential"
)

// SourceType is the vendor slug this connector serves.
const SourceType = "semaphore"

// defaultTimeout bounds a single API round-trip so a hung Semaphore fails closed promptly.
const defaultTimeout = 20 * time.Second

// maxItems bounds any single list response (projects/inventories/keys) so a misbehaving backend can never
// make Sync allocate/loop unbounded (fail closed).
const maxItems = 100_000

// maxInventoryBytes caps a single inventory's `inventory` text blob before it is parsed. Real ansible
// inventories are small; a larger blob is a misconfiguration or an attack (a big-but-flat DoS payload, or a
// YAML alias bomb) and is refused before decode so the inventory is skipped fail-closed. yaml.v3 itself
// rejects billion-laughs alias expansion ("excessive aliasing"); this bound is the complementary defense
// against sheer input size for both the INI and YAML parsers.
const maxInventoryBytes = 1 << 20 // 1 MiB

// Doer is the minimal HTTP contract; *http.Client satisfies it and the oracle injects a fake Semaphore.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Client is a read-only Semaphore REST API client. It holds a Bearer API token (resolved from a SecretRef,
// cached after first use, never logged).
type Client struct {
	baseURL  string // e.g. "http://dc1semaphore01.example.net:3000" (no trailing slash, no /api)
	tokenRef config.SecretRef
	http     Doer

	mu     sync.Mutex
	cached string // cached resolved token; never logged
}

// Config constructs a Client. BaseURL is required. TokenRef (a Bearer API token reference) is required for
// the authenticated inventory pull; it may be empty ONLY for a Ping-only (health) client. CACertPath, if set,
// is trusted for TLS (Semaphore behind a private CA / HTTPS). HTTPClient overrides transport entirely (the
// oracle injects a fake).
type Config struct {
	BaseURL    string
	TokenRef   config.SecretRef
	CACertPath string
	HTTPClient Doer
}

// New builds a Client. It fails closed if the base URL is missing or a configured CA cert cannot be loaded.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, fmt.Errorf("semaphore: base URL is required")
	}
	c := &Client{baseURL: strings.TrimRight(cfg.BaseURL, "/"), tokenRef: cfg.TokenRef, http: cfg.HTTPClient}
	if c.http == nil {
		hc := &http.Client{Timeout: defaultTimeout}
		if cfg.CACertPath != "" {
			pem, err := os.ReadFile(cfg.CACertPath)
			if err != nil {
				return nil, fmt.Errorf("semaphore: read CA cert: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("semaphore: CA cert %q contains no valid certificate", cfg.CACertPath)
			}
			hc.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
		}
		c.http = hc
	}
	return c, nil
}

// token resolves the Bearer token once and caches it. An empty/unset token fails closed.
func (c *Client) token() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached != "" {
		return c.cached, nil
	}
	tok, err := c.tokenRef.Resolve()
	if err != nil {
		return "", fmt.Errorf("semaphore: resolve token: %w", err)
	}
	if strings.TrimSpace(tok) == "" {
		return "", fmt.Errorf("semaphore: token is empty (fail closed)")
	}
	c.cached = tok
	return tok, nil
}

// ---- HTTP plumbing ----------------------------------------------------------------------------------

// Ping hits GET /api/ping (unauthenticated). A 200 means Semaphore is reachable and serving. Used by the
// env-gated live integration test as the safe live assertion (no token needed).
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.raw(ctx, c.baseURL+"/api/ping", false)
	return err
}

// getJSON issues an authenticated GET against a path (joined onto the base) and decodes the JSON body into v.
// It fails closed on unreachable/denied/not-found/malformed.
func (c *Client) getJSON(ctx context.Context, urlPath string, v any) error {
	full, err := c.resolveURL(urlPath)
	if err != nil {
		return err
	}
	raw, err := c.raw(ctx, full, true)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("semaphore: decode %s: %w", scrub(urlPath), err)
	}
	return nil
}

// resolveURL joins a request path onto the client's base. Defense-in-depth (parity with the AWX connector):
// Semaphore returns no server-supplied follow links, but if a URL ever arrives absolute it MUST stay on the
// configured host+scheme so a compromised/MITM'd Semaphore can never redirect the pull — which carries the
// Bearer token — to another host. Any host/scheme mismatch fails closed (INV-13, credential safety).
func (c *Client) resolveURL(u string) (string, error) {
	switch {
	case strings.HasPrefix(u, "http://"), strings.HasPrefix(u, "https://"):
		nu, err := url.Parse(u)
		if err != nil {
			return "", fmt.Errorf("semaphore: unparseable url: %w", err)
		}
		bu, err := url.Parse(c.baseURL)
		if err != nil {
			return "", fmt.Errorf("semaphore: unparseable base url: %w", err)
		}
		if !strings.EqualFold(nu.Host, bu.Host) || !strings.EqualFold(nu.Scheme, bu.Scheme) {
			return "", fmt.Errorf("semaphore: refusing off-host url (got host %q scheme %q, expected %q %q)",
				nu.Host, nu.Scheme, bu.Host, bu.Scheme)
		}
		return u, nil
	case strings.HasPrefix(u, "/"):
		return c.baseURL + u, nil
	default:
		return c.baseURL + "/" + u, nil
	}
}

// raw issues one GET, attaching the Bearer token when authed, and returns the body on 2xx or an error (never
// a body with a nil error). The token is NEVER placed in an error message.
func (c *Client) raw(ctx context.Context, fullURL string, authed bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if authed {
		tok, terr := c.token()
		if terr != nil {
			return nil, terr
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("semaphore: GET %s: %w", scrub(fullURL), err) // unreachable → fail closed
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("semaphore: GET %s: status %d: %s", scrub(fullURL), resp.StatusCode, apiErr(out))
	}
	return out, nil
}

// ---- Semaphore object shapes (only the fields this connector reads) ---------------------------------

type semProject struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type semInventory struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	ProjectID   int    `json:"project_id"`
	Inventory   string `json:"inventory"`     // the ansible inventory TEXT (INI for "static", YAML for "static-yaml")
	SSHKeyID    int    `json:"ssh_key_id"`    // assigned Key Store key (connection secret) — a REFERENCE by id
	BecomeKeyID int    `json:"become_key_id"` // assigned become/privilege-escalation key — a REFERENCE by id
	Type        string `json:"type"`          // static | static-yaml | file | terraform-workspace
}

type semKey struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"` // none | ssh | login_password  (NO secret material is ever returned)
}

// ---- CredentialSource (REQ-1612) --------------------------------------------------------------------

// Source is a read-only credential.CredentialSource backed by Semaphore inventory. Each ansible host in a
// static/static-yaml inventory maps to a host-selector entry and each group carrying connection defaults maps
// to a group-selector entry; the Bundle's secret field is a SecretRef derived from the inventory's assigned
// Key Store key name (or a per-host ansible var), never a value (INV-13).
type Source struct {
	id        string
	client    *Client
	projectID int    // 0 = all projects; otherwise scope to a single project id
	refScheme string // scheme for emitted key references ("store", "vault", "bao", …)
	refPrefix string // prefix prepended to the derived key name
	refField  string // optional "#<field>" suffix (used by vault:/bao: KV v2 refs)

	mu      sync.Mutex
	skipped []SkipRecord // hosts/groups/inventories skipped this Sync, with a reason (coverage observability)
}

// SkipRecord records one host/group/inventory that could not be mapped to a valid Bundle and was skipped
// (fail closed: no blank identity injected). The reason is safe to log (never a secret).
type SkipRecord struct {
	Kind   credential.SelectorKind
	Name   string
	Reason string
}

// SourceConfig configures a Source. ID and Client are required. ProjectID (optional) scopes the pull to a
// single Semaphore project. RefScheme/RefPrefix/RefField build the emitted SecretRef from a key name (e.g.
// RefScheme "vault", RefPrefix "secret/data/semaphore/", RefField "ssh_key" →
// "vault:secret/data/semaphore/<keyname>#ssh_key"). RefScheme defaults to "store" → "store:<prefix><keyname>".
type SourceConfig struct {
	ID        string
	Client    *Client
	ProjectID int
	RefScheme string
	RefPrefix string
	RefField  string
}

// NewSource builds a Source. It fails closed if the client or id is missing.
func NewSource(cfg SourceConfig) (*Source, error) {
	if cfg.Client == nil {
		return nil, fmt.Errorf("semaphore: source requires a client")
	}
	if strings.TrimSpace(cfg.ID) == "" {
		return nil, fmt.Errorf("semaphore: source requires an id")
	}
	return &Source{
		id:        cfg.ID,
		client:    cfg.Client,
		projectID: cfg.ProjectID,
		refScheme: orDefault(cfg.RefScheme, "store"),
		refPrefix: cfg.RefPrefix,
		refField:  cfg.RefField,
	}, nil
}

// ID implements credential.CredentialSource.
func (s *Source) ID() string { return s.id }

// Plane implements credential.CredentialSource — Semaphore feeds the machine → host plane.
func (s *Source) Plane() credential.Plane { return credential.PlaneMachine }

// Skipped returns the hosts/groups/inventories skipped during the most recent Sync (coverage observability).
// It is a copy safe to read concurrently.
func (s *Source) Skipped() []SkipRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SkipRecord, len(s.skipped))
	copy(out, s.skipped)
	return out
}

// Sync performs the READ-ONLY pull (REQ-1612): it lists projects → inventories → keys and parses each
// static/static-yaml inventory's ansible text into SourceEntries with SecretRef-only Bundles. It FAILS
// CLOSED — an unreachable Semaphore, a denied read (401/403), a missing object (404), or a malformed page
// aborts with an error, leaving the prior converged state intact. A host/group/inventory that lacks the info
// needed to build a valid Bundle is skipped-with-record (never injected as a blank identity), NOT a
// whole-Sync failure.
func (s *Source) Sync(ctx context.Context) ([]credential.SourceEntry, error) {
	s.mu.Lock()
	s.skipped = nil
	s.mu.Unlock()

	projects, err := s.listProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("semaphore: source %q list projects: %w", s.id, err)
	}

	var entries []credential.SourceEntry
	for _, p := range projects {
		keys, err := s.listKeys(ctx, p.ID)
		if err != nil {
			return nil, fmt.Errorf("semaphore: source %q project %d list keys: %w", s.id, p.ID, err)
		}
		keyByID := map[int]semKey{}
		for _, k := range keys {
			keyByID[k.ID] = k
		}
		inventories, err := s.listInventories(ctx, p.ID)
		if err != nil {
			return nil, fmt.Errorf("semaphore: source %q project %d list inventory: %w", s.id, p.ID, err)
		}
		for _, inv := range inventories {
			es := s.entriesForInventory(inv, keyByID)
			entries = append(entries, es...)
		}
	}
	return entries, nil
}

func (s *Source) listProjects(ctx context.Context) ([]semProject, error) {
	if s.projectID > 0 {
		var p semProject
		if err := s.client.getJSON(ctx, "/api/project/"+strconv.Itoa(s.projectID), &p); err != nil {
			return nil, err
		}
		return []semProject{p}, nil
	}
	var ps []semProject
	if err := s.client.getJSON(ctx, "/api/projects", &ps); err != nil {
		return nil, err
	}
	if len(ps) > maxItems {
		return nil, fmt.Errorf("semaphore: projects list too large (%d > %d, fail closed)", len(ps), maxItems)
	}
	return ps, nil
}

func (s *Source) listInventories(ctx context.Context, projectID int) ([]semInventory, error) {
	var inv []semInventory
	if err := s.client.getJSON(ctx, "/api/project/"+strconv.Itoa(projectID)+"/inventory", &inv); err != nil {
		return nil, err
	}
	if len(inv) > maxItems {
		return nil, fmt.Errorf("semaphore: inventory list too large (%d > %d, fail closed)", len(inv), maxItems)
	}
	return inv, nil
}

func (s *Source) listKeys(ctx context.Context, projectID int) ([]semKey, error) {
	var keys []semKey
	if err := s.client.getJSON(ctx, "/api/project/"+strconv.Itoa(projectID)+"/keys", &keys); err != nil {
		return nil, err
	}
	if len(keys) > maxItems {
		return nil, fmt.Errorf("semaphore: keys list too large (%d > %d, fail closed)", len(keys), maxItems)
	}
	return keys, nil
}

// entriesForInventory parses one inventory's text into host/group SourceEntries. It skips-with-record any
// inventory type it cannot parse in-band (file/terraform-workspace point OUTSIDE the API — there is no inline
// text), any oversized text (DoS bound), and any host/group with no derivable connection identity or key
// reference. It never fails the whole Sync for a single bad inventory; a transport error already aborted
// earlier.
func (s *Source) entriesForInventory(inv semInventory, keyByID map[int]semKey) []credential.SourceEntry {
	switch inv.Type {
	case "static", "static-yaml":
		// parseable inline ansible inventory text — handled below.
	default:
		// "file" (inventory lives in the linked repo) and "terraform-workspace" carry no inline host text the
		// API exposes; there is nothing to map read-only. Skip-with-record, never a blank identity.
		s.record(credential.KindHost, inv.Name, "inventory type "+strconv.Quote(inv.Type)+" has no inline host text (skip)")
		return nil
	}
	if len(inv.Inventory) > maxInventoryBytes {
		s.record(credential.KindHost, inv.Name, fmt.Sprintf("inventory text too large (%d bytes > %d limit)", len(inv.Inventory), maxInventoryBytes))
		return nil
	}

	// The inventory-level assigned Key Store key name is the DEFAULT secret reference for every host that has
	// no per-host key var (Semaphore's key-ASSIGNMENT model). We resolve id → NAME only; the value stays in
	// Semaphore's write-only store and is dereferenced elsewhere.
	invKeyName := assignedKeyName(inv.SSHKeyID, keyByID)
	becomeKeyName := assignedKeyName(inv.BecomeKeyID, keyByID)

	var parsed *ansibleInventory
	var perr error
	if inv.Type == "static-yaml" {
		parsed, perr = parseYAMLInventory(inv.Inventory)
	} else {
		parsed, perr = parseINIInventory(inv.Inventory)
	}
	if perr != nil {
		s.record(credential.KindHost, inv.Name, "unparseable inventory text: "+perr.Error())
		return nil
	}

	var out []credential.SourceEntry
	for _, h := range parsed.hosts {
		e, ok, reason := s.toEntry(credential.KindHost, h.name,
			fmt.Sprintf("project:%d/inventory:%d/host:%s", inv.ProjectID, inv.ID, h.name),
			parsed.effectiveVars(h), invKeyName, becomeKeyName)
		if !ok {
			s.record(credential.KindHost, h.name, reason)
			continue
		}
		out = append(out, e)
	}
	for _, gname := range parsed.groupNames() {
		gv := parsed.groupVars[gname]
		if len(gv) == 0 {
			continue // purely organizational group (no connection defaults) → no entry, no record noise
		}
		merged := mergeVars(parsed.globalVars, gv)
		e, ok, reason := s.toEntry(credential.KindGroup, gname,
			fmt.Sprintf("project:%d/inventory:%d/group:%s", inv.ProjectID, inv.ID, gname),
			merged, invKeyName, becomeKeyName)
		if !ok {
			s.record(credential.KindGroup, gname, reason)
			continue
		}
		out = append(out, e)
	}
	return out
}

// toEntry maps one host/group's effective ansible vars (plus the inventory-assigned key names as fallbacks)
// to a SourceEntry, or reports (false, reason) when it cannot build a valid, SecretRef-only Bundle. It NEVER
// reads a literal secret into a Bundle — only a key REFERENCE.
func (s *Source) toEntry(kind credential.SelectorKind, name, nativeID string, vars map[string]string, invKeyName, becomeKeyName string) (credential.SourceEntry, bool, string) {
	user := firstNonEmpty(vars["ansible_user"], vars["ansible_ssh_user"])
	if user == "" {
		return credential.SourceEntry{}, false, "no ansible_user (no connection identity)"
	}
	port := 22
	if p := firstNonEmpty(vars["ansible_port"], vars["ansible_ssh_port"]); p != "" {
		n, perr := strconv.Atoi(p)
		if perr != nil || n < 1 || n > 65535 {
			return credential.SourceEntry{}, false, "invalid ansible_port " + strconv.Quote(p)
		}
		port = n
	}
	scheme, ok := mapScheme(vars["ansible_connection"])
	if !ok {
		return credential.SourceEntry{}, false, "unsupported ansible_connection " + strconv.Quote(vars["ansible_connection"])
	}

	spec := credential.BundleSpec{User: user, Port: port, Scheme: scheme}
	ref, ok, reason := s.secretRef(vars, scheme, invKeyName)
	if !ok {
		return credential.SourceEntry{}, false, reason
	}
	switch scheme {
	case credential.SchemeSSH, credential.SchemeNetconf:
		spec.SSHKeyRef = ref
	case credential.SchemeAPI:
		spec.APITokenRef = ref
	case credential.SchemeWinRM:
		spec.Become = ref
	}
	// An inventory-assigned become key (privilege escalation) becomes an optional become reference, when the
	// scheme did not already consume the become slot.
	if becomeKeyName != "" && spec.Become == "" {
		spec.Become = s.buildRef(becomeKeyName)
	}

	b, berr := credential.NewBundle(spec)
	if berr != nil {
		return credential.SourceEntry{}, false, "bundle rejected: " + berr.Error()
	}
	return credential.SourceEntry{
		NativeID: nativeID,
		Selector: credential.Selector{Kind: kind, Pattern: name},
		Bundle:   b,
	}, true, ""
}

// secretRef derives the sealed SecretRef for a host/group — WHICH key applies, never its value. Priority:
// an explicit sealed tg_secret_ref (verbatim), else a per-host key NAME (ansible_ssh_private_key_file basename
// for ssh/netconf, tg_credential otherwise), else the inventory-level ASSIGNED Key Store key name — mapped
// through the source's RefScheme/RefPrefix/RefField. Reports (false, reason) when no reference can be derived.
func (s *Source) secretRef(vars map[string]string, scheme credential.ConnectionScheme, invKeyName string) (config.SecretRef, bool, string) {
	if explicit := strings.TrimSpace(vars["tg_secret_ref"]); explicit != "" {
		// Used verbatim; NewBundle enforces it is a sealed scheme (env:/file:/store:/vault:/bao:).
		return config.SecretRef(explicit), true, ""
	}
	var keyName string
	switch scheme {
	case credential.SchemeSSH, credential.SchemeNetconf:
		if kf := strings.TrimSpace(vars["ansible_ssh_private_key_file"]); kf != "" {
			keyName = strings.TrimSuffix(path.Base(kf), path.Ext(kf))
		}
	}
	if keyName == "" {
		keyName = strings.TrimSpace(vars["tg_credential"])
	}
	if keyName == "" {
		keyName = invKeyName // Semaphore's inventory-level Key Store assignment (the common case)
	}
	if keyName == "" {
		return "", false, "no key reference derivable (Semaphore does not expose secret values; assign a Key Store key to the inventory, or set tg_secret_ref/tg_credential/ansible_ssh_private_key_file)"
	}
	return s.buildRef(keyName), true, ""
}

// buildRef assembles the source's sealed reference from a key name.
func (s *Source) buildRef(keyName string) config.SecretRef {
	ref := s.refScheme + ":" + s.refPrefix + keyName
	if s.refField != "" {
		ref += "#" + s.refField
	}
	return config.SecretRef(ref)
}

func (s *Source) record(kind credential.SelectorKind, name, reason string) {
	s.mu.Lock()
	s.skipped = append(s.skipped, SkipRecord{Kind: kind, Name: name, Reason: reason})
	s.mu.Unlock()
}

// compile-time proof the source satisfies the stable credential-source interface.
var _ credential.CredentialSource = (*Source)(nil)

// ---- ansible inventory parsing ----------------------------------------------------------------------

// ansibleHost is one parsed inventory host with its inline vars and the groups it belongs to (ordered).
type ansibleHost struct {
	name      string
	inlineVar map[string]string
	groups    []string
}

// ansibleInventory is a parsed static/static-yaml inventory: the ordered hosts, the per-group vars, and the
// global ([all:vars] / top-level all.vars) defaults. Var precedence for a host is
// globalVars < group vars (in the host's group order) < inline host vars.
type ansibleInventory struct {
	hosts      []ansibleHost
	groupVars  map[string]map[string]string
	globalVars map[string]string
	groupOrder []string
}

func newInventory() *ansibleInventory {
	return &ansibleInventory{groupVars: map[string]map[string]string{}, globalVars: map[string]string{}}
}

// effectiveVars computes a host's merged connection vars honoring precedence.
func (inv *ansibleInventory) effectiveVars(h ansibleHost) map[string]string {
	out := mergeVars(nil, inv.globalVars)
	for _, g := range h.groups {
		out = mergeVars(out, inv.groupVars[g])
	}
	return mergeVars(out, h.inlineVar)
}

// groupNames returns the groups that carry vars, in first-seen order (stable output).
func (inv *ansibleInventory) groupNames() []string {
	return inv.groupOrder
}

func (inv *ansibleInventory) noteGroup(name string) {
	if _, seen := inv.groupVars[name]; !seen {
		inv.groupVars[name] = map[string]string{}
		inv.groupOrder = append(inv.groupOrder, name)
	}
}

// parseINIInventory parses an ansible INI-format inventory. Supported: ungrouped hosts (before any section),
// [group] host sections with inline `k=v` host vars, [group:vars] group var sections, [all:vars] global
// defaults. [group:children] is tolerated but child var inheritance is not applied (documented limitation:
// TG resolves per-host explicit + group + global vars, which covers the credential-relevant connection vars).
// Host ranges (web[01:03]) are NOT expanded — such a host is skipped-with-record downstream via a name check.
func parseINIInventory(text string) (*ansibleInventory, error) {
	inv := newInventory()
	hostIdx := map[string]int{} // hostname → index in inv.hosts (dedup across groups)
	section := ""               // "" = ungrouped; "<g>" = group hosts; "<g>:vars"; "all:vars"; "<g>:children"
	for _, rawLine := range strings.Split(text, "\n") {
		line := strings.TrimSpace(stripComment(rawLine))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return nil, fmt.Errorf("malformed section header %q", line)
			}
			section = strings.TrimSpace(line[1 : len(line)-1])
			if section == "" {
				return nil, fmt.Errorf("empty section header")
			}
			// register a plain group so an organizational-only group is known (even if it yields no entry).
			if !strings.Contains(section, ":") {
				inv.noteGroup(section)
			} else if strings.HasSuffix(section, ":vars") {
				g := strings.TrimSuffix(section, ":vars")
				if g != "all" {
					inv.noteGroup(g)
				}
			}
			continue
		}
		switch {
		case section == "all:vars":
			k, v, ok := splitKV(line)
			if ok {
				inv.globalVars[k] = v
			}
		case strings.HasSuffix(section, ":vars"):
			g := strings.TrimSuffix(section, ":vars")
			k, v, ok := splitKV(line)
			if ok {
				inv.groupVars[g][k] = v
			}
		case strings.HasSuffix(section, ":children"):
			// child group name; membership-only, no vars to record.
		default:
			// a host line in the ungrouped section ("") or a [group] host section.
			name, hv, err := parseHostLine(line)
			if err != nil {
				return nil, err
			}
			idx, seen := hostIdx[name]
			if !seen {
				idx = len(inv.hosts)
				hostIdx[name] = idx
				inv.hosts = append(inv.hosts, ansibleHost{name: name, inlineVar: map[string]string{}})
			}
			for k, v := range hv {
				inv.hosts[idx].inlineVar[k] = v
			}
			if section != "" {
				inv.hosts[idx].groups = appendUnique(inv.hosts[idx].groups, section)
			}
		}
	}
	return inv, nil
}

// parseHostLine parses "hostname k1=v1 k2=v2" into the name and its inline vars. A hostname carrying an
// unexpanded range ("web[01:03]") is rejected (fail closed: we do not silently synthesize hosts).
func parseHostLine(line string) (string, map[string]string, error) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", nil, fmt.Errorf("empty host line")
	}
	name := fields[0]
	if strings.Contains(name, "[") && strings.Contains(name, ":") {
		return "", nil, fmt.Errorf("unsupported host range %q (not expanded)", name)
	}
	vars := map[string]string{}
	for _, f := range fields[1:] {
		if k, v, ok := splitKV(f); ok {
			vars[k] = v
		}
	}
	return name, vars, nil
}

// yamlInv is the shape of an ansible YAML inventory node used for decoding static-yaml inventories.
type yamlInv struct {
	Hosts    map[string]map[string]any `yaml:"hosts"`
	Vars     map[string]any            `yaml:"vars"`
	Children map[string]*yamlInv       `yaml:"children"`
}

// parseYAMLInventory parses an ansible static-yaml inventory. The document root is a mapping of top-level
// group names (conventionally "all") to group nodes; each node may carry hosts, vars, and nested children.
// yaml.v3 rejects billion-laughs alias expansion; the caller has already bounded the input size.
func parseYAMLInventory(text string) (*ansibleInventory, error) {
	root := map[string]*yamlInv{}
	if err := yaml.Unmarshal([]byte(text), &root); err != nil {
		return nil, err
	}
	inv := newInventory()
	hostIdx := map[string]int{}
	var walk func(groupName string, node *yamlInv, isAll bool)
	walk = func(groupName string, node *yamlInv, isAll bool) {
		if node == nil {
			return
		}
		gv := flattenVars(node.Vars)
		if isAll {
			for k, v := range gv {
				inv.globalVars[k] = v
			}
		} else {
			inv.noteGroup(groupName)
			for k, v := range gv {
				inv.groupVars[groupName][k] = v
			}
		}
		for hn, hv := range node.Hosts {
			idx, seen := hostIdx[hn]
			if !seen {
				idx = len(inv.hosts)
				hostIdx[hn] = idx
				inv.hosts = append(inv.hosts, ansibleHost{name: hn, inlineVar: map[string]string{}})
			}
			for k, v := range flattenVars(hv) {
				inv.hosts[idx].inlineVar[k] = v
			}
			if !isAll {
				inv.hosts[idx].groups = appendUnique(inv.hosts[idx].groups, groupName)
			}
		}
		for childName, child := range node.Children {
			walk(childName, child, false)
		}
	}
	for topName, node := range root {
		walk(topName, node, topName == "all")
	}
	return inv, nil
}

// ---- helpers ----------------------------------------------------------------------------------------

// assignedKeyName resolves an inventory's assigned key id to the key NAME, or "" when unassigned / a "none"
// key / an unknown id. Only the name crosses over — the secret value stays in Semaphore's write-only store.
func assignedKeyName(id int, keyByID map[int]semKey) string {
	if id <= 0 {
		return ""
	}
	k, ok := keyByID[id]
	if !ok || strings.EqualFold(strings.TrimSpace(k.Type), "none") {
		return ""
	}
	return strings.TrimSpace(k.Name)
}

// flattenVars renders an ansible vars mapping (values may be scalars) into a flat string map.
func flattenVars(m map[string]any) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		if v == nil {
			continue
		}
		out[k] = fmt.Sprint(v)
	}
	return out
}

// mergeVars returns a new map that is base overlaid by over (over wins). A nil input is treated as empty.
func mergeVars(base, over map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

// splitKV splits "k=v" (v may be empty). Reports ok=false when there is no '='.
func splitKV(s string) (string, string, bool) {
	i := strings.IndexByte(s, '=')
	if i < 0 {
		return "", "", false
	}
	return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:]), true
}

// stripComment removes an INI full-line comment (a line whose first non-space char is '#' or ';').
func stripComment(line string) string {
	t := strings.TrimSpace(line)
	if strings.HasPrefix(t, "#") || strings.HasPrefix(t, ";") {
		return ""
	}
	return line
}

func appendUnique(xs []string, x string) []string {
	for _, e := range xs {
		if e == x {
			return xs
		}
	}
	return append(xs, x)
}

// mapScheme maps an ansible_connection value to TG's closed ConnectionScheme vocabulary. An empty connection
// defaults to ssh (Ansible's own default). "local" and any unknown connection are unsupported (false).
func mapScheme(conn string) (credential.ConnectionScheme, bool) {
	switch strings.ToLower(strings.TrimSpace(conn)) {
	case "", "ssh", "smart", "paramiko", "paramiko_ssh", "libssh":
		return credential.SchemeSSH, true
	case "network_cli", "netconf", "ansible.netcommon.network_cli", "ansible.netcommon.netconf":
		return credential.SchemeNetconf, true
	case "winrm", "psrp":
		return credential.SchemeWinRM, true
	case "httpapi", "ansible.netcommon.httpapi":
		return credential.SchemeAPI, true
	default:
		return "", false // "local" and unknown connections carry no remote credential
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// apiErr extracts Semaphore's {"error": "..."} / {"message": "..."} error body into a short, secret-free
// message.
func apiErr(body []byte) string {
	var e struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) == nil {
		if e.Error != "" {
			return e.Error
		}
		if e.Message != "" {
			return e.Message
		}
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// scrub strips any query string from a URL before it enters an error message (defensive: a token must never
// travel in a query param, but never echo one if a caller ever puts it there).
func scrub(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return u[:i]
	}
	return u
}
