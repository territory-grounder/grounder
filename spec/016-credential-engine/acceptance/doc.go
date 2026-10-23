// Package acceptance holds the executable acceptance oracles for spec/016 (the Credential / Identity
// Engine). The scenarios live in credential-engine.feature; the step definitions in acceptance_test.go drive
// the real core/credential resolver core. Scenarios tagged @pending specify not-yet-built behavior (sync,
// the connectors, the two-plane router, the policy compose seam, the console UX — later slices T-016-5..13)
// and are skipped until implemented (tracked in acceptance/_test_mapping.json). This first slice
// (T-016-1..4) binds the resolver-core scenarios for REQ-1600/1601/1602/1603/1605/1606.
package acceptance
