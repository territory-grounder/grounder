// Package acceptance holds the runnable godog acceptance oracles for spec/007 (content-aware spec↔code
// lockstep). The bound scenarios drive the real code path: REQ-703's two re-stamp-authorization scenarios
// exercise core/governance.AuthorizeRestamp against an in-memory RBAC authority + audit ledger. The
// REQ-701/702/704 scenarios (the lockstep hash mechanism in tools/specvalidate) remain @pending and are
// skipped by the runner (Tags: "~@pending") until that mechanism is exposed as an importable surface. See
// _test_mapping.json for the honest coverage frontier.
package acceptance
