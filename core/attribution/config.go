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

// DispositionFor resolves the disposition for a finding.
//
// THE CONTRADICTION RULE APPLIES ONLY TO AN UNRESOLVED FINDING. Attribute() adjudicates a multi-candidate
// evidence set by PRECEDENCE before it ever gets here, and it leaves the taxonomy at its zero value
// (Unattributable) in exactly one case: the contradiction it could NOT resolve (step 5 — "a non-suspicious,
// NON-TEST contradiction"). Every other taxonomy reaching this function was decided on purpose:
//   - AttributedSuspicious (step 3) — an unsanctioned actor dominates everything, including a co-occurring
//     carve-out or self record (REQ-2304). Already carved out below, and unchanged.
//   - AuthorizedTest (step 4) — a currently-valid carve-out matched the actor AND the host, reached only
//     when no unsanctioned actor is present (REQ-2309).
//   - AttributedAuthorized / AttributedSelf (step 6) — reachable only with EXACTLY ONE candidate, so the
//     contradiction rule could never have applied to them anyway.
//
// So `candidates > 1` on a resolved taxonomy could only ever have hit AuthorizedTest, and it did: measured
// live 2026-07-29, authorized-test is the LARGEST taxonomy on the estate at 848 rows in 7 days, and the
// operator's own rules-as-data mapping declares `authorized-test → ladder-unchanged` (rule id
// "authorized-test-heal"). The hardcoded demotion silently overrode that declared rule and forced a human
// poll on every one — for the harness's sanctioned injector colliding with TG's own actuation identity in
// the same evidence set. Not a security signal; the single largest autonomy drag in the system, and rules-as-
// data that the code does not honour is not rules-as-data.
//
// The fail direction is UNCHANGED: a suspicious actor never reaches step 4, so nothing here can promote a
// hostile reading into authorized-test. An UNMAPPED taxonomy still fails closed — unattributable →
// ladder-unchanged (REQ-2303: the pre-feature ladder, never a forced poll), anything else → escalate
// (REQ-2308).
func (m Mapping) DispositionFor(t Taxonomy, candidates int) Disposition {
	// REQ-2304 dominance, stated explicitly. NOTE: with the contradiction rule now restricted to the
	// unresolved case below, this branch is REDUNDANT — a suspicious taxonomy reaches its mapping either way,
	// and an unmapped one falls to the same escalate at the bottom. I verified that by deleting the whole
	// branch: every oracle stayed green. It is kept anyway, and that is a deliberate choice rather than an
	// oversight: this is the one place a reader checks that a positively-unknown actor cannot be demoted by
	// any later rule, and a future edit to the contradiction logic below would silently make it load-bearing
	// again. Redundant law that is visible beats implicit law that is not.
	if t == AttributedSuspicious {
		if d, ok := m[t]; ok {
			return d // security-escalate, regardless of a co-occurring contradiction (REQ-2304 dominates)
		}
		return DispositionEscalate
	}
	// The UNRESOLVED contradiction (REQ-2310) — and only that one.
	if t == Unattributable && candidates > 1 {
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
	cfg := Config{Sanctioned: map[string][]string{}, SelfActors: map[string]string{}, SelfReaders: map[string][]string{}, SanctionedGroups: map[string][]string{}}
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

	// AN OPERATOR OVERRIDE MUST COVER THE WHOLE CLOSED ENUMERATION, OR IT IS REJECTED.
	//
	// This loop only ever visited the rows PRESENT in the document, and an unmapped taxonomy falls through
	// DispositionFor to `return DispositionEscalate` (config.go, bottom). So a config that simply OMITS a
	// taxonomy silently turned it into a forced human poll — no error, no log line, no diff anyone would read
	// as a behaviour change. Omitting `authorized-test` alone would have reinstated the single largest
	// autonomy drag in the system: it is the biggest taxonomy on this estate (measured live 2026-07-29: 88 of
	// 200 consecutive sessions; 848 rows over 7 days when !687 removed the equivalent hardcoded demotion).
	//
	// Rejecting is a LOUDER version of the same fail-closed direction, not a weaker one. cmd/worker/main.go
	// answers a parse error by falling back to the EMPTY mapping and logging "failing CLOSED to the empty
	// mapping (every non-unattributable attribution escalates)". So an incomplete ruleset still escalates —
	// the difference is that the operator now sees which taxonomy they left out, instead of discovering it as
	// an unexplained halving of autonomy weeks later.
	//
	// The check is over the CLOSED ENUMERATION, listed here once. A taxonomy added to the enum without being
	// added here would not be caught, so the oracle asserts this list against Taxonomy.String() rather than
	// against a second copy of the list.
	var missing []string
	for _, t := range AllTaxonomies() {
		if _, ok := m[t]; !ok {
			missing = append(missing, t.String())
		}
	}
	if len(missing) > 0 {
		return nil, Config{}, fmt.Errorf(
			"actor_attribution: ruleset does not map every taxonomy — missing %s. An unmapped taxonomy "+
				"escalates to a human, so an incomplete ruleset silently withdraws autonomy; declare a "+
				"disposition for each (closed enum, REQ-2308)", strings.Join(missing, ", "))
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
		// ★ BOTH BOUNDS ARE MANDATORY, AND THE WINDOW IS CAPPED. REQ-2309's carve-out is a *temporally
		// bounded* exception to the security path: inside it, a listed actor on a listed host reads
		// authorized-test (heal) instead of attributed-suspicious (security-escalate). Until 2026-07-29 both
		// bounds were OPTIONAL, and an absent bound meant NO bound — so the declared property was simply not
		// in force wherever the operator left the field out.
		//
		// Measured live on this estate the day it was found: BOTH carve-outs in the deployed ruleset omitted
		// valid_until. `shadowbench-pool` (pve, root@pam, 15 pool guests) and `shadowbench-pool-ssh`
		// (journal, the operator's admin SSH key fingerprint, 15 guests) were therefore PERMANENT grants.
		// The consequence is not abstract: on those guests a change made with that key can never resolve to
		// attributed-suspicious, so the security-escalate disposition is structurally unreachable there —
		// which is exactly why attributed-suspicious read 0 for all time. And because this estate's fault
		// harness and its operator hold the SAME key, "the harness broke mealie" and "someone with the admin
		// key broke mealie" were indistinguishable, forever, by construction.
		//
		// A missing bound now FAILS THE LOAD rather than defaulting to forever. The caller's parse-failure
		// path already fails closed (every non-unattributable attribution escalates), so the unsafe
		// direction of this check is a louder alarm, never a silent permission.
		if co.ValidFrom == "" || co.ValidUntil == "" {
			return nil, Config{}, fmt.Errorf(
				"actor_attribution: carve-out %q must declare BOTH valid_from and valid_until (got from=%q "+
					"until=%q): a carve-out is a temporally-bounded exception to the security path (REQ-2309), "+
					"and an absent bound is not a wide bound — it is NO bound, which permanently sanctions the "+
					"listed actors on the listed hosts and makes attributed-suspicious unreachable for them",
				co.ID, co.ValidFrom, co.ValidUntil)
		}
		if out.ValidFrom, err = time.Parse(time.RFC3339, co.ValidFrom); err != nil {
			return nil, Config{}, fmt.Errorf("actor_attribution: carve-out %q: bad valid_from %q: %w", co.ID, co.ValidFrom, err)
		}
		if out.ValidUntil, err = time.Parse(time.RFC3339, co.ValidUntil); err != nil {
			return nil, Config{}, fmt.Errorf("actor_attribution: carve-out %q: bad valid_until %q: %w", co.ID, co.ValidUntil, err)
		}
		if !out.ValidUntil.After(out.ValidFrom) {
			return nil, Config{}, fmt.Errorf(
				"actor_attribution: carve-out %q: valid_until %s is not after valid_from %s — an empty or "+
					"inverted window never matches, so the carve-out is inert and its hosts silently escalate",
				co.ID, out.ValidUntil.Format(time.RFC3339), out.ValidFrom.Format(time.RFC3339))
		}
		// ★ THE CAP IS WHAT GIVES THE BOUND TEETH. Requiring the field without capping its value would be
		// cosmetic: `valid_until: 9999-01-01` re-creates the permanent grant while satisfying the schema.
		// Renewal has to be a deliberate, recurring act, which is the whole point of a bounded exception.
		if d := out.ValidUntil.Sub(out.ValidFrom); d > MaxCarveOutWindow {
			return nil, Config{}, fmt.Errorf(
				"actor_attribution: carve-out %q spans %.0f days, over the %.0f-day maximum: a carve-out "+
					"suspends the security path for its actors, so it must be renewed deliberately rather "+
					"than declared once and forgotten (REQ-2309)",
				co.ID, d.Hours()/24, MaxCarveOutWindow.Hours()/24)
		}
		cfg.CarveOuts = append(cfg.CarveOuts, out)
	}
	return m, cfg, nil
}
