package httpapi

import (
	"net/http"
	"testing"
)

// TestRollbackHandler_MapsRefusalsAndSucceeds proves the endpoint's status mapping (TG-462): the typed backend
// refusals become honest statuses — unknown/never-executed → 404, not-reversible → 400, already-inverted
// (idempotency, no double-undo) → 409 — and a success returns 202 with the sealed inverse id, deriving the
// operator from the authenticated principal (never the body) and the forward id from the path.
func TestRollbackHandler_MapsRefusalsAndSucceeds(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"unknown-or-never-executed", ErrRollbackUnknownAction, http.StatusNotFound},
		{"not-reversible", ErrRollbackIrreversible, http.StatusBadRequest},
		{"already-inverted", ErrRollbackAlreadyInverted, http.StatusConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mr := &MemRollbacker{Err: tc.err}
			rt, c := adminSurfaceRig(t, Deps{Rollback: mr}, true)
			rec := adminPostJSON(rt, c, "/v1/actions/forward-abc/rollback", `{}`)
			if rec.Code != tc.want {
				t.Fatalf("got %d, want %d (%s)", rec.Code, tc.want, rec.Body.String())
			}
			if mr.LastForwardID != "forward-abc" {
				t.Errorf("the path action_id was not forwarded to the backend: %q", mr.LastForwardID)
			}
		})
	}

	// Success → 202, inverse id echoed, operator derived from the authenticated principal (not the body).
	mr := &MemRollbacker{Out: RollbackRequested{ForwardActionID: "forward-abc", InverseActionID: "inverse-xyz", Band: "POLL_PAUSE", Status: "pending-approval"}}
	rt, c := adminSurfaceRig(t, Deps{Rollback: mr}, true)
	rec := adminPostJSON(rt, c, "/v1/actions/forward-abc/rollback", `{}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("success: got %d, want 202 (%s)", rec.Code, rec.Body.String())
	}
	if mr.LastOperator == "" {
		t.Error("the operator must be derived from the authenticated principal (INV-01) — the backend saw none")
	}
}

// TestRollbackFailClosedAndAdminOnly is TG-462 assertion 4 (auth): the surface is structurally admin-only. A nil
// backend fails closed to 503; an UNDER-TIERED principal (a plain, unelevated session) is rejected at the
// AuthAdminSession router class with 401 and NEVER reaches the backend.
func TestRollbackFailClosedAndAdminOnly(t *testing.T) {
	// nil backend ⇒ 503.
	rt, c := adminSurfaceRig(t, Deps{}, true)
	if rec := adminPostJSON(rt, c, "/v1/actions/forward-abc/rollback", `{}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil backend: got %d, want 503", rec.Code)
	}
	// Plain (unelevated) session — an under-tiered principal — is rejected before the handler.
	mr := &MemRollbacker{}
	rt2, c2 := adminSurfaceRig(t, Deps{Rollback: mr}, false)
	if rec := adminPostJSON(rt2, c2, "/v1/actions/forward-abc/rollback", `{}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("plain (under-tiered) session on rollback: got %d, want 401", rec.Code)
	}
	if mr.Calls != 0 {
		t.Fatal("an under-tiered request reached the rollback backend — the mutation surface must be admin-only")
	}
}
