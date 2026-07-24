// Package acceptance holds the executable acceptance oracles for spec/017 (the Actuation Regime Engine).
// The scenarios live in actuation-regime-engine.feature. This is the SDD DESIGN GATE (TG-110): no product
// code is built yet, so EVERY scenario is tagged @pending and is skipped by the runner
// (Tags: "~@pending"), tracked as declared debt in acceptance/_test_mapping.json. As tasks T-017-* land
// (the AWX-job actuator first, after the resolver core + lane seam), each scenario's steps bind to the real
// core/regime.Engine and modules/actuation/awxjob lane and its mapping entry flips to present. Every lane is
// an effect leaf that plugs into the spec/013 interceptor beneath the mode chokepoint; it never lifts the
// constitutional mechanical never-auto floor (INV-09) and never bypasses the policy or credential engines.
package acceptance
