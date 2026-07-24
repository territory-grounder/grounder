// Package attribution is the deterministic author of the actor-attribution dimension (spec/023 — "WHO is
// the actor behind the observed change?"). Before TG proposes or actuates anything it already asks what
// changed, how risky the fix is, and whether it has earned the right to act; this package answers the
// missing question — WHO made the observed change — from typed, reader-captured evidence records, NEVER
// from model narrative (INV-11, REQ-2312). The taxonomy can only stand down, no-op, or escalate a
// session; it can never raise autonomy (REQ-2305).
//
// Provenance: [F] owner epic (actor-attribution grounding) · [O] INV-04/INV-08/INV-09/INV-11/INV-19.
package attribution

import (
	"sort"
	"strings"
	"time"
)

// Taxonomy is the closed actor-attribution enumeration (REQ-2300). The ZERO VALUE is Unattributable by
// deliberate design (the spec/023 zero-value note): absent or failed evidence maps to the pre-feature
// ladder, NOT to suspicion (REQ-2303 — evidence-gated honesty), and REQ-2305 guarantees no taxonomy
// value can grant autonomy, so the least-restrictive value equals the already-governed baseline.
type Taxonomy int

const (
	// Unattributable is the zero value: no admissible actor evidence exists for the subject (including
	// when no reader covers its domain). The classification and heal ladder proceed EXACTLY as they
	// would without this capability (REQ-2303) — this is NOT a suspicious reading.
	Unattributable Taxonomy = iota
	// AttributedAuthorized: the fault-shaped change is attributed to a sanctioned principal that is not
	// the platform's own actuation identity, and no carve-out matches. The session stands down to the
	// approver graph — coordinate with the actor, never undo an intentional change (REQ-2301).
	AttributedAuthorized
	// AttributedSelf: the platform's own actuation identity already remediated this (target, fault
	// class) inside the self-recognition window. The session terminates already-remediated — no
	// re-actuation (REQ-2302).
	AttributedSelf
	// AttributedSuspicious: positive evidence of an unsanctioned actor. (REQ-2304's second half — a
	// covered audit trail with NO entry for an observed mutation — is a Phase-2 disposition that needs
	// the reader to report coverage-with-no-entry; Phase 1 does not synthesize it.) A suspicious reading
	// DOMINATES a co-occurring carve-out or contradiction: escalate, never auto-heal (REQ-2304).
	AttributedSuspicious
	// AuthorizedTest: a currently-valid sanctioned-pool carve-out matched the attributed actor and
	// target host — a manufactured learning fault on an allowlisted pool host. The heal ladder proceeds
	// unchanged and the attribution is recorded honestly (REQ-2309).
	AuthorizedTest
)

// String renders the taxonomy in the canonical wire/ledger form.
func (t Taxonomy) String() string {
	switch t {
	case AttributedAuthorized:
		return "attributed-authorized"
	case AttributedSelf:
		return "attributed-self"
	case AttributedSuspicious:
		return "attributed-suspicious"
	case AuthorizedTest:
		return "authorized-test"
	default:
		return "unattributable"
	}
}

// Evidence is one reader-captured actor-evidence record (REQ-2306/REQ-2312): typed, timestamped,
// target-named, and carrying its domain-native reference. The model never sees raw log lines — only
// these minimized fields (REQ-2313).
type Evidence struct {
	Domain     string    `json:"domain"`      // "pve" | "journal" | "k8s-audit" | "netbox" | "gitops-mr" | "awx" | "docker"
	Actor      string    `json:"actor"`       // principal as the domain records it, e.g. "root@pam!tg-actuate"
	ActionKind string    `json:"action_kind"` // domain verb, e.g. "vzstop", "vzstart", "sudo", "MR-merged"
	Target     string    `json:"target"`      // the investigated subject the record names
	ObservedAt time.Time `json:"observed_at"` // domain timestamp (window-checked, REQ-2312)
	Ref        string    `json:"ref"`         // domain-native id (UPID, journal cursor, audit-event id, changelog id)
	Covered    bool      `json:"covered"`     // this reader AFFIRMATIVELY covers the target's audit trail (REQ-2304 half 2)
}

// Finding is the attributor's required-field output (the INV-19 pattern): the resolved taxonomy, the
// matched mapping/carve-out rule id where one matched ("" = the built-in default path), every candidate
// taxonomy the evidence supported (>1 ⇒ a contradiction escalated per REQ-2310), the admissible
// evidence, and any reader warnings (REQ-2307 — recorded, never fatal).
type Finding struct {
	Taxonomy   Taxonomy
	RuleID     string
	Candidates []Taxonomy
	Evidence   []Evidence
	Warnings   []string
}

// Config is the deterministic input the attributor derives over — the loadable rules-as-data (the
// taxonomy→disposition mapping, sanctioned principals, and the temporally-bounded carve-outs) plus the
// platform's own actuation identity per domain and the attribution window. None of it is a compiled
// constant: it is parsed and validated at load time (REQ-2308) and handed in here as typed config.
type Config struct {
	// SelfActors maps a domain to the platform's own actuation identity as that domain records it
	// (e.g. "pve" → "root@pam!tg-actuate"). Resolved from the credential engine's configuration
	// (spec/016), never a hardcoded token string, so self-recognition survives a token rotation.
	SelfActors map[string]string
	// Sanctioned maps a domain to the sanctioned non-TG principals for that domain (e.g. "pve" →
	// ["root@pam"]). A change attributed to one is AttributedAuthorized unless a carve-out matches.
	Sanctioned map[string][]string
	// SanctionedGroups maps a domain to the directory admin GROUP names whose enabled, non-service members
	// the identity/auth enrichment may PROMOTE to sanctioned for a session (REQ-2317, loadable rules-as-data).
	// It is consumed only by the AttributeActivity enrichment fold, never by the pure derivation below —
	// the deterministic core reads only Sanctioned, so this field cannot alter Attribute()'s behavior.
	SanctionedGroups map[string][]string
	// CarveOuts are the temporally-bounded sanctioned-pool rules (REQ-2309, INV-20-shaped). An expired,
	// future, or invalid row NEVER matches.
	CarveOuts []CarveOut
	// Window is the attribution lookback: only evidence observed within [now-Window, now] is admissible
	// (REQ-2312). Zero means the caller supplies an explicit bound per call.
	Window time.Duration
	// Now supplies the clock (injectable for deterministic tests); nil ⇒ time.Now.
	Now func() time.Time
}

// CarveOut is one temporally-bounded sanctioned-pool rule (REQ-2309): a manufactured fault by a listed
// actor on an allowlisted pool host inside [ValidFrom, ValidUntil] resolves to AuthorizedTest — the
// learning regime's purpose — while its attribution is recorded honestly. Expiry reverts toward
// AttributedAuthorized (stand-down, which withholds actuation) — the safe direction.
type CarveOut struct {
	ID         string
	Domain     string
	Actors     []string
	Hosts      []string
	ValidFrom  time.Time
	ValidUntil time.Time
}

// now returns the configured clock or the wall clock.
func (c Config) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// The derivation is total, evidence-first, and suspicion-dominant. Each admissible record is classified
// ONCE — self ▸ sanctioned ▸ valid-carve-out ▸ (else) unsanctioned — so the carve-out is the record's own
// authorization, not a whole-finding short-circuit. Then dominance resolves across the candidate set:
//  1. Keep only admissible evidence: in-window AND naming the subject (REQ-2312).
//  2. Classify each record into a candidate taxonomy. An actor that is the platform's own identity ⇒
//     self; a sanctioned principal ⇒ authorized; a match on a currently-valid carve-out (actor + host) ⇒
//     authorized-test (the record's injector is sanctioned FOR THAT POOL); anything else ⇒ suspicious.
//  3. Any suspicious candidate ⇒ AttributedSuspicious — a positively-unknown actor DOMINATES everything,
//     including a co-occurring carve-out or self record (REQ-2304): a carve-out can never mask a genuine
//     intruder into authorized-test. (Classifying carve-out per-record — not as a first-match
//     short-circuit — is what preserves this: the intruder's own record is never a carve-out match.)
//  4. Else any authorized-test candidate ⇒ AuthorizedTest with the matched rule id (REQ-2309) — the
//     learning regime heals the manufactured pool fault. The carve-out actor list is the sanctioned
//     INJECTOR set, never TG's own actuation identity, so a lone self-heal on a pool host (no injector
//     record) stays attributed-self (REQ-2302).
//  5. Else >1 candidate ⇒ REQ-2310: every candidate is recorded and Taxonomy stays the zero value — the
//     escalate-to-human signal the classifier reads (Candidates > 1).
//  6. Else exactly one candidate ⇒ resolve it (self ⇒ already-remediated REQ-2302; authorized ⇒
//     stand-down REQ-2301).
//  7. No candidates ⇒ Unattributable (REQ-2303 — the pre-feature ladder, not suspicion).
func Attribute(subject, faultClass string, ev []Evidence, warnings []string, cfg Config) Finding {
	now := cfg.now()
	since := now.Add(-cfg.Window)
	f := Finding{Taxonomy: Unattributable, Warnings: warnings}

	// Canonicalise the subject host ONCE. Hostnames are case-insensitive (DNS), but the readers disagree on
	// casing: the journal reader lowercases its evidence Target (journal.go), while the PVE/NetBox/AWX/gitops
	// readers pass it through. A case-SENSITIVE compare of a lowercased Target against a raw mixed-case subject
	// would SILENTLY drop admissible evidence (→ unattributable, which on the security path masks a suspicious
	// actor) — the same reader-vs-matcher key mismatch that made the PVE reader inert. Fold both sides of every
	// host comparison below instead. Behaviour-identical on the all-lowercase estate today; defensive for any
	// mixed-case host. (spec/023 hardening.)
	subject = strings.ToLower(strings.TrimSpace(subject))

	// (1) Admissible = timestamped inside the window AND naming the investigated subject (REQ-2312).
	// Evidence failing either is discarded silently — it proves nothing about THIS change.
	var adm []Evidence
	for _, e := range ev {
		if e.Target == "" || strings.ToLower(strings.TrimSpace(e.Target)) != subject {
			continue
		}
		if e.ObservedAt.Before(since) || e.ObservedAt.After(now.Add(time.Minute)) {
			continue
		}
		adm = append(adm, e)
	}
	f.Evidence = adm
	if len(adm) == 0 {
		return f // (7) Unattributable — the pre-feature ladder, not suspicion.
	}

	// (2) Classify each record ONCE. Precedence self ▸ carve-out ▸ sanctioned ▸ suspicious: the carve-out is
	// checked BEFORE the general sanctioned-principal because it is the more specific authorization — on an
	// allowlisted pool host inside the window, a sanctioned admin's fault is authorized-TEST (heal), not
	// authorized (stand-down). An actor that is none of self / carve-out / sanctioned is UNSANCTIONED
	// (REQ-2304 first half).
	cand := map[Taxonomy]bool{}
	carveRule := ""
	for _, e := range adm {
		if sa := cfg.SelfActors[e.Domain]; sa != "" && e.Actor == sa {
			cand[AttributedSelf] = true
			continue
		}
		if id, ok := matchCarveOut(cfg, e, subject, now); ok {
			cand[AuthorizedTest] = true
			if carveRule == "" {
				carveRule = id
			}
			continue
		}
		if contains(cfg.Sanctioned[e.Domain], e.Actor) {
			cand[AttributedAuthorized] = true
			continue
		}
		cand[AttributedSuspicious] = true
	}
	f.Candidates = keys(cand)

	// (3) A suspicious candidate dominates EVERYTHING (REQ-2304): a positively-unknown actor is never
	// averaged away and never masked into authorized-test by a co-occurring sanctioned / self / carve-out
	// record on the same subject. A hostile action during an active pool-carve-out window resolves
	// suspicious — the reverse ordering (carve-out first-match short-circuit) was the security defeat.
	if cand[AttributedSuspicious] {
		f.Taxonomy = AttributedSuspicious
		return f
	}
	// (4) A valid carve-out match heals the manufactured pool fault (REQ-2309) — reached only when NO
	// unsanctioned actor is present.
	if cand[AuthorizedTest] {
		f.Taxonomy = AuthorizedTest
		f.RuleID = carveRule
		return f
	}

	// REQ-2304 second half (a covered audit trail with NO entry for an observed mutation ⇒ suspicious)
	// is a Phase-2 disposition: it requires the reader to REPORT coverage-with-no-matching-entry, which
	// the Phase-1 seam does not carry (an empty covered read returns zero admissible evidence, already
	// handled as Unattributable above — the pre-feature ladder, evidence-gated honesty). No Phase-1
	// warning is synthesized: the previous warning fired on the INVERSE case (every covered row that DID
	// have an entry), which is noise, not the missing-entry signal. See spec/023 REQ-2304.

	// (5) A non-suspicious, non-test contradiction escalates with every candidate recorded.
	if len(f.Candidates) > 1 {
		return f
	}
	// (6) Exactly one candidate.
	if cand[AttributedAuthorized] {
		f.Taxonomy = AttributedAuthorized
		return f
	}
	if cand[AttributedSelf] {
		f.Taxonomy = AttributedSelf
		return f
	}
	return f
}

// matchCarveOut reports the id of the first currently-valid carve-out that sanctions this record's actor
// on the subject host (REQ-2309), or ("", false). A carve-out authorizes the LISTED INJECTOR principal on
// its allowlisted pool hosts inside its validity window — it is never TG's own actuation identity.
func matchCarveOut(cfg Config, e Evidence, subject string, now time.Time) (string, bool) {
	for _, co := range cfg.CarveOuts {
		if !carveOutValid(co, now) {
			continue
		}
		if co.Domain != "" && e.Domain != co.Domain {
			continue
		}
		if contains(co.Actors, e.Actor) && containsFold(co.Hosts, subject) {
			return co.ID, true
		}
	}
	return "", false
}

// carveOutValid reports whether a carve-out is temporally valid at `now` (REQ-2309: an expired, future,
// or invalid row never matches).
func carveOutValid(co CarveOut, now time.Time) bool {
	if !co.ValidFrom.IsZero() && now.Before(co.ValidFrom) {
		return false
	}
	if !co.ValidUntil.IsZero() && now.After(co.ValidUntil) {
		return false
	}
	return true
}

func contains(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}

// containsFold is contains for HOST identifiers — case-insensitive, since hostnames are (DNS). The
// case-sensitive contains is kept for ACTOR identities: an LDAP/Kerberos realm principal (alice@SEC.REALM)
// is case-significant and must never be widened by folding.
func containsFold(xs []string, x string) bool {
	for _, s := range xs {
		if strings.EqualFold(strings.TrimSpace(s), x) {
			return true
		}
	}
	return false
}

func keys(m map[Taxonomy]bool) []Taxonomy {
	out := make([]Taxonomy, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
