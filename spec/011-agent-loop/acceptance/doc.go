// Package acceptance holds the runnable godog acceptance oracles for spec/011 (native Go agent loop).
// The scenarios drive the real agent package with a scripted in-memory model; @pending scenarios are
// skipped by the runner (Tags: "~@pending"). See _test_mapping.json for the honest coverage frontier.
package acceptance
