package deploy

// THE ESTATE-DOC GROUNDING CORPUS MUST STAY FORWARDABLE, AND OPT-IN (TG-86 slice 1c: ARM).
//
// The worker's boot coverage job (cmd/worker/estate_doc_coverage.go) reads TG_ESTATE_DOCS_DIR and
// TG_ESTATE_DOC_CORPUS via getenv. Slices 1+1b shipped that job, but the worker compose service forwarded
// NEITHER variable. A compose `.env` is INTERPOLATION-ONLY — it is not injected into containers — so a
// variable an operator set in .env reached this container only if it was named in the service's
// `environment:` block. It was not, so the override was silently discarded, the job booted with an unset
// docs root, and the live tg_estate_doc_files gauge sat as an empty vector: the TG-86 arming oracle stuck
// RED on a blank default. That is the TG-384 compose-parity failure class exactly — a feature reachable in
// code, inert in prod on a missing forward.
//
// deploy/envparity_test.go now scans estate_doc_coverage.go so the two keys can never again be read-but-
// unforwarded. THIS guard pins the two properties envparity does not check:
//
//	1. the value is forwarded FROM THE OPERATOR ENV (`${VAR}`), not hardcoded to some path; and
//	2. its default is EMPTY (`${VAR:-}`), so arming stays an explicit opt-in — the feature can never
//	   silently default ON, and TG_ESTATE_DOCS_DIR can never carry a baked-in estate path into the image
//	   (a STONITH concern: no environment-specific literal belongs in a shipped default).
//
// A stricter-looking default here (`${VAR:?set me}`) would force EVERY stack to set the vars or fail to
// boot, breaking the honest-absence contract slices 1b / TG-394 established (unconfigured ⇒ emits nothing).
// The empty fallback is what keeps an un-armed stack byte-for-byte unchanged, so it is the value under test.

import (
	"os"
	"strings"
	"testing"
)

func TestWorkerForwardsEstateDocGroundingVars(t *testing.T) {
	raw, err := os.ReadFile("docker-compose.yml")
	if err != nil {
		t.Fatalf("read docker-compose.yml: %v — this guard cannot assert anything about a file it cannot "+
			"open, and must fail rather than pass vacuously", err)
	}
	// Comment-stripped so the substring scan below cannot match one of the WHY-comments that name these
	// same variables (the failure TG-241's category floor was built to prevent: prose is not configuration).
	worker := stripYAMLComments(serviceBlock(t, string(raw), "worker"))

	// The arming variables, each with the EXACT safe-default fallback it must carry. The `${VAR:-}` token is
	// load-bearing: it means "operator env, empty when unset" — the forwarding (interpolation-only compose)
	// and the opt-in default in one string. Assert the whole token, not just the key, so an edit to a
	// non-empty or hardcoded default reddens here even though envparity (key-presence only) would stay green.
	for _, want := range []string{
		"TG_ESTATE_DOC_CORPUS: ${TG_ESTATE_DOC_CORPUS:-}",
		"TG_ESTATE_DOCS_DIR: ${TG_ESTATE_DOCS_DIR:-}",
	} {
		if !strings.Contains(worker, want) {
			t.Errorf("the worker service does not forward %q.\n"+
				"cmd/worker/estate_doc_coverage.go reads that key via getenv at boot; without this exact line an "+
				"operator override in .env is silently dropped and the tg_estate_doc_* grounding gauges never "+
				"leave their empty-vector state — the TG-86 arming oracle cannot go green. Restore it as "+
				"`KEY: ${KEY:-}` (empty default = opt-in; an un-armed stack stays byte-for-byte unchanged).", want)
		}
	}
}
