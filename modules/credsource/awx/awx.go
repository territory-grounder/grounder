// Package awx is the native-Go AWX (Ansible AWX / automation-controller) credential connector
// (spec/016 T-016-8, REQ-1612, TG-88). It is a READ-ONLY INVENTORY source on the machine plane: it pulls
// AWX's hosts and groups and maps each to a credential.SourceEntry whose Bundle carries SecretRef
// REFERENCES only — never a raw secret.
//
// WHY inventory-only (grounded in the AWX REST API reality, not an assumption): AWX does NOT expose
// credential SECRET values over its API. A Machine credential's secret fields (ssh key, password) read back
// as the literal "$encrypted$", and Machine credentials are attached to JOB TEMPLATES, not to hosts. So the
// per-host connection identity lives in the host/group ANSIBLE VARS instead: ansible_user → user,
// ansible_port / ansible_ssh_port → port, ansible_connection → scheme, and the KEY is referenced by name
// (ansible_ssh_private_key_file, or an explicit tg_secret_ref / tg_credential var) — never by value. This
// connector resolves the OBJECT MODEL + connection metadata + WHICH key applies; the actual secret is
// dereferenced elsewhere (the OpenBao/Vault vault:/bao: scheme, or the native store: scheme) at USE TIME
// (INV-13). A host for which no key REFERENCE can be derived is skipped-with-record (fail closed: a blank or
// literal identity is never injected), while a transport/auth/decode failure fails the whole Sync closed.
//
// SECOND MAPPING MODE — job-template → inventory + named machine credential (the USABLE bundle path):
// AWX hosts carry no inline ssh identity in this estate (no ansible_user / ansible_ssh_private_key_file), so
// the per-host mode above legitimately ingests zero here. The REAL binding AWX models is JOB TEMPLATE →
// INVENTORY + MACHINE credential(s). When (and only when) the operator supplies a cred-name → SecretRef map
// (CredRefMap, env TG_AWX_CRED_REF_MAP), this connector ALSO walks /job_templates/ and, for each JT that has
// an inventory and at least one Machine-type credential, emits a GROUP-selector entry keyed by the JT's
// inventory NAME (so every host in that AWX inventory resolves) whose Bundle's SSHKeyRef is the SecretRef the
// OPERATOR mapped for that AWX credential NAME. The AWX credential name CANNOT be auto-mapped to a local key
// (AWX never reveals the key; it reads back "$encrypted$"), so a JT whose machine-credential name is not in
// the map is SKIPPED-WITH-RECORD — never a blank or guessed identity (fail closed). With no map configured,
// the JT walk does not run at all: zero behaviour change from the host-only mode.
//
// DISTROLESS-SAFE: net/http + crypto/tls with an optional private-CA cert; NO subprocess, no awx-cli, no
// ansible binary (INV-02). Auth is a long-lived OAuth2 token supplied as a core/config.SecretRef
// (env:/file:/store:), sent as "Authorization: Bearer <token>" and NEVER logged (INV-13).
//
// Grounded in the AWX REST API (docs.ansible.com/projects/awx + awx source, verified live against
// https://awx.example.net v24.6.1):
//   - GET /api/v2/ping/          — unauthenticated health (ha/version/active_node/instances); the safe live probe.
//   - GET /api/v2/hosts/         — paginated {count,next,previous,results[]}; host has id/name/enabled/inventory
//     and .variables (a JSON/YAML string of the ansible_* connection vars).
//   - GET /api/v2/groups/        — same envelope; group has id/name/inventory/.variables (group-level defaults).
//   - GET /api/v2/inventories/   — the inventories a source may scope to (?inventory=<id>).
//   - GET /api/v2/job_templates/ — same envelope; JT has id/name and summary_fields.inventory.{id,name} +
//     summary_fields.credentials[].{id,name} (the credentials bound to the JT).
//   - GET /api/v2/credentials/   — same envelope; each has id/name, summary_fields.credential_type.name
//     ("Machine" is the ssh machine type) and inputs.username (plaintext; the
//     secret input fields read back as the literal "$encrypted$", never used).
//
// Pagination follows the .next link (relative or absolute) until null; page size via ?page_size.
//
// Provenance: [O] INV-13/INV-05/INV-02, spec/016 (REQ-1612), TG-88.
package awx

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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/credential"
)

// SourceType is the vendor slug this connector serves.
const SourceType = "awx"

// defaultTimeout bounds a single API round-trip so a hung AWX fails closed promptly.
const defaultTimeout = 20 * time.Second

// defaultPageSize is the page size requested when paginating hosts/groups (AWX caps this server-side).
const defaultPageSize = 200

// maxPages bounds pagination so a misbehaving/looping .next can never hang Sync forever (fail closed).
const maxPages = 10_000

// Doer is the minimal HTTP contract; *http.Client satisfies it and the oracle injects a fake AWX.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Client is a read-only AWX REST API client. It holds a Bearer OAuth2 token (resolved from a SecretRef,
// cached after first use, never logged).
type Client struct {
	baseURL  string // e.g. "https://awx.example.net" (no trailing slash, no /api)
	tokenRef config.SecretRef
	http     Doer

	mu     sync.Mutex
	cached string // cached resolved token; never logged
}

// Config constructs a Client. BaseURL is required. TokenRef (a Bearer OAuth2 token reference) is required
// for the authenticated inventory pull; it may be empty ONLY for a Ping-only (health) client. CACertPath, if
// set, is trusted for TLS (AWX behind a private CA). HTTPClient overrides transport entirely (the oracle
// injects a fake).
type Config struct {
	BaseURL    string
	TokenRef   config.SecretRef
	CACertPath string
	HTTPClient Doer
}

// New builds a Client. It fails closed if the base URL is missing or a configured CA cert cannot be loaded.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, fmt.Errorf("awx: base URL is required")
	}
	c := &Client{baseURL: strings.TrimRight(cfg.BaseURL, "/"), tokenRef: cfg.TokenRef, http: cfg.HTTPClient}
	if c.http == nil {
		hc := &http.Client{Timeout: defaultTimeout}
		if cfg.CACertPath != "" {
			pem, err := os.ReadFile(cfg.CACertPath)
			if err != nil {
				return nil, fmt.Errorf("awx: read CA cert: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("awx: CA cert %q contains no valid certificate", cfg.CACertPath)
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
		return "", fmt.Errorf("awx: resolve token: %w", err)
	}
	if strings.TrimSpace(tok) == "" {
		return "", fmt.Errorf("awx: token is empty (fail closed)")
	}
	c.cached = tok
	return tok, nil
}

// ---- HTTP plumbing ----------------------------------------------------------------------------------

// Ping hits GET /api/v2/ping/ (unauthenticated). A 200 means AWX is reachable and serving. Used by the
// env-gated live integration test as the safe live assertion (no token needed).
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.raw(ctx, c.baseURL+"/api/v2/ping/", false)
	return err
}

// getJSON issues an authenticated GET against a URL (absolute, or a path/relative URL to be joined onto the
// base) and decodes the JSON body into v. It fails closed on unreachable/denied/not-found/malformed.
func (c *Client) getJSON(ctx context.Context, urlOrPath string, v any) error {
	full, err := c.resolveURL(urlOrPath)
	if err != nil {
		return err
	}
	raw, err := c.raw(ctx, full, true)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("awx: decode %s: %w", scrub(urlOrPath), err)
	}
	return nil
}

// resolveURL turns a .next link (which AWX returns as an absolute URL OR a root-relative path like
// "/api/v2/hosts/?page=2") into a full URL against the client's base.
func (c *Client) resolveURL(u string) (string, error) {
	switch {
	case strings.HasPrefix(u, "http://"), strings.HasPrefix(u, "https://"):
		// An absolute .next MUST stay on the configured AWX host+scheme. A compromised or MITM'd AWX must
		// not be able to redirect the pull — which carries the Bearer token — to another host. Fail closed
		// on any host/scheme mismatch rather than following the untrusted link (INV-13, credential safety).
		nu, err := url.Parse(u)
		if err != nil {
			return "", fmt.Errorf("awx: unparseable pagination url: %w", err)
		}
		bu, err := url.Parse(c.baseURL)
		if err != nil {
			return "", fmt.Errorf("awx: unparseable base url: %w", err)
		}
		if !strings.EqualFold(nu.Host, bu.Host) || !strings.EqualFold(nu.Scheme, bu.Scheme) {
			return "", fmt.Errorf("awx: refusing off-host pagination url (got host %q scheme %q, expected %q %q)",
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
		return nil, fmt.Errorf("awx: GET %s: %w", scrub(fullURL), err) // unreachable → fail closed
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("awx: GET %s: status %d: %s", scrub(fullURL), resp.StatusCode, awxErr(out))
	}
	return out, nil
}

// page is the AWX list-response envelope (count/next/previous/results) shared by hosts and groups.
type page[T any] struct {
	Count    int    `json:"count"`
	Next     string `json:"next"`
	Previous string `json:"previous"`
	Results  []T    `json:"results"`
}

// paginate walks the .next chain from an initial path, accumulating every result. It fails closed on any
// page error and bounds the walk at maxPages.
func paginate[T any](ctx context.Context, c *Client, firstPath string) ([]T, error) {
	var all []T
	next := firstPath
	for i := 0; next != "" && i < maxPages; i++ {
		var p page[T]
		if err := c.getJSON(ctx, next, &p); err != nil {
			return nil, err
		}
		all = append(all, p.Results...)
		next = p.Next
	}
	if next != "" {
		return nil, fmt.Errorf("awx: pagination exceeded %d pages (fail closed)", maxPages)
	}
	return all, nil
}

// ---- AWX object shapes (only the fields this connector reads) ---------------------------------------

type awxHost struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
	Inventory int    `json:"inventory"`
	Variables string `json:"variables"` // JSON or YAML string of the host's ansible_* connection vars
}

type awxGroup struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	Inventory int    `json:"inventory"`
	Variables string `json:"variables"`
}

// awxInventory is the subset of an /inventories/ result the membership walk reads: id + name. The inventory
// NAME is the estate group a host in that inventory belongs to (the same name the JT-mode group bundle is
// keyed by), so host↔inventory membership is what lets those bundles resolve.
type awxInventory struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// awxCredential is the subset of a /credentials/ result the JT mode reads: the name (the key the operator's
// CredRefMap is keyed by), the credential-type name (to keep only "Machine" ssh credentials), and the
// plaintext login username. inputs.username is NOT a secret field (the secret inputs read back "$encrypted$"
// and are never touched); a defensive guard still rejects a "$encrypted$" username at use.
type awxCredential struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Inputs struct {
		Username string `json:"username"`
	} `json:"inputs"`
	SummaryFields struct {
		CredentialType struct {
			Name string `json:"name"`
		} `json:"credential_type"`
	} `json:"summary_fields"`
}

// awxJobTemplate is the subset of a /job_templates/ result the JT mode reads: the JT id/name and its bound
// inventory + credentials via summary_fields. summary_fields.inventory is null (zero id/name) when the JT has
// no inventory; summary_fields.credentials lists every credential bound to the JT (machine + others).
type awxJobTemplate struct {
	ID            int    `json:"id"`
	Name          string `json:"name"`
	SummaryFields struct {
		Inventory struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"inventory"`
		Credentials []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"credentials"`
	} `json:"summary_fields"`
}

// ---- CredentialSource (REQ-1612) --------------------------------------------------------------------

// Source is a read-only credential.CredentialSource backed by AWX inventory. Each AWX host maps to a
// host-selector entry and each group carrying connection defaults maps to a group-selector entry; the
// Bundle's secret field is a SecretRef derived from the ansible connection vars (INV-13), never a value.
type Source struct {
	id          string
	client      *Client
	inventoryID int    // 0 = all inventories; otherwise scope to ?inventory=<id>
	refScheme   string // scheme for emitted key references ("store", "vault", "bao", …)
	refPrefix   string // prefix prepended to the derived key name
	refField    string // optional "#<field>" suffix (used by vault:/bao: KV v2 refs)

	// JT mode (job-template → inventory + named machine credential). credRefMap maps an AWX Machine
	// credential NAME to a sealed SecretRef the operator holds locally; it is the ONLY way a JT bundle is
	// emitted (an unmapped cred name is skipped-with-record, never guessed). An empty map disables the JT
	// walk entirely (zero behaviour change). defaultUser is the login user when the AWX credential carries no
	// plaintext username.
	credRefMap  map[string]string
	defaultUser string

	mu      sync.Mutex
	skipped []SkipRecord // hosts/groups/job-templates skipped this Sync, with a reason (coverage observability)
}

// SkipRecord records one host/group that could not be mapped to a valid Bundle and was skipped (fail closed:
// no blank identity injected). The reason is safe to log (never a secret).
type SkipRecord struct {
	Kind   credential.SelectorKind
	Name   string
	Reason string
}

// SourceConfig configures a Source. ID and Client are required. InventoryID (optional) scopes the pull to a
// single AWX inventory. RefScheme/RefPrefix/RefField build the emitted SecretRef from a host's key name
// (e.g. RefScheme "vault", RefPrefix "secret/data/awx/", RefField "ssh_key" →
// "vault:secret/data/awx/<keyname>#ssh_key"). RefScheme defaults to "store" → "store:<prefix><keyname>".
type SourceConfig struct {
	ID          string
	Client      *Client
	InventoryID int
	RefScheme   string
	RefPrefix   string
	RefField    string

	// CredRefMap maps an AWX Machine-credential NAME to a sealed SecretRef the operator holds locally (e.g.
	// "SSH ED25519 (one_key)" → "file:/secrets/one_key"). It enables the job-template → inventory group-bundle
	// mode: a JT whose machine credential is in this map yields a group(inventory) entry; an UNMAPPED name is
	// skipped-with-record (never a blank/guessed identity). Empty (the default) ⇒ the JT walk does not run.
	CredRefMap map[string]string
	// DefaultUser is the login user for a JT bundle when the mapped AWX credential carries no plaintext
	// username. Defaults to "root". Never invented beyond this operator-set default.
	DefaultUser string
}

// NewSource builds a Source. It fails closed if the client or id is missing.
func NewSource(cfg SourceConfig) (*Source, error) {
	if cfg.Client == nil {
		return nil, fmt.Errorf("awx: source requires a client")
	}
	if strings.TrimSpace(cfg.ID) == "" {
		return nil, fmt.Errorf("awx: source requires an id")
	}
	return &Source{
		id:          cfg.ID,
		client:      cfg.Client,
		inventoryID: cfg.InventoryID,
		refScheme:   orDefault(cfg.RefScheme, "store"),
		refPrefix:   cfg.RefPrefix,
		refField:    cfg.RefField,
		credRefMap:  cfg.CredRefMap,
		defaultUser: orDefault(strings.TrimSpace(cfg.DefaultUser), "root"),
	}, nil
}

// ID implements credential.CredentialSource.
func (s *Source) ID() string { return s.id }

// Plane implements credential.CredentialSource — AWX feeds the machine → host plane.
func (s *Source) Plane() credential.Plane { return credential.PlaneMachine }

// Skipped returns the hosts/groups skipped during the most recent Sync (coverage observability). It is a
// copy safe to read concurrently.
func (s *Source) Skipped() []SkipRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SkipRecord, len(s.skipped))
	copy(out, s.skipped)
	return out
}

// Sync performs the READ-ONLY pull (REQ-1612): it paginates AWX hosts and groups and maps each to a
// SourceEntry with a SecretRef-only Bundle. It FAILS CLOSED — an unreachable AWX, a denied read (401/403), a
// missing inventory (404), or a malformed page aborts with an error, leaving the prior converged state
// intact. A host/group that lacks the connection info needed to build a valid Bundle is skipped-with-record
// (never injected as a blank identity), NOT a whole-Sync failure.
func (s *Source) Sync(ctx context.Context) ([]credential.SourceEntry, error) {
	s.mu.Lock()
	s.skipped = nil
	s.mu.Unlock()

	hosts, err := paginate[awxHost](ctx, s.client, s.listPath("hosts"))
	if err != nil {
		return nil, fmt.Errorf("awx: source %q list hosts: %w", s.id, err)
	}
	groups, err := paginate[awxGroup](ctx, s.client, s.listPath("groups"))
	if err != nil {
		return nil, fmt.Errorf("awx: source %q list groups: %w", s.id, err)
	}

	entries := make([]credential.SourceEntry, 0, len(hosts)+len(groups))
	for _, h := range hosts {
		if !h.Enabled {
			s.record(credential.KindHost, h.Name, "host disabled in AWX")
			continue
		}
		e, ok, reason := s.toEntry(credential.KindHost, h.Name, fmt.Sprintf("host:%d", h.ID), h.Variables)
		if !ok {
			s.record(credential.KindHost, h.Name, reason)
			continue
		}
		entries = append(entries, e)
	}
	for _, g := range groups {
		// A group only yields an entry when its group-level vars carry a full connection identity; purely
		// organizational groups (no ansible_user) are skipped, not errors.
		e, ok, reason := s.toEntry(credential.KindGroup, g.Name, fmt.Sprintf("group:%d", g.ID), g.Variables)
		if !ok {
			s.record(credential.KindGroup, g.Name, reason)
			continue
		}
		entries = append(entries, e)
	}

	// SECOND MODE: job-template → inventory + named machine credential. Only runs when the operator has
	// supplied a cred-name → SecretRef map; with no map this is a pure no-op (zero behaviour change). Any
	// transport/read failure here fails the whole Sync closed (leaving the prior converged state intact).
	if len(s.credRefMap) > 0 {
		jtEntries, err := s.syncJobTemplates(ctx)
		if err != nil {
			return nil, err
		}
		entries = append(entries, jtEntries...)
	}
	return entries, nil
}

// Membership implements credential.MembershipSource (spec/016 T-052, REQ-1605): it returns a host→[inventory
// NAME] map so the credential engine can populate Target.Groups and let a JT-mode group(inventory) bundle
// RESOLVE for a host that lives in that inventory. It walks /api/v2/inventories/ (respecting the source's
// optional single-inventory scope) and each inventory's /api/v2/inventories/<id>/hosts/ — paginated, with the
// SAME bounded/off-host guards as Sync. It is READ-ONLY and NON-SECRET: the map carries host names and
// inventory names ONLY — never the Bearer token, a key reference, or any secret value. It fails CLOSED: any
// transport/read failure returns an error and no partial map, leaving the engine's prior membership intact.
func (s *Source) Membership(ctx context.Context) (map[string][]string, error) {
	invs, err := paginate[awxInventory](ctx, s.client, s.plainListPath("inventories"))
	if err != nil {
		return nil, fmt.Errorf("awx: source %q list inventories: %w", s.id, err)
	}
	out := map[string][]string{}
	for _, inv := range invs {
		name := strings.TrimSpace(inv.Name)
		if inv.ID == 0 || name == "" {
			continue
		}
		if s.inventoryID > 0 && inv.ID != s.inventoryID {
			continue // a scoped source contributes membership only for its one configured inventory
		}
		hosts, err := paginate[awxHost](ctx, s.client, s.invHostsPath(inv.ID))
		if err != nil {
			return nil, fmt.Errorf("awx: source %q list inventory %d hosts: %w", s.id, inv.ID, err)
		}
		for _, h := range hosts {
			hn := strings.TrimSpace(h.Name)
			if hn == "" {
				continue
			}
			out[hn] = append(out[hn], name) // host → its inventory name (the estate group)
		}
	}
	return out, nil
}

// invHostsPath is the first-page path for ONE inventory's hosts (GET /api/v2/inventories/<id>/hosts/), the
// flattened host membership of that inventory. Paginated via the shared page_size + .next walk.
func (s *Source) invHostsPath(invID int) string {
	q := url.Values{}
	q.Set("page_size", strconv.Itoa(defaultPageSize))
	return "/api/v2/inventories/" + strconv.Itoa(invID) + "/hosts/?" + q.Encode()
}

// syncJobTemplates walks /credentials/ and /job_templates/ and emits a GROUP(inventory-name) entry for each
// (job-template inventory, mapped Machine credential) pair. It reads the login username and Machine-ness from
// the /credentials/ list (each result already carries summary_fields.credential_type + inputs.username — the
// same fields /credentials/<id>/ returns — so one paginated walk replaces an N+1 per-credential GET). A JT
// with no inventory, no Machine credential, or a Machine credential whose NAME is not in the operator's
// CredRefMap is skipped-with-record (fail closed: never a blank or guessed identity). It fails the whole Sync
// closed on any transport/read error.
func (s *Source) syncJobTemplates(ctx context.Context) ([]credential.SourceEntry, error) {
	creds, err := paginate[awxCredential](ctx, s.client, s.plainListPath("credentials"))
	if err != nil {
		return nil, fmt.Errorf("awx: source %q list credentials: %w", s.id, err)
	}
	machine := make(map[int]awxCredential, len(creds))
	for _, c := range creds {
		if strings.EqualFold(strings.TrimSpace(c.SummaryFields.CredentialType.Name), "Machine") {
			machine[c.ID] = c
		}
	}

	jts, err := paginate[awxJobTemplate](ctx, s.client, s.listPath("job_templates"))
	if err != nil {
		return nil, fmt.Errorf("awx: source %q list job templates: %w", s.id, err)
	}

	// Emit JTs in a deterministic (id-ascending) order so the DEDUP below is stable across syncs: when several
	// JTs bind the SAME inventory to the SAME credential, the lowest-id JT's entry is the one kept.
	sort.Slice(jts, func(i, j int) bool { return jts[i].ID < jts[j].ID })
	// seen fingerprints an already-emitted (inventory, user, key-ref) triple — i.e. an identical
	// (group selector, bundle) — so two JTs binding the same inventory to the same credential do NOT emit two
	// identical entries. Identical selector+bundle is NOT a real equal-specificity conflict (it resolves to one
	// and the same identity), so deduping it here keeps the engine from failing that inventory closed as
	// spuriously ambiguous. A DIFFERENT credential on the same inventory (a different ref/user) has a different
	// fingerprint and is still emitted — a genuine equal-specificity conflict the resolver rightly refuses.
	seen := map[string]bool{}

	var out []credential.SourceEntry
	for _, jt := range jts {
		inv := jt.SummaryFields.Inventory
		if inv.ID == 0 || strings.TrimSpace(inv.Name) == "" {
			s.record(credential.KindGroup, jtLabel(jt), "job template has no inventory")
			continue
		}
		// Collect the JT's Machine-type credential ids, deterministically ordered (stable output + a stable
		// tie at resolve time). A JT can bind several credentials; only Machine ones carry a login identity.
		var mids []int
		for _, ref := range jt.SummaryFields.Credentials {
			if _, ok := machine[ref.ID]; ok {
				mids = append(mids, ref.ID)
			}
		}
		if len(mids) == 0 {
			s.record(credential.KindGroup, inv.Name, "job template "+strconv.Quote(jt.Name)+" has no Machine credential")
			continue
		}
		sort.Ints(mids)

		// MULTIPLE-MACHINE-CREDENTIAL RULE: emit ONE entry per (inventory, mapped Machine credential). When a
		// JT binds more than one MAPPED machine credential to the same inventory, the two entries carry the
		// SAME group(inventory) selector and thus EQUAL specificity — the resolver fails that inventory closed
		// as ambiguous (never silently picks one). The operator disambiguates by mapping only the intended
		// credential name. Unmapped credential names emit nothing (skip-with-record), so a JT with one mapped +
		// one unmapped machine credential resolves cleanly on the single mapped one.
		for _, cid := range mids {
			cred := machine[cid]
			ref, ok := s.credRefMap[cred.Name]
			if !ok {
				s.record(credential.KindGroup, inv.Name, "no TG SecretRef mapped for AWX credential "+strconv.Quote(cred.Name))
				continue
			}
			user := strings.TrimSpace(cred.Inputs.Username)
			if user == "" || user == "$encrypted$" {
				user = s.defaultUser
			}
			// Dedup identical (inventory, user, key-ref) entries emitted by more than one JT (see seen, above).
			fp := strings.ToLower(strings.TrimSpace(inv.Name)) + "\x00" + user + "\x00" + strings.TrimSpace(ref)
			if seen[fp] {
				continue
			}
			spec := credential.BundleSpec{
				User:      user,
				Port:      22,
				Scheme:    credential.SchemeSSH,
				SSHKeyRef: config.SecretRef(strings.TrimSpace(ref)),
			}
			b, berr := credential.NewBundle(spec)
			if berr != nil {
				// A non-sealed / malformed mapped ref is a skip — never inject a blank or literal identity.
				s.record(credential.KindGroup, inv.Name, "bundle rejected for AWX credential "+strconv.Quote(cred.Name)+": "+berr.Error())
				continue
			}
			seen[fp] = true
			out = append(out, credential.SourceEntry{
				NativeID: fmt.Sprintf("jt:%d:cred:%d", jt.ID, cid),
				Selector: credential.Selector{Kind: credential.KindGroup, Pattern: inv.Name},
				Bundle:   b,
			})
		}
	}
	return out, nil
}

// plainListPath is the first-page path for a collection that is NOT inventory-scoped (credentials are global;
// job-template inventory filtering is done by listPath when an inventory id is configured).
func (s *Source) plainListPath(collection string) string {
	q := url.Values{}
	q.Set("page_size", strconv.Itoa(defaultPageSize))
	return "/api/v2/" + collection + "/?" + q.Encode()
}

// jtLabel is a stable, non-secret label for a job template used in a skip record when its inventory name is
// unavailable.
func jtLabel(jt awxJobTemplate) string {
	if n := strings.TrimSpace(jt.Name); n != "" {
		return n
	}
	return fmt.Sprintf("job-template:%d", jt.ID)
}

// listPath is the first-page path for a collection, scoped to the configured inventory when set.
func (s *Source) listPath(collection string) string {
	q := url.Values{}
	q.Set("page_size", strconv.Itoa(defaultPageSize))
	if s.inventoryID > 0 {
		q.Set("inventory", strconv.Itoa(s.inventoryID))
	}
	return "/api/v2/" + collection + "/?" + q.Encode()
}

// toEntry maps one host/group's ansible connection vars to a SourceEntry, or reports (false, reason) when it
// cannot build a valid, SecretRef-only Bundle. It NEVER reads a literal secret (ansible_password / raw key
// material) into a Bundle — only a key REFERENCE.
func (s *Source) toEntry(kind credential.SelectorKind, name, nativeID, rawVars string) (credential.SourceEntry, bool, string) {
	vars, err := parseVars(rawVars)
	if err != nil {
		return credential.SourceEntry{}, false, "unparseable ansible variables"
	}
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
	ref, ok, reason := s.secretRef(vars, scheme)
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

	b, berr := credential.NewBundle(spec)
	if berr != nil {
		// A non-sealed tg_secret_ref, or any other construction failure, is a skip — never inject a blank
		// or literal identity (fail closed).
		return credential.SourceEntry{}, false, "bundle rejected: " + berr.Error()
	}
	return credential.SourceEntry{
		NativeID: nativeID,
		Selector: credential.Selector{Kind: kind, Pattern: name},
		Bundle:   b,
	}, true, ""
}

// secretRef derives the sealed SecretRef for a host/group from its ansible vars — WHICH key applies, never
// its value. Priority: an explicit sealed tg_secret_ref (verbatim), else a key NAME
// (ansible_ssh_private_key_file basename for ssh/netconf, tg_credential otherwise) mapped through the
// source's RefScheme/RefPrefix/RefField. Reports (false, reason) when no reference can be derived.
func (s *Source) secretRef(vars map[string]string, scheme credential.ConnectionScheme) (config.SecretRef, bool, string) {
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
		return "", false, "no key reference derivable (AWX does not expose secret values; set tg_secret_ref, tg_credential, or ansible_ssh_private_key_file)"
	}
	ref := s.refScheme + ":" + s.refPrefix + keyName
	if s.refField != "" {
		ref += "#" + s.refField
	}
	return config.SecretRef(ref), true, ""
}

func (s *Source) record(kind credential.SelectorKind, name, reason string) {
	s.mu.Lock()
	s.skipped = append(s.skipped, SkipRecord{Kind: kind, Name: name, Reason: reason})
	s.mu.Unlock()
}

// compile-time proof the source satisfies the stable credential-source interface AND the optional
// membership-source capability (host↔inventory reconciliation, REQ-1605).
var _ credential.CredentialSource = (*Source)(nil)
var _ credential.MembershipSource = (*Source)(nil)

// ---- helpers ----------------------------------------------------------------------------------------

// parseVars decodes an AWX .variables field (JSON or YAML — YAML is a JSON superset, so yaml.v3 parses both)
// into a flat string map. An empty field is an empty map (not an error); a non-mapping document is an error.
// maxVarsBytes caps an AWX host/group .variables blob. Real inventory vars are small; a larger blob is a
// misconfiguration or an attack (a big-but-flat DoS payload) and is refused before decode so the entry is
// skipped fail-closed. yaml.v3 itself rejects billion-laughs alias expansion ("excessive aliasing"); this
// bound is the complementary defense against sheer input size.
const maxVarsBytes = 1 << 20 // 1 MiB

func parseVars(raw string) (map[string]string, error) {
	if strings.TrimSpace(raw) == "" {
		return map[string]string{}, nil
	}
	if len(raw) > maxVarsBytes {
		return nil, fmt.Errorf("awx: variables blob too large (%d bytes > %d limit)", len(raw), maxVarsBytes)
	}
	var m map[string]any
	if err := yaml.Unmarshal([]byte(raw), &m); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if v == nil {
			continue
		}
		out[k] = fmt.Sprint(v)
	}
	return out, nil
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

// awxErr extracts AWX's {"detail": "..."} error body into a short, secret-free message.
func awxErr(body []byte) string {
	var e struct {
		Detail string `json:"detail"`
	}
	if json.Unmarshal(body, &e) == nil && e.Detail != "" {
		return e.Detail
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
