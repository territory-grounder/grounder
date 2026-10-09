package gitopsmr

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

// TestOpenerPerformsExactlyTwoWritesAndReturnsTheHandle: the concrete Opener issues the commit THEN the MR
// create (two calls, that order — the branch must exist before the MR sources it), authenticates with the
// resolved PAT, never merges, and returns the iid/branch/url handle.
func TestOpenerPerformsExactlyTwoWritesAndReturnsTheHandle(t *testing.T) {
	os.Setenv("TG_TEST_GITOPS_TOKEN", "glpat-test")
	var kinds []string
	var gotToken string
	var commitBody commitRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("PRIVATE-TOKEN")
		p := r.URL.EscapedPath()
		b, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(p, "/repository/commits"):
			kinds = append(kinds, "commit")
			_ = json.Unmarshal(b, &commitBody)
			w.WriteHeader(201)
			io.WriteString(w, `{"id":"abc123"}`)
		case strings.HasSuffix(p, "/merge_requests"):
			kinds = append(kinds, "mr")
			w.WriteHeader(201)
			io.WriteString(w, `{"iid":42,"web_url":"https://gl/mr/42"}`)
		default:
			t.Errorf("unexpected path %q", p)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	op := NewOpener(srv.Client())
	pol := RepoPolicy{BaseURL: srv.URL, ProjectPath: "infra/prod", TargetBranch: "main", TokenRef: config.SecretRef("env:TG_TEST_GITOPS_TOKEN")}
	opened, err := op.OpenMR(context.Background(), pol, "tg/change-1", "chore: bump", "body", map[string][]byte{"k8s/main.tf": []byte("replicas = 3\n")})
	if err != nil {
		t.Fatalf("OpenMR: %v", err)
	}
	// EXACTLY two writes, commit BEFORE mr.
	if len(kinds) != 2 || kinds[0] != "commit" || kinds[1] != "mr" {
		t.Fatalf("want exactly [commit, mr], got %v", kinds)
	}
	if gotToken != "glpat-test" {
		t.Fatalf("PAT not sent as PRIVATE-TOKEN: %q", gotToken)
	}
	// Atomic branch+commit off the target branch, one update action for the rendered file — never create/delete.
	if commitBody.Branch != "tg/change-1" || commitBody.StartBranch != "main" || len(commitBody.Actions) != 1 ||
		commitBody.Actions[0].Action != "update" || commitBody.Actions[0].FilePath != "k8s/main.tf" {
		t.Fatalf("commit body wrong: %+v", commitBody)
	}
	if opened.IID != 42 || opened.Branch != "tg/change-1" || opened.URL != "https://gl/mr/42" {
		t.Fatalf("opened handle wrong: %+v", opened)
	}
}

// TestOpenerRefusesEmptyFiles: an empty file set is never opened (an empty MR is a no-op).
func TestOpenerRefusesEmptyFiles(t *testing.T) {
	if _, err := NewOpener(nil).OpenMR(context.Background(), RepoPolicy{}, "b", "t", "body", nil); err == nil {
		t.Fatal("want a refusal for zero files")
	}
}

// TestOpenerFailsClosedOnCommitError: a non-2xx on the commit fails closed — the MR create is NEVER reached, so
// a rejected write never leaves a dangling MR.
func TestOpenerFailsClosedOnCommitError(t *testing.T) {
	os.Setenv("TG_TEST_GITOPS_TOKEN", "glpat-test")
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(403)
		io.WriteString(w, `{"message":"403 Forbidden"}`)
	}))
	defer srv.Close()
	op := NewOpener(srv.Client())
	pol := RepoPolicy{BaseURL: srv.URL, ProjectPath: "infra/prod", TokenRef: config.SecretRef("env:TG_TEST_GITOPS_TOKEN")}
	if _, err := op.OpenMR(context.Background(), pol, "b", "t", "body", map[string][]byte{"f.tf": []byte("x")}); err == nil {
		t.Fatal("want an error on a 403 commit")
	}
	if calls != 1 {
		t.Fatalf("a failed commit must NOT proceed to create the MR; calls=%d want 1", calls)
	}
}

// TestMakeHandleRoundTrips: MakeHandle is the inverse of SplitHandle.
func TestMakeHandleRoundTrips(t *testing.T) {
	repo, iid, err := SplitHandle(MakeHandle("7", 42))
	if err != nil || repo != "7" || iid != 42 {
		t.Fatalf("MakeHandle/SplitHandle round-trip: repo=%q iid=%d err=%v", repo, iid, err)
	}
}
