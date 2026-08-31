package acceptance

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/core/knowledge"
	"github.com/territory-grounder/grounder/modules/knowledge/awxplaybooks"
)

// T-017-5 — the read-only playbooks-as-knowledge lane (REQ-1713/REQ-1714). Registered from this file's init()
// so the shared acceptance harness is never edited by parallel task work.
func init() {
	stepRegistrars = append(stepRegistrars, registerAwxPlaybooksSteps)
}

// acceptFakeAWX is a canned READ-ONLY AWX for the acceptance oracle. It records every HTTP method + path and
// flags any non-GET request or any /launch/ hit — the discovery lane must never do either. Serving the same
// object by id twice is fine; the point is that the lane RE-READS by id.
type acceptFakeAWX struct {
	mu        sync.Mutex
	methods   []string
	paths     []string
	sawNonGET bool
	sawLaunch bool
}

func (f *acceptFakeAWX) Do(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.methods = append(f.methods, req.Method)
	f.paths = append(f.paths, req.URL.Path)
	if req.Method != http.MethodGet {
		f.sawNonGET = true
	}
	if strings.Contains(req.URL.Path, "/launch/") {
		f.sawLaunch = true
	}
	f.mu.Unlock()

	body := "{}"
	switch req.URL.Path {
	case "/api/v2/job_templates/":
		body = `{"count":2,"next":null,"previous":null,"results":[{"id":7},{"id":9}]}`
	case "/api/v2/job_templates/7/":
		body = `{"id":7,"name":"nginx-restart","description":"Restart the nginx service and verify health.",
			"job_type":"run","inventory":3,"ask_variables_on_launch":true,
			"summary_fields":{"inventory":{"id":3,"name":"web-tier"}}}`
	case "/api/v2/job_templates/9/":
		body = `{"id":9,"name":"gather-facts","description":"Read-only setup fact-gathering.",
			"job_type":"check","inventory":3,"ask_variables_on_launch":false,
			"summary_fields":{"inventory":{"id":3,"name":"web-tier"}}}`
	case "/api/v2/inventories/3/":
		body = `{"id":3,"name":"web-tier","description":"Front-end web hosts."}`
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}

// acceptCorpus is an in-memory CorpusStore standing in for the shared wiki+RAG corpus.
type acceptCorpus struct {
	corpus []knowledge.Incident
}

func (m *acceptCorpus) Load(context.Context) ([]knowledge.Incident, error) { return m.corpus, nil }
func (m *acceptCorpus) Save(_ context.Context, c []knowledge.Incident) error {
	m.corpus = c
	return nil
}

type awxKnowledgeWorld struct {
	awx       *acceptFakeAWX
	store     *acceptCorpus
	res       awxplaybooks.Result
	surfaced  knowledge.Incident
	ingestErr error
}

func (w *awxKnowledgeWorld) ingest() error {
	_ = os.Setenv("TG_AWXPB_ACCEPT_TOKEN", "ro-sensor-token")
	w.awx = &acceptFakeAWX{}
	w.store = &acceptCorpus{}
	client, err := awxplaybooks.NewClient(awxplaybooks.ClientConfig{
		BaseURL:    "https://awx.test",
		TokenRef:   "env:TG_AWXPB_ACCEPT_TOKEN", // the READ-ONLY sensor token, a sealed SecretRef (REQ-1708)
		HTTPClient: w.awx,
	})
	if err != nil {
		return err
	}
	ing, err := awxplaybooks.NewIngest(client, w.store)
	if err != nil {
		return err
	}
	w.res, w.ingestErr = ing.Run(context.Background())
	return w.ingestErr
}

func (w *awxKnowledgeWorld) surfaced7() (knowledge.Incident, bool) {
	for _, inc := range w.store.corpus {
		if inc.ExternalRef == awxplaybooks.RefPrefix+"7" {
			return inc, true
		}
	}
	return knowledge.Incident{}, false
}

func registerAwxPlaybooksSteps(sc *godog.ScenarioContext) {
	w := &awxKnowledgeWorld{}

	// ---- REQ-1713: read-only ingest into the wiki + RAG plane, re-read by id ----
	sc.Step(`^AWX job templates descriptions and inventory$`, func() error {
		// The canned read-only AWX is set up; the ingest runs in the When step.
		return nil
	})
	sc.Step(`^the knowledge lane ingests them$`, func() error {
		return w.ingest()
	})
	sc.Step(`^it pulls them read-only into the wiki and the RAG plane and re-reads each object from the AWX API by id rather than trusting a cached copy$`, func() error {
		if w.ingestErr != nil {
			return fmt.Errorf("ingest must succeed: %w", w.ingestErr)
		}
		// READ-ONLY: every request was a GET and none hit a launch endpoint.
		if w.awx.sawNonGET {
			return fmt.Errorf("the knowledge lane issued a non-GET request: %v", w.awx.methods)
		}
		if w.awx.sawLaunch {
			return fmt.Errorf("the knowledge lane hit a launch endpoint: %v", w.awx.paths)
		}
		// RE-READ BY ID: the per-object detail endpoints were hit, not just the list.
		joined := strings.Join(w.awx.paths, " ")
		for _, want := range []string{"/api/v2/job_templates/7/", "/api/v2/job_templates/9/", "/api/v2/inventories/3/"} {
			if !strings.Contains(joined, want) {
				return fmt.Errorf("each object must be re-read by id; missing %s in %v", want, w.awx.paths)
			}
		}
		// INTO THE WIKI + RAG PLANE: the runbooks are in the shared knowledge.Incident corpus (which the wiki
		// lessons surface and the pgvector RAG plane both consume) and are retrievable.
		if w.res.Templates != 2 {
			return fmt.Errorf("both templates must ingest, got %+v", w.res)
		}
		inc, ok := w.surfaced7()
		if !ok {
			return fmt.Errorf("the runbook must be ingested into the corpus; corpus=%+v", w.store.corpus)
		}
		if !strings.Contains(inc.Summary, "nginx-restart") || !strings.Contains(inc.Summary, "web-tier") {
			return fmt.Errorf("the runbook must carry its real description + inventory, got %q", inc.Summary)
		}
		hits := knowledge.NewLexicalRetriever(w.store.corpus).Retrieve(
			knowledge.Query{Summary: "nginx restart", Tags: []string{"awx-runbook"}}, 5)
		for _, h := range hits {
			if h.Incident.ExternalRef == awxplaybooks.RefPrefix+"7" {
				return nil
			}
		}
		return fmt.Errorf("the ingested runbook must be retrievable from the RAG plane; hits=%+v", hits)
	})

	// ---- REQ-1714: launches nothing; a surfaced runbook is a proposal, not an authority ----
	sc.Step(`^a sanctioned runbook the knowledge lane surfaced$`, func() error {
		if err := w.ingest(); err != nil {
			return fmt.Errorf("the knowledge lane must surface a runbook: %w", err)
		}
		inc, ok := w.surfaced7()
		if !ok {
			return fmt.Errorf("a runbook must have been surfaced into the corpus")
		}
		w.surfaced = inc
		return nil
	})
	sc.Step(`^the agent acts on the discovered runbook$`, func() error {
		// The agent "acts" by reading the surfaced runbook DATA from the retrieval plane — there is no launch
		// method on the knowledge lane to call. Prove the runbook is discoverable content.
		hits := knowledge.NewLexicalRetriever(w.store.corpus).Retrieve(
			knowledge.Query{Summary: "nginx restart", Tags: []string{"awx-runbook"}}, 5)
		if len(hits) == 0 {
			return fmt.Errorf("the agent must be able to discover the surfaced runbook")
		}
		return nil
	})
	sc.Step(`^the knowledge lane launches no job and mutates nothing and the runbook enters the pipeline only as a proposal subject to the full interceptor chain$`, func() error {
		// LAUNCHES NOTHING / MUTATES NOTHING: the read-only AWX saw no non-GET and no launch endpoint at any
		// point (the lane has no launch method by construction, REQ-1714).
		if w.awx.sawNonGET || w.awx.sawLaunch {
			return fmt.Errorf("the knowledge lane must launch nothing and mutate nothing; methods=%v paths=%v", w.awx.methods, w.awx.paths)
		}
		// PROPOSAL, NOT AUTHORITY: the surfaced runbook is DATA carrying the proposal discipline — re-enter
		// through the full interceptor chain; discovery grants no authority.
		if !strings.Contains(w.surfaced.Resolution, "propose") ||
			!strings.Contains(w.surfaced.Resolution, "interceptor") ||
			!strings.Contains(strings.ToLower(w.surfaced.Resolution), "launches nothing") {
			return fmt.Errorf("the surfaced runbook must carry the proposal-only discipline, got %q", w.surfaced.Resolution)
		}
		return nil
	})
}
