package runner

import (
	"context"
	"errors"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/temporal/wfauth"
	"github.com/territory-grounder/grounder/temporal/wfauth/wfauthtest"
)

// TG-660 — a manual rollback ACTUATES, so its start carries the signed envelope and the seal activity verifies it
// FIRST: an unsigned (or tampered) order seals nothing, and the refusal is ledgered. KILLING MUTATION (executed
// 2026-11-07): removing the Require call from SealRollbackActivity turned the first case RED — an unsigned order
// sealed an inverse at POLL_PAUSE, one vote away from executing.
func TestSealRollbackRefusesAnUnsignedOrTamperedOrderBeforeSealing(t *testing.T) {
	s, v := wfauthtest.Pair(t)
	base := RollbackInput{
		ForwardActionID: "forward-660", ForwardOpClass: "start-service", ForwardOp: "start",
		ForwardTarget: "app01", ForwardParams: map[string]string{"unit": "nginx"}, ForwardReversible: true,
		RollbackExternalRef: "rollback:forward-660", Operator: "kyriakos",
	}
	run := func(in RollbackInput) (SealRollbackResult, error, *captureSink, *captureLedger) {
		sink, led := &captureSink{}, &captureLedger{}
		d := permissive(Deps{ManifestSink: sink, Ledger: audit.NewLedger().WithSink(led)})
		d.WorkflowAuth = v // the REAL verifier
		res, err := (&Activities{D: d}).SealRollbackActivity(context.Background(), in)
		return res, err, sink, led
	}
	// unsigned
	res, err, sink, led := run(base)
	var ref *wfauth.Refusal
	if !errors.As(err, &ref) || ref.Class != "unsigned" || res.Sealed || sink.sealed != nil {
		t.Fatalf("an unsigned rollback order must be refused before anything is sealed: err=%v res=%+v sealed=%v", err, res, sink.sealed != nil)
	}
	if !led.has(wfauth.RefusalDecision) {
		t.Fatalf("the refusal must be ledgered; ledger=%v", decisions(led))
	}
	// tampered: signed for app01, replayed against db01
	signed := base
	env, _ := s.Sign(RollbackAuthKind, signed.AuthFields()...)
	signed.Auth = env
	tampered := signed
	tampered.ForwardTarget = "db01"
	res, err, sink, _ = run(tampered)
	if !errors.As(err, &ref) || ref.Class != "tampered" || res.Sealed || sink.sealed != nil {
		t.Fatalf("a re-targeted order under a captured MAC must be refused as tampered: err=%v", err)
	}
	// signed: seals
	res, err, sink, _ = run(signed)
	if err != nil || !res.Sealed || sink.sealed == nil {
		t.Fatalf("the grounder's signed order must seal: err=%v res=%+v", err, res)
	}
}
