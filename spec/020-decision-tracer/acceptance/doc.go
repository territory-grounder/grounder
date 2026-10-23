// Package acceptance holds the executable acceptance oracles for spec/020 (the governed decision tracer).
// The scenarios live in decision-tracer.feature. This is the SDD DESIGN GATE: no product code is built yet,
// so EVERY scenario is tagged @pending and is skipped by the runner (Tags: "~@pending"), tracked as declared
// debt in acceptance/_test_mapping.json. As tasks T-020-* land (the Tier-0 additive persistence quartet
// first, then the per-step evidence tables, then the read + render surface), each scenario's steps bind to
// the real decision-signal persistence, the agent_step / interceptor_gate_verdict tables, and the
// GET /v1/sessions/{external_ref} assembler, and its mapping entry flips to present. The tracer is READ-ONLY
// / OBSERVE-ONLY: it sits OFF every actuation chokepoint, authorizes nothing, and changes no mutation
// posture; it persists references and schemes only, never a secret value (INV-13), and Scrubs every agent
// thought / tool-result before it reaches any trace table (INV-08).
package acceptance
