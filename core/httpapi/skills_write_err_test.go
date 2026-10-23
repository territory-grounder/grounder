package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/skillstore"
)

// TG-588: the selector-law refusal reaches the operator as a 422 carrying the predicate named in the error — never
// the default "retry" 503, which would send an operator retrying a structural refusal for ever.
func TestSkillWriteErrMapsPredicateShadowedToUnprocessable(t *testing.T) {
	rec := httptest.NewRecorder()
	skillWriteErr(rec, fmt.Errorf("%w: k8s-triage — compiled selector = {domains=[kubernetes]}", skillstore.ErrPredicateShadowed))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "domains=[kubernetes]") || !strings.Contains(body, "selector") {
		t.Fatalf("the body must carry the refusal with the compiled predicate, got %q", body)
	}
}
