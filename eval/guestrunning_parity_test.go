package eval

import (
	"context"
	"testing"
)

// The TG-378 parity reader's three arms, executed (the sixth degraded gate arm of 2026-08-14 is the
// incident behind this file): fixture truth answers ONLY for the session's own down-declared host;
// everything else stays could-not-establish so the seal-time gate keeps its fail-closed default.
func TestEvalGuestRunningAnswersOnlyTheFixtureTruth(t *testing.T) {
	inc := Incident{ExternalRef: "eval-01", Host: "dc1bookwyrm01", AlertRule: "Devices up/down"}
	read := evalGuestRunning(inc)

	running, prov, ok := read(context.Background(), "dc1bookwyrm01")
	if !ok || running {
		t.Fatalf("the session's own down-declared host must read not-running with ok: running=%v ok=%v (%s)", running, ok, prov)
	}
	if _, prov, ok := read(context.Background(), "dc1calibre01"); ok {
		t.Fatalf("a foreign target must stay could-not-establish, got ok (%s)", prov)
	}
	up := Incident{ExternalRef: "eval-x", Host: "dc1ap02", AlertRule: "01 Ping Latency"}
	if _, prov, ok := evalGuestRunning(up)(context.Background(), "dc1ap02"); ok {
		t.Fatalf("a non-down rule must not fabricate a not-running observation, got ok (%s)", prov)
	}
}
