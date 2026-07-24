// Package acceptance holds the runnable godog acceptance oracles for spec/012 (read-only Runner
// Temporal workflow). The scenarios drive the real RunnerWorkflow in the Temporal in-process test env
// with in-memory governed primitives; @pending scenarios are skipped (Tags: "~@pending"). See
// _test_mapping.json for the honest coverage frontier.
package acceptance
