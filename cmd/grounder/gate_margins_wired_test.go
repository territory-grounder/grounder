package main

import (
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/httpapi"
)

// The production GateMargins reader threaded into buildPublicAPI (main.go passes db.NewGateVerdictStore(pool)
// as the final gateMargins argument) MUST satisfy httpapi.GateMarginReader, or the within-ε gate-decision
// boundary-case surface (TG-178) fail-closes to 503 for the life of the binary. This is a compile-time proof
// of the exact type main() wires — the same "built but never served" discipline the proposals/ingest-refusal
// wiring tests enforce: a store, a route, and a renderer can all exist and the operator still sees nothing if
// the wired value is the wrong type. The route-registration + auth are proven in core/httpapi
// (TestGateMarginsRequiresTraceReadAuth asserts the route is registered, not a 404), and the param threading
// is compiler-enforced end to end (main.go -> buildPublicAPI -> Deps.GateMargins).
var _ httpapi.GateMarginReader = db.NewGateVerdictStore(nil)
