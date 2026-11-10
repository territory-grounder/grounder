package modetransition

import (
	"context"
	"errors"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/temporal/wfauth"
	"github.com/territory-grounder/grounder/temporal/wfauth/wfauthtest"
)

// TG-660 — Actor and AdminAuthorized are CLAIMS in the workflow input; anything on the backplane can start this
// workflow with them set. The activity verifies the grounder's signed envelope before the controller is
// consulted. KILLING MUTATION (executed 2026-11-07): removing the wfauth.Require call let the unsigned Full-auto
// request below FLIP THE MODE — the exact forgery the deep review named (F1, pack 12).
func TestUnsignedTamperedOrReplayedTransitionsAreRefusedBeforeTheController(t *testing.T) {
	ctx := context.Background()
	s, v := wfauthtest.Pair(t)
	led := audit.NewLedger()
	ctl := newCtl(t, led, policy.NewModeTransitionAuthority(nil), greenPreflight{})
	refusals := audit.NewLedger()
	a := &Activities{D: Deps{Controller: ctl, Auth: v, Ledger: refusals}}

	req := Request{To: "Full-auto", ExpectedFrom: "Shadow", Actor: "operator:intruder", Reason: "forged", AdminAuthorized: true}
	var ref *wfauth.Refusal

	// UNSIGNED: the claim alone.
	if _, err := a.ApplyModeTransitionActivity(ctx, req); !errors.As(err, &ref) || ref.Class != "unsigned" {
		t.Fatalf("an unsigned admin-authorized request must be refused as unsigned, got %v", err)
	}
	if ctl.Current() != policy.ModeShadow {
		t.Fatalf("the controller moved on an UNSIGNED request: %v", ctl.Current())
	}
	if led.Len() != 0 {
		t.Fatalf("the controller's own ledger must see nothing (the request never reached it): %+v", led.Entries())
	}
	if refusals.Len() != 1 || refusals.Entries()[0].Decision != wfauth.RefusalDecision {
		t.Fatalf("the refusal must be ledgered once as %s: %+v", wfauth.RefusalDecision, refusals.Entries())
	}

	// TAMPERED: signed as a Shadow transition by an admin, replayed as Full-auto.
	benign := req
	benign.To = "Shadow"
	env, err := s.Sign(AuthKind, benign.AuthFields()...)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	escalated := req
	escalated.Auth = env
	if _, err := a.ApplyModeTransitionActivity(ctx, escalated); !errors.As(err, &ref) || ref.Class != "tampered" {
		t.Fatalf("a Shadow envelope spliced onto a Full-auto request must be refused as tampered, got %v", err)
	}
	if ctl.Current() != policy.ModeShadow {
		t.Fatalf("the controller moved on a TAMPERED request: %v", ctl.Current())
	}

	// SIGNED: the grounder's own request proceeds to the controller (and flips under the admin fallback).
	good := Request{To: "Full-auto", ExpectedFrom: "Shadow", Actor: "operator:kyriakos", Reason: "owner canary GO", AdminAuthorized: true}
	good.Auth, err = s.Sign(AuthKind, good.AuthFields()...)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	res, err := a.ApplyModeTransitionActivity(ctx, good)
	if err != nil || res.Mode != "Full-auto" || ctl.Current() != policy.ModeFullAuto {
		t.Fatalf("the signed request must reach and flip the controller: res=%+v err=%v mode=%v", res, err, ctl.Current())
	}

	// REPLAYED: the same signed envelope presented again (a captured Temporal payload) is refused.
	again := good
	again.ExpectedFrom = ""
	again.To = "Full-auto"
	// identical fields and identical envelope ⇒ the MAC verifies, the nonce does not.
	again = good
	if _, err := a.ApplyModeTransitionActivity(ctx, again); !errors.As(err, &ref) || ref.Class != "replayed" {
		t.Fatalf("a replayed envelope must be refused as replayed, got %v", err)
	}
	// nil verifier (no key) ⇒ even a correctly signed request is refused, honestly classed.
	none := &Activities{D: Deps{Controller: ctl, Ledger: refusals}}
	if _, err := none.ApplyModeTransitionActivity(ctx, good); !errors.As(err, &ref) || ref.Class != "no-key" {
		t.Fatalf("a worker with no key must refuse as no-key, got %v", err)
	}
}
