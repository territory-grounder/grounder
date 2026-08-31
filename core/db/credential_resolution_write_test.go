package db

// ORACLE for the credential-resolution audit projection (spec/016 REQ-1617): the in-memory sink records each
// non-secret ResolutionEvent, and NO secret material or full SecretRef value ever reaches a recorded row —
// only identity metadata and the ref SCHEME.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/credential"
)

func TestMemCredentialResolution_RecordsNonSecretEvent(t *testing.T) {
	store := NewMemCredentialResolutionStore()
	ev := credential.ResolutionEvent{
		Target: "dc1librespeed01", Plane: credential.PlaneMachine, Outcome: credential.OutcomeResolved,
		Source: "native-hostdiag", Native: false, RuleID: "hostdiag:0:dc1*",
		User: "root", Scheme: credential.SchemeSSH, KeyRefScheme: "file",
	}
	if err := store.AppendResolution(context.Background(), ev); err != nil {
		t.Fatalf("append: %v", err)
	}
	if len(store.Events) != 1 || store.Events[0].Outcome != credential.OutcomeResolved {
		t.Fatalf("expected one resolved event, got %+v", store.Events)
	}

	// A resolution event carries only NON-SECRET fields — assert nothing that looks like key material or a full
	// ref value can appear (the KeyRefScheme is only the scheme token, "file").
	blob, _ := json.Marshal(store.Events)
	for _, leak := range []string{"/secrets/", "BEGIN", "PRIVATE KEY", "file:/"} {
		if strings.Contains(string(blob), leak) {
			t.Errorf("SECRET LEAK: recorded resolution contains %q\n%s", leak, blob)
		}
	}
	if store.Events[0].KeyRefScheme != "file" {
		t.Errorf("KeyRefScheme = %q, want the scheme token only", store.Events[0].KeyRefScheme)
	}
}
