// Package acceptance holds the executable acceptance oracles for spec/001 (three-band risk
// classification). The scenarios live in risk-classification.feature; the step definitions in
// acceptance_test.go drive the real core/safety code. Scenarios tagged @pending specify not-yet-built
// classifier behavior and are skipped until implemented (tracked in acceptance/_test_mapping.json).
package acceptance
