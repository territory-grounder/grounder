package db

import (
	"regexp"
	"strings"
	"testing"
)

// The overlay's safety rests on DDL properties that no Go code can restore once the table is wrong: an
// append-only grant history, one live grant per slug, a threshold that can only rise, and a chain-bound row
// hash. CI has no Postgres, so these are structural guards over the DDL text — the established pattern in
// this file's sibling migration tests.

// TestOverlayIsAppendOnlyWithOneLiveGrantPerClass pins spec/028 REQ-2803's four structural guarantees.
// RED CONTROLS EXECUTED (each mutation applied to 0049_opclass_ratified.up.sql, observed, reverted):
//   - dropped the REVOKE line                      -> "an operator-authored ACTUATION vector that can be UPDATEd..."
//   - PK (op_class) instead of (op_class, seq)     -> "the overlay must be APPEND-ONLY: PRIMARY KEY (op_class, seq)..."
//   - dropped the partial unique index             -> "two live grants for one slug would make WHICH TEMPLATE RUNS..."
//   - CHECK (promote_threshold >= 1)               -> "ratify may only ever RAISE the ladder bar..."
func TestOverlayIsAppendOnlyWithOneLiveGrantPerClass(t *testing.T) {
	sql := readMigration(t, "0049_opclass_ratified.up.sql")

	// (1) APPEND-ONLY. UPDATE/DELETE revoked from the runtime role. A grant that can be edited after the fact
	// is not a grant — and unlike ordinary state, this row IS an argv template that runs as root somewhere.
	if !regexp.MustCompile(`(?i)REVOKE\s+UPDATE\s*,\s*DELETE\s+ON\s+opclass_ratified\s+FROM\s+tg_runtime`).MatchString(sql) {
		t.Error("an operator-authored ACTUATION vector that can be UPDATEd or DELETEd is not a grant — " +
			"0049 must REVOKE UPDATE, DELETE ON opclass_ratified FROM tg_runtime (INV-19, migration 0015/0042/0043/0048 precedent)")
	}

	// (2) HISTORY, NOT OPINION. PK (op_class, seq) — every ratification/revocation is a NEW row.
	if !regexp.MustCompile(`(?i)PRIMARY\s+KEY\s*\(\s*op_class\s*,\s*seq\s*\)`).MatchString(sql) {
		t.Error("the overlay must be APPEND-ONLY: PRIMARY KEY (op_class, seq) so every ratification and " +
			"revocation is a NEW row and the table reads as the complete grant history of a capability")
	}
	if !regexp.MustCompile(`(?i)seq\s+bigint\s+NOT NULL\s+GENERATED\s+ALWAYS\s+AS\s+IDENTITY`).MatchString(sql) {
		t.Error("seq must be GENERATED ALWAYS AS IDENTITY — a client-supplied sequence could rewrite grant order")
	}

	// (3) ONE LIVE GRANT PER SLUG. Two live rows would make "which template runs?" depend on row order —
	// an actuation vector decided by a sort.
	if !regexp.MustCompile(`(?i)CREATE\s+UNIQUE\s+INDEX\s+\w+\s+ON\s+opclass_ratified\s*\(\s*op_class\s*\)\s*WHERE\s+NOT\s+revoked`).MatchString(sql) {
		t.Error("two live grants for one slug would make WHICH TEMPLATE RUNS depend on row order — " +
			"0049 needs a partial UNIQUE INDEX on (op_class) WHERE NOT revoked")
	}

	// (4) THE THRESHOLD ONLY RISES. core/policy.DefaultPromoteThreshold is 5; ratify sets the per-class N from
	// tier and may raise it, never lower it. A CHECK below 5 would let a ratification buy a faster climb than
	// the code's own conservative default.
	if !regexp.MustCompile(`(?i)promote_threshold\s+int\s+NOT NULL\s+CHECK\s*\(\s*promote_threshold\s*>=\s*5\s*\)`).MatchString(sql) {
		t.Error("ratify may only ever RAISE the ladder bar: promote_threshold needs CHECK (promote_threshold >= 5), " +
			"matching core/policy.DefaultPromoteThreshold — a lower bar would let ratification outrun the compiled default")
	}

	// (5) THE CHAIN BINDING. entry_hash is what makes ROW CONTENT chain-covered rather than merely the fact
	// that a ratification happened.
	if !strings.Contains(sql, "entry_hash") {
		t.Error("the overlay row must carry entry_hash — without it the ledger records THAT a class was ratified " +
			"but not WHAT was ratified, and a tampered spec would load as authorized")
	}
}

// TestLadderWideningIsForwardSafe pins REQ-2804: the new rung, the separate streak, the honest provenance
// outcome, and exactly-once credit.
// RED CONTROLS EXECUTED (on 0050_ladder_auto_notice.up.sql, observed, reverted):
//   - level CHECK left at ('approve','auto')       -> "the ladder CHECK must admit 'auto_notice'..."
//   - dropped the graduation_credit UNIQUE          -> "exactly-once ladder credit needs UNIQUE (op_class, external_ref)..."
//   - dropped 'ratified' from last_outcome          -> "ratification is an operator act, not a run..."
func TestLadderWideningIsForwardSafe(t *testing.T) {
	sql := readMigration(t, "0050_ladder_auto_notice.up.sql")

	// The new rung must be admissible, and the widening must follow 0040's drop-then-re-add shape so an OLD
	// worker's unknown-level parse (-> approve, fail closed) keeps working mid-rollout.
	if !regexp.MustCompile(`(?i)CHECK\s*\(\s*level\s+IN\s*\(\s*'approve'\s*,\s*'auto_notice'\s*,\s*'auto'\s*\)`).MatchString(sql) {
		t.Error("the ladder CHECK must admit 'auto_notice' between approve and auto (REQ-2804) — " +
			"widened by the 0040 drop-then-re-add precedent so an old worker parses the unknown level as approve")
	}

	// A SEPARATE streak for the second climb: sharing clean_run_count would make a demotion ambiguous about
	// which climb the count belonged to.
	if !regexp.MustCompile(`(?i)ADD\s+COLUMN\s+IF\s+NOT\s+EXISTS\s+notice_run_count\s+int\s+NOT NULL\s+DEFAULT\s+0`).MatchString(sql) {
		t.Error("auto_notice -> auto must count on its OWN streak: notice_run_count int NOT NULL DEFAULT 0")
	}

	// PROVENANCE HONESTY (the 0040 'seeded' lesson): a ratified class has verified nothing.
	if !regexp.MustCompile(`(?i)last_outcome\s+IN\s*\([^)]*'ratified'`).MatchString(sql) {
		t.Error("ratification is an operator act, not a run: last_outcome must admit 'ratified' so a freshly " +
			"ratified class never masquerades as verified_clean (the 0040 'seeded' honesty precedent)")
	}

	// EXACTLY-ONCE CREDIT — the 4x-credit lesson applied to the ladder.
	if !regexp.MustCompile(`(?i)UNIQUE\s*\(\s*op_class\s*,\s*external_ref\s*\)`).MatchString(sql) {
		t.Error("exactly-once ladder credit needs UNIQUE (op_class, external_ref) on graduation_credit — " +
			"one incident raising four alert rules must not manufacture four rungs' worth of streak")
	}
	if !regexp.MustCompile(`(?i)REVOKE\s+UPDATE\s*,\s*DELETE\s+ON\s+graduation_credit\s+FROM\s+tg_runtime`).MatchString(sql) {
		t.Error("credit that can be rewritten is not credit — REVOKE UPDATE, DELETE ON graduation_credit")
	}
}
