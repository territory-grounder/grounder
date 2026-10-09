// Package acceptance holds the runnable godog acceptance oracles for spec/004 (governance auto-demote +
// judge-liveness). The scenarios drive the real core/governance Demoter, JudgeLivenessMonitor, and
// RetentionManager with in-memory fakes; @pending scenarios are skipped (Tags: "~@pending"). See
// _test_mapping.json for the honest coverage frontier.
package acceptance
