// Package acceptance holds the executable acceptance oracles for spec/006 (interface contracts). The
// GREEN scenarios drive the already-implemented core/auth mandatory-auth guarantees (REQ-501: a route
// with auth=none panics at registration; a verifier without replay protection fails closed at
// construction). Scenarios tagged @pending specify the not-yet-built request-time behavior + generated
// wire contracts and are skipped until implemented (tracked in acceptance/_test_mapping.json).
package acceptance
