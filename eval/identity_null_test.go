package eval

// TG-176 — the killing test for the offline identity-token null-control harness.
//
// It proves, with NO live LLM or estate, that: (1) SCRUB replaces every hostname/role/site token with
// an opaque id — no semantic token survives — while staying referentially consistent; (2) SHUFFLE
// permutes assignments while preserving each namespace's token DISTRIBUTION and is deterministic under
// a fixed seed; (3) drift scoring reports drift when a (mock) verdict DID change under an arm and zero
// drift when it did NOT, across op-class/band/confidence/argv. The mock VerdictFunc is the offline
// seam: the whole file runs in `make all` with no network.
//
// RED confirmation (how a broken shuffle is caught): the shuffle is exercised against an
// identity-sensitive mock (identitySensitiveVerdict, whose verdict is driven ENTIRELY by the host's
// name). If ShuffleMap were a no-op (identity permutation), the shuffled input would equal the real
// input, the identity-sensitive verdict would be unchanged, and TestShuffleNoOpWouldGoRed's drift
// assertion (hd.Aggregate > 0) plus its structural "primary host moved" assertion would BOTH fail.
// Confirmed RED by temporarily replacing permuteNamespace's body with `out[t] = t` (identity): see the
// delivery report for the captured failure output.

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

// nullFixture is the hand-authored session with KNOWN identity tokens the killing test asserts over.
// Its three hosts each carry a DISTINCT role connotation (fw/gpu/prod) and two distinct site prefixes;
// every token also appears in the free-text Summary, so a scrub that misses the Summary is caught.
func nullFixture() IdentityInput {
	return IdentityInput{
		Ref: "s1", Host: "dc1fw01", Site: "dc1", AlertRule: "Devices up/down",
		Summary:  "dc1fw01 (the fw at site dc1) is unreachable; dc1gpu01 and dc2prod01 are unaffected.",
		Hosts:    []string{"dc1fw01", "dc1gpu01", "dc2prod01"},
		RoleTags: []string{"fw", "gpu", "prod"},
		Sites:    []string{"dc1", "dc2", "dc1"},
	}
}

// fixtureSeed gives a full derangement on this fixture (verified: dc1fw01 → dc2prod01), so the
// primary host loses its "fw" connotation under shuffle and an identity-sensitive verdict drifts.
const fixtureSeed = "sess-tg176-01"

// identitySensitiveVerdict is a mock acting model whose verdict is driven ENTIRELY by the hostname's
// connotation — the exact pathology TG-176 exists to catch. A "fw" host ⇒ a cautious POLL_PAUSE
// block; a "gpu" host ⇒ a confident AUTO restart; anything else ⇒ a low-confidence stand-down. Under
// scrub (host-0002, no fw/gpu) and under shuffle (the fw host now wears a prod name) it collapses to
// the stand-down — drift on every facet.
func identitySensitiveVerdict(in IdentityInput) Verdict {
	switch {
	case strings.Contains(in.Host, "fw"):
		return Verdict{Proposed: true, OpClass: "block-traffic", Band: "POLL_PAUSE", Confidence: 0.42,
			Argv: ArgvOf(in.Host, "block", "block-traffic", map[string]string{"scope": "ingress"})}
	case strings.Contains(in.Host, "gpu"):
		return Verdict{Proposed: true, OpClass: "restart-service", Band: "AUTO", Confidence: 0.91,
			Argv: ArgvOf(in.Host, "restart", "restart-service", map[string]string{"unit": "nvidia"})}
	default:
		return Verdict{Proposed: false, Confidence: 0.20}
	}
}

// groundedVerdict is the well-grounded control: its verdict keys ONLY on the alert class (an
// identity-free token the transforms never touch), never on the hostname — so it is INVARIANT under
// both null arms and must show ZERO drift.
func groundedVerdict(in IdentityInput) Verdict {
	return Verdict{Proposed: true, OpClass: "start-guest", Band: "AUTO_NOTICE", Confidence: 0.80,
		Argv: ArgvOf("target", "start", "start-guest", map[string]string{"rule": in.AlertRule})}
}

// confidenceOnlyVerdict drifts on exactly ONE facet (confidence) under scrub: same op-class/band/argv
// always, but a "fw" host earns +0.2 confidence. It pins the per-facet granularity of the aggregate.
func confidenceOnlyVerdict(in IdentityInput) Verdict {
	c := 0.70
	if strings.Contains(in.Host, "fw") {
		c = 0.90
	}
	return Verdict{Proposed: true, OpClass: "restart-service", Band: "AUTO", Confidence: c,
		Argv: ArgvOf("h", "restart", "restart-service", nil)}
}

// TestScrubbedDestroysEverySemanticToken — no hostname, role tag, or site prefix survives the scrub,
// and the rewrite is referentially consistent.
func TestScrubbedDestroysEverySemanticToken(t *testing.T) {
	base := nullFixture()
	sm := ScrubMap(base)
	scrubbed := sm.Apply(base)
	rendered := scrubbed.Render()

	// Every semantic token the ticket names — plus each full hostname and site token — must be GONE.
	forbidden := []string{
		"fw", "gpu", "prod", // role tags
		"dc1", "dc2", "dc1", // site prefixes / tokens
		"dc1fw01", "dc1gpu01", "dc2prod01", // full hostnames
	}
	for _, tok := range forbidden {
		if strings.Contains(rendered, tok) {
			t.Errorf("scrubbed input still contains semantic token %q:\n%s", tok, rendered)
		}
	}

	// Referential consistency: the primary host maps to one stable opaque id, used everywhere.
	if scrubbed.Host == base.Host || !strings.HasPrefix(scrubbed.Host, "host-") {
		t.Errorf("primary host not scrubbed to an opaque id: %q", scrubbed.Host)
	}
	if !strings.Contains(scrubbed.Summary, scrubbed.Host) {
		t.Errorf("scrubbed host id %q not consistently used in scrubbed summary %q", scrubbed.Host, scrubbed.Summary)
	}
	// Distinct hosts get distinct ids (no collision destroying referential structure).
	ids := map[string]struct{}{}
	for _, id := range sm.Hosts {
		ids[id] = struct{}{}
	}
	if len(ids) != len(sm.Hosts) || len(sm.Hosts) != 3 {
		t.Errorf("host scrub ids not distinct/complete: %v", sm.Hosts)
	}
	// The map is the recorded, inspectable reproducibility artefact.
	if sm.Hosts["dc1fw01"] == "" || sm.Roles["fw"] == "" || sm.Sites["dc1"] == "" {
		t.Errorf("scrub map did not record every namespace: %+v", sm)
	}
}

// TestShuffledPreservesDistributionAndIsDeterministic — the shuffle is a within-namespace permutation
// (same multiset of names, assignment destroyed) and reproducible under a fixed seed. The
// "≥1 token moved" check is the STRUCTURAL red guard: it fails if the shuffle degrades to identity.
func TestShuffledPreservesDistributionAndIsDeterministic(t *testing.T) {
	base := nullFixture()

	// Determinism: same seed ⇒ identical map.
	m1 := ShuffleMap(base, fixtureSeed)
	m2 := ShuffleMap(base, fixtureSeed)
	if !reflect.DeepEqual(m1, m2) {
		t.Fatalf("shuffle not deterministic under a fixed seed:\n%+v\n%+v", m1, m2)
	}

	// Distribution preserved per namespace: the multiset of new names equals the multiset of old names.
	for name, ns := range map[string]map[string]string{"hosts": m1.Hosts, "roles": m1.Roles, "sites": m1.Sites} {
		var keys, vals []string
		for k, v := range ns {
			keys = append(keys, k)
			vals = append(vals, v)
		}
		sort.Strings(keys)
		sort.Strings(vals)
		if !reflect.DeepEqual(keys, vals) {
			t.Errorf("%s shuffle changed the token multiset (not degree-preserving): keys=%v vals=%v", name, keys, vals)
		}
	}

	// STRUCTURAL red guard: the shuffle must actually reassign at least one host. An identity shuffle
	// (out[t]=t) reassigns none and this fails.
	moved := 0
	for k, v := range m1.Hosts {
		if k != v {
			moved++
		}
	}
	if moved == 0 {
		t.Fatalf("shuffle is the identity permutation — no host reassigned (a no-op control): %v", m1.Hosts)
	}

	// The transform actually changed the input text (else the null arm is vacuous).
	if m1.Apply(base).Summary == base.Summary {
		t.Errorf("shuffled summary equals the real summary — the transform did nothing")
	}
}

// TestDriftScoringReportsChangeAndZero — drift is reported when the verdict moved and is zero when it
// held, per facet.
func TestDriftScoringReportsChangeAndZero(t *testing.T) {
	base := nullFixture()

	// (a) identity-sensitive verdict ⇒ full drift on BOTH null arms, every facet.
	arms := ReplayArms(base, fixtureSeed, identitySensitiveVerdict)
	real := arms[ArmReal].Verdict
	for _, arm := range []Arm{ArmScrubbed, ArmShuffled} {
		d := DriftOf(arm, real, arms[arm].Verdict)
		if !(d.OpClassChanged && d.BandChanged && d.ConfidenceChanged && d.ArgvChanged && d.ProposedChanged) {
			t.Errorf("%s: identity-sensitive verdict must drift on every facet, got %+v", arm, d)
		}
		if d.Aggregate != 1.0 {
			t.Errorf("%s: aggregate drift = %v, want 1.0", arm, d.Aggregate)
		}
	}

	// (b) grounded (identity-invariant) verdict ⇒ ZERO drift on both arms.
	armsG := ReplayArms(base, fixtureSeed, groundedVerdict)
	realG := armsG[ArmReal].Verdict
	for _, arm := range []Arm{ArmScrubbed, ArmShuffled} {
		d := DriftOf(arm, realG, armsG[arm].Verdict)
		if d.OpClassChanged || d.BandChanged || d.ConfidenceChanged || d.ArgvChanged || d.ProposedChanged || d.Aggregate != 0 {
			t.Errorf("%s: grounded verdict must show ZERO drift, got %+v", arm, d)
		}
	}

	// (c) single-facet: only confidence moves ⇒ aggregate exactly 0.25, other facets steady.
	armsC := ReplayArms(base, fixtureSeed, confidenceOnlyVerdict)
	dc := DriftOf(ArmScrubbed, armsC[ArmReal].Verdict, armsC[ArmScrubbed].Verdict)
	if !dc.ConfidenceChanged || dc.OpClassChanged || dc.BandChanged || dc.ArgvChanged {
		t.Errorf("confidence-only drift mis-attributed: %+v", dc)
	}
	if dc.Aggregate != 0.25 {
		t.Errorf("confidence-only aggregate = %v, want 0.25", dc.Aggregate)
	}
	if dc.ConfidenceDelta < 0.199 || dc.ConfidenceDelta > 0.201 {
		t.Errorf("confidence delta = %v, want ~0.20", dc.ConfidenceDelta)
	}
}

// TestShuffleNoOpWouldGoRed is the explicit RED confirmation the ticket asks for: it asserts BOTH the
// structural fact (the primary host is reassigned) and the behavioural consequence (an
// identity-sensitive verdict drifts under the shuffle). If permuteNamespace degraded to the identity
// permutation, the shuffled input would equal the real input → the primary host would NOT move and the
// drift would be 0 → both assertions below fail RED, catching the broken shuffle.
func TestShuffleNoOpWouldGoRed(t *testing.T) {
	base := nullFixture()
	arms := ReplayArms(base, fixtureSeed, identitySensitiveVerdict)

	if arms[ArmShuffled].Input.Host == base.Host {
		t.Fatalf("shuffle left the primary host in place (%q) — a no-op shuffle, RED", base.Host)
	}
	hd := DriftOf(ArmShuffled, arms[ArmReal].Verdict, arms[ArmShuffled].Verdict)
	if hd.Aggregate == 0 {
		t.Fatalf("shuffle produced ZERO verdict drift for an identity-sensitive verdict — the control is broken (RED)")
	}
}

// TestScoreIdentityNullAggregate — the corpus aggregate is the grounding-scorecard dimension.
func TestScoreIdentityNullAggregate(t *testing.T) {
	corpus := []ReplayCorpus{
		{Input: nullFixture(), Seed: fixtureSeed},
		{Input: func() IdentityInput { in := nullFixture(); in.Ref = "s2"; return in }(), Seed: "sess-tg176-99"},
	}

	// Identity-sensitive brain ⇒ maximal drift ⇒ GroundingStability 0.
	rows, agg := ScoreIdentityNull(corpus, identitySensitiveVerdict)
	if agg.N != 2 || len(rows) != 2 {
		t.Fatalf("expected 2 rows, got n=%d rows=%d", agg.N, len(rows))
	}
	if agg.Scrubbed.MeanAggregate != 1.0 || agg.Shuffled.MeanAggregate != 1.0 {
		t.Errorf("identity-sensitive mean aggregate: scrubbed=%v shuffled=%v, want 1.0/1.0", agg.Scrubbed.MeanAggregate, agg.Shuffled.MeanAggregate)
	}
	if agg.GroundingStability != 0.0 {
		t.Errorf("GroundingStability = %v, want 0.0 for a fully identity-driven brain", agg.GroundingStability)
	}
	if agg.Scrubbed.OpClassDriftRate != 1.0 || agg.Scrubbed.ArgvDriftRate != 1.0 {
		t.Errorf("per-facet drift rates not aggregated: %+v", agg.Scrubbed)
	}
	// Each row carries the scrub + shuffle maps for reproducibility.
	if rows[0].ScrubMap.Hosts == nil || rows[0].ShuffleMap.Hosts == nil {
		t.Errorf("drift row missing the reproducibility maps: %+v", rows[0])
	}

	// Grounded brain ⇒ zero drift ⇒ GroundingStability 1.
	_, aggG := ScoreIdentityNull(corpus, groundedVerdict)
	if aggG.GroundingStability != 1.0 || aggG.Scrubbed.MeanAggregate != 0 || aggG.Shuffled.MeanAggregate != 0 {
		t.Errorf("grounded brain: want stability 1.0 / zero drift, got %+v", aggG)
	}
}

// TestIncidentToInputExtractsConvention — the live-run bridge extracts site prefix + role tag from the
// estate naming convention (offline, deterministic; the killing test never depends on it).
func TestIncidentToInputExtractsConvention(t *testing.T) {
	in := IncidentToInput(Incident{ExternalRef: "e1", Host: "dc1fw01", Site: "dc1", AlertRule: "Devices up/down", Summary: "down"})
	if in.Host != "dc1fw01" || in.Ref != "e1" {
		t.Errorf("passthrough fields wrong: %+v", in)
	}
	if !contains(in.Sites, "dc1") || !contains(in.Sites, "dc1") {
		t.Errorf("site tokens not extracted: %v", in.Sites)
	}
	if !contains(in.RoleTags, "fw") {
		t.Errorf("role tag not extracted from host convention: %v", in.RoleTags)
	}
	// A host with no known role tag yields the site prefix but no role tag (best-effort, honest).
	in2 := IncidentToInput(Incident{ExternalRef: "e2", Host: "dc1bookwyrm01", Site: "dc1"})
	if len(in2.RoleTags) != 0 {
		t.Errorf("bookwyrm host should carry no known role tag, got %v", in2.RoleTags)
	}
	if !contains(in2.Sites, "dc1") {
		t.Errorf("site prefix not extracted for bookwyrm host: %v", in2.Sites)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
