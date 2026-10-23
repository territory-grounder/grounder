// Package acceptance holds the executable acceptance oracles for spec/019 (Scheduling awareness & the
// maintenance-window seam). The scenarios live in maintenance-window.feature; the step definitions in
// acceptance_test.go drive the REAL modules/schedule/cronicle connector against an in-process fake Cronicle
// serving the exact REST wire shapes verified live (get_schedule/get_event, X-API-Key auth, code-0 envelope,
// timing arrays), so the window derivation, the freeze-overrides-maintenance rule, the defer-to-POLL_PAUSE
// mapping, the re-read-by-id property, and the fail-closed-safe unreadable default are all proven end-to-end.
// Every scenario binds to real code (no @pending); the honest coverage frontier is acceptance/_test_mapping.json.
package acceptance
