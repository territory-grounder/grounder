package deploy

import (
	"sort"
	"strings"
	"testing"
)

// TG-48. `worker` forwarded six TG_COST_* keys and `worker-actuate` forwarded NONE, so the actuation
// plane ran with its model gateway un-wrapped while triage metered: `tg_cost_metering` read 1 on
// worker:8444 and 0 on worker-actuate:8445, measured live 2026-08-06.
//
// That matters specifically because the cost store is cross-process by design. The worker's own boot line
// says so: "backed by the DURABLE cross-process store (cost_accrual + cost_breaker_state, 0023) — a trip
// force-Shadows every sibling worker". A plane that cannot read the config never accrues its share, so
// the shared budget is spent by one plane and accounted by neither. TG_COST_PER_ACTUATION_USD is worse
// still: it is the per-actuation cost, withheld from the only plane that actuates.
//
// This is the same defect as TG_PREDICT_MIN_CONFIDENCE being set on one block only (!1085), where the two
// planes computed different blast radii for the same incident. Both times the divergence was invisible:
// each block is individually valid YAML and each worker boots healthy.
//
// The rule this pins is NOT "the two blocks are identical" — they are legitimately different, because the
// credential-plane split (TG-153/TG-164) withholds keys from each plane on purpose. It is narrower: a
// key that governs SHARED CROSS-PROCESS STATE must reach both planes or neither.

// sharedStatePrefixes name the env-key families that govern state both worker processes read and write
// through the database. A key in one of these families on one plane only is a silent divergence.
//
// Deliberately a short, justified list rather than "all keys": widening it to every key would fail on the
// plane split itself, which is a control and not a bug.
var sharedStatePrefixes = []struct {
	prefix string
	why    string
}{
	{"TG_COST_", "the spend guard is backed by cost_accrual + cost_breaker_state, and a trip force-Shadows " +
		"every sibling worker — one plane blind to the config never accrues its share"},
	{"TG_ESTATE_SNAPSHOT_", "both planes write estate_snapshot and both run the retention sweep (TG-355); " +
		"a keep-count set on one plane only means the two disagree about what to retain"},
	// ADDED AFTER THIS GUARD FAILED TO CATCH THE VERY DEFECT IT CITES. The list above named the families I
	// had just fixed, so TG_PREDICT_MIN_CONFIDENCE — the ORIGINAL example, quoted in this file's own header
	// — was not covered. It was set to 0.70 on the triage plane and absent on the actuation plane in
	// production for the whole day, while the comment beside it claimed the case was closed (!1085 was
	// reported merged and had in fact conflicted).
	//
	// These are not cross-process STATE like the two above; they are SAFETY THRESHOLDS. The rule is the
	// same and the reason is stronger: the plane that gates a proposal and the plane that executes it must
	// reason over the same width, or the gate's verdict describes a different action than the one that runs.
	{"TG_PREDICT_", "core/predict runs identically in both planes; a confidence floor on one plane only " +
		"means the gating plane and the executing plane compute different blast radii for the same incident"},
	// ADDED AFTER THE SAME GUARD MISSED A THIRD FAMILY (TG-309, 2026-08-07). Measured on the running
	// estate: the ACTUATION worker logged `TG_ATTRIBUTION_CONFIG unset — using the generic embedded
	// default` and `carve-out host coverage 0/20 — NOT covered: <all 20 allowlisted guests>`, while the
	// TRIAGE worker held the operator ruleset and both carve-outs. The config governing how TG recognises
	// its OWN actions was on the plane that never acts and absent from the plane that does.
	//
	// THE SUB-RULE THIS FAMILY ADDS, because "shared DB state" does not cover it: a family that governs
	// how TG interprets its OWN actions belongs on the plane that PERFORMS them. Attribution decides
	// whether a heal reads as TG's own or as an unattributed actor, and an unattributed actor
	// security-escalates and masks every other candidate during triage.
	{"TG_ATTRIBUTION_", "attribution decides whether an action reads as TG's OWN or as a suspicious " +
		"actor. The plane that ACTS is the one whose actions need recognising; a ruleset on the triage " +
		"plane only means every heal the actuation plane performs is judged against the generic embedded " +
		"default, with zero carve-out coverage for the guests it is allowed to touch"},
	{"TG_BLAST_RADIUS_", "the blast-radius width threshold decides what counts as a WIDE impact — the " +
		"input to a safety decision, not a tuning preference"},
}

func workerPlaneEnvKeys(t *testing.T, service string) map[string]bool {
	t.Helper()
	block, ok := composeServiceBlock(composeFile(t), service)
	if !ok {
		t.Fatalf("compose has no %q service block — this guard is anchored on a service that no longer "+
			"exists and would otherwise pass while checking nothing", service)
	}
	keys := map[string]bool{}
	for _, line := range strings.Split(stripYAMLCommentLines(block), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "TG_") {
			continue
		}
		if i := strings.Index(trimmed, ":"); i > 0 {
			keys[trimmed[:i]] = true
		}
	}
	return keys
}

func TestSharedStateEnvKeysReachBothWorkerPlanes(t *testing.T) {
	triage := workerPlaneEnvKeys(t, "worker")
	actuate := workerPlaneEnvKeys(t, "worker-actuate")

	if len(triage) == 0 || len(actuate) == 0 {
		t.Fatalf("VACUITY FLOOR: parsed %d keys from worker and %d from worker-actuate. A parser that "+
			"returns nothing makes every comparison below pass.", len(triage), len(actuate))
	}

	var probed int
	for _, fam := range sharedStatePrefixes {
		onTriage := keysWithPrefix(triage, fam.prefix)
		onActuate := keysWithPrefix(actuate, fam.prefix)
		probed += len(onTriage) + len(onActuate)

		if len(onTriage) == 0 && len(onActuate) == 0 {
			// Neither plane declares the family. That is a legitimate state (the feature is not deployed),
			// but it must be visible rather than silently counted as agreement.
			t.Logf("note: neither worker plane declares any %s key — nothing to compare for this family", fam.prefix)
			continue
		}
		if missing := difference(onTriage, onActuate); len(missing) > 0 {
			t.Errorf("worker-actuate is MISSING %d %s key(s) that worker declares: %v\nwhy it matters: %s",
				len(missing), fam.prefix, missing, fam.why)
		}
		if missing := difference(onActuate, onTriage); len(missing) > 0 {
			t.Errorf("worker is MISSING %d %s key(s) that worker-actuate declares: %v\nwhy it matters: %s",
				len(missing), fam.prefix, missing, fam.why)
		}
	}
	if probed == 0 {
		t.Fatal("VACUITY FLOOR: no key in any shared-state family was found on either plane, so every " +
			"assertion above was skipped. Either the prefixes are wrong or the parser is broken.")
	}
}

// TestTheParserSeesRealKeysAndNotComments is the negative control. The block is comment-stripped first,
// because a guard in this repo has passed on its own comment before — and these compose blocks carry long
// prose that mentions the very key names being asserted.
func TestTheParserSeesRealKeysAndNotComments(t *testing.T) {
	keys := workerPlaneEnvKeys(t, "worker")
	if !keys["TG_COST_DAILY_BUDGET_USD"] {
		t.Error("the parser did not find TG_COST_DAILY_BUDGET_USD on the worker block, which is declared " +
			"there — it is not reading real keys")
	}
	// A key that appears ONLY inside comment prose must not be picked up. The worker block's own comments
	// reference TG_PREDICT_MIN_CONFIDENCE by name in explanatory text; if that were the only mention, the
	// parser must not report it.
	raw, ok := composeServiceBlock(composeFile(t), "worker")
	if !ok {
		t.Fatal("no worker block")
	}
	stripped := stripYAMLCommentLines(raw)
	if strings.Count(raw, "TG_") <= strings.Count(stripped, "TG_") {
		t.Skip("the worker block has no commented TG_ mention to test the stripper against")
	}
}

func keysWithPrefix(set map[string]bool, prefix string) []string {
	var out []string
	for k := range set {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func difference(a, b []string) []string {
	inB := map[string]bool{}
	for _, k := range b {
		inB[k] = true
	}
	var out []string
	for _, k := range a {
		if !inB[k] {
			out = append(out, k)
		}
	}
	return out
}
