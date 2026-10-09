package cisco

// The READ-ONLY SHOW-COMMAND CATALOG (TG-85 component 3): a CLOSED, named set of diagnostic commands this
// transport may run, replacing "any word that starts with `show`".
//
// WHY A CATALOG AND NOT JUST A VERB. The slice-1 guard admits a command whose first token is `show` and whose
// tokens carry no mutating word. That is a sound floor, but `show` is not one capability — it is a family, and
// some of its members READ SECRETS. On ASA/IOS the running-config contains IPsec pre-shared keys, SNMP
// community strings, RADIUS/TACACS keys and local password hashes; `show crypto ikev1 pre-shared-key` prints
// them outright. Those outputs would land in TG's transcript, its logs, and — once a read tool exists — the
// model's context. Nothing in the mutating-verb guard stops that, because reading a secret is not a mutation.
//
// So the catalog is TWO controls in one:
//   1. a positive allowlist — only a catalogued command runs, so a novel `show` cannot be improvised; and
//   2. a NEGATIVE floor (credentialBearing) that refuses the credential-revealing members BY NAME, so a
//      future catalog entry cannot reintroduce one by accident. The floor is checked even for an entry that
//      is in the catalog: a positive list is a decision someone made once, and this is the rule that
//      outlives it.
//
// The catalog is DATA, not code: an entry names its command and the platform it belongs to. Nothing rendered
// from it becomes control flow (INV-08) — the transport still sends only the exact argv an entry declares.
//
// This ships DARK and BEHAVIOUR-PRESERVING: the read Module admits its usual verb-guarded commands until an
// operator wiring passes WithCatalog. The agent-facing read TOOL that would surface these to the model is a
// separate slice — registering a tool changes the model's preamble, which is an eval-visible surface.

import (
	"fmt"
	"sort"
	"strings"
)

// Platform distinguishes the two CLI dialects this transport speaks. A command that exists on both carries
// PlatformAny; the divergent ones (ASA's `show conn` vs IOS's `show ip route`) are declared per-platform so a
// wiring cannot offer an ASA-only command to an IOS device.
type Platform string

const (
	PlatformAny Platform = "any"
	PlatformIOS Platform = "ios"
	PlatformASA Platform = "asa"
)

// ShowCommand is one catalogued read-only diagnostic.
type ShowCommand struct {
	// Name is the stable slug an operator config (and, later, a tool) refers to.
	Name string
	// Argv is the EXACT fixed token vector sent to the device. No token is ever built from caller input:
	// a command that needs a subject (an interface, a peer) is a distinct catalog entry with its own
	// Params, never a format string (INV-02).
	Argv []string
	// Params names the ordered free parameters appended after Argv, if any. Each is validated by the
	// module's argument guard before it is appended; an entry with no Params takes no caller input at all.
	Params []string
	// Platform restricts the entry to one dialect (PlatformAny = both).
	Platform Platform
	// Why states what the command is FOR, so a catalog review reads as diagnosis rather than trivia.
	Why string
}

// DefaultCatalog is the shipped read-only diagnostic set: the ladder a network triage actually walks —
// reachability, interface and routing state, the ACL/NAT path, the IPsec/VPN chain, and the box's own health.
// Deliberately EXCLUDES every configuration dump (see credentialBearing): a config dump is not a diagnostic,
// it is a credential store, and the runbooks that need "what is configured" ask the IaC repo, which is the
// declared source of truth for device config.
func DefaultCatalog() []ShowCommand {
	return []ShowCommand{
		{Name: "version", Argv: []string{"show", "version"}, Platform: PlatformAny,
			Why: "platform, image and uptime — establishes what the box IS before anything else is believed"},
		{Name: "interfaces", Argv: []string{"show", "interface"}, Platform: PlatformAny,
			Why: "per-interface state, errors and drops — the first stop for a link-layer symptom"},
		{Name: "interface-brief", Argv: []string{"show", "ip", "interface", "brief"}, Platform: PlatformIOS,
			Why: "one-line up/down per interface — the fastest read of which link died"},
		{Name: "interface-detail", Argv: []string{"show", "interface"}, Params: []string{"interface"}, Platform: PlatformAny,
			Why: "one named interface in detail, when the brief view has narrowed it"},
		{Name: "routes", Argv: []string{"show", "ip", "route"}, Platform: PlatformIOS,
			Why: "the routing table — whether the path a service needs still exists"},
		{Name: "route-for", Argv: []string{"show", "route"}, Params: []string{"address"}, Platform: PlatformASA,
			Why: "the selected route for one destination, for a reachability claim about a specific host"},
		{Name: "bgp-summary", Argv: []string{"show", "bgp", "summary"}, Platform: PlatformAny,
			Why: "BGP neighbour states — a flapping or idle peer explains a partition"},
		{Name: "access-lists", Argv: []string{"show", "access-list"}, Platform: PlatformAny,
			Why: "ACL entries WITH hit counters — the counter is the evidence a rule is the one blocking"},
		{Name: "nat", Argv: []string{"show", "nat"}, Platform: PlatformASA,
			Why: "NAT rules and their hits — a translation ordering fault presents as a one-way outage"},
		{Name: "xlate", Argv: []string{"show", "xlate"}, Platform: PlatformASA,
			Why: "live translation table — whether the flow ever translated"},
		{Name: "connections", Argv: []string{"show", "conn"}, Platform: PlatformASA,
			Why: "live connection table — whether the flow exists at all, before blaming the far end"},
		{Name: "ikev2-sa", Argv: []string{"show", "crypto", "ikev2", "sa"}, Platform: PlatformAny,
			Why: "IKEv2 security associations — the tunnel-down ladder starts here"},
		{Name: "ipsec-sa", Argv: []string{"show", "crypto", "ipsec", "sa"}, Platform: PlatformAny,
			Why: "IPsec SAs with packet counters — distinguishes 'no tunnel' from 'tunnel, no traffic'"},
		{Name: "vpn-sessions", Argv: []string{"show", "vpn-sessiondb"}, Platform: PlatformASA,
			Why: "active VPN sessions — peer presence without touching the config"},
		{Name: "logging", Argv: []string{"show", "logging"}, Platform: PlatformAny,
			Why: "the device's own recent log buffer — its account of what happened, in its words"},
		{Name: "cpu", Argv: []string{"show", "processes", "cpu"}, Platform: PlatformAny,
			Why: "CPU by process — a control-plane starve looks like a network fault from outside"},
		{Name: "memory", Argv: []string{"show", "memory"}, Platform: PlatformAny,
			Why: "memory headroom — exhaustion presents as unexplained drops"},
	}
}

// THE CREDENTIAL FLOOR — the negative half of the catalog, checked against every admitted command including
// catalogued ones, because a positive allowlist records a decision someone made once and this rule has to
// outlive it.
//
// IT IS SCOPE-AWARE, NOT A BLANKET BAN, and that distinction is the whole design. `show running-config` with
// no section dumps the box's credential store — IPsec pre-shared keys, SNMP communities, RADIUS/TACACS keys,
// local password hashes. But `show running-config interface Gi0/1` is a genuine diagnostic ("what is actually
// configured on the link that died") carrying none of that, and the read slice's own oracle pins it as
// admitted. Refusing the family wholesale would delete a real capability to close a hole that only some of
// its members have. So the floor refuses a config dump that is UNSCOPED, or scoped to a section that IS the
// secret, and admits the rest.
var (
	// alwaysSecret are token sequences that reveal credentials regardless of scoping.
	alwaysSecret = [][]string{
		{"pre-shared-key"},
		{"password"},
		{"username"},
		{"tech-support"}, // bundles the whole running-config
		{"more"},         // ASA's file-read back door onto the config
		{"key", "chain"},
		{"snmp-server", "community"},
	}
	// configDump names the commands whose UNSCOPED form is a credential store.
	configDump = map[string]bool{"running-config": true, "startup-config": true}
	// secretSections are running-config sections that ARE the secret material. `all` is here because it
	// re-widens a scoped dump back to everything.
	secretSections = map[string]bool{
		"all": true, "crypto": true, "aaa-server": true, "aaa": true, "snmp-server": true,
		"username": true, "tunnel-group": true, "radius": true, "tacacs+": true, "key": true,
	}
)

// RefuseCredentialBearing reports a refusal reason if argv reads a secret, or "" if it does not. Exported so a
// wiring slice and its oracles can assert the floor directly, independent of the catalog.
func RefuseCredentialBearing(argv []string) string {
	lower := make([]string, len(argv))
	for i, a := range argv {
		lower[i] = strings.ToLower(strings.TrimSpace(a))
	}
	for _, seq := range alwaysSecret {
		if containsSeq(lower, seq) {
			return fmt.Sprintf("%q reads device credentials (pre-shared keys, community strings, password hashes) — refused: reading a secret is not a mutation, so the mutating-verb guard does not cover it", strings.Join(seq, " "))
		}
	}
	for i, tok := range lower {
		if !configDump[tok] {
			continue
		}
		// UNSCOPED: the dump IS the credential store.
		if i == len(lower)-1 {
			return fmt.Sprintf("%q with no section dumps the whole configuration, which carries pre-shared keys, community strings and password hashes — refused; ask for a specific section (e.g. `%s interface <if>`), or read the IaC repo, which is the declared source of truth for device config", tok, tok)
		}
		// SCOPED to a section that is itself the secret.
		if sec := lower[i+1]; secretSections[sec] {
			return fmt.Sprintf("%q section %q is credential material — refused", tok, sec)
		}
	}
	return ""
}

// containsSeq reports whether tokens contains seq as a contiguous run.
func containsSeq(tokens, seq []string) bool {
	if len(seq) == 0 || len(tokens) < len(seq) {
		return false
	}
	for i := 0; i+len(seq) <= len(tokens); i++ {
		match := true
		for j := range seq {
			if tokens[i+j] != seq[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// Catalog is a validated, indexed view of a command set for one platform.
type Catalog struct {
	byName map[string]ShowCommand
	plat   Platform
}

// NewCatalog validates a command set for a platform and indexes it by name. It FAILS CLOSED on a set that
// could not be honoured: an empty set (a catalog that admits nothing is a wiring error, not a policy), a
// duplicate name, an entry with no argv, an entry whose first token is not an admitted read verb, or an entry
// that trips the credential floor — the last so a catalog can never SHIP a secret-reading command, however it
// was reviewed.
func NewCatalog(cmds []ShowCommand, plat Platform) (*Catalog, error) {
	if len(cmds) == 0 {
		return nil, fmt.Errorf("cisco catalog: empty command set — a catalog that admits nothing is a wiring error (fail closed)")
	}
	byName := make(map[string]ShowCommand, len(cmds))
	for i, c := range cmds {
		name := strings.ToLower(strings.TrimSpace(c.Name))
		if name == "" {
			return nil, fmt.Errorf("cisco catalog: entry %d has no name", i)
		}
		if _, dup := byName[name]; dup {
			return nil, fmt.Errorf("cisco catalog: duplicate entry name %q", name)
		}
		if len(c.Argv) == 0 {
			return nil, fmt.Errorf("cisco catalog: entry %q has no argv", name)
		}
		if !readOnlyVerbs[strings.ToLower(c.Argv[0])] {
			return nil, fmt.Errorf("cisco catalog: entry %q starts with %q, which is not a read-only verb", name, c.Argv[0])
		}
		if why := RefuseCredentialBearing(c.Argv); why != "" {
			return nil, fmt.Errorf("cisco catalog: entry %q is credential-bearing and may not be catalogued: %s", name, why)
		}
		if c.Platform != PlatformAny && plat != PlatformAny && c.Platform != plat {
			continue // an entry for the other dialect is skipped, not an error — one catalog per device
		}
		byName[name] = c
	}
	if len(byName) == 0 {
		return nil, fmt.Errorf("cisco catalog: no entry applies to platform %q — the catalog would admit nothing (fail closed)", plat)
	}
	return &Catalog{byName: byName, plat: plat}, nil
}

// Names returns the catalogued entry names, sorted — for an operator listing and for stable errors.
func (c *Catalog) Names() []string {
	out := make([]string, 0, len(c.byName))
	for n := range c.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Lookup returns the entry for a name.
func (c *Catalog) Lookup(name string) (ShowCommand, bool) {
	e, ok := c.byName[strings.ToLower(strings.TrimSpace(name))]
	return e, ok
}

// Admits reports whether an already-built argv corresponds to a catalogued entry (its Argv prefix), and
// returns a refusal reason otherwise. This is the check a wiring slice installs on the read Module: with a
// catalog present, an improvised `show` that the verb guard would have allowed is refused by name.
func (c *Catalog) Admits(argv []string) error {
	if why := RefuseCredentialBearing(argv); why != "" {
		return fmt.Errorf("cisco catalog: %s", why)
	}
	for _, e := range c.byName {
		if len(argv) < len(e.Argv) {
			continue
		}
		match := true
		for i := range e.Argv {
			if !strings.EqualFold(strings.TrimSpace(argv[i]), e.Argv[i]) {
				match = false
				break
			}
		}
		// An entry with no Params admits ONLY its exact argv; a param-bearing entry admits AT MOST its
		// declared parameter count beyond it (bounded 2026-08-25: the unbounded form accepted any longer
		// vector for a param entry — unreachable through the read tool, which appends at most one
		// validated token, but Admits is the WRITE lane's guard too and a guard's bound must not depend
		// on its callers' manners).
		if match && len(argv) <= len(e.Argv)+len(e.Params) {
			return nil
		}
	}
	return fmt.Errorf("cisco catalog: %q is not a catalogued diagnostic — admitted: %v", strings.Join(argv, " "), c.Names())
}
