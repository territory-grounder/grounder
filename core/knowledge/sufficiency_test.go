package knowledge

import (
	"strings"
	"testing"
)

// adequacyHit builds a Hit for the sufficiency tests (distinctively named to avoid colliding with the
// knowledge package's other test helpers). Reasons are passed through verbatim so a test can inject the exact
// "semantic similarity X.XX" string the fuser emits, via semanticReason.
func adequacyHit(ref, rule, host string, reasons ...string) Hit {
	return Hit{Incident: Incident{ExternalRef: ref, AlertRule: rule, Host: host}, Reasons: reasons}
}

func TestHasAdequatePrecedent_SameRuleIsAdequate(t *testing.T) {
	q := Query{AlertRule: "Device-Down", Host: "web01"}
	// Shares the alert rule though NOT the host — the dominant channel alone is a strong match.
	hits := []Hit{adequacyHit("A", "Device-Down", "other-host", "same alert rule")}
	if !HasAdequatePrecedent(q, hits, 0) {
		t.Fatal("a same-alert-rule precedent must be adequate")
	}
}

func TestHasAdequatePrecedent_SameHostIsAdequate(t *testing.T) {
	q := Query{AlertRule: "Some-Rule", Host: "web01"}
	// Different rule, SAME host. KILLING MUTATION: drop the host branch in HasAdequatePrecedent → reddens.
	hits := []Hit{adequacyHit("A", "Other-Rule", "web01", "same host")}
	if !HasAdequatePrecedent(q, hits, 0) {
		t.Fatal("a same-host precedent must be adequate")
	}
}

func TestHasAdequatePrecedent_WeakOverlapIsInadequate(t *testing.T) {
	q := Query{AlertRule: "Device-Down", Host: "web01", Site: "nl", Tags: []string{"net"}}
	// Shares NEITHER rule nor host — only site/tag overlap. This is the CRAG-fire case: a weak, off-target
	// precedent the seed should NOT pad with.
	hits := []Hit{adequacyHit("A", "Unrelated-Rule", "db07", "same site", "shared tags")}
	if HasAdequatePrecedent(q, hits, 0) {
		t.Fatal("a set sharing neither rule nor host (only weak overlap) must be INADEQUATE")
	}
}

func TestHasAdequatePrecedent_SemanticStrongYesWeakNo(t *testing.T) {
	q := Query{AlertRule: "Device-Down", Host: "web01"}
	// A strong semantic neighbor on a DIFFERENT rule+host (the paraphrase case the semantic channel exists
	// for) is adequate.
	strong := []Hit{adequacyHit("A", "Other-Rule", "other-host", semanticReason(0.83))}
	if !HasAdequatePrecedent(q, strong, 0) {
		t.Fatalf("a strong semantic neighbor (0.83 ≥ %.2f) must be adequate", StrongSemanticSimilarity)
	}
	// A below-bar neighbor is NOT adequate on its own. KILLING MUTATION: `sim >= minCosine` → `sim >= 0`
	// (or dropping the compare) → this wrongly passes.
	weak := []Hit{adequacyHit("B", "Other-Rule", "other-host", semanticReason(0.60))}
	if HasAdequatePrecedent(q, weak, 0) {
		t.Fatalf("a below-bar semantic neighbor (0.60 < %.2f) must NOT be adequate on its own", StrongSemanticSimilarity)
	}
}

func TestHasAdequatePrecedent_ChecksEveryHitNotJustTop(t *testing.T) {
	q := Query{AlertRule: "Device-Down", Host: "web01"}
	// The FIRST hit is a weak cross-match; a LOWER hit shares the rule. RRF fusion can rank a same-rule
	// precedent below a multi-channel one, so the SET is adequate iff ANY member is strong.
	// KILLING MUTATION: check only hits[0] instead of ranging → the same-rule hit at [1] is missed, reddens.
	hits := []Hit{
		adequacyHit("weak", "Unrelated", "db07", "same site"),
		adequacyHit("strong", "Device-Down", "other-host", "same alert rule"),
	}
	if !HasAdequatePrecedent(q, hits, 0) {
		t.Fatal("a same-rule precedent anywhere in the set makes the set adequate")
	}
}

func TestHasAdequatePrecedent_EmptyIsInadequate(t *testing.T) {
	if HasAdequatePrecedent(Query{AlertRule: "X", Host: "y"}, nil, 0) {
		t.Fatal("an empty set has no adequate precedent")
	}
}

func TestHasAdequatePrecedent_ExplicitMinCosineBar(t *testing.T) {
	q := Query{AlertRule: "R", Host: "h"} // no rule/host share, so only the semantic bar decides
	hits := []Hit{adequacyHit("A", "other", "other", semanticReason(0.70))}
	if !HasAdequatePrecedent(q, hits, 0.65) {
		t.Fatal("0.70 must clear an explicit 0.65 bar")
	}
	if HasAdequatePrecedent(q, hits, 0.80) {
		t.Fatal("0.70 must NOT clear an explicit 0.80 bar")
	}
}

func TestNoAdequatePrecedentBlock_StatesFactGivesNoInstruction(t *testing.T) {
	for _, xml := range []bool{false, true} {
		b := NoAdequatePrecedentBlock(xml)
		if b == "" || !strings.Contains(b, "no adequate precedent") {
			t.Fatalf("xml=%v: want a non-empty 'no adequate precedent' signal, got %q", xml, b)
		}
		if !strings.HasSuffix(b, "\n") {
			t.Errorf("xml=%v: the block must end in a newline like Context/ContextXML", xml)
		}
		// It STATES A FACT and gives NO per-row hedge (the eval-measured blanket-caveat regression on
		// Provenance.Label). Guard against a future edit sliding an imperative hedge back in. ("verify against
		// live evidence" is the established, eval-passed framing carried by every precedent block, so it is
		// deliberately NOT banned.)
		low := strings.ToLower(b)
		for _, banned := range []string{"do not", "be careful", "caution", "distrust", "you must", "ignore "} {
			if strings.Contains(low, banned) {
				t.Errorf("xml=%v: the signal must state a fact, not instruct — found %q in %q", xml, banned, b)
			}
		}
	}
	if NoAdequatePrecedentBlock(true) == NoAdequatePrecedentBlock(false) {
		t.Error("the xml and plain renderings must differ")
	}
}
