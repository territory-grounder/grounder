// Package acceptance holds the runnable godog acceptance oracles for spec/008 (the day-1 connector fleet).
// The REQ-821 module-framework scenarios are bound: they drive the real capability-scoped registry
// (modules.Registry) — an unregistered or disabled module has no execution path (INV-17), and each surface
// admits exactly one implementation per source type (INV-18). Each per-connector scenario stays @pending
// (skipped by the runner via Tags: "~@pending") until its owning task builds the module. See
// _test_mapping.json for the honest coverage frontier.
package acceptance
