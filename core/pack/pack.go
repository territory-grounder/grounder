// Package pack defines the typed platform-pack descriptor (TG-80 P2-5 / TG-81 borrow 5): a compiled,
// validated bundle that names — never bodies — the competences a platform domain gets: which read-only
// tools compose, which skills stay in scope, how a vendor transport is reached, and what safety posture
// the platform demands. Clean-room reimplementation of h-apache-stack's per-request policy/role packs and
// h-network's platform-pack adapter pattern (attribution: SOURCE-BENCHMARK-CATALOG R7/R19); nothing is
// vendored.
//
// The discipline mirrors modules/desc: a pack is DATA over closed vocabularies, validated once at catalog
// load, and every referenced capability resolves against a live registry at request time (Resolve) — a
// pack can therefore degrade to fewer reads but can never smuggle a capability into existence. Selection
// is a pure function of typed alert facts (For), the same INV-08 shape as agent/skills.DomainOf: no model
// token ever chooses a pack.
//
// Posture composes in the safe direction ONLY: TierHint escalates and never demotes (EscalateTier), and
// BandOverlay rides the ONE band-floor seam (core/risk.GatedInput.BandFloor) through
// proposal.ComposeFloor, so a pack can raise the approval bar and structurally cannot lower it.
package pack

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/territory-grounder/grounder/core/safety"
)

// Transport is the closed vocabulary for how a pack's platform is reached. Unknown values fail Validate —
// fail closed, exactly as skillstore refuses an unknown artifact class.
const (
	// TransportNone: the pack declares no vendor lane; its tools ride whatever transport they were
	// registered with. The zero value, and the only value that carries no ConfigMode obligation.
	TransportNone = ""
	// TransportSSHArgv: the sealed-argv SSH effect lane (modules/actuation/ssh).
	TransportSSHArgv = "ssh-argv"
	// TransportCiscoInteractive: the interactive prompt-driven SSH transport (modules/actuation/cisco).
	TransportCiscoInteractive = "cisco-interactive"
	// TransportHTTPSAPI: a read-only HTTPS API client (librenms/netbox shape).
	TransportHTTPSAPI = "https-api"
)

// ConfigModeReadOnly is the only configuration mode a pack may declare. The enum is closed at ONE value
// on purpose: "read-only" is a floor the type enforces, not a default an author may widen. A future write
// mode is a law change (spec + owner ratification), not a new string.
const ConfigModeReadOnly = "read-only"

// VendorHint carries the platform's transport selection as DATA — the cisco Device shape (prompt profile,
// pager-off command family, transport kind) generalized so the composition root, not the pack, decides
// which concrete client serves it. A zero VendorHint means "no vendor lane".
type VendorHint struct {
	// Transport is one of the Transport* constants above.
	Transport string
	// PromptProfile names the device dialect for an interactive transport ("ios", "asa", "pve").
	PromptProfile string
	// CommandTemplate names the read-command family the transport renders (a slug, resolved by the
	// transport package — never a raw command line; a pack carries no argv).
	CommandTemplate string
	// ConfigMode must be ConfigModeReadOnly whenever Transport is set.
	ConfigMode string
}

// zero reports whether no vendor lane is declared.
func (v VendorHint) zero() bool {
	return v.Transport == "" && v.PromptProfile == "" && v.CommandTemplate == "" && v.ConfigMode == ""
}

// BandOverlay declares the platform's safety posture as a band FLOOR for the risk classifier's one
// composition seam. Applies exists because safety.Band's zero value is BandPollPause — the MOST
// restrictive band — so a bare `Floor:` literal without Applies would either clamp the whole estate
// (if trusted) or silently do nothing (if dropped). Validate refuses that ambiguity outright: a floor
// without Applies, or Applies without a Reason, is a malformed pack, not a quiet default.
type BandOverlay struct {
	Floor   safety.Band
	Applies bool
	// Reason is recorded on the audit row beside the composed floor (BandFloorReason), so a polled
	// operator sees WHICH pack raised the bar.
	Reason string
}

// Pack is one platform pack. Every slice field NAMES an artifact owned elsewhere: Tools are registered
// read-only tool names (agent.ToolSet), Skills are skill names (agent/skills), and the pack composes
// nothing that is not already registered — Resolve reports what is missing rather than inventing it.
type Pack struct {
	// ID is the stable selector key and ledger token ("pack:<id>@<version>").
	ID string
	// Title and Summary render in operator surfaces; neither reaches a prompt.
	Title   string
	Summary string
	// Version rides the ledger token so a judged session binds to the exact pack revision it ran.
	Version string
	// Domains selects the pack — STRICT, the skillstore semantic: a pack loads ONLY on a listed domain,
	// and the unknown domain ("") matches none. At least one domain is required: a pack that could apply
	// everywhere is a base prompt, not a platform pack.
	Domains []string
	// VendorHint optionally names the platform transport lane (data only).
	VendorHint VendorHint
	// Tools is the pack's read-tool allowlist: the session's ToolSet is subset to exactly these names
	// (agent.ToolSet.SubsetFor). Empty = no tool scoping (the full registered read-only set).
	Tools []string
	// Skills is the pack's skill allowlist: the composed skill set is filtered to these names. Empty =
	// no skill scoping. A name here is a FILTER over what AppliesWhen already composed — never a second
	// selection authority and never a body.
	Skills []string
	// TierHint optionally escalates the investigate model tier ("primary"). Escalate-only: EscalateTier
	// can raise fast→primary and can never demote — the compiled tier branches stay the floor (MECH-402).
	TierHint string
	// Band optionally declares the platform's band floor (raise-only, see BandOverlay).
	Band BandOverlay
}

var idRE = regexp.MustCompile(`^[a-z][a-z0-9-]{1,63}$`)

// Validate refuses a malformed pack. Called once for the whole catalog by All — a malformed pack fails
// the test suite once rather than composing wrongly forever (the modules/catalog discipline).
func (p Pack) Validate() error {
	if !idRE.MatchString(p.ID) {
		return fmt.Errorf("pack: ID %q must be a lowercase slug (%s)", p.ID, idRE)
	}
	if strings.TrimSpace(p.Title) == "" {
		return fmt.Errorf("pack %s: Title is required", p.ID)
	}
	if p.Version == "" || strings.ContainsAny(p.Version, " \t\n@#:") {
		return fmt.Errorf("pack %s: Version %q must be non-empty and free of ledger-token separators", p.ID, p.Version)
	}
	if len(p.Domains) == 0 {
		return fmt.Errorf("pack %s: at least one domain is required — a pack with no domain either never loads (strict semantics) or would have to load everywhere; both are authoring errors", p.ID)
	}
	for _, d := range p.Domains {
		if strings.TrimSpace(d) == "" {
			return fmt.Errorf("pack %s: a blank domain entry would silently never match (strict semantics hide it); remove it", p.ID)
		}
	}
	switch p.VendorHint.Transport {
	case TransportNone, TransportSSHArgv, TransportCiscoInteractive, TransportHTTPSAPI:
	default:
		return fmt.Errorf("pack %s: unknown transport %q — the vocabulary is closed and unknown fails closed", p.ID, p.VendorHint.Transport)
	}
	if p.VendorHint.Transport == TransportNone && !p.VendorHint.zero() {
		return fmt.Errorf("pack %s: a vendor hint without a transport reaches no code — declare the transport or clear the hint", p.ID)
	}
	if p.VendorHint.Transport != TransportNone && p.VendorHint.ConfigMode != ConfigModeReadOnly {
		return fmt.Errorf("pack %s: ConfigMode must be %q — a pack cannot widen the configuration mode", p.ID, ConfigModeReadOnly)
	}
	for _, t := range p.Tools {
		if strings.TrimSpace(t) == "" || strings.ContainsAny(t, " \t\n") {
			return fmt.Errorf("pack %s: tool name %q must be a bare registered name", p.ID, t)
		}
	}
	for _, s := range p.Skills {
		if strings.TrimSpace(s) == "" || strings.ContainsAny(s, " \t\n") {
			return fmt.Errorf("pack %s: skill name %q must be a bare registered name", p.ID, s)
		}
	}
	switch p.TierHint {
	case "", tierPrimary:
	default:
		return fmt.Errorf("pack %s: TierHint %q — only %q (or empty) is meaningful; a hint that could demote is refused at authoring time, not silently ignored", p.ID, p.TierHint, tierPrimary)
	}
	if !p.Band.Applies && (p.Band.Floor != 0 || p.Band.Reason != "") {
		return fmt.Errorf("pack %s: a band floor without Applies is the zero-value trap (core/risk/input.go) — it would either clamp the estate or silently vanish; set Applies with a Reason, or clear the overlay", p.ID)
	}
	if p.Band.Applies && strings.TrimSpace(p.Band.Reason) == "" {
		return fmt.Errorf("pack %s: an applied band floor needs a Reason — it is recorded on the audit row a polled operator adjudicates", p.ID)
	}
	return nil
}

// The two abstract investigate tiers EscalateTier reasons about. Deliberately NOT exported: callers pass
// what investigateTierFor resolved (which may be an operator alias); only the exact abstract names
// participate in escalation.
const (
	tierFast    = "fast"
	tierPrimary = "primary"
)

// EscalateTier composes a pack's tier hint OVER the compiled tier floor in the safe direction only: a
// "primary" hint raises the abstract "fast" tier to "primary"; every other combination — no hint, an
// already-primary base, an operator-aliased base name, an unknown hint — returns the base unchanged. The
// compiled branches in investigateTierFor stay the floor (MECH-402): a pack can buy MORE reasoning for
// its platform, never less.
func EscalateTier(base, hint string) string {
	if hint == tierPrimary && base == tierFast {
		return tierPrimary
	}
	return base
}

// LedgerToken renders the per-session provenance entry that rides the skill_load record
// ("pack:<id>@<version>") — the deterministic-heal precedent for a non-skill token on that lane. The
// "pack:" prefix carries no "#id", so judge.StoreVersionIDs ignores it by construction.
func (p Pack) LedgerToken() string { return "pack:" + p.ID + "@" + p.Version }
