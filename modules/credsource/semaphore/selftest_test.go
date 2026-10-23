package semaphore_test

// ORACLE tests for the Semaphore credential source's console TEST probe (core/selftest.Tester). CI has no
// Semaphore, so a httptest.Server fakes /api/projects, /api/project/{id} and /api/project/{id}/inventory,
// and the tests drive the REAL Source — the real client, the real Bearer token resolved from its real
// SecretRef — through it. They prove: the Summary reports what the SERVED payload contained (the instance
// host, the project name and the inventory counts); a rejected token and a refused project read are
// classified as DIFFERENT faults; an unreachable host is an error and not a pass; a token that can see
// nothing is a pass that says so; and — the killing oracle — a fully-configured source pointed at a server
// that rejects the token FAILS.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/selftest"
	"github.com/territory-grounder/grounder/modules/credsource/semaphore"
)

// probeSemaphore serves only what the probe reads, so an oracle can refuse ONE of the two requests and prove
// the classifier tells them apart.
type probeSemaphore struct {
	projectStatus   int              // non-zero → the project read/list is refused with this status
	inventoryStatus int              // non-zero → the inventory list is refused with this status
	projects        []map[string]any // served for /api/projects
	project         map[string]any   // served for /api/project/{id}
	inventories     []map[string]any // served for /api/project/{id}/inventory
	seen            []string
}

func (f *probeSemaphore) server(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+goodToken {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "unauthorized"})
			return
		}
		f.seen = append(f.seen, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/inventory"):
			if f.inventoryStatus != 0 {
				w.WriteHeader(f.inventoryStatus)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "forbidden"})
				return
			}
			_ = json.NewEncoder(w).Encode(f.inventories)
		case r.URL.Path == "/api/projects":
			if f.projectStatus != 0 {
				w.WriteHeader(f.projectStatus)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "denied"})
				return
			}
			_ = json.NewEncoder(w).Encode(f.projects)
		case strings.HasPrefix(r.URL.Path, "/api/project/"):
			if f.projectStatus != 0 {
				w.WriteHeader(f.projectStatus)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "denied"})
				return
			}
			_ = json.NewEncoder(w).Encode(f.project)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// probeSource builds the REAL source against url with every configured value present and non-empty — the
// precondition the killing oracle depends on. tokenValue lets a case supply a token the server will refuse.
func probeSource(t *testing.T, url string, projectID int, tokenValue string) *semaphore.Source {
	t.Helper()
	t.Setenv("TG_PROBE_SEM_TOKEN", tokenValue)
	c, err := semaphore.New(semaphore.Config{
		BaseURL:    url,
		TokenRef:   config.SecretRef("env:TG_PROBE_SEM_TOKEN"),
		HTTPClient: http.DefaultClient,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	s, err := semaphore.NewSource(semaphore.SourceConfig{ID: "sem01", Client: c, ProjectID: projectID})
	if err != nil {
		t.Fatalf("new source: %v", err)
	}
	return s
}

func TestSelfTestReportsTheProjectAndItsInventories(t *testing.T) {
	f := &probeSemaphore{
		project: map[string]any{"id": 3, "name": "Lab"},
		inventories: []map[string]any{
			{"id": 1, "name": "lab-ini", "project_id": 3, "type": "static"},
			{"id": 2, "name": "lab-yaml", "project_id": 3, "type": "static-yaml"},
			{"id": 3, "name": "from-repo", "project_id": 3, "type": "file"},
		},
	}
	srv := f.server(t)
	s := probeSource(t, srv.URL, 3, goodToken)

	res, err := s.SelfTest(context.Background(), "alice")
	if err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	host := strings.TrimPrefix(srv.URL, "http://")
	// Every fact must come from the SERVED payload: the project NAME is the strongest signal an operator has
	// that this is the right instance, and the inline count is what says the sync can read anything at all.
	for _, want := range []string{host, `"Lab"`, "id 3", "3 inventories", "2 with inline host text"} {
		if !strings.Contains(res.Summary, want) {
			t.Fatalf("summary %q does not report %q", res.Summary, want)
		}
	}
	// Scoped to a project id, the probe must read THAT project — not the whole project list the sync would
	// only walk when unscoped.
	for _, path := range f.seen {
		if path == "/api/projects" {
			t.Fatalf("a scoped source must not list every project: %v", f.seen)
		}
	}
	if strings.Contains(res.Summary+res.Detail, goodToken) {
		t.Fatal("the probe leaked the Bearer token into its result")
	}
}

func TestSelfTestUnscopedReportsHowManyProjectsAreVisible(t *testing.T) {
	f := &probeSemaphore{
		projects: []map[string]any{{"id": 5, "name": "Prod"}, {"id": 6, "name": "Lab"}},
		inventories: []map[string]any{
			{"id": 1, "name": "prod-ini", "project_id": 5, "type": "static"},
		},
	}
	s := probeSource(t, f.server(t).URL, 0, goodToken)

	res, err := s.SelfTest(context.Background(), "alice")
	if err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	for _, want := range []string{`"Prod"`, "2 projects"} {
		if !strings.Contains(res.Summary, want) {
			t.Fatalf("summary %q does not report %q", res.Summary, want)
		}
	}
}

func TestSelfTestFailureClassification(t *testing.T) {
	cases := []struct {
		name       string
		fake       *probeSemaphore
		token      string
		closed     bool
		wantDetail []string
	}{
		{
			// The token itself is refused. Naming that rather than a project membership is what stops an
			// operator editing permissions for a credential the server never accepted.
			name:       "rejected token names the credential",
			fake:       &probeSemaphore{project: map[string]any{"id": 3, "name": "Lab"}},
			token:      "a-token-that-was-deleted",
			wantDetail: []string{"REJECTED THE TOKEN"},
		},
		{
			// The project reads fine and the inventory list is refused: in Semaphore that is project
			// MEMBERSHIP, which is not where an operator looks first unless the message says so.
			name: "refused inventory list names project membership",
			fake: &probeSemaphore{
				project:         map[string]any{"id": 3, "name": "Lab"},
				inventoryStatus: http.StatusForbidden,
			},
			token:      goodToken,
			wantDetail: []string{"membership", "list that project's inventories"},
		},
		{
			name:       "closed server is unreachable and is an error",
			fake:       &probeSemaphore{},
			token:      goodToken,
			closed:     true,
			wantDetail: []string{"could not be reached"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := tc.fake.server(t)
			addr := srv.URL
			if tc.closed {
				srv.Close() // nothing listens on this port any more
			}
			s := probeSource(t, addr, 3, tc.token)

			res, err := s.SelfTest(context.Background(), "alice")
			if err == nil {
				t.Fatalf("expected an error, got summary=%q detail=%q", res.Summary, res.Detail)
			}
			if res.Detail == "" {
				t.Fatal("a failed probe must carry an actionable Detail, never a bare error")
			}
			for _, want := range tc.wantDetail {
				if !strings.Contains(strings.ToLower(res.Detail), strings.ToLower(want)) {
					t.Fatalf("detail %q does not carry %q", res.Detail, want)
				}
			}
			if strings.Contains(res.Summary+res.Detail+err.Error(), tc.token) {
				t.Fatal("the probe leaked the Bearer token into its result or error")
			}
		})
	}
}

func TestRejectedTokenDetailAgreesWithTheTokenCache(t *testing.T) {
	// Client.token() caches the resolved token for the PROCESS LIFETIME — the descriptor's header note says so
	// at length and marks the token field EffectRestart because of it. A 401 Detail that told the operator
	// this button had just tested the token they saved a moment ago would contradict the dialog next to it and
	// cost them a diagnosis loop. The Detail must name the cache and the restart instead.
	f := &probeSemaphore{project: map[string]any{"id": 3, "name": "Lab"}}
	s := probeSource(t, f.server(t).URL, 3, "a-token-that-was-deleted")

	res, err := s.SelfTest(context.Background(), "alice")
	if err == nil {
		t.Fatalf("a refused token must fail: %q", res.Summary)
	}
	for _, want := range []string{"caches", "restart the worker"} {
		if !strings.Contains(res.Detail, want) {
			t.Fatalf("the 401 detail must say the tested token is the cached one and that a save needs a "+
				"restart; %q does not carry %q", res.Detail, want)
		}
	}
}

func TestSelfTestSaysWhenTheTokenSeesNoProjects(t *testing.T) {
	// Semaphore enforces project access by FILTERING the list, so a token with no membership gets a 200 and
	// an empty array — a "success" that imports nothing. It is a pass (the credential is proven) but it must
	// say plainly that no host identity will come from it.
	f := &probeSemaphore{projects: []map[string]any{}}
	s := probeSource(t, f.server(t).URL, 0, goodToken)

	res, err := s.SelfTest(context.Background(), "alice")
	if err != nil {
		t.Fatalf("an empty project list is a pass with a warning, not a failure: %v", err)
	}
	if !strings.Contains(res.Summary, "no projects") || !strings.Contains(res.Detail, "filtering") {
		t.Fatalf("an invisible-project token must be reported as such: %q / %q", res.Summary, res.Detail)
	}
}

func TestSelfTestWarnsWhenNoInventoryHasInlineText(t *testing.T) {
	f := &probeSemaphore{
		project: map[string]any{"id": 3, "name": "Lab"},
		inventories: []map[string]any{
			{"id": 1, "name": "from-repo", "project_id": 3, "type": "file"},
		},
	}
	s := probeSource(t, f.server(t).URL, 3, goodToken)

	res, err := s.SelfTest(context.Background(), "alice")
	if err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	if !strings.Contains(res.Detail, "inline host text") {
		t.Fatalf("an all-\"file\" project must warn that nothing can be read: %q", res.Detail)
	}
}

// TestSelfTestFailsWithEveryValueConfigured is THE KILLING ORACLE.
//
// Every configured value is present and non-empty: a real base URL, a project id, and a token reference that
// RESOLVES to a real value. Only the server disagrees — it rejects the token the way a deleted one, a
// membership never granted, or an instance the token does not belong to does. A SelfTest implemented as "the
// configured values are all set" passes this test; the real one must fail it. This is what makes the probe
// more than a mock.
func TestSelfTestFailsWithEveryValueConfigured(t *testing.T) {
	f := &probeSemaphore{
		project:     map[string]any{"id": 3, "name": "Lab"},
		inventories: []map[string]any{{"id": 1, "name": "lab", "project_id": 3, "type": "static"}},
	}
	s := probeSource(t, f.server(t).URL, 3, "revoked-but-present-token")

	res, err := s.SelfTest(context.Background(), "alice")
	if err == nil {
		t.Fatalf("a fully-configured source whose token is refused MUST fail: %q", res.Summary)
	}
	if res.Detail == "" {
		t.Fatal("a failed probe must carry an actionable Detail")
	}
}

// TestSourceImplementsTester pins the capability the console detects by assertion. Without it the dialog
// would report "no test is implemented" while promising a read.
func TestSourceImplementsTester(t *testing.T) {
	f := &probeSemaphore{}
	if _, ok := selftest.Of(probeSource(t, f.server(t).URL, 0, goodToken)); !ok {
		t.Fatal("the semaphore credential source must be detected as a selftest.Tester")
	}
}
