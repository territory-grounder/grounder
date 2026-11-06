package gitopsmr

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeGitlab serves the two endpoints the reader uses: paginated /merge_requests (with an X-Next-Page
// header) and per-MR /diffs. No live GitLab needed.
type fakeGitlab struct {
	mrPages map[int]string // page number -> JSON array of MRs
	diffs   map[int]string // MR iid -> JSON array of diffs
	diffErr map[int]bool   // MR iid -> return 500 on its /diffs (simulate a per-MR read failure)
	seen    []string
	auth    string
}

func (f *fakeGitlab) Do(req *http.Request) (*http.Response, error) {
	f.seen = append(f.seen, req.URL.Path+"?"+req.URL.RawQuery)
	f.auth = req.Header.Get("PRIVATE-TOKEN")
	hdr := http.Header{}
	body, code := "", 200
	switch {
	case strings.HasSuffix(req.URL.Path, "/merge_requests"):
		page := 1
		if p := req.URL.Query().Get("page"); p != "" {
			page, _ = strconv.Atoi(p)
		}
		if b, ok := f.mrPages[page]; ok {
			body = b
		} else {
			body = "[]"
		}
		if _, has := f.mrPages[page+1]; has {
			hdr.Set("X-Next-Page", strconv.Itoa(page+1))
		}
	case strings.Contains(req.URL.Path, "/merge_requests/") && strings.HasSuffix(req.URL.Path, "/diffs"):
		parts := strings.Split(req.URL.Path, "/")
		iid := 0
		for i, p := range parts {
			if p == "merge_requests" && i+1 < len(parts) {
				iid, _ = strconv.Atoi(parts[i+1])
			}
		}
		if f.diffErr != nil && f.diffErr[iid] {
			code, body = 500, `{"message":"boom"}`
		} else if b, ok := f.diffs[iid]; ok {
			body = b
		} else {
			body = "[]"
		}
	default:
		code, body = 404, `{"message":"404"}`
	}
	if f.auth != "test-token" {
		code, body = 401, `{"message":"401"}`
	}
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: hdr}, nil
}

func newReader(t *testing.T, f *fakeGitlab) *Module {
	t.Helper()
	t.Setenv("GL_TEST_TOKEN", "test-token")
	return New("https://gitlab.example", "48", "env:GL_TEST_TOKEN", WithHTTPClient(f))
}

func window() (time.Time, time.Time) {
	return time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 24, 23, 0, 0, 0, time.UTC)
}

func TestReadEmitsEvidenceForDeployMR(t *testing.T) {
	f := &fakeGitlab{
		mrPages: map[int]string{1: `[{"iid":556,"merged_at":"2026-07-24T10:54:36Z","merge_commit_sha":"582b6f33","merged_by":{"username":"alice"}}]`},
		diffs:   map[int]string{556: `[{"new_path":"deploy/docker-compose.yml","old_path":"deploy/docker-compose.yml"},{"new_path":"README.md","old_path":"README.md"}]`},
	}
	r := newReader(t, f)
	since, until := window()
	ev, err := r.Read(context.Background(), "web01", since, until)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(ev) != 1 {
		t.Fatalf("expected 1 evidence, got %d: %+v", len(ev), ev)
	}
	e := ev[0]
	if e.Domain != "gitops-mr" || e.Actor != "alice" || e.ActionKind != "MR-merged" || e.Target != "web01" || e.Ref != "582b6f33" {
		t.Fatalf("evidence fields wrong: %+v", e)
	}
	if e.Covered {
		t.Fatalf("gitops-mr is target-agnostic — Covered must be false (candidate evidence, not affirmed coverage), got %+v", e)
	}
	if f.auth != "test-token" {
		t.Fatalf("GitLab uses the PRIVATE-TOKEN scheme, got %q", f.auth)
	}
}

// An MR that changed no deploy-manifest file is outside this reader's coverage — no evidence.
func TestReadSkipsNonManifestMR(t *testing.T) {
	f := &fakeGitlab{
		mrPages: map[int]string{1: `[{"iid":10,"merged_at":"2026-07-24T10:00:00Z","merge_commit_sha":"abc","merged_by":{"username":"bob"}}]`},
		diffs:   map[int]string{10: `[{"new_path":"docs/README.md","old_path":"docs/README.md"}]`},
	}
	r := newReader(t, f)
	since, until := window()
	ev, err := r.Read(context.Background(), "web01", since, until)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(ev) != 0 {
		t.Fatalf("a non-manifest MR must yield no evidence, got %+v", ev)
	}
}

// An MR merged outside the window is filtered even if it touched a manifest.
func TestReadWindowFilters(t *testing.T) {
	f := &fakeGitlab{
		mrPages: map[int]string{1: `[{"iid":9,"merged_at":"2026-07-01T00:00:00Z","merge_commit_sha":"old","merged_by":{"username":"carol"}}]`},
		diffs:   map[int]string{9: `[{"new_path":"deploy/x.yml","old_path":"deploy/x.yml"}]`},
	}
	r := newReader(t, f)
	since, until := window()
	ev, err := r.Read(context.Background(), "web01", since, until)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(ev) != 0 {
		t.Fatalf("an out-of-window MR must be filtered, got %+v", ev)
	}
}

// An MR whose merged_by GitLab records as empty yields no evidence (no principal ⇒ not admissible).
func TestReadSkipsBlankActor(t *testing.T) {
	f := &fakeGitlab{
		mrPages: map[int]string{1: `[{"iid":11,"merged_at":"2026-07-24T10:00:00Z","merge_commit_sha":"z","merged_by":{"username":""}}]`},
		diffs:   map[int]string{11: `[{"new_path":"deploy/x.yml","old_path":"deploy/x.yml"}]`},
	}
	r := newReader(t, f)
	since, until := window()
	ev, err := r.Read(context.Background(), "web01", since, until)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(ev) != 0 {
		t.Fatalf("a blank-actor MR must yield no evidence, got %+v", ev)
	}
}

// A per-MR /diffs failure SKIPS that one MR (its coverage is unknown) but must NOT abort the whole Read —
// the other MRs' evidence still returns (advisory per REQ-2307, consistent with the AWX per-job skip).
func TestReadDiffErrorSkipsOnlyThatMR(t *testing.T) {
	f := &fakeGitlab{
		mrPages: map[int]string{1: `[
			{"iid":1,"merged_at":"2026-07-24T09:00:00Z","merge_commit_sha":"s1","merged_by":{"username":"alice"}},
			{"iid":2,"merged_at":"2026-07-24T10:00:00Z","merge_commit_sha":"s2","merged_by":{"username":"bob"}}]`},
		diffs:   map[int]string{1: `[{"new_path":"deploy/a.yml","old_path":"deploy/a.yml"}]`},
		diffErr: map[int]bool{2: true}, // MR#2's diffs 500s
	}
	r := newReader(t, f)
	since, until := window()
	ev, err := r.Read(context.Background(), "web01", since, until)
	if err != nil {
		t.Fatalf("a single MR's diffs failure must not abort the whole Read: %v", err)
	}
	if len(ev) != 1 || ev[0].Actor != "alice" {
		t.Fatalf("MR#1 must still be attributed while MR#2 (diffs error) is skipped, got %+v", ev)
	}
}

// The reader follows X-Next-Page pagination — an in-window deploy MR on page 2 is not silently dropped.
func TestReadFollowsPagination(t *testing.T) {
	f := &fakeGitlab{
		mrPages: map[int]string{
			1: `[{"iid":1,"merged_at":"2026-07-24T09:00:00Z","merge_commit_sha":"s1","merged_by":{"username":"a"}}]`,
			2: `[{"iid":2,"merged_at":"2026-07-24T11:00:00Z","merge_commit_sha":"s2","merged_by":{"username":"b"}}]`,
		},
		diffs: map[int]string{
			1: `[{"new_path":"deploy/a.yml","old_path":"deploy/a.yml"}]`,
			2: `[{"new_path":"deploy/b.yml","old_path":"deploy/b.yml"}]`,
		},
	}
	r := newReader(t, f)
	since, until := window()
	ev, err := r.Read(context.Background(), "web01", since, until)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(ev) != 2 {
		t.Fatalf("both pages' in-window deploy MRs must be returned (pagination followed), got %d: %+v", len(ev), ev)
	}
}
