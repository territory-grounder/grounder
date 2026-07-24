package attribution

import (
	"strings"
	"testing"
	"time"
)

// The rules-as-data loader (REQ-2308): the mapping and carve-outs are loadable data validated at load
// time, an unknown taxonomy/disposition fails closed, and the resolution rules honor REQ-2308 (unmapped
// non-unattributable ⇒ escalate) and REQ-2310 (a non-suspicious contradiction ⇒ escalate).

func TestParseConfigDefault(t *testing.T) {
	m, cfg, err := ParseConfig(DefaultConfigDocument())
	if err != nil {
		t.Fatalf("the shipped default must parse: %v", err)
	}
	// Every taxonomy value must be mapped by the default (a fresh install is governed out of the box).
	for _, tax := range []Taxonomy{Unattributable, AttributedAuthorized, AttributedSelf, AttributedSuspicious, AuthorizedTest} {
		if _, ok := m[tax]; !ok {
			t.Fatalf("the default mapping must cover %v", tax)
		}
	}
	// The dispositions match the spec's mapping.
	if m[AttributedAuthorized] != StandDownCoordinate || m[AttributedSelf] != SelfNoop ||
		m[Unattributable] != LadderUnchanged || m[AttributedSuspicious] != SecurityEscalate ||
		m[AuthorizedTest] != LadderUnchanged {
		t.Fatalf("the default dispositions drifted: %v", m)
	}
	// The embedded default is GENERIC — install-neutral. It must name NO sanctioned principals and NO
	// carve-outs: those are site-specific (a realm's admins, a lab's pool hostnames) and belong in the
	// deploy-time override, never baked into the shipped binary. A non-self actor therefore reads
	// suspicious out of the box until an operator declares their principals (the safe direction).
	if len(cfg.CarveOuts) != 0 {
		t.Fatalf("the generic default must ship NO carve-outs (site-specific), got %+v", cfg.CarveOuts)
	}
	if len(cfg.Sanctioned) != 0 {
		t.Fatalf("the generic default must name NO sanctioned principals (site-specific), got %+v", cfg.Sanctioned)
	}
}

// TestParseConfigOverride proves the deploy-time OVERRIDE path: an operator ruleset carrying site
// principals + a temporally-bounded pool carve-out parses into the typed Config. This is the document an
// operator mounts at TG_ATTRIBUTION_CONFIG — the site-specific data that is NEVER embedded in the binary.
func TestParseConfigOverride(t *testing.T) {
	override := `{"actor_attribution":{
		"mapping":[
			{"id":"authorized-coordinate","taxonomy":"attributed-authorized","disposition":"stand-down-coordinate"},
			{"id":"self-noop","taxonomy":"attributed-self","disposition":"self-noop"},
			{"id":"unattributable-base","taxonomy":"unattributable","disposition":"ladder-unchanged"},
			{"id":"suspicious-escalate","taxonomy":"attributed-suspicious","disposition":"security-escalate"},
			{"id":"authorized-test-heal","taxonomy":"authorized-test","disposition":"ladder-unchanged"}
		],
		"sanctioned_principals":[{"id":"estate-admins","domain":"pve","actors":["root@example"]}],
		"sanctioned_groups":[{"id":"journal-admins","domain":"journal","groups":["admins","tg-admins"]}],
		"carve_outs":[{"id":"test-pool","domain":"pve","actors":["root@example"],"hosts":["pool-guest-01"],
			"valid_from":"2026-01-01T00:00:00Z","valid_until":"2026-12-31T00:00:00Z"}]
	}}`
	m, cfg, err := ParseConfig([]byte(override))
	if err != nil {
		t.Fatalf("the operator override must parse: %v", err)
	}
	if m[AttributedSuspicious] != SecurityEscalate {
		t.Fatalf("the override mapping must carry the security-escalate row, got %v", m[AttributedSuspicious])
	}
	if len(cfg.Sanctioned["pve"]) != 1 || cfg.Sanctioned["pve"][0] != "root@example" {
		t.Fatalf("the override must declare the site sanctioned principal, got %+v", cfg.Sanctioned)
	}
	// The identity seam's sanctioned admin groups parse as loadable rules-as-data (REQ-2317).
	if len(cfg.SanctionedGroups["journal"]) != 2 || cfg.SanctionedGroups["journal"][0] != "admins" {
		t.Fatalf("the override must declare the site sanctioned groups, got %+v", cfg.SanctionedGroups)
	}
	if len(cfg.CarveOuts) != 1 || cfg.CarveOuts[0].ID != "test-pool" ||
		cfg.CarveOuts[0].ValidUntil.Before(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("the override carve-out must parse with its temporal bound, got %+v", cfg.CarveOuts)
	}
}

func TestParseConfigFailsClosedOnUnknownEnum(t *testing.T) {
	for _, doc := range []string{
		`{"actor_attribution":{"mapping":[{"id":"x","taxonomy":"attributed-maybe","disposition":"ladder-unchanged"}]}}`,
		`{"actor_attribution":{"mapping":[{"id":"x","taxonomy":"attributed-self","disposition":"become-root"}]}}`,
		`{"actor_attribution":{"carve_outs":[{"id":"x","valid_from":"not-a-time"}]}}`,
	} {
		if _, _, err := ParseConfig([]byte(doc)); err == nil {
			t.Fatalf("an unknown taxonomy/disposition/time must fail validation (fail-closed), doc: %s", doc)
		}
	}
}

func TestDispositionForResolution(t *testing.T) {
	m, _, err := ParseConfig(DefaultConfigDocument())
	if err != nil {
		t.Fatal(err)
	}
	// A non-suspicious contradiction ALWAYS escalates (REQ-2310), whatever the taxonomy.
	if d := m.DispositionFor(AttributedAuthorized, 2); d != DispositionEscalate {
		t.Fatalf("a contradiction must escalate, got %v", d)
	}
	// REQ-2304 dominance: a suspicious taxonomy keeps its SECURITY escalation even WITH a contradiction
	// (candidates > 1) — the generic contradiction demotion must not strip the security signal.
	if d := m.DispositionFor(AttributedSuspicious, 2); d != SecurityEscalate {
		t.Fatalf("a suspicious-with-contradiction must stay security-escalate, got %v", d)
	}
	// An UNMAPPED non-unattributable taxonomy escalates (REQ-2308); unattributable is ladder-unchanged.
	empty := Mapping{}
	if d := empty.DispositionFor(AttributedSuspicious, 1); d != DispositionEscalate {
		t.Fatalf("an unmapped suspicious must escalate, got %v", d)
	}
	if d := empty.DispositionFor(Unattributable, 1); d != LadderUnchanged {
		t.Fatalf("an unmapped unattributable must be ladder-unchanged, got %v", d)
	}
	// The mapped dispositions resolve.
	if d := m.DispositionFor(AttributedSelf, 1); d != SelfNoop {
		t.Fatalf("self must map to self-noop, got %v", d)
	}
}

// The disposition zero value is the fail-closed escalate (never a permissive fallback), and every
// disposition/taxonomy string round-trips through the closed validator.
func TestClosedEnums(t *testing.T) {
	if DispositionEscalate.String() != "escalate-to-human" {
		t.Fatalf("the zero disposition must be escalate-to-human, got %q", DispositionEscalate.String())
	}
	if _, ok := dispositionFromString(""); ok {
		t.Fatal("an empty disposition must not validate")
	}
	if _, ok := taxonomyFromString("attributed-authorized-ish"); ok {
		t.Fatal("a non-canonical taxonomy must not validate")
	}
	for _, s := range []string{"ladder-unchanged", "stand-down-coordinate", "self-noop", "security-escalate", "escalate-to-human"} {
		d, ok := dispositionFromString(s)
		if !ok || d.String() != strings.TrimSpace(s) {
			t.Fatalf("disposition %q must round-trip, got %v ok=%v", s, d, ok)
		}
	}
}
