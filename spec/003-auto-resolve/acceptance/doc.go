// Package acceptance holds the runnable godog acceptance oracles for spec/003 (per-incident
// auto-resolve + escalation requeue). The scenarios drive the real core/reconcile and core/escalation
// packages with in-memory fakes; @pending scenarios are skipped (Tags: "~@pending"). See
// _test_mapping.json for the honest coverage frontier.
package acceptance
