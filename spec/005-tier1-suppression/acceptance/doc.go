// Package acceptance holds the runnable godog acceptance oracles for spec/005 (tier-1 suppression).
// The scenarios drive the real core/suppression Chain, stages, verifier, and registry with in-memory
// fakes; @pending scenarios are skipped (Tags: "~@pending"). See _test_mapping.json for the frontier.
package acceptance
