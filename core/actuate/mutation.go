package actuate

// The former actuate.EnableMutation — the proof-gated flip of the retired core/safety.MutationGate — has been
// ABSORBED into the mode-driven actuation chokepoint (spec/015 task T-015-13, REQ-1520/1521). There is no
// longer a single "enable mutation" call that turns a boolean switch on. Instead:
//
//   - the boot proof obligation (the interceptor's SelfTest must prove the interception chain wired) is
//     discharged by safety.Chokepoint.ProvePreflight(interceptor), which marks the preflight GREEN but does
//     NOT actuate — the mode still governs "may this actuate?";
//   - enabling actuation is an operator-authorized, audited policy.ModeController transition into
//     Semi-auto/Full-auto, gated on that same green preflight (the exact obligation EnableMutation enforced).
//
// This file is intentionally left as a signpost so a reader searching for EnableMutation lands on the
// absorption rationale. See core/safety/mutation_chokepoint.go and
// docs/adr/0013-mode-is-the-actuation-chokepoint.md.
