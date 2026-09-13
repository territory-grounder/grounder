// Package acceptance holds the executable acceptance oracles for spec/021 (the groundnet contract).
// The scenarios live in groundnet.feature. This is a far-future Draft CONTRACT: no product code is built
// yet (groundnet build is blocked on the flywheel graduating an artifact, the loadable-not-hardcoded prose
// migration, and the decision-tracer archive — docs/FEDERATION-VISION.md § 7), so EVERY scenario is tagged
// @pending and is skipped by the runner (Tags: "~@pending"), tracked as declared debt in
// acceptance/_test_mapping.json. As tasks T-021-* land, each scenario's steps bind to the real
// core/groundnet envelope + adapter seam and its mapping entry flips to present. The seam authorizes
// nothing and actuates nothing; an ingested chunk is a SUBORDINATE hint that re-graduates locally before it
// earns trust and never lifts the constitutional never-auto floor (INV-09), the actuation interceptor /
// mutation keystone (INV-21), or the mode chokepoint. Emit sources ONLY the spec/020 REQ-2017 generalizable
// layer; the estate-specific layer has no export path (INV-13).
package acceptance
