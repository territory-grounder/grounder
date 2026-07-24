package attribution

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Disposition is the CLOSED taxonomy→action enumeration (REQ-2308). The ZERO VALUE is escalate-to-human
// — the fail-closed default everywhere else in TG (Band zero = POLL_PAUSE): an unmapped or unloadable
// disposition resolves to escalate, never to a permissive fallback.
type Disposition int

const (
	// DispositionEscalate is the zero value: route to the approver graph (POLL_PAUSE). Also the
	// resolution for a mapping that is absent, corrupt, or fails validation (REQ-2308) and for a
	// non-suspicious contradiction (REQ-2310).
	DispositionEscalate Disposition = iota
	// LadderUnchanged: the heal ladder proceeds exactly as without this capability (unattributable's
	// disposition; authorized-test's disposition — a manufactured learning fault heals).
	LadderUnchanged
	// StandDownCoordinate: do not propose or execute an actuation that reverses the change; coordinate
	// with the actor via the approver graph (attributed-authorized, REQ-2301).
	StandDownCoordinate
	// SelfNoop: terminate already-remediated without re-actuation (attributed-self, REQ-2302).
	SelfNoop
	// SecurityEscalate: POLL_PAUSE with the security_escalation signal, routed to the security channel
	// in addition to the approver graph (attributed-suspicious, REQ-2304).
	SecurityEscalate
)

// String renders the disposition in its canonical wire form (validated at load, REQ-2308).
func (d Disposition) String() string {
	switch d {
	case LadderUnchanged:
		return "ladder-unchanged"
	case StandDownCoordinate:
		return "stand-down-coordinate"
	case SelfNoop:
		return "self-noop"
	case SecurityEscalate:
		return "security-escalate"
	default:
		return "escalate-to-human"
	}
}

// dispositionFromString parses a disposition in its canonical form, rejecting anything outside the
// closed enum (REQ-2308 — an unknown disposition fails validation, never silently maps).
func dispositionFromString(s string) (Disposition, bool) {
	switch strings.TrimSpace(s) {
	case "ladder-unchanged":
		return LadderUnchanged, true
	case "stand-down-coordinate":
		return StandDownCoordinate, true
	case "self-noop":
		return SelfNoop, true
	case "security-escalate":
		return SecurityEscalate, true
	case "escalate-to-human":
		return DispositionEscalate, true
	}
	return DispositionEscalate, false
}

// taxonomyFromString parses a taxonomy in its canonical wire form.
func taxonomyFromString(s string) (Taxonomy, bool) {
	switch strings.TrimSpace(s) {
	case "attributed-authorized":
		return AttributedAuthorized, true
	case "attributed-self":
		return AttributedSelf, true
	case "attributed-suspicious":
		return AttributedSuspicious, true
	case "authorized-test":
		return AuthorizedTest, true
	case "unattributable":
		return Unattributable, true
	}
	return Unattributable, false
}

// Mapping is the taxonomy→disposition rules-as-data (REQ-2308), loadable and validated at load time.
type Mapping map[Taxonomy]Disposition

// DispositionFor resolves the disposition for a finding. A resolved suspicious taxonomy ALWAYS routes to
// its mapped disposition (security-escalate) — REQ-2304 dominates the generic contradiction demotion, so
// a suspicious reading that ALSO carried a contradiction (candidates > 1) never loses its security signal.
// A NON-suspicious contradiction (candidates > 1) escalates to a human (REQ-2310); an UNMAPPED taxonomy
// fails closed — unattributable → ladder-unchanged, anything else → escalate (REQ-2308).
func (m Mapping) DispositionFor(t Taxonomy, candidates int) Disposition {
	if t == AttributedSuspicious {
		if d, ok := m[t]; ok {
			return d // security-escalate, regardless of a co-occurring contradiction (REQ-2304 dominates)
		}
		return DispositionEscalate
	}
	if candidates > 1 {
		return DispositionEscalate
	}
	if d, ok := m[t]; ok {
		return d
	}
	if t == Unattributable {
		return LadderUnchanged
	}
	return DispositionEscalate
}

// configDocument is the actor_attribution section shape in the versioned ruleset store (REQ-2308/2309).
type configDocument struct {
	ActorAttribution struct {
		Mapping []struct {
			ID          string `json:"id"`
			Taxonomy    string `json:"taxonomy"`
			Disposition string `json:"disposition"`
		} `json:"mapping"`
		SanctionedPrincipals []struct {
			ID     string   `json:"id"`
			Domain string   `json:"domain"`
			Actors []string `json:"actors"`
		} `json:"sanctioned_principals"`
		SanctionedGroups []struct {
			ID     string   `json:"id"`
			Domain string   `json:"domain"`
			Groups []string `json:"groups"`
		} `json:"sanctioned_groups"`
		CarveOuts []struct {
			ID         string   `json:"id"`
			Domain     string   `json:"domain"`
			Actors     []string `json:"actors"`
			Hosts      []string `json:"hosts"`
			ValidFrom  string   `json:"valid_from"`
			ValidUntil string   `json:"valid_until"`
		} `json:"carve_outs"`
	} `json:"actor_attribution"`
}

// ParseConfig parses + validates an actor_attribution ruleset document into the typed Mapping and the
// attributor's Config. Validation is FAIL-CLOSED (REQ-2308): an unknown taxonomy or disposition, or a
// malformed carve-out time bound, is an error — the caller resolves the whole mapping to escalate rather
// than run on a partial read. The self-identity actors are NOT parsed here (they come from the credential
// engine's configuration, spec/016 — never a ruleset string).
func ParseConfig(document []byte) (Mapping, Config, error) {
	var doc configDocument
	if err := json.Unmarshal(document, &doc); err != nil {
		return nil, Config{}, fmt.Errorf("actor_attribution: document is not valid JSON: %w", err)
	}
	m := Mapping{}
	cfg := Config{Sanctioned: map[string][]string{}, SelfActors: map[string]string{}, SanctionedGroups: map[string][]string{}}
	for _, row := range doc.ActorAttribution.Mapping {
		t, ok := taxonomyFromString(row.Taxonomy)
		if !ok {
			return nil, Config{}, fmt.Errorf("actor_attribution: mapping rule %q: unknown taxonomy %q (closed enum, REQ-2308)", row.ID, row.Taxonomy)
		}
		d, ok := dispositionFromString(row.Disposition)
		if !ok {
			return nil, Config{}, fmt.Errorf("actor_attribution: mapping rule %q: unknown disposition %q (closed enum, REQ-2308)", row.ID, row.Disposition)
		}
		m[t] = d
	}
	for _, sp := range doc.ActorAttribution.SanctionedPrincipals {
		cfg.Sanctioned[sp.Domain] = append(cfg.Sanctioned[sp.Domain], sp.Actors...)
	}
	for _, sg := range doc.ActorAttribution.SanctionedGroups {
		cfg.SanctionedGroups[sg.Domain] = append(cfg.SanctionedGroups[sg.Domain], sg.Groups...)
	}
	for _, co := range doc.ActorAttribution.CarveOuts {
		out := CarveOut{ID: co.ID, Domain: co.Domain, Actors: co.Actors, Hosts: co.Hosts}
		var err error
		if co.ValidFrom != "" {
			if out.ValidFrom, err = time.Parse(time.RFC3339, co.ValidFrom); err != nil {
				return nil, Config{}, fmt.Errorf("actor_attribution: carve-out %q: bad valid_from %q: %w", co.ID, co.ValidFrom, err)
			}
		}
		if co.ValidUntil != "" {
			if out.ValidUntil, err = time.Parse(time.RFC3339, co.ValidUntil); err != nil {
				return nil, Config{}, fmt.Errorf("actor_attribution: carve-out %q: bad valid_until %q: %w", co.ID, co.ValidUntil, err)
			}
		}
		cfg.CarveOuts = append(cfg.CarveOuts, out)
	}
	return m, cfg, nil
}
