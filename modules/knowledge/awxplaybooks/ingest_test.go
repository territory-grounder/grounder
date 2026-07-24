package awxplaybooks

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/territory-grounder/grounder/core/knowledge"
)

// recordingDoer is a fake AWX that serves canned read-only JSON AND asserts the discovery lane never mutates:
// it FAILS the test the instant it sees any HTTP method other than GET or any request to a /launch/ path (the
// only way an AWX job template is actuated). It records every method+path so a test can prove re-read-by-id.
type recordingDoer struct {
	t          *testing.T
	mu         sync.Mutex
	methods    []string
	paths      []string
	authHeader string
	// template7Desc lets a test mutate template 7's description to prove an INV-05 re-read picks up the edit.
	template7Desc string
	// notFound is a set of paths to answer 404 (skip-with-record path).
	notFound map[string]bool
	// failList makes the job_templates LIST return 500 (fail-closed path).
	failList bool
}

func (d *recordingDoer) Do(req *http.Request) (*http.Response, error) {
	d.mu.Lock()
	d.methods = append(d.methods, req.Method)
	d.paths = append(d.paths, req.URL.Path)
	if h := req.Header.Get("Authorization"); h != "" {
		d.authHeader = h
	}
	d.mu.Unlock()

	// The load-bearing read-only assertion: a knowledge lane that ever issues a non-GET or hits /launch/ has
	// stopped being discovery-only. Fail hard, not soft.
	if req.Method != http.MethodGet {
		d.t.Fatalf("read-only knowledge lane issued a non-GET request: %s %s", req.Method, req.URL.Path)
	}
	if strings.Contains(req.URL.Path, "/launch/") {
		d.t.Fatalf("read-only knowledge lane hit a launch endpoint: %s", req.URL.Path)
	}

	path := req.URL.Path
	if d.notFound[path] {
		return resp(404, `{"detail":"Not found."}`), nil
	}
	desc7 := d.template7Desc
	if desc7 == "" {
		desc7 = "Restart the nginx service and verify it comes back healthy."
	}
	switch {
	case path == "/api/v2/job_templates/" && d.failList:
		return resp(500, `{"detail":"boom"}`), nil
	case path == "/api/v2/job_templates/":
		// LIST returns ids ONLY (the lane re-reads each by id; it must not trust this copy).
		return resp(200, `{"count":2,"next":null,"previous":null,"results":[{"id":7},{"id":9}]}`), nil
	case path == "/api/v2/job_templates/7/":
		return resp(200, fmt.Sprintf(`{"id":7,"name":"nginx-restart","description":%q,"job_type":"run",
			"inventory":3,"ask_variables_on_launch":true,
			"summary_fields":{"inventory":{"id":3,"name":"web-tier"}}}`, desc7)), nil
	case path == "/api/v2/job_templates/9/":
		return resp(200, `{"id":9,"name":"gather-facts","description":"Read-only setup fact-gathering.",
			"job_type":"check","inventory":3,"ask_variables_on_launch":false,
			"summary_fields":{"inventory":{"id":3,"name":"web-tier"}}}`), nil
	case path == "/api/v2/inventories/3/":
		return resp(200, `{"id":3,"name":"web-tier","description":"Front-end web hosts."}`), nil
	}
	return resp(200, `{}`), nil
}

func resp(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

// memStore is an in-memory CorpusStore for tests.
type memStore struct {
	corpus []knowledge.Incident
	saves  int
}

func (m *memStore) Load(context.Context) ([]knowledge.Incident, error) { return m.corpus, nil }
func (m *memStore) Save(_ context.Context, c []knowledge.Incident) error {
	m.corpus = c
	m.saves++
	return nil
}

func newClientT(t *testing.T, d Doer) *Client {
	t.Helper()
	t.Setenv("AWXPB_RO_TOKEN", "ro-sensor-token-value")
	c, err := NewClient(ClientConfig{
		BaseURL:    "https://awx.test",
		TokenRef:   "env:AWXPB_RO_TOKEN",
		HTTPClient: d,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// TestIngest_ReadOnlyReReadById proves REQ-1713: templates + descriptions + inventory are pulled read-only
// into the knowledge corpus (which the wiki lessons surface and the pgvector RAG plane both consume) and each
// object is RE-READ from the AWX API by id rather than trusting the cached list copy.
func TestIngest_ReadOnlyReReadById(t *testing.T) {
	d := &recordingDoer{t: t}
	store := &memStore{}
	ing, err := NewIngest(newClientT(t, d), store)
	if err != nil {
		t.Fatalf("NewIngest: %v", err)
	}

	res, err := ing.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Templates != 2 || res.Added != 2 || res.Updated != 0 {
		t.Fatalf("want 2 templates / 2 added / 0 updated, got %+v", res)
	}
	if len(res.Skipped) != 0 {
		t.Fatalf("no skips expected, got %+v", res.Skipped)
	}

	// Re-read BY ID: the per-object detail endpoints must have been hit (not just the list).
	joined := strings.Join(d.paths, " ")
	for _, want := range []string{"/api/v2/job_templates/", "/api/v2/job_templates/7/", "/api/v2/job_templates/9/", "/api/v2/inventories/3/"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected the lane to re-read %s by id; paths were %v", want, d.paths)
		}
	}
	// EVERY request was a GET (recordingDoer.Do already fails on any non-GET; assert non-empty for clarity).
	if len(d.methods) == 0 {
		t.Fatal("expected at least one GET")
	}
	for _, m := range d.methods {
		if m != http.MethodGet {
			t.Fatalf("non-GET method recorded: %s", m)
		}
	}

	// The corpus now carries both runbooks as plain knowledge.Incident DATA, namespaced awx-template:<id>.
	byRef := map[string]knowledge.Incident{}
	for _, inc := range store.corpus {
		byRef[inc.ExternalRef] = inc
	}
	inc7, ok := byRef["awx-template:7"]
	if !ok {
		t.Fatalf("awx-template:7 not ingested; corpus=%+v", store.corpus)
	}
	if !strings.Contains(inc7.Summary, "nginx-restart") || !strings.Contains(inc7.Summary, "web-tier") {
		t.Fatalf("runbook summary must carry the real name + inventory, got %q", inc7.Summary)
	}
	if !strings.Contains(inc7.Summary, "Restart the nginx service") {
		t.Fatalf("runbook summary must carry the re-read description, got %q", inc7.Summary)
	}

	// It reached the RAG retrieval plane: a lexical query over the ingested corpus surfaces the runbook.
	r := knowledge.NewLexicalRetriever(store.corpus)
	hits := r.Retrieve(knowledge.Query{Summary: "nginx restart web-tier", Tags: []string{"awx-runbook"}}, 5)
	found := false
	for _, h := range hits {
		if h.Incident.ExternalRef == "awx-template:7" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the ingested runbook must be retrievable from the RAG plane; hits=%+v", hits)
	}
}

// TestIngest_LaunchesNothing_ProposalOnly proves REQ-1714: the knowledge lane launches no job and mutates
// nothing; the surfaced runbook is DATA carrying the proposal discipline (re-enter through the interceptor
// chain), never an executable capability. recordingDoer.Do fails on any non-GET or /launch/ request, so a
// clean Run is itself the proof no launch was attempted; here we also assert the surfaced content is a
// proposal, not an authority.
func TestIngest_LaunchesNothing_ProposalOnly(t *testing.T) {
	d := &recordingDoer{t: t}
	store := &memStore{}
	ing, _ := NewIngest(newClientT(t, d), store)
	if _, err := ing.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, p := range d.paths {
		if strings.Contains(p, "/launch/") {
			t.Fatalf("a launch endpoint was reached: %s", p)
		}
	}
	var inc7 knowledge.Incident
	for _, inc := range store.corpus {
		if inc.ExternalRef == "awx-template:7" {
			inc7 = inc
		}
	}
	// The surfaced runbook explicitly tells the agent it grants no authority and must be PROPOSED through the
	// interceptor chain — the discovery-grants-no-authority contract rendered as agent-facing data.
	if !strings.Contains(inc7.Resolution, "propose") || !strings.Contains(inc7.Resolution, "interceptor") {
		t.Fatalf("surfaced runbook must carry the proposal discipline, got %q", inc7.Resolution)
	}
	if !strings.Contains(strings.ToLower(inc7.Resolution), "launches nothing") {
		t.Fatalf("surfaced runbook must state the lane launches nothing, got %q", inc7.Resolution)
	}
}

// TestIngest_Idempotent proves a second unchanged ingest is a no-op (no new/updated rows, no corpus churn).
func TestIngest_Idempotent(t *testing.T) {
	d := &recordingDoer{t: t}
	store := &memStore{}
	ing, _ := NewIngest(newClientT(t, d), store)

	if _, err := ing.Run(context.Background()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	savesAfterFirst := store.saves
	res, err := ing.Run(context.Background())
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if res.Added != 0 || res.Updated != 0 {
		t.Fatalf("re-ingest must be a no-op, got %+v", res)
	}
	if store.saves != savesAfterFirst {
		t.Fatalf("unchanged re-ingest must not re-write the corpus (saves %d -> %d)", savesAfterFirst, store.saves)
	}
}

// TestIngest_ReReadPicksUpEdit proves the INV-05 re-read matters: an edited template description is ingested
// as an update, not ignored as a stale cached copy.
func TestIngest_ReReadPicksUpEdit(t *testing.T) {
	d := &recordingDoer{t: t}
	store := &memStore{}
	ing, _ := NewIngest(newClientT(t, d), store)
	if _, err := ing.Run(context.Background()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	d.template7Desc = "Restart nginx AND flush the cache before verifying." // the template was edited in AWX
	res, err := ing.Run(context.Background())
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if res.Updated != 1 || res.Added != 0 {
		t.Fatalf("an edited re-read template must be a single update, got %+v", res)
	}
	for _, inc := range store.corpus {
		if inc.ExternalRef == "awx-template:7" && !strings.Contains(inc.Summary, "flush the cache") {
			t.Fatalf("the corpus must reflect the re-read edit, got %q", inc.Summary)
		}
	}
}

// TestIngest_FailClosedOnListError proves a list-level read failure fails closed: an error is returned and the
// prior corpus is left untouched (never a partial write).
func TestIngest_FailClosedOnListError(t *testing.T) {
	d := &recordingDoer{t: t, failList: true}
	store := &memStore{corpus: []knowledge.Incident{{ExternalRef: "awx-template:1", Summary: "prior"}}}
	ing, _ := NewIngest(newClientT(t, d), store)
	if _, err := ing.Run(context.Background()); err == nil {
		t.Fatal("a list-level failure must return an error (fail closed)")
	}
	if store.saves != 0 {
		t.Fatalf("fail-closed must not write the corpus, saves=%d", store.saves)
	}
	if len(store.corpus) != 1 || store.corpus[0].ExternalRef != "awx-template:1" {
		t.Fatalf("prior corpus must be intact, got %+v", store.corpus)
	}
}

// TestIngest_SkipWithRecord proves a single template that cannot be re-read is skipped-with-record while the
// healthy remainder still ingests (fail closed per object, not per Run).
func TestIngest_SkipWithRecord(t *testing.T) {
	d := &recordingDoer{t: t, notFound: map[string]bool{"/api/v2/job_templates/9/": true}}
	store := &memStore{}
	ing, _ := NewIngest(newClientT(t, d), store)
	res, err := ing.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Templates != 1 || res.Added != 1 {
		t.Fatalf("the healthy template must still ingest, got %+v", res)
	}
	if len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0].Ref, "9") {
		t.Fatalf("the unreadable template must be skipped-with-record, got %+v", res.Skipped)
	}
}

// TestToken_ResolvedFromSecretRef proves the Bearer token is resolved from the SecretRef and sent as a header,
// and that the literal token value never travels in a request path/query (INV-13).
func TestToken_ResolvedFromSecretRef(t *testing.T) {
	d := &recordingDoer{t: t}
	store := &memStore{}
	ing, _ := NewIngest(newClientT(t, d), store)
	if _, err := ing.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if d.authHeader != "Bearer ro-sensor-token-value" {
		t.Fatalf("token must be resolved from the SecretRef and sent as a Bearer header, got %q", d.authHeader)
	}
	for _, p := range d.paths {
		if strings.Contains(p, "ro-sensor-token-value") {
			t.Fatalf("the token literal must never appear in a request path: %s", p)
		}
	}
}

// TestNewClient_FailClosed proves the client refuses to build without a base URL or a token reference.
func TestNewClient_FailClosed(t *testing.T) {
	if _, err := NewClient(ClientConfig{TokenRef: "env:X"}); err == nil {
		t.Fatal("missing base URL must fail closed")
	}
	if _, err := NewClient(ClientConfig{BaseURL: "https://awx.test"}); err == nil {
		t.Fatal("missing token reference must fail closed")
	}
}

// TestFileCorpus_RoundTrip proves the production FileCorpus store round-trips the ingested runbooks through
// the SAME knowledge.ParseCorpus/WriteCorpus interfaces the wiki + RAG plane read (an atomic write).
func TestFileCorpus_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "knowledge.json")
	store := FileCorpus{Path: path}

	d := &recordingDoer{t: t}
	ing, _ := NewIngest(newClientT(t, d), store)
	if _, err := ing.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("corpus file must be written: %v", err)
	}
	reloaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(reloaded) != 2 {
		t.Fatalf("the file corpus must round-trip 2 runbooks, got %d", len(reloaded))
	}
}
