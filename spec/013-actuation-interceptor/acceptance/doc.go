// Package acceptance holds the runnable godog acceptance oracles for spec/013 (wired-by-construction
// actuation interceptor + mutation gate). The scenarios drive the real core/actuate.Interceptor with an
// in-memory actuator + ledger; @pending scenarios are skipped (Tags: "~@pending"). See _test_mapping.json
// for the honest coverage frontier.
package acceptance
