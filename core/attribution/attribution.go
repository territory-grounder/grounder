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
	// AttributedSuspicious: positive evidence of an unsanctioned actor — OR REQ-2304's second half, a reader
	// that AFFIRMATIVELY COVERS the subject's audit trail, answered, and recorded NO actor for an OBSERVED
	// MUTATION (covered-but-empty on a confirmed mutation; TG-407 — gated in AttributeObserving on
	// Observation.MutationObserved so an ordinary no-actor fault, a crash, never escalates). A suspicious
	// reading DOMINATES a co-occurring carve-out or contradiction: escalate, never auto-heal (REQ-2304).
	AttributedSuspicious
	// AuthorizedTest: a currently-valid sanctioned-pool carve-out matched the attributed actor and
	// target host — a manufactured learning fault on an allowlisted pool host. The heal ladder proceeds
	// unchanged and the attribution is recorded honestly (REQ-2309).
	AuthorizedTest
)

// AllTaxonomies is the closed enumeration, in declaration order. It exists so a completeness check has ONE
// list to consult rather than a second copy that can drift from the const block — a mapping that silently
// omits a taxonomy turns it into a forced human poll (see ParseConfig).
func AllTaxonomies() []Taxonomy {
	return []Taxonomy{Unattributable, AttributedAuthorized, AttributedSelf, AttributedSuspicious, AuthorizedTest}
}

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

// CoverageMarker is the row a reader emits when it AFFIRMATIVELY covers the subject's audit trail for the
// investigation window but recorded NO actor for the observed mutation (REQ-2304 half 2, TG-407). It carries
// coverage without an actor — Covered=true, Actor="" — because "this domain covered the target and found
// nothing" is otherwise inexpressible: an empty result set has no row to carry the flag, so the one condition
// half 2 is about (a mutation with no covering-domain entry) could never reach Attribute. It is NOT actor
// evidence; Attribute reads it only to distinguish covered-but-empty (⇒ suspicious) from genuinely blind
// (⇒ unattributable). Only a reader that would set Covered=true on a real hit emits this on a clean miss
// (pve/journal/awx/netbox); a reader that cannot affirmatively cover (gitops-mr) never does.
func CoverageMarker(domain, target string, at time.Time) Evidence {
	return Evidence{Domain: domain, Target: target, ObservedAt: at, Covered: true}
}

// IsCoverageMarker reports whether e is a coverage-only row (affirmative coverage, no actor) rather than actor
// evidence. Keyed on Covered && empty Actor so a real covered hit (Covered=true WITH an actor) is never mistaken
// for a marker.
func IsCoverageMarker(e Evidence) bool {
	return e.Covered && strings.TrimSpace(e.Actor) == ""
}

// advisoryDomains are evidence domains that may CORRELATE but may never MINT a taxonomy (REQ-2321).
//
// An authentication-event source names a network-access-server, a session, or a login principal — never
// the mutated target — so it cannot satisfy REQ-2312 admissibility on its own terms, and its ABSENCE
// proves nothing at all: key-based, console and local logins bypass RADIUS entirely. A record that can
// neither confirm nor deny is not evidence about who changed the estate.
//
// The Target filter below was doing this job by accident and only by accident. A reader that stamped
// the INVESTIGATED HOST as its Target — the natural thing for a "who logged into web01" reader to do —
// became admissible, and its unsanctioned login principal then minted attributed-suspicious with
// nothing else in the set. Demonstrated red before this guard existed
// (TestAnAuthRecordNamingTheSubjectIsStillNotAMover). No authentication reader is wired today, so this
// is a no-op on the running estate and a gate on the next one.
//
// A CLOSED SET, not a substring rule, and it is the conservative direction that matters: the failure
// mode of adding a domain here is that a genuine mover stops moving, which is a security regression,
// so membership is an explicit operator/spec decision rather than a pattern anything can match into.
var advisoryDomains = map[string]bool{
	"radius":        true,
	"auth-observed": true,
}

// IsAdvisoryDomain reports whether a domain may correlate but never adjudicate (REQ-2321). Exported so
// a reader author can ask the same question the derivation asks, rather than discovering the answer by
// watching a taxonomy fail to move.
func IsAdvisoryDomain(domain string) bool {
	return advisoryDomains[strings.ToLower(strings.TrimSpace(domain))]
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
	// CoveredButEmpty is the REQ-2304-half-2 signal (TG-407): a reader affirmatively covered the subject's audit
	// trail in-window, answered, and recorded NO actor. It is ALWAYS surfaced for review. Whether it ALSO changes
	// the Taxonomy depends on the session's observed-change context (AttributeObserving): WITH a confirmed
	// observed mutation it escalates to AttributedSuspicious (a mutation with no covering-domain entry is the
	// intrusion signal); WITHOUT one the Taxonomy stays Unattributable (observe-only) — because covered-but-empty
	// is otherwise indistinguishable from "nothing happened" (a crash, an in-flight job, a system-triggered
	// change), and escalating every such session would flood SECURITY and neuter auto-heal.
	CoveredButEmpty bool
}

// Observation is the session's observed-CHANGE context for the attribution call — the facts about WHAT was
// observed on the estate, kept SEPARATE from the reader-captured actor Evidence (which answers WHO). It is a
// per-session input, not rules-as-data and not evidence, so it is passed per call rather than living on Config.
//
// MutationObserved is the POSITIVE observed-mutation signal REQ-2304 half 2 turns on: the session has
// confirmation that a state transition / configuration change actually occurred on the subject (the alert
// asserts a state transition, or a resource/config-hash diff confirms a change) — as opposed to a bare fault
// that may be a crash, an in-flight job, or a system-triggered change. It is the discriminator between an
// unaudited MUTATION (covered-but-empty ⇒ suspicious) and an ordinary no-actor fault (covered-but-empty ⇒
// observe-only). The zero value is false, so the plain Attribute() — which passes Observation{} — can never
// mint the covered-but-empty escalation; a caller opts in only by supplying a true signal here.
type Observation struct {
	MutationObserved bool
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
	// SelfReaders maps a domain to the platform's own READ-ONLY investigation identities as that domain
	// records them (e.g. "journal" → ["root!SHA256:<hostdiag-key-fp>"]). These are the identities TG
	// authenticates AS when it DIAGNOSES a faulted subject (hostdiag's classify-SSH login), distinct from
	// the actuation SelfActor it heals AS. A login by one of them is TG reading the subject during triage —
	// not a remediation and not an intruder — so Attribute recognises it and mints NO candidate for it
	// (TG-453). Derived from the reader CREDENTIAL at the composition root the same way SelfActors is (never
	// a ruleset token, so it survives rotation and cannot be asserted by anyone who does not hold the key);
	// kept SEPARATE from SelfActors so a reader identity can never be mistaken for the actuation identity —
	// self-recognition of a HEAL must key on the credential that actually actuates (REQ-2302).
	SelfReaders map[string][]string
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
// learning regime's purpose — while its attribution is recorded honestly.
//
// ★ EXPIRY REVERTS TOWARD AttributedAuthorized (stand-down, which withholds actuation) — the safe direction —
// BUT ONLY IF THE CARVE-OUT'S ACTORS ARE ALSO SANCTIONED IN THAT DOMAIN. That precondition used to be
// unstated here, and it does not hold by default. Classification precedence is
// self ▸ carve-out ▸ sanctioned ▸ unsanctioned, so once the window closes an actor with no sanctioned entry
// falls through to UNSANCTIONED and resolves AttributedSuspicious ⇒ security-escalate.
//
// Measured on this estate against the real Attribute(): the journal carve-out lists the operator's admin SSH
// fingerprint, the journal domain has NO sanctioned principals, and after ValidUntil the same key resolves
// attributed-suspicious rather than attributed-authorized. That is not stand-down: it routes a SECURITY
// incident, on every ordinary admin login across the carve-out's hosts, on a known date.
// Sanctioning the actor is inert while the window is open (the carve-out has precedence, so the learning
// regime is unaffected) and is what makes expiry degrade the way this comment claims. CarveOutExpiryRisk
// below reports the configurations where it would not.
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

// Attribute derives the actor-attribution taxonomy from reader evidence WITHOUT any session observed-change
// context — it passes the zero Observation to AttributeObserving. Consequently it can never mint the
// REQ-2304-half-2 covered-but-empty escalation (that needs a positive observed-mutation signal): a covered
// reader that answered with no actor stays Unattributable+CoveredButEmpty (observe-only). Callers that hold a
// confirmed observed-mutation signal MUST use AttributeObserving to arm the escalation; keeping it unreachable
// from the plain entry is the fail-safe-by-construction default — no session floods SECURITY unless something
// deliberately asserts a mutation occurred. The signature is preserved so every existing caller is unchanged.
func Attribute(subject, faultClass string, ev []Evidence, warnings []string, cfg Config) Finding {
	return AttributeObserving(subject, faultClass, ev, warnings, cfg, Observation{})
}

// AttributeObserving is Attribute plus the session's Observation (REQ-2304 half 2, TG-407): the same
// deterministic, suspicion-dominant derivation, with one added disposition — a reader that AFFIRMATIVELY
// COVERS the subject, answered, and recorded NO actor escalates to AttributedSuspicious IFF the session
// carries a confirmed observed mutation (obs.MutationObserved). Without that signal the covered-but-empty
// case stays observe-only, exactly as before.
//
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
func AttributeObserving(subject, faultClass string, ev []Evidence, warnings []string, cfg Config, obs Observation) Finding {
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

	// (1) Admissible = timestamped inside the window AND naming the investigated subject (REQ-2312),
	// AND not from an ADVISORY domain (REQ-2321). Evidence failing the first two is discarded silently —
	// it proves nothing about THIS change. An advisory record is discarded LOUDLY, because it is a
	// record someone deliberately wired and its exclusion is a stated position rather than a near-miss.
	var adm []Evidence
	coveredSubject := false // a covering reader affirmatively covered THIS subject in-window (REQ-2304 half 2)
	for _, e := range ev {
		if IsAdvisoryDomain(e.Domain) {
			f.Warnings = append(f.Warnings, "advisory domain "+strings.ToLower(strings.TrimSpace(e.Domain))+
				" is correlation only and never mints a taxonomy (REQ-2321)")
			continue
		}
		if e.Target == "" || strings.ToLower(strings.TrimSpace(e.Target)) != subject {
			continue
		}
		if e.ObservedAt.Before(since) || e.ObservedAt.After(now.Add(time.Minute)) {
			continue
		}
		if IsCoverageMarker(e) {
			// A coverage signal, NOT actor evidence: this domain covered the subject's audit trail for the
			// window and recorded no actor. Held aside (never classified as an actor) so it cannot self / carve /
			// sanction / suspect on its empty principal — it only decides covered-but-empty below (REQ-2304 half 2).
			coveredSubject = true
			continue
		}
		adm = append(adm, e)
	}
	f.Evidence = adm
	if len(adm) == 0 {
		if coveredSubject {
			// COVERED-BUT-EMPTY (REQ-2304 half 2, TG-407): a reader affirmatively covered the subject's audit
			// trail in-window, ANSWERED (a failed read contributes no marker — see IsCoverageMarker/the fanout),
			// and recorded NO actor. The fact is ALWAYS surfaced (the machine-readable flag + a warning). Whether
			// it ALSO escalates turns on the session's observed-change context, and ONLY on it.
			f.CoveredButEmpty = true
			if obs.MutationObserved {
				// …WITH a confirmed observed mutation ⇒ AttributedSuspicious. A state transition / config change
				// actually occurred on the subject, yet the domain that authoritatively covers its audit trail
				// recorded no actor for it — a mutation with no covering-domain entry, which is REQ-2304's
				// intrusion signal (INV-09: fail toward the human on a security signal). It routes through the SAME
				// security-escalate disposition (config.go DispositionFor) → POLL_PAUSE + security_escalation
				// (core/risk) → never auto-heal, exactly like an unsanctioned-actor reading.
				f.Taxonomy = AttributedSuspicious
				f.Warnings = append(f.Warnings, "covered-but-empty on an OBSERVED MUTATION: a reader affirmatively "+
					"covered "+subject+" in-window and recorded no actor for a confirmed state change — REQ-2304 "+
					"half 2 intrusion signal, escalated attributed-suspicious (mutation with no covering-domain entry)")
				return f
			}
			// …WITHOUT a confirmed observed mutation ⇒ OBSERVE-ONLY, taxonomy stays Unattributable. Covered-but-
			// empty is the COMMON case, not a rare intrusion: most faults are not actor mutations — a crash, an
			// in-flight job, a system-triggered change all leave no actor entry, indistinguishable HERE from an
			// unaudited mutation. Escalating every such session would route the majority of no-actor sessions to
			// SECURITY and neuter auto-heal, so the safe default is preserved byte-for-byte in taxonomy while the
			// signal that was previously INEXPRESSIBLE is recorded for a mutation-confirmed downstream read.
			f.Warnings = append(f.Warnings, "covered-but-empty: a reader affirmatively covered "+subject+
				" in-window and recorded no actor — REQ-2304 half 2 signal, surfaced for review, NOT escalated "+
				"(no confirmed observed-mutation signal, so it may be a crash rather than an unaudited change)")
		}
		return f // Unattributable here (either genuinely blind, or covered-but-empty with no observed mutation).
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
			// REQ-2302 keys self-recognition on (target, FAULT CLASS) — both halves. Until 2026-07-28 only the
			// target half was implemented: `faultClass` was a parameter of this function and appeared exactly
			// once, in the signature. Any self record on the host, of any kind, terminated the session
			// `already-remediated`.
			//
			// Measured live the day it was fixed: nginx was stopped on a guest, TG proposed `start-service` —
			// the correct verb — and attribution matched it against a `vzstart` (a Proxmox GUEST start) that
			// TG's own identity had performed 29 minutes earlier for an unrelated device-down. SelfNoop fired,
			// the session terminated "already remediated", and nginx stayed down. A guest start was accepted as
			// proof that a systemd service start had already happened.
			//
			// The evidence needed to tell them apart was already captured and simply never consulted:
			// Evidence.ActionKind carries the domain verb ("vzstart", "vzstop", …).
			//
			// Narrowed HERE and only here, deliberately. The suspicious and authorized paths below still see
			// EVERY admissible record on the host — an unsanctioned actor doing something unrelated must still
			// dominate (REQ-2304), and narrowing their admissibility would be a security regression. This
			// narrowing can only cause TG to ACT on a fault it has not actually remediated; it can never cause
			// it to miss an intruder.
			if selfActionAccomplishes(faultClass, e.ActionKind) {
				cand[AttributedSelf] = true
			}
			continue
		}
		// TG-453: a login by one of TG's OWN read-only investigation identities (hostdiag's classify-SSH
		// into the faulted subject DURING triage) is neither a remediation nor an intruder — it is TG
		// reading the subject AFTER the fault to diagnose it. Left unrecognised it is not the actuation
		// self-actor, not sanctioned, and not a carve-out, so it would fall through to AttributedSuspicious
		// below and SECURITY-ESCALATE TG's own investigation, blocking a legitimately-approved heal (the
		// live defect). Recognised HERE and dropped from candidate-minting: it mints NO taxonomy (a read
		// accomplishes no remediation ⇒ never AttributedSelf; it is TG's own key ⇒ never
		// AttributedAuthorized/AttributedSuspicious). Placed BEFORE carve-out/sanctioned so TG's own reader
		// can never be adjudicated as an external principal. Crucially it does NOT narrow the suspicious
		// path: a real unknown actor's record on the same subject still reaches the fall-through and
		// dominates (REQ-2304 intact) — only the reader-self record itself is excluded, not the finding.
		if contains(cfg.SelfReaders[e.Domain], e.Actor) {
			f.Warnings = append(f.Warnings, "recognised TG's own read-only investigation identity "+e.Actor+
				" on "+subject+" ("+e.Domain+") — excluded from actor attribution as TG's own diagnostic "+
				"access, not a fault actor (TG-453)")
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

	// REQ-2304 second half (a covered audit trail with NO entry for an observed mutation ⇒ suspicious) is
	// handled ABOVE, in the len(adm)==0 branch: a covering reader that answered with no actor sets
	// CoveredButEmpty, and escalates to AttributedSuspicious when the session carries a confirmed observed
	// mutation (obs.MutationObserved). It cannot arise HERE — this point is reached only with admissible ACTOR
	// evidence present (len(adm) > 0), i.e. the trail is NOT empty, so its actors have already been classified
	// above and a suspicious one would have dominated. See spec/023 REQ-2304.

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

// MaxCarveOutWindow caps how long a single carve-out may suspend the security path for its actors
// (REQ-2309). A carve-out is an EXCEPTION: inside it, an actor who would otherwise read
// attributed-suspicious reads authorized-test instead, so security-escalate cannot fire for them. An
// exception that outlives the memory of why it was granted is indistinguishable from policy, which is why
// the window is bounded and must be renewed by a deliberate act rather than declared once.
//
// 90 days is chosen to be short enough that a renewal lands inside the same operational quarter as the
// grant, and long enough that a continuously-running benchmark harness is not re-authorised weekly.
const MaxCarveOutWindow = 90 * 24 * time.Hour

// carveOutValid reports whether a carve-out is temporally valid at `now` (REQ-2309: an expired, future,
// or invalid row never matches).
//
// ★ AN UNBOUNDED CARVE-OUT IS INVALID, NOT ETERNAL. This function used to skip each comparison when the
// corresponding bound was the zero time, so a carve-out missing valid_until matched FOREVER — the precise
// inverse of the "temporally bounded" property its own doc comment claims. ParseConfig now rejects such a
// document at load, and this is the second, independent layer: a Config assembled in code (a test, a future
// caller that does not route through ParseConfig) cannot obtain a permanent security-path exemption by
// leaving a field at its zero value. Both layers are needed — the parser guards the data, this guards the
// type, and neither one implies the other.
//
// The direction is fail-CLOSED: an under-specified carve-out stops matching, so its actors revert toward
// attributed-authorized/suspicious (stand-down or escalate) rather than staying silently sanctioned.
func carveOutValid(co CarveOut, now time.Time) bool {
	if co.ValidFrom.IsZero() || co.ValidUntil.IsZero() {
		return false
	}
	if now.Before(co.ValidFrom) {
		return false
	}
	if now.After(co.ValidUntil) {
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

// selfActionAccomplishes reports whether a domain verb TG's own identity performed could have accomplished
// the proposed op-class — the FAULT-CLASS half of REQ-2302's (target, fault class) key.
//
// The mapping is grounded in what the readers ACTUALLY emit, not in what they might: across the whole live
// ledger the only action kinds ever recorded are `pve/vzstart` (361) and `pve/vzstop` (698). A guest lifecycle
// verb is therefore the only self-evidence that exists today, and it accomplishes exactly one op-class.
//
// An op-class with NO mapped action kind can never be self-recognised, so TG will act on it. That is the
// correct direction: the alternative — the shipped behaviour — was to suppress remediation of a live fault on
// the strength of an unrelated action. Every other gate (mode chokepoint, policy verdict, graduation ladder,
// operator allowlist, the target host's argv allowlist) is untouched by this, so "TG acts" still means "TG
// asks all the same questions".
//
// An UNKNOWN op-class returns false — fail toward remediating rather than toward silently standing down.
func selfActionAccomplishes(faultClass, actionKind string) bool {
	fc := strings.ToLower(strings.TrimSpace(faultClass))
	ak := strings.ToLower(strings.TrimSpace(actionKind))
	for _, k := range selfActionKinds[fc] {
		if k == ak {
			return true
		}
	}
	return false
}

// selfActionKinds maps an op-class to the domain verbs that would have ACCOMPLISHED it. Only verbs a reader
// can actually produce belong here; adding a speculative one re-opens the defect this table closes, because a
// verb that never occurs costs nothing while a verb that occurs for a DIFFERENT purpose suppresses a real heal.
var selfActionKinds = map[string][]string{
	// Proxmox guest lifecycle — the one domain with live readers today.
	"start-guest": {"vzstart", "qmstart"},
	// Service and container lifecycle have NO reader emitting their verbs yet, so they are deliberately absent
	// rather than guessed. When a systemd/docker actor-evidence reader ships, add its verbs here AND extend
	// TestSelfRecognitionRequiresAMatchingActionKind — the oracle walks the live op-class registry, so a new
	// op-class with no entry is visible rather than silent.
}

// CarveOutRenewalWarning is how long before a carve-out lapses that its renewal becomes urgent enough to
// report at every boot. Sized to leave room for a working week plus a weekend after the first warning.
const CarveOutRenewalWarning = 14 * 24 * time.Hour

// CarveOutExpiry is one carve-out's remaining life. Reported so a BOUNDED carve-out cannot lapse silently.
//
// ★ THIS EXISTS BECAUSE THE BOUND ITSELF CREATED A NEW FAILURE MODE. Requiring an expiry closes a real
// hole (an absent bound was a permanent exemption from the security path), but it also means the harness
// carve-out now has a date on which the learning regime STOPS: past it, the injector's sanctioned faults
// stop resolving to authorized-test and revert toward attributed-authorized — stand-down, which withholds
// actuation. That is the safe direction and it is the right default, but it is indistinguishable from "the
// estate went quiet" unless something says so out loud. A safety bound whose lapse is invisible would just
// trade a security hole for an availability one.
//
// Pure and total: the caller states `now`, so the log line and the gauge derive from one reproducible value
// rather than two clock reads that can straddle a boundary.
type CarveOutExpiry struct {
	ID         string
	Domain     string
	ValidUntil time.Time
	Remaining  time.Duration // negative once lapsed
	Expired    bool
	Renew      bool // within CarveOutRenewalWarning of lapsing (or already lapsed)
}

// ExpiryRisk is one carve-out whose lapse would degrade UNSAFELY: its actors are not sanctioned in its
// domain, so when the window closes they resolve attributed-suspicious (security-escalate) instead of
// attributed-authorized (stand-down). Reported so the configuration cannot sit there silently until the date
// arrives — which is the whole hazard of a bound: it fires on a schedule nobody is watching.
type ExpiryRisk struct {
	CarveOutID string
	Domain     string
	Actors     []string // the specific actors with no sanctioned entry — the remedy is per-actor
	ValidUntil time.Time
}

// CarveOutExpiryRisk reports the carve-outs whose expiry would produce false security escalations rather than
// a stand-down. An actor is safe if it is sanctioned in the domain OR is the platform's own identity there
// (self resolves to attributed-self, which is also not suspicion).
//
// Pure and total: no clock is read, because the risk is a property of the CONFIGURATION and not of the
// current time — reporting it only once the window is nearly closed would be reporting it too late to matter.
func CarveOutExpiryRisk(cfg Config) []ExpiryRisk {
	out := make([]ExpiryRisk, 0, len(cfg.CarveOuts))
	for _, co := range cfg.CarveOuts {
		var unsanctioned []string
		for _, a := range co.Actors {
			if a == "" {
				continue
			}
			if cfg.SelfActors != nil && cfg.SelfActors[co.Domain] == a {
				continue // self ⇒ attributed-self, never suspicion
			}
			if cfg.Sanctioned != nil && contains(cfg.Sanctioned[co.Domain], a) {
				continue
			}
			if cfg.SanctionedGroups != nil && len(cfg.SanctionedGroups[co.Domain]) > 0 {
				// A group grant is resolved by the identity seam at classification time, not here. Treat the
				// presence of any group as "possibly covered" and do NOT report — over-reporting a security
				// warning trains operators to ignore it, which is its own failure.
				continue
			}
			unsanctioned = append(unsanctioned, a)
		}
		if len(unsanctioned) > 0 {
			out = append(out, ExpiryRisk{CarveOutID: co.ID, Domain: co.Domain, Actors: unsanctioned, ValidUntil: co.ValidUntil})
		}
	}
	return out
}

// CarveOutExpiries reports every carve-out's remaining life at `now`, in declaration order.
func CarveOutExpiries(cfg Config, now time.Time) []CarveOutExpiry {
	out := make([]CarveOutExpiry, 0, len(cfg.CarveOuts))
	for _, co := range cfg.CarveOuts {
		e := CarveOutExpiry{ID: co.ID, Domain: co.Domain, ValidUntil: co.ValidUntil}
		// An unbounded carve-out no longer matches anything (carveOutValid), so it is reported as EXPIRED
		// rather than as infinite life — the reading an operator needs is "this rule is not in force".
		if co.ValidUntil.IsZero() {
			e.Expired, e.Renew = true, true
			out = append(out, e)
			continue
		}
		e.Remaining = co.ValidUntil.Sub(now)
		e.Expired = !now.Before(co.ValidUntil)
		e.Renew = e.Expired || e.Remaining <= CarveOutRenewalWarning
		out = append(out, e)
	}
	return out
}

// CarveOutHostCoverage reports which of `pool` the currently-valid carve-outs name, and which they do not.
//
// WHY THIS EXISTS. matchCarveOut requires containsFold(co.Hosts, subject) — an EXACT, case-folded string
// match with no glob, prefix or CIDR support. That is deliberate (a wildcard in an authorization rule is a
// standing grant), but it means the carve-out host list and the estate's actual guest pool are two lists
// that must agree, maintained in different places, with nothing comparing them. A guest added to the pool
// later is simply absent from every carve-out, so the harness cycle on it — the injector's sanctioned change
// plus TG's own heal — stops resolving to authorized-test and lands in the {AttributedAuthorized,
// AttributedSelf} contradiction instead, which escalates to a human (config.go, the candidates > 1 rule).
//
// The failure is silent and it is an AUTONOMY loss, not a safety loss: TG asks a human about a change it
// could previously adjudicate. Nothing logs it, and the symptom (this one guest always polls) looks like
// estate noise.
//
// An EXPIRED or not-yet-valid carve-out covers NOTHING — carveOutValid is consulted per row, so a rule that
// lapsed at midnight stops providing coverage at midnight. That is the case most worth reporting, because
// the config still LOOKS like it covers the host.
//
// Pure and total: it reads no clock of its own and touches no I/O, so the caller states `now` and the result
// is reproducible. Order of `uncovered` follows `pool` so a log line is stable across restarts.
func CarveOutHostCoverage(cfg Config, pool []string, now time.Time) (covered, uncovered []string) {
	for _, host := range pool {
		if strings.TrimSpace(host) == "" {
			continue
		}
		hit := false
		for _, co := range cfg.CarveOuts {
			if !carveOutValid(co, now) {
				continue
			}
			if containsFold(co.Hosts, host) {
				hit = true
				break
			}
		}
		if hit {
			covered = append(covered, host)
		} else {
			uncovered = append(uncovered, host)
		}
	}
	return covered, uncovered
}

// DomainConfigGap is one armed evidence reader whose domain has no identity declared for it.
type DomainConfigGap struct {
	Domain       string // the reader's domain slug ("pve", "journal", "awx", "netbox", "gitops-mr", …)
	NoSelfActor  bool   // SelfActors[domain] is empty — TG's OWN actions in this domain read as suspicious
	NoSanctioned bool   // Sanctioned[domain] is empty — EVERY human/automation actor here reads as suspicious
}

// DomainConfigGaps reports, for each armed reader domain, whether the config gives it an identity to reason
// with. It changes no decision — it exists so a gap that silently maximises escalation is legible at boot.
//
// WHY BOTH FIELDS MATTER, and why they are different failures:
//
//   - NoSanctioned: Attribute() falls through the carve-out and sanctioned checks to `AttributedSuspicious`
//     for any actor it cannot place. With an EMPTY sanctioned list for a domain, every actor in that domain
//     is suspicious by construction, and suspicion DOMINATES every other candidate. This is the correct fail
//     direction — an undeclared actor IS the intruder case — but it is driven by the ABSENCE of a config
//     row, so nothing about the running system announces it.
//
//   - NoSelfActor: worse in practice, because the actor is TG. SelfActors is keyed per-DOMAIN and only "pve"
//     is populated (cmd/worker sets it from the actuation credential). Arm the journal reader and TG's own
//     privileged actions arrive as journal evidence with no self-identity to match, so TG raises a SECURITY
//     escalation ON ITSELF — and because suspicious dominates, that reading masks every other candidate in
//     the set.
//
// PER-DOMAIN IDENTITY IS THE POINT, NOT A BUG TO ROUND OFF. Matching an actor against any SelfActors value
// would let a credential stolen in one domain be auto-excused in another.
//
// ★ THE REMEDY IS CODE, NOT CONFIG, AND SAYING OTHERWISE SENDS AN OPERATOR NOWHERE. SelfActors is
// deliberately NOT parsed from the ruleset document — ParseConfig says so outright: the self-identity comes
// from the credential engine (spec/016), never a ruleset string, because a string an operator can edit is a
// string an attacker can be named in. It is written in exactly ONE place in the tree,
// cmd/worker/main.go, for "pve", derived from the ACTUATION credential so it survives a token rotation.
//
// So a domain reported here cannot be fixed by editing a config file. It is fixed by deriving that domain's
// self-identity from ITS credential at the composition root, the same way pve's is. Until that wiring exists
// for a domain, arming its reader means TG reads its own actions as hostile — which is why this is reported
// loudly rather than quietly tolerated.
//
// It also does NOT suggest declaring a self-actor alone is sufficient: selfActionKinds maps only
// `start-guest` today, so a self record in a domain whose op-classes are unmapped adds no candidate and
// changes nothing — see the note on that table before extending either.
//
// ★ A SELF-ACTOR IS ONLY MISSING IN A DOMAIN WHERE TG ACTS. This function used to report NoSelfActor for
// every armed domain, which over-reports and — the part that matters — dilutes the one case that is real.
//
// A self-identity exists to stop TG reading ITS OWN action as a stranger's. In a domain TG only READS, there
// is no TG action in the evidence to misread, so there is nothing for a self-identity to match and demanding
// one sends an operator to wire something with no effect. Measured 2026-07-29: `netbox` has NO write path
// anywhere in the tree — the reader declares ReadOnly() and nothing posts, puts, patches or deletes — so TG
// can never appear in a NetBox changelog. `awx` is the opposite: opschema.json declares `effect_kind:
// awx-launch`, so TG genuinely launches AWX jobs and its own runs DO land in AWX job history.
//
// This is the SECOND correction to this same diagnostic today. The first (!706) fixed it naming a remedy
// that does not exist ("declare the identity" — SelfActors is not parsable from the ruleset). This one fixes
// it demanding the remedy in a domain that does not need it. A warning is not free: each false one spends an
// operator's attention and buys distrust of the next.
//
// `actsIn` is the caller's set of domains TG ACTUATES in. It is the caller's to state because the
// composition root is what wires actuation; this package must not infer it.
//
// Pure and total; `armed` is the caller's list of registered reader domains, so this reads no global state.
func DomainConfigGaps(cfg Config, armed []string, actsIn map[string]bool) []DomainConfigGap {
	var out []DomainConfigGap
	for _, d := range armed {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		g := DomainConfigGap{
			Domain: d,
			// A read-only domain cannot misattribute a TG action, because there is none to attribute.
			NoSelfActor:  actsIn[d] && strings.TrimSpace(cfg.SelfActors[d]) == "",
			NoSanctioned: len(cfg.Sanctioned[d]) == 0,
		}
		if g.NoSelfActor || g.NoSanctioned {
			out = append(out, g)
		}
	}
	return out
}
