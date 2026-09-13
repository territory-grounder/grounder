package rerank

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTEIClientRerankParsesScores(t *testing.T) {
	var gotQuery string
	var gotTexts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rerank" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req teiRerankRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotQuery, gotTexts = req.Query, req.Texts
		_ = json.NewEncoder(w).Encode([]teiRerankResult{{Index: 1, Score: 0.9}, {Index: 0, Score: 0.1}})
	}))
	defer srv.Close()

	c := &TEIClient{BaseURL: srv.URL}
	scores, err := c.Rerank(context.Background(), "db out of disk", []string{"restart nginx", "grow the postgres volume"})
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if gotQuery != "db out of disk" || len(gotTexts) != 2 || gotTexts[1] != "grow the postgres volume" {
		t.Errorf("client must POST the query + texts verbatim, got query=%q texts=%v", gotQuery, gotTexts)
	}
	if len(scores) != 2 || scores[0].Index != 1 || scores[0].Score != 0.9 {
		t.Fatalf("client must parse the TEI {index,score} results, got %+v", scores)
	}
}

func TestTEIClientDegradesOnErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	if _, err := (&TEIClient{BaseURL: srv.URL}).Rerank(context.Background(), "q", []string{"a"}); err == nil {
		t.Fatal("a non-2xx status must return an error (RerankRetriever degrades to base on it)")
	}
}

func TestTEIClientNoEndpointErrorsNotPanics(t *testing.T) {
	if _, err := (&TEIClient{}).Rerank(context.Background(), "q", []string{"a"}); err == nil {
		t.Fatal("an empty BaseURL must error, not panic")
	}
	var nilC *TEIClient
	if _, err := nilC.Rerank(context.Background(), "q", nil); err == nil {
		t.Fatal("a nil client must error, not panic")
	}
}
