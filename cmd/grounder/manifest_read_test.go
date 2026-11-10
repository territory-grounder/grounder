package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/worldmodel"
)

// fakeManifestLister drives the adapter without a database so the composition oracle is always-on.
type fakeManifestLister struct {
	entries       []worldmodel.Entry
	drafts, total int
}

func (f fakeManifestLister) AllEntries(_ context.Context, _ int) ([]worldmodel.Entry, int, int, error) {
	return f.entries, f.drafts, f.total, nil
}

func oneDraft() fakeManifestLister {
	return fakeManifestLister{
		entries: []worldmodel.Entry{{
			ID: 1, EntityType: estate.TypeService, Name: "mealie.service", Host: "dc1mealie01",
			Source: estate.SourceDeclared, Confidence: 0.85, Status: worldmodel.StatusDraft,
			LastSeenAt: time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC),
		}},
		drafts: 9, total: 57, // deliberately larger than the page — the badge must show these, not len()
	}
}

// TestManifestAdapterPassesTheStoresOwnCounts is the field-mapping half of the composition oracle for
// spec/027 REQ-2703: the adapter must hand the console the STORE's counts, never the page size.
func TestManifestAdapterPassesTheStoresOwnCounts(t *testing.T) {
	fake := oneDraft()
	rows, drafts, total, err := manifestReadStore{s: fake}.ManifestEntries(context.Background(), auth.Principal{}, 500)
	if err != nil {
		t.Fatalf("adapter: %v", err)
	}
	if drafts != 9 || total != 57 {
		t.Fatalf("the adapter must pass the STORE's counts through untouched, got drafts=%d total=%d", drafts, total)
	}
	want := fake.entries[0]
	if len(rows) != 1 || rows[0].Name != want.Name || rows[0].Confidence != want.Confidence ||
		rows[0].Status != want.Status || !rows[0].LastSeenAt.Equal(want.LastSeenAt) {
		t.Fatalf("the adapter dropped or mutated a field: %+v", rows)
	}
}

// --- the aliveness half -------------------------------------------------------------------------------
//
// THE STAGE-1 DEFECT: /v1/proposals shipped fully unit-tested and permanently 503 because Deps was never
// populated at the composition root, and the fail-closed design made the deadness look intentional.
//
// THE STAGE-3 LESSON ON TOP OF IT: the Stage-1 fix was then "proven" by a route-WALK oracle — but every
// route here is mounted unconditionally and the handler 503s on a nil dependency, so a walk passes with
// Deps.Manifest nil. Signature-shaped aliveness is not aliveness. So this oracle SERVES an authenticated
// request through the exact router buildPublicAPI hands main, and asserts the fake store's row comes back
// in the body. It can only pass if the dependency is threaded end to end.

type fixedSource struct{ secret []byte }

func (f fixedSource) LookupSource(_ context.Context, id string) (auth.Source, error) {
	return auth.Source{SourceID: id, HMACSecret: f.secret}, nil
}

type freshNonces struct{}

func (freshNonces) SeenBefore(_ context.Context, _, _ string, _ time.Time) (bool, error) {
	return false, nil
}

// signedPost builds a machine-principal (HMAC) POST with a signed body — used to prove the WRITE lane
// refuses a machine caller on principal class alone, not on some incidental malformed-request path.
func signedPost(t *testing.T, path string, secret []byte, body string) *http.Request {
	t.Helper()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	const nonce = "manifest-machine-write"
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(ts + "\n" + nonce + "\n" + body))
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-TG-Source", "oracle")
	r.Header.Set("X-TG-Timestamp", ts)
	r.Header.Set("X-TG-Nonce", nonce)
	r.Header.Set("X-TG-Signature", hex.EncodeToString(mac.Sum(nil)))
	return r
}

// signedGet builds a machine-principal (HMAC) GET, the credential a read-only surface admits.
func signedGet(t *testing.T, path string, secret []byte) *http.Request {
	t.Helper()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	const nonce = "manifest-aliveness"
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(ts + "\n" + nonce + "\n")) // empty body
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("X-TG-Source", "oracle")
	r.Header.Set("X-TG-Timestamp", ts)
	r.Header.Set("X-TG-Nonce", nonce)
	r.Header.Set("X-TG-Signature", hex.EncodeToString(mac.Sum(nil)))
	return r
}

func TestServedGrounderAnswersTheManifestSurfaceWithRealRows(t *testing.T) {
	secret := []byte("oracle-secret-not-a-credential")
	v, err := auth.NewVerifier(fixedSource{secret: secret}, freshNonces{}, time.Minute)
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	api := buildPublicAPI(v, safety.NewReadOnlyChokepoint(), nil, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, 0, nil, nil, nil, nil, nil, nil, nil, nil, manifestReadStore{s: oneDraft()}, nil, nil, nil, nil, nil, nil, nil, 0, nil, nil)

	w := httptest.NewRecorder()
	api.Mux().ServeHTTP(w, signedGet(t, "/v1/manifest?limit=500", secret))

	if w.Code != http.StatusOK {
		t.Fatalf("the served manifest surface answered %d, not 200 — a 503 here is the Stage-1 dead-seam defect reborn; body: %s",
			w.Code, w.Body.String())
	}
	var page struct {
		Entries []struct {
			Name         string `json:"name"`
			Status       string `json:"status"`
			Materializes bool   `json:"materializes"`
		} `json:"entries"`
		Drafts int `json:"drafts"`
		Total  int `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v (body %s)", err, w.Body.String())
	}
	if page.Drafts != 9 || page.Total != 57 {
		t.Fatalf("the served page must carry the STORE's counts, got drafts=%d total=%d", page.Drafts, page.Total)
	}
	if len(page.Entries) != 1 || page.Entries[0].Name != "mealie.service" || page.Entries[0].Status != "draft" {
		t.Fatalf("the fake store's row did not reach the wire — the dependency is not threaded: %s", w.Body.String())
	}
	if !page.Entries[0].Materializes {
		t.Error("a service entry materializes into the unit allowlist; the served view must say so, not guess")
	}
}

// compile-time proof the real pgx store is what main hands the adapter: if db.WorldManifestStore ever
// stops satisfying manifestLister, THIS fails to build rather than 503-ing in production.
var _ = manifestReadStore{s: db.NewWorldManifestStore(nil)}
