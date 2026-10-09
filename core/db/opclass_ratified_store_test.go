package db

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The store's correctness here is almost entirely a property of its SQL TEXT, and CI has no Postgres — the
// same constraint that made the sibling migration tests structural. But the reason to guard this file is
// sharper than convenience: every mistake below COMPILES, passes `go vet`, and survives every behavioural
// test that can run without a database. A `Revoke` rewritten as `UPDATE ... SET revoked = true` type-checks
// perfectly and fails only in production, against a role that lacks UPDATE — i.e. at the exact moment an
// operator is trying to withdraw a capability that is actively running as root somewhere.
//
// So these assertions read the source. That is a real limitation (they check the query the code CONTAINS,
// not the rows Postgres returns) and it is stated rather than papered over: the DDL guarantees are pinned in
// opclass_ratified_migration_test.go, and what remains here is that the Go side does not ask the database
// for something the DDL will refuse.

func ratifiedStoreSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("opclass_ratified.go")
	if err != nil {
		t.Fatalf("read store source: %v", err)
	}
	return string(b)
}

// REQ-2803 / ADR-0016. The overlay is a grant HISTORY. A revocation must append a row, never mutate one —
// both because the runtime role cannot UPDATE, and because an UPDATE would destroy the evidence (who granted
// this, on what rationale) that an incident review needs precisely when a capability has misbehaved.
//
// RED CONTROLS EXECUTED (each mutation applied to opclass_ratified.go, observed, reverted):
//   - Revoke rewritten as `UPDATE opclass_ratified SET revoked = true WHERE op_class = $1 AND NOT revoked`
//     -> "the store issues UPDATE/DELETE against opclass_ratified — migration 0049 REVOKEd both from..."
//   - LiveOverlay's `WHERE NOT revoked` dropped
//     -> "LiveOverlay must exclude revoked rows: a withdrawn capability that still reaches SetOverlay is..."
func TestRevocationAppendsAndTheOverlayNeverServesAWithdrawnClass(t *testing.T) {
	src := ratifiedStoreSource(t)

	// (1) APPEND-ONLY, from the Go side. No UPDATE or DELETE may be aimed at this table at all — the DDL
	// would refuse it, so any such statement is a runtime failure someone shipped past a green build.
	if regexp.MustCompile(`(?is)(UPDATE\s+opclass_ratified|DELETE\s+FROM\s+opclass_ratified)`).MatchString(src) {
		t.Error("the store issues UPDATE/DELETE against opclass_ratified — migration 0049 REVOKEd both from " +
			"tg_runtime, so this compiles, passes CI, and fails only when an operator tries to withdraw a " +
			"live actuation capability; a revocation must INSERT a new revoked=true row")
	}

	// (2) The revocation path is an INSERT that carries the prior grant forward. Copying the original row's
	// spec and entry_hash is what keeps the pair joinable — the history must show WHAT was withdrawn, not
	// merely that something was.
	revoke := funcBody(t, src, "func (s *OpClassRatifiedStore) Revoke(")
	if !strings.Contains(revoke, "INSERT INTO opclass_ratified") {
		t.Error("Revoke must INSERT the withdrawal as a new row — that is the only write the table's " +
			"privileges permit, and the only shape that preserves what was granted")
	}
	if !regexp.MustCompile(`(?i)SELECT\s+op_class,\s*spec,\s*entry_hash`).MatchString(revoke) {
		t.Error("the revocation row must carry the ORIGINAL spec and entry_hash forward — a revocation that " +
			"records only the slug leaves an incident review unable to see what capability was withdrawn")
	}

	// (3) A withdrawn class must reach rung 0 (registry absence) the moment its revocation lands. The
	// cheapest guarantee is that it never leaves this query.
	live := funcBody(t, src, "func (s *OpClassRatifiedStore) LiveOverlay(")
	if !regexp.MustCompile(`(?i)WHERE\s+NOT\s+revoked`).MatchString(live) {
		t.Error("LiveOverlay must exclude revoked rows: a withdrawn capability that still reaches SetOverlay " +
			"is a capability that keeps actuating after an operator revoked it — the one outcome revocation exists to prevent")
	}

	// (4) No string-built SQL anywhere (INV: the repo-wide non-negotiable). Every value is a bind parameter,
	// and op_class is operator-supplied text that ends up selecting an argv template.
	if regexp.MustCompile(`(?i)(fmt\.Sprintf|"\s*\+\s*\w+\s*\+\s*")`).MatchString(sqlLiterals(src)) {
		t.Error("SQL in this store must be parameterized — op_class is caller-supplied and selects an " +
			"actuation template; interpolating it is the highest-consequence injection site in the system")
	}
}

// funcBody returns the source from a function's signature to the next top-level `}` line, so an assertion
// about one method cannot be satisfied by text that happens to live in another.
func funcBody(t *testing.T, src, sig string) string {
	t.Helper()
	i := strings.Index(src, sig)
	if i < 0 {
		t.Fatalf("method not found — the oracle is pinned to a signature that no longer exists: %s", sig)
	}
	rest := src[i:]
	if j := strings.Index(rest, "\n}\n"); j > 0 {
		return rest[:j]
	}
	return rest
}

// sqlLiterals extracts the backquoted query blocks, so ordinary error-message formatting elsewhere in the
// file does not read as SQL interpolation.
func sqlLiterals(src string) string {
	var b strings.Builder
	for _, part := range strings.Split(src, "`")[1:] {
		b.WriteString(part)
		b.WriteString("\n")
	}
	return b.String()
}
