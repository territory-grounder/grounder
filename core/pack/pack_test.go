package pack

import (
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/safety"
)

func validPack() Pack {
	return Pack{
		ID: "cisco", Title: "Cisco network pack", Summary: "read-only Cisco diagnostics",
		Version: "1.0.0", Domains: []string{"cisco"},
		VendorHint: VendorHint{Transport: TransportCiscoInteractive, PromptProfile: "asa", ConfigMode: ConfigModeReadOnly},
		Tools:      []string{"show-device-config"}, Skills: []string{"cisco-triage"},
		TierHint: "primary",
		Band:     BandOverlay{Floor: safety.BandPollPause, Applies: true, Reason: "cisco-never-auto"},
	}
}

// Every Validate refusal, one mutation each. KILLING MUTATION: delete any single rule in Validate and
// exactly one case here fails on "expected a refusal".
func TestValidateRefusesEachAuthoringError(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Pack)
		want string
	}{
		{"bad id", func(p *Pack) { p.ID = "Cisco Pack" }, "lowercase slug"},
		{"no title", func(p *Pack) { p.Title = " " }, "Title"},
		{"version with separator", func(p *Pack) { p.Version = "1.0@0" }, "separator"},
		{"no domains", func(p *Pack) { p.Domains = nil }, "at least one domain"},
		{"blank domain", func(p *Pack) { p.Domains = []string{""} }, "blank domain"},
		{"unknown transport", func(p *Pack) { p.VendorHint.Transport = "telnet" }, "closed"},
		{"hint without transport", func(p *Pack) { p.VendorHint = VendorHint{PromptProfile: "asa"} }, "reaches no code"},
		{"write config mode", func(p *Pack) { p.VendorHint.ConfigMode = "read-write" }, "cannot widen"},
		{"tool with space", func(p *Pack) { p.Tools = []string{"show config"} }, "bare registered name"},
		{"blank skill", func(p *Pack) { p.Skills = []string{""} }, "bare registered name"},
		{"demoting tier hint", func(p *Pack) { p.TierHint = "fast" }, "demote"},
		{"floor without applies", func(p *Pack) { p.Band = BandOverlay{Floor: safety.BandAutoNotice} }, "zero-value trap"},
		{"orphan reason without applies", func(p *Pack) { p.Band = BandOverlay{Reason: "why"} }, "zero-value trap"},
		{"applies without reason", func(p *Pack) { p.Band = BandOverlay{Floor: safety.BandPollPause, Applies: true} }, "Reason"},
	}
	if err := validPack().Validate(); err != nil {
		t.Fatalf("the fixture must validate before mutation: %v", err)
	}
	for _, c := range cases {
		p := validPack()
		c.mut(&p)
		err := p.Validate()
		if err == nil {
			t.Errorf("%s: expected a refusal, got nil", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: refusal %q does not name the rule (%q)", c.name, err, c.want)
		}
	}
}

// The floor-shaped trap specifically: a POLL_PAUSE floor literal without Applies is Band's ZERO value on
// Floor — indistinguishable from unset — and must be refused when any overlay field is set.
func TestValidateNamesTheZeroValueTrap(t *testing.T) {
	p := validPack()
	p.Band = BandOverlay{Floor: safety.BandPollPause, Applies: false, Reason: "cisco"}
	if err := p.Validate(); err == nil {
		t.Fatal("an inert overlay with a reason must be refused, not silently dropped")
	}
}

// Escalate-only, by construction. KILLING MUTATION: flip EscalateTier's comparison to allow
// primary→fast and the "never demotes" cases fail.
func TestEscalateTierNeverDemotes(t *testing.T) {
	cases := []struct{ base, hint, want string }{
		{"fast", "primary", "primary"}, // the one escalation
		{"primary", "primary", "primary"},
		{"primary", "", "primary"},
		{"primary", "fast", "primary"},          // a demoting hint is inert (and Validate refuses authoring it)
		{"fast", "", "fast"},                    // no hint, no change
		{"fast", "haiku-mega", "fast"},          // unknown hint is inert
		{"operator-alias", "primary", "operator-alias"}, // an aliased base is never rewritten
	}
	for _, c := range cases {
		if got := EscalateTier(c.base, c.hint); got != c.want {
			t.Errorf("EscalateTier(%q,%q) = %q, want %q", c.base, c.hint, got, c.want)
		}
	}
}

func TestLedgerTokenShapeCarriesNoVersionRowID(t *testing.T) {
	tok := validPack().LedgerToken()
	if tok != "pack:cisco@1.0.0" {
		t.Fatalf("token %q", tok)
	}
	if strings.Contains(tok, "#") {
		t.Fatalf("a '#' would read as a skill_version row id to judge.StoreVersionIDs: %q", tok)
	}
}

// The compiled catalog validates as a whole — the authoring-time gate. With zero packs declared the
// check is VACUOUS BY DESIGN and says so (the substrate ships inert; the first content pack makes this
// test real), rather than passing silently as if it had checked something.
func TestCatalogValidates(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if len(all) == 0 {
		t.Log("catalog is EMPTY (substrate only) — this check is vacuous until the first pack lands")
	}
}

func TestSelectionIsStrictAndPure(t *testing.T) {
	a, b := validPack(), validPack()
	b.ID, b.Domains = "kube", []string{"kubernetes"}
	all := []Pack{a, b}

	if _, ok := selectIn(all, ""); ok {
		t.Fatal("the unknown domain must select NO pack — the DomainUnknown discipline")
	}
	if p, ok := selectIn(all, "kubernetes"); !ok || p.ID != "kube" {
		t.Fatalf("kubernetes selected %v %v", p.ID, ok)
	}
	if p, ok := selectIn(all, "cisco"); !ok || p.ID != "cisco" {
		t.Fatalf("cisco selected %v %v", p.ID, ok)
	}
	if _, ok := selectIn(all, "proxmox"); ok {
		t.Fatal("an unlisted domain must select nothing")
	}
	if _, ok := For("no-such-domain"); ok {
		t.Fatal("For over the compiled catalog must not select on an unknown domain")
	}
}

func TestResolvePartitionsAndFailsClosedOnMissingResolver(t *testing.T) {
	p := validPack()
	p.Tools = []string{"present", "absent"}
	has := func(n string) bool { return n == "present" }

	av := Resolve(p, has, func(VendorHint) (bool, string) { return true, "" })
	if len(av.ToolsPresent) != 1 || av.ToolsPresent[0] != "present" ||
		len(av.ToolsMissing) != 1 || av.ToolsMissing[0] != "absent" {
		t.Fatalf("partition wrong: %+v", av)
	}
	if !av.TransportOK {
		t.Fatalf("transport should resolve: %+v", av)
	}

	// A declared vendor lane with NO resolver wired is not-installed, never a silent pass.
	av = Resolve(p, has, nil)
	if av.TransportOK || av.Reason == "" {
		t.Fatalf("nil resolver must fail closed with a reason: %+v", av)
	}

	// A refusing resolver's reason is carried.
	av = Resolve(p, has, func(VendorHint) (bool, string) { return false, "cisco: no execution path" })
	if av.TransportOK || !strings.Contains(av.Reason, "no execution path") {
		t.Fatalf("refusal reason lost: %+v", av)
	}

	// No vendor lane: transport is trivially OK and the resolver is never consulted.
	p.VendorHint = VendorHint{}
	av = Resolve(p, has, func(VendorHint) (bool, string) { t.Fatal("resolver consulted with no lane declared"); return false, "" })
	if !av.TransportOK {
		t.Fatalf("no lane must be OK: %+v", av)
	}
}
