// Package awxplaybooks is the READ-ONLY "playbooks-as-knowledge" lane of the Actuation Regime Engine
// (spec/017 T-017-5, REQ-1713/REQ-1714, TG-110). It pulls AWX job templates, their descriptions, and their
// inventory from the AWX / AWX-Tower REST API v2 and ingests them — as retrieval DATA, never an executable
// capability — into the SAME knowledge.Incident corpus the wiki lessons surface (spec/006 REQ-521) and the
// pgvector RAG retrieval plane (spec/012) already consume, so the agent DISCOVERS sanctioned runbooks and can
// PROPOSE them.
//
// DISCOVERY GRANTS NO AUTHORITY (REQ-1714, the load-bearing property). This lane READS and WRITES-TO-CORPUS
// only; it launches NOTHING and mutates NOTHING. Its AWX client exposes only HTTP GET methods — there is no
// launch, no POST, no path to /job_templates/{id}/launch/. A surfaced runbook is a knowledge.Incident (plain
// string data) the agent can cite; to have any EFFECT it must re-enter the actuation pipeline as a PROPOSAL
// subject to the full spec/013 interceptor chain (admission → never-auto floor → policy authorize →
// credential authenticate → mode chokepoint → execute → verify, spec/017 REQ-1702). This package imports no
// actuation, interceptor, regime-lane, or launch code — the discipline is structural, not merely documented.
//
// RE-READ BY ID (REQ-1713, INV-05). The lane lists templates to learn their ids, then RE-READS each job
// template AND each referenced inventory FROM the AWX API BY ID at ingest time, rather than trusting the
// cached mutable list copy — so an edited template/inventory is ingested as it currently is, never as a stale
// snapshot. The per-ingest inventory cache lives for one Run only; the next Run re-reads from AWX afresh.
//
// READ-ONLY SENSOR TOKEN (REQ-1708, INV-13). Authentication is a Bearer OAuth2 token resolved at runtime from
// a core/config.SecretRef (env:/file:/store:) — never a literal in code, config, the ledger, or an export.
// This lane uses the READ-ONLY SENSOR token, declared distinctly from any launch-capable token (there is no
// launch path here to use one). The token is cached in memory after first resolve and is NEVER logged.
//
// FAIL CLOSED (the task discipline: skip + log, never crash). A list-level transport/auth/decode failure
// returns an error and leaves the prior corpus intact (the caller — a worker CRON — logs it and skips the
// cycle). A single template/inventory that cannot be re-read or mapped is skipped-with-record and the healthy
// remainder still ingests, exactly as the credsource/awx inventory connector skips an un-mappable host.
//
// DISTROLESS-SAFE (INV-02). net/http + crypto/tls with an optional private-CA cert; NO subprocess, no
// awx-cli, no ansible binary. The client patterns (Bearer token from a SecretRef, bounded pagination, the
// off-host pagination guard, the token-scrubbing error path) mirror the verified-live modules/credsource/awx
// connector so both speak the SAME grounded AWX REST API.
//
// Grounded in the AWX REST API v2 (docs.ansible.com/projects/awx, job template + inventory schemas; the
// job_type / ask_variables_on_launch / inventory fields and the /job_templates/{id}/launch/ endpoint are the
// standard AWX JobTemplate surface), consistent with modules/credsource/awx (verified live against
// https://awx.example.net v24.6.1). Endpoints this lane reads (GET only):
//   - GET /api/v2/job_templates/            — paginated {count,next,previous,results[{id,...}]}; the id list.
//   - GET /api/v2/job_templates/{id}/       — one template re-read by id: id/name/description/job_type/
//     inventory/ask_variables_on_launch + summary_fields.inventory.{id,name}.
//   - GET /api/v2/inventories/{id}/         — one inventory re-read by id: id/name/description.
// It deliberately does NOT define /launch/ — the mutating effect is the awxjob lane (T-017-3), gated OFF.
//
// FOLLOW-ON (documented, out of scope for T-017-5). The ingest CRON is wired in cmd/worker/main.go in a LATER
// wave: construct a Client from TG_AWXPLAYBOOKS_* env (base URL + the read-only sensor SecretRef) and an
// Ingest over a CorpusStore (a FileCorpus at the playbooks knowledge corpus), call Run on an interval — the
// same read-merge-write shape as the lessons hop — then trigger knowledge.SyncIndex so the ingested runbooks
// become semantically retrievable. This package leaves that seam clean; it wires nothing into main.go itself.
//
// Provenance: [O] INV-05/INV-13/INV-21/INV-02, spec/017 (REQ-1713/REQ-1714), TG-110.
package awxplaybooks

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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/knowledge"
)

// SourceType is the vendor slug this knowledge lane serves.
const SourceType = "awxplaybooks"

// RefPrefix namespaces every ingested runbook's knowledge.Incident.ExternalRef ("awx-template:<id>") so a
// discovered playbook is always distinguishable from a resolved-incident lesson and can never collide with an
// operator's incident external_ref namespace.
const RefPrefix = "awx-template:"

// defaultTimeout bounds a single API round-trip so a hung AWX fails closed promptly.
const defaultTimeout = 20 * time.Second

// defaultPageSize is the page size requested when paginating job templates (AWX caps this server-side).
const defaultPageSize = 200

// maxPages bounds pagination so a misbehaving/looping .next can never hang a Run forever (fail closed).
const maxPages = 10_000

// Doer is the minimal HTTP contract; *http.Client satisfies it and tests inject a fake AWX. It is the ONLY
// way this lane reaches the network, and every request it issues is an HTTP GET (see raw).
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Client is a READ-ONLY AWX REST API client for the knowledge lane. It holds the read-only sensor Bearer
// token (resolved from a SecretRef, cached after first use, never logged) and exposes ONLY GET reads — it has
// no launch, POST, or mutate method by construction (REQ-1714).
type Client struct {
	baseURL  string // e.g. "https://awx.example.net" (no trailing slash, no /api)
	tokenRef config.SecretRef
	http     Doer

	mu     sync.Mutex
	cached string // cached resolved read-only sensor token; never logged
}

// ClientConfig constructs a Client. BaseURL is required. TokenRef (the READ-ONLY sensor Bearer token, a
// core/config.SecretRef — never a literal) is required. CACertPath, if set, is trusted for TLS (AWX behind a
// private CA). HTTPClient overrides transport entirely (tests inject a fake AWX).
type ClientConfig struct {
	BaseURL    string
	TokenRef   config.SecretRef
	CACertPath string
	HTTPClient Doer
}

// NewClient builds a read-only AWX client. It fails closed if the base URL is missing, the token reference is
// empty (this lane authenticates — an anonymous pull is refused), or a configured CA cert cannot be loaded.
func NewClient(cfg ClientConfig) (*Client, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, fmt.Errorf("awxplaybooks: base URL is required")
	}
	if strings.TrimSpace(string(cfg.TokenRef)) == "" {
		return nil, fmt.Errorf("awxplaybooks: read-only sensor token reference is required (fail closed)")
	}
	c := &Client{baseURL: strings.TrimRight(cfg.BaseURL, "/"), tokenRef: cfg.TokenRef, http: cfg.HTTPClient}
	if c.http == nil {
		hc := &http.Client{Timeout: defaultTimeout}
		if cfg.CACertPath != "" {
			pem, err := os.ReadFile(cfg.CACertPath)
			if err != nil {
				return nil, fmt.Errorf("awxplaybooks: read CA cert: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("awxplaybooks: CA cert %q contains no valid certificate", cfg.CACertPath)
			}
			hc.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
		}
		c.http = hc
	}
	return c, nil
}

// token resolves the read-only Bearer token once and caches it. An empty/unset token fails closed.
func (c *Client) token() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached != "" {
		return c.cached, nil
	}
	tok, err := c.tokenRef.Resolve()
	if err != nil {
		return "", fmt.Errorf("awxplaybooks: resolve read-only sensor token: %w", err)
	}
	if strings.TrimSpace(tok) == "" {
		return "", fmt.Errorf("awxplaybooks: read-only sensor token is empty (fail closed)")
	}
	c.cached = tok
	return tok, nil
}

// getJSON issues an authenticated GET against a URL (absolute, or a path/relative URL joined onto the base)
// and decodes the JSON body into v. It fails closed on unreachable/denied/not-found/malformed.
func (c *Client) getJSON(ctx context.Context, urlOrPath string, v any) error {
	full, err := c.resolveURL(urlOrPath)
	if err != nil {
		return err
	}
	raw, err := c.raw(ctx, full)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("awxplaybooks: decode %s: %w", scrub(urlOrPath), err)
	}
	return nil
}

// resolveURL turns a .next link (absolute URL OR a root-relative path like "/api/v2/job_templates/?page=2")
// into a full URL against the client's base, refusing any off-host absolute link so a compromised or MITM'd
// AWX cannot redirect a Bearer-token-carrying read to another host (INV-13).
func (c *Client) resolveURL(u string) (string, error) {
	switch {
	case strings.HasPrefix(u, "http://"), strings.HasPrefix(u, "https://"):
		nu, err := url.Parse(u)
		if err != nil {
			return "", fmt.Errorf("awxplaybooks: unparseable pagination url: %w", err)
		}
		bu, err := url.Parse(c.baseURL)
		if err != nil {
			return "", fmt.Errorf("awxplaybooks: unparseable base url: %w", err)
		}
		if !strings.EqualFold(nu.Host, bu.Host) || !strings.EqualFold(nu.Scheme, bu.Scheme) {
			return "", fmt.Errorf("awxplaybooks: refusing off-host pagination url (got host %q scheme %q, expected %q %q)",
				nu.Host, nu.Scheme, bu.Host, bu.Scheme)
		}
		return u, nil
	case strings.HasPrefix(u, "/"):
		return c.baseURL + u, nil
	default:
		return c.baseURL + "/" + u, nil
	}
}

// raw issues one GET — this lane's ONLY HTTP method — attaching the read-only Bearer token, and returns the
// body on 2xx or an error (never a body with a nil error). The token is NEVER placed in an error message.
func (c *Client) raw(ctx context.Context, fullURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	tok, terr := c.token()
	if terr != nil {
		return nil, terr
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("awxplaybooks: GET %s: %w", scrub(fullURL), err) // unreachable → fail closed
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("awxplaybooks: GET %s: status %d: %s", scrub(fullURL), resp.StatusCode, awxErr(out))
	}
	return out, nil
}

// page is the AWX list-response envelope (count/next/previous/results).
type page[T any] struct {
	Count    int    `json:"count"`
	Next     string `json:"next"`
	Previous string `json:"previous"`
	Results  []T    `json:"results"`
}

// paginate walks the .next chain from an initial path, accumulating every result. It fails closed on any page
// error and bounds the walk at maxPages.
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
		return nil, fmt.Errorf("awxplaybooks: pagination exceeded %d pages (fail closed)", maxPages)
	}
	return all, nil
}

// ---- AWX object shapes (only the fields this lane reads) --------------------------------------------

// templateListItem is the minimal shape read from the LIST endpoint — only the id, because REQ-1713 requires
// re-reading each template from the API by id rather than trusting this cached list copy.
type templateListItem struct {
	ID int `json:"id"`
}

// JobTemplate is the subset of a /job_templates/{id}/ result this lane ingests: identity + the human-authored
// description that IS the runbook knowledge, the job_type (run vs the read-only check/setup), the referenced
// inventory id, and the inventory name via summary_fields.
type JobTemplate struct {
	ID                   int    `json:"id"`
	Name                 string `json:"name"`
	Description          string `json:"description"`
	JobType              string `json:"job_type"`
	Inventory            int    `json:"inventory"`
	AskVariablesOnLaunch bool   `json:"ask_variables_on_launch"`
	SummaryFields        struct {
		Inventory struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"inventory"`
	} `json:"summary_fields"`
}

// Inventory is the subset of an /inventories/{id}/ result this lane ingests: the estate group name + its
// description (non-secret; host names and inventory names only).
type Inventory struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ListJobTemplateIDs pulls the job-template id list (GET /api/v2/job_templates/, paginated). It returns ids
// ONLY — the content of each template is re-read by id (REQ-1713), never taken from this list copy.
func (c *Client) ListJobTemplateIDs(ctx context.Context) ([]int, error) {
	items, err := paginate[templateListItem](ctx, c, listPath("job_templates"))
	if err != nil {
		return nil, fmt.Errorf("awxplaybooks: list job templates: %w", err)
	}
	ids := make([]int, 0, len(items))
	for _, it := range items {
		if it.ID > 0 {
			ids = append(ids, it.ID)
		}
	}
	return ids, nil
}

// GetJobTemplate RE-READS one job template from the AWX API by id (GET /api/v2/job_templates/{id}/), INV-05.
func (c *Client) GetJobTemplate(ctx context.Context, id int) (JobTemplate, error) {
	var jt JobTemplate
	if err := c.getJSON(ctx, "/api/v2/job_templates/"+strconv.Itoa(id)+"/", &jt); err != nil {
		return JobTemplate{}, err
	}
	return jt, nil
}

// GetInventory RE-READS one inventory from the AWX API by id (GET /api/v2/inventories/{id}/), INV-05.
func (c *Client) GetInventory(ctx context.Context, id int) (Inventory, error) {
	var inv Inventory
	if err := c.getJSON(ctx, "/api/v2/inventories/"+strconv.Itoa(id)+"/", &inv); err != nil {
		return Inventory{}, err
	}
	return inv, nil
}

// listPath is the first-page path for a collection (page_size only; this lane pulls all templates).
func listPath(collection string) string {
	q := url.Values{}
	q.Set("page_size", strconv.Itoa(defaultPageSize))
	return "/api/v2/" + collection + "/?" + q.Encode()
}

// ---- the knowledge corpus seam (reuses the existing wiki + RAG plane) -------------------------------

// CorpusStore is the read-merge-write seam over the SHARED knowledge.Incident corpus that BOTH the wiki
// lessons surface (spec/006 REQ-521) and the pgvector RAG retrieval plane (spec/012) already consume. The
// ingest reuses this existing plane rather than building a second retrieval store: it maps AWX templates to
// knowledge.Incident rows and merges them in via knowledge.MergeCorpus. A production file-backed store is
// FileCorpus (mirroring the lessons hop's atomic corpus write); tests inject an in-memory store.
type CorpusStore interface {
	Load(ctx context.Context) ([]knowledge.Incident, error)
	Save(ctx context.Context, corpus []knowledge.Incident) error
}

// FileCorpus is the production CorpusStore: the JSON corpus file knowledge.ParseCorpus reads and the retriever
// reloads. Save is an atomic temp-file + rename so a concurrent reader never sees a torn corpus — the exact
// discipline the lessons persistence hop uses.
type FileCorpus struct {
	Path string
}

// Load parses the corpus file. A missing file is the empty corpus (nil, nil), not an error — the lane has not
// ingested yet.
func (f FileCorpus) Load(_ context.Context) ([]knowledge.Incident, error) {
	if strings.TrimSpace(f.Path) == "" {
		return nil, fmt.Errorf("awxplaybooks: FileCorpus has no path")
	}
	fh, err := os.Open(f.Path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("awxplaybooks: open corpus %s: %w", f.Path, err)
	}
	defer fh.Close()
	return knowledge.ParseCorpus(fh)
}

// Save writes the corpus atomically (temp + rename), serialized with knowledge.WriteCorpus.
func (f FileCorpus) Save(_ context.Context, corpus []knowledge.Incident) error {
	if strings.TrimSpace(f.Path) == "" {
		return fmt.Errorf("awxplaybooks: FileCorpus has no path")
	}
	tmp := f.Path + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("awxplaybooks: create corpus temp %s: %w", tmp, err)
	}
	if werr := knowledge.WriteCorpus(out, corpus); werr != nil {
		out.Close()
		os.Remove(tmp)
		return fmt.Errorf("awxplaybooks: serialize corpus: %w", werr)
	}
	if cerr := out.Close(); cerr != nil {
		os.Remove(tmp)
		return fmt.Errorf("awxplaybooks: close corpus temp: %w", cerr)
	}
	if rerr := os.Rename(tmp, f.Path); rerr != nil {
		os.Remove(tmp)
		return fmt.Errorf("awxplaybooks: replace corpus %s: %w", f.Path, rerr)
	}
	return nil
}

// ---- the read-only knowledge lane -------------------------------------------------------------------

// SkipRecord records one template/inventory that could not be re-read or mapped and was skipped (fail closed:
// nothing half-ingested). The reason is safe to log (never a secret).
type SkipRecord struct {
	Ref    string
	Reason string
}

// Result is one Run's outcome (coverage observability). Templates is how many templates were re-read and
// mapped; Added/Updated are net-new/content-changed runbook rows merged into the corpus; Skipped lists what
// was skipped and why.
type Result struct {
	Templates int
	Added     int
	Updated   int
	Skipped   []SkipRecord
}

// Ingest is the read-only playbooks-as-knowledge lane (REQ-1713/1714). It pulls AWX job templates + their
// inventory READ-ONLY (re-read by id) and merges them, as knowledge.Incident DATA, into the shared corpus the
// wiki + RAG plane consume. It launches nothing and mutates nothing.
type Ingest struct {
	Client *Client
	Store  CorpusStore
	Logf   func(format string, args ...any) // skip/summary logging; nil ⇒ no-op. NEVER passed a secret.
}

// NewIngest builds a lane. It fails closed if the client or store is missing.
func NewIngest(client *Client, store CorpusStore) (*Ingest, error) {
	if client == nil {
		return nil, fmt.Errorf("awxplaybooks: ingest requires a read-only client")
	}
	if store == nil {
		return nil, fmt.Errorf("awxplaybooks: ingest requires a corpus store")
	}
	return &Ingest{Client: client, Store: store}, nil
}

// Run performs the READ-ONLY ingest: list template ids, RE-READ each template AND its inventory from AWX by id
// (INV-05), map each to a knowledge.Incident, and merge the runbook rows into the shared corpus. It FAILS
// CLOSED — a list-level read failure or a corpus load/save failure returns an error with the prior corpus
// intact; a single template/inventory that cannot be re-read or mapped is skipped-with-record and the healthy
// remainder still ingests. It LAUNCHES NOTHING and MUTATES NOTHING on any target (REQ-1714): its only writes
// are corpus rows the retrieval plane reads.
func (ing *Ingest) Run(ctx context.Context) (Result, error) {
	var res Result

	ids, err := ing.Client.ListJobTemplateIDs(ctx)
	if err != nil {
		return res, err // list-level failure: fail closed, corpus untouched
	}

	invCache := map[int]Inventory{} // re-read once per inventory id per Run; the next Run re-reads afresh
	incidents := make([]knowledge.Incident, 0, len(ids))
	for _, id := range ids {
		jt, terr := ing.Client.GetJobTemplate(ctx, id) // RE-READ BY ID (INV-05) — never the list copy
		if terr != nil {
			res.Skipped = append(res.Skipped, SkipRecord{Ref: RefPrefix + strconv.Itoa(id), Reason: "re-read failed: " + terr.Error()})
			continue
		}
		var inv Inventory
		if jt.Inventory > 0 {
			cached, ok := invCache[jt.Inventory]
			if !ok {
				got, ierr := ing.Client.GetInventory(ctx, jt.Inventory) // RE-READ inventory BY ID (INV-05)
				if ierr != nil {
					// A missing/denied inventory read is not fatal to the runbook's knowledge value: fall back to
					// the name AWX already returned in the template's summary_fields, and record the skip.
					got = Inventory{ID: jt.Inventory, Name: strings.TrimSpace(jt.SummaryFields.Inventory.Name)}
					res.Skipped = append(res.Skipped, SkipRecord{
						Ref:    RefPrefix + strconv.Itoa(id),
						Reason: "inventory " + strconv.Itoa(jt.Inventory) + " re-read failed: " + ierr.Error(),
					})
				}
				invCache[jt.Inventory] = got
				inv = got
			} else {
				inv = cached
			}
		}
		inc, ok, reason := toIncident(jt, inv)
		if !ok {
			res.Skipped = append(res.Skipped, SkipRecord{Ref: RefPrefix + strconv.Itoa(id), Reason: reason})
			continue
		}
		incidents = append(incidents, inc)
		res.Templates++
	}

	existing, err := ing.Store.Load(ctx)
	if err != nil {
		return res, fmt.Errorf("awxplaybooks: load corpus: %w", err) // fail closed, do not write a partial view
	}

	// Count net-new + content-changed runbook rows against the existing corpus (idempotent re-ingest is a
	// no-op; an edited description — the reason INV-05 re-reads — is an update). Only persist when something
	// actually changed, so an unchanged ingest never churns the corpus file or the reload loop.
	prior := make(map[string]knowledge.Incident, len(existing))
	for _, e := range existing {
		prior[e.ExternalRef] = e
	}
	for _, inc := range incidents {
		p, ok := prior[inc.ExternalRef]
		switch {
		case !ok:
			res.Added++
		case knowledge.ContentHash(p) != knowledge.ContentHash(inc):
			res.Updated++
		}
	}
	if res.Added == 0 && res.Updated == 0 {
		ing.logf("awxplaybooks: %d template(s) ingested, corpus unchanged (%d skipped)", res.Templates, len(res.Skipped))
		return res, nil
	}

	merged := knowledge.MergeCorpus(existing, incidents)
	if serr := ing.Store.Save(ctx, merged); serr != nil {
		return res, fmt.Errorf("awxplaybooks: save corpus: %w", serr)
	}
	ing.logf("awxplaybooks: ingested %d runbook(s) into the knowledge corpus (%d new, %d updated, %d skipped)",
		res.Templates, res.Added, res.Updated, len(res.Skipped))
	return res, nil
}

// toIncident maps one re-read job template (+ its re-read inventory) to a knowledge.Incident — plain retrieval
// DATA, never an executable capability (REQ-1714). The Summary carries the human-authored runbook knowledge
// (name + description + job_type + inventory) the retriever ranks over; the Resolution carries the PROPOSAL
// DISCIPLINE — a discovered runbook has no authority and only re-enters as a proposal through the full
// interceptor chain. A template with no name AND no description carries no knowledge and is skipped.
func toIncident(jt JobTemplate, inv Inventory) (knowledge.Incident, bool, string) {
	name := strings.TrimSpace(jt.Name)
	desc := strings.TrimSpace(jt.Description)
	if name == "" && desc == "" {
		return knowledge.Incident{}, false, "job template has neither name nor description (no knowledge to ingest)"
	}
	invName := strings.TrimSpace(inv.Name)
	if invName == "" {
		invName = strings.TrimSpace(jt.SummaryFields.Inventory.Name)
	}

	var b strings.Builder
	if name != "" {
		b.WriteString("AWX job template \"" + name + "\"")
	} else {
		b.WriteString("AWX job template #" + strconv.Itoa(jt.ID))
	}
	if jtype := strings.TrimSpace(jt.JobType); jtype != "" {
		b.WriteString(" (job_type " + jtype + ")")
	}
	if invName != "" {
		b.WriteString(" over inventory \"" + invName + "\"")
	}
	b.WriteString(".")
	if desc != "" {
		b.WriteString(" " + desc)
	}
	if invDesc := strings.TrimSpace(inv.Description); invDesc != "" {
		b.WriteString(" Inventory: " + invDesc)
	}

	tags := []string{"awx-runbook", "awx-template"}
	if jtype := strings.TrimSpace(jt.JobType); jtype != "" {
		tags = append(tags, "job_type:"+jtype)
	}
	if invName != "" {
		tags = append(tags, "inventory:"+invName)
	}

	return knowledge.Incident{
		ExternalRef: RefPrefix + strconv.Itoa(jt.ID),
		Summary:     b.String(),
		// Resolution is the agent-facing PROPOSAL DISCIPLINE (rendered as data, never an instruction): a
		// discovered runbook grants no authority — it must re-enter the actuation pipeline as a proposal
		// subject to the full spec/013 interceptor chain (policy authorize, credential authenticate, mode
		// chokepoint). This lane launches nothing.
		Resolution: "Sanctioned AWX runbook (discovery only): propose it through the actuation interceptor " +
			"chain — policy authorize, credential authenticate, mode chokepoint. Discovery grants no authority; " +
			"this knowledge lane launches nothing.",
		Tags: tags,
	}, true, ""
}

func (ing *Ingest) logf(format string, args ...any) {
	if ing.Logf != nil {
		ing.Logf(format, args...)
	}
}

// ---- helpers ----------------------------------------------------------------------------------------

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
