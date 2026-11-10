package main

// TG-662 causality wiring (owner prompt 2026-11-07 §2.D): the terminus asks TG's OWN durable evidence whether
// another actor restored the fault — the injector's rows, the provider recovery transitions, the actor-evidence
// readers the attribution step already holds — and the composition root appends the per-terminus record. Lives
// outside main() (the TG-501 ratchet) and reads only seams main() already resolved (deps.ActorReaders,
// deps.AttributionConfig, the pool).

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/territory-grounder/grounder/adapters/actorevidence"
	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/temporal/runner"
)

// injectorReader / recoveryReader are the two core/db reads the causality seam composes (interfaces so the
// composer is testable without a database).
type injectorReader interface {
	InjectorActivity(ctx context.Context, hosts []string, restoreFrom, injectFrom, to time.Time) (restores, injections int, err error)
}

type recoveryReader interface {
	RecoveriesBetween(ctx context.Context, host, alertRule string, from, to time.Time) (int, error)
}

// wireCausality installs runner.Deps.CausalityEvidence and TerminusRecord. A nil pool leaves both nil: every
// causality then reads indeterminate (no clean credit, no clean lesson) and the record is logged only.
func wireCausality(deps *runner.Deps, pool *db.Pool) {
	if pool == nil {
		return
	}
	cs := db.NewCausalityStore(pool)
	deps.CausalityEvidence = causalityEvidenceReader(cs, cs, deps.ActorReaders, deps.AttributionConfig)
	tr := db.NewTerminusRecordStore(pool)
	deps.TerminusRecord = func(ctx context.Context, r runner.TerminusRecord) error {
		return tr.Append(ctx, db.TerminusRecord{
			ExternalRef: r.ExternalRef, ActionID: r.ActionID, OpClass: r.OpClass, Host: r.Host, TargetHost: r.TargetHost, Site: r.Site,
			ExecutedAt: r.ExecutedAt, ClearedAt: r.ClearedAt, Executed: r.Executed, HasVerdict: r.HasVerdict, Verdict: r.Verdict,
			ConfirmedClear: r.ConfirmedClear, ClearReason: r.ClearReason, ObjectState: r.ObjectState, Collateral: r.Collateral,
			CreditDisposition: r.CreditDisposition, CreditFed: r.CreditFed, CreditClean: r.CreditClean,
			Causality: r.Causality, CausalityEvidence: r.CausalityEvidence, WindowState: r.WindowState, Source: r.Source,
		})
	}
}

// causalityEvidenceReader surveys the three evidence sources over the heal's interval. Every source is read
// POSITIVELY: a reader that errors leaves its Known=false (the classifier reads indeterminate), it never
// contributes a silent zero. The actor reading is KNOWN only when EVERY wired reader answered — one failed
// domain is one domain in which another actor could hide.
func causalityEvidenceReader(inj injectorReader, rec recoveryReader, readers []actorevidence.Reader, cfg attribution.Config) func(context.Context, runner.CausalityQuery) (runner.CausalityEvidence, error) {
	return func(ctx context.Context, q runner.CausalityQuery) (runner.CausalityEvidence, error) {
		var ev runner.CausalityEvidence
		hosts := []string{q.Host}
		if t := strings.TrimSpace(q.TargetHost); t != "" && !strings.EqualFold(t, q.Host) {
			hosts = append(hosts, t)
		}
		if inj != nil {
			if r, i, err := inj.InjectorActivity(ctx, hosts, q.IncidentAt, q.ExecutedAt, q.ClearedAt); err == nil {
				ev.InjectorKnown, ev.InjectorRestores, ev.InjectorInjections = true, r, i
			} else {
				ev.Notes = append(ev.Notes, "injector read failed: "+err.Error())
			}
		}
		if rec != nil {
			if n, err := rec.RecoveriesBetween(ctx, q.Host, q.AlertRule, q.IncidentAt, q.ExecutedAt); err == nil {
				ev.RecoveryKnown, ev.RecoveredBeforeExecute = true, n > 0
			} else {
				ev.Notes = append(ev.Notes, "recovery read failed: "+err.Error())
			}
		}
		wired := 0
		subjectKnown := true
		perHost := map[string][]attribution.Evidence{}
		// A guest's POWER STATE has one witness (TG-694): see guestPowerWitness. For a guest-lifecycle class a
		// failed subject read in another domain is held here and blinds only if that witness did not cover the
		// subject.
		powerClass := isGuestPowerOn(q.OpClass)
		witnessCovered, markerOnly := false, false
		var deferred []string
		for _, r := range readers {
			if r == nil {
				continue
			}
			wired++
			for _, h := range hosts {
				got, err := r.Read(ctx, h, q.ExecutedAt, q.ClearedAt)
				if err != nil {
					// A failed read of the SUBJECT blinds the quiet reading (ActorsKnown=false). A failed read of the
					// TARGET blinds too — EXCEPT the one answer that means "this reader has no such object" (the pve
					// reader's "not found" for a PVE node name: a node is not a guest): that is noted, not blinding,
					// because the target is a best-effort widening while the subject's own audit trail is the
					// evidence base. A transient failure on the target (a 5xx, a timeout) is a domain another actor
					// could hide in, so it blinds like a subject failure (review of !1819, round 4).
					switch {
					case strings.EqualFold(h, q.Host) && powerClass && r.Domain() != guestPowerWitness:
						deferred = append(deferred, "actor reader "+r.Domain()+" failed for the subject "+h+": "+err.Error())
					case strings.EqualFold(h, q.Host):
						subjectKnown = false
						ev.Notes = append(ev.Notes, "actor reader "+r.Domain()+" failed for the subject "+h+": "+err.Error())
					case strings.Contains(strings.ToLower(err.Error()), "not found"):
						ev.Notes = append(ev.Notes, "actor reader "+r.Domain()+" has no object named like the target "+h+" (noted, not blinding): "+err.Error())
					default:
						subjectKnown = false
						ev.Notes = append(ev.Notes, "actor reader "+r.Domain()+" failed for the target "+h+" (blinding): "+err.Error())
					}
					continue
				}
				if powerClass && r.Domain() == guestPowerWitness && strings.EqualFold(h, q.Host) && len(got) > 0 {
					// A coverage marker or a record: the task log was read for the subject. (Belt and braces with
					// ActorCovered below, which a subject answer with no rows also leaves false.)
					witnessCovered = true
					markerOnly = true
					for _, e := range got {
						if !attribution.IsCoverageMarker(e) {
							markerOnly = false
						}
					}
				}
				perHost[h] = append(perHost[h], got...)
			}
		}
		if witnessCovered && markerOnly && len(deferred) > 0 {
			// Said out loud (review of !1850): with nothing but the marker, the claim rests on "no lifecycle task in
			// [executed, cleared]" plus start-guest's not-running precondition at execute time and the recovery
			// read over [incident, executed]. TG's own start usually precedes executed_at by a few seconds, so it
			// is outside this window and does not appear.
			ev.Notes = append(ev.Notes, guestPowerWitness+" task log: coverage marker only — no lifecycle task on the subject in [executed, cleared]")
		}
		for _, n := range deferred {
			if witnessCovered {
				ev.Notes = append(ev.Notes, n+" (noted, not blinding: a guest's power state is witnessed by the "+
					guestPowerWitness+" task log, which covered the subject — TG-694)")
				continue
			}
			subjectKnown = false
			ev.Notes = append(ev.Notes, n)
		}
		if wired == 0 {
			ev.Notes = append(ev.Notes, "no actor-evidence readers wired")
		} else {
			// The deterministic core (core/attribution) classifies the records PER SUBJECT — it admits only
			// records naming the subject it is asked about, so the incident host and the action's target are each
			// attributed on their own records and the WORST reading wins: TG's own actuation identity is SELF
			// (never another actor), the sanctioned-pool carve-out is AUTHORIZED-TEST (the injector's hand), a
			// sanctioned admin is AUTHORIZED and an unknown principal SUSPICIOUS — the last three are OTHER actors.
			// With more than one candidate taxonomy the core leaves Taxonomy at its zero value and lists them in
			// Candidates, so the fold reads Candidates, never Taxonomy alone. Coverage is read on the SUBJECT: a
			// reader that emitted a coverage marker (or a record) for it affirmatively covers its audit trail;
			// without that the readers are blind, not quiet. The window is widened to cover the whole interval
			// from the execution to now so the core's own in-window filter admits every record returned. The fold
			// runs even when the subject read failed: POSITIVE evidence from the readers that answered is never
			// discarded (the classifier reads the taxonomy regardless of ActorsKnown). A record the core could NOT
			// bind to the queried host (stamped with another identity — a VMID, an FQDN) is counted as unattributed:
			// an actor may hide in it.
			c := cfg
			c.Window = time.Since(q.ExecutedAt) + time.Minute
			c.Now = nil
			worst := attribution.Unattributable
			candidates := map[attribution.Taxonomy]bool{}
			for _, h := range hosts {
				f := attribution.Attribute(h, q.OpClass, perHost[h], nil, c)
				// Advisory domains (REQ-2321: they correlate, never adjudicate — radius, auth-observed) are not action
				// records, so they are neither attributable nor "unattributed"; the core discards them by design.
				records := 0
				for _, e := range perHost[h] {
					if !attribution.IsCoverageMarker(e) && !attribution.IsAdvisoryDomain(e.Domain) {
						records++
					}
				}
				ev.ActorActions += len(f.Evidence)
				if dropped := records - len(f.Evidence); dropped > 0 {
					ev.ActorUnattributed += dropped
				}
				if strings.EqualFold(h, q.Host) && (f.CoveredButEmpty || len(f.Evidence) > 0) {
					ev.ActorCovered = true
				}
				ev.Notes = append(ev.Notes, otherActorNotes(h, q.OpClass, f.Evidence, c)...)
				for _, t := range f.Candidates {
					candidates[t] = true
					if taxonomySeverity(t) > taxonomySeverity(worst) {
						worst = t
					}
				}
				if taxonomySeverity(f.Taxonomy) > taxonomySeverity(worst) {
					worst = f.Taxonomy
				}
			}
			ev.ActorsKnown, ev.ActorTaxonomy, ev.ActorCandidates = subjectKnown, worst.String(), len(candidates)
			if worst == attribution.Unattributable && len(candidates) == 0 {
				ev.ActorTaxonomy = ""
			}
		}
		return ev, nil
	}
}

// otherActorNotes names, for one host, each actor that produced an OTHER-actor reading (authorized-test, authorized,
// suspicious) in the causality window: its taxonomy, domain, a prefix of its identity, how many records it left and
// when the first was. The fold keeps only the worst taxonomy and a candidate count, so without these notes a withheld
// credit cannot say which record withheld it (TG-709: L2's credit read attributed-suspicious and nothing recorded
// why). Each record is classified on its own with the same config, so a self record's pairing marks are honoured.
// Notes only: no reading, taxonomy or decision changes.
func otherActorNotes(host, opClass string, records []attribution.Evidence, cfg attribution.Config) []string {
	type group struct {
		taxonomy, domain, actor string
		n                       int
		first                   time.Time
		selfUnexplained         bool
	}
	var groups []*group
	byKey := map[string]*group{}
	for _, e := range records {
		one := attribution.Attribute(host, opClass, []attribution.Evidence{e}, nil, cfg)
		tax := one.Taxonomy
		if tax != attribution.AttributedSuspicious && tax != attribution.AttributedAuthorized && tax != attribution.AuthorizedTest {
			continue
		}
		k := tax.String() + "|" + e.Domain + "|" + e.Actor
		g, ok := byKey[k]
		if !ok {
			g = &group{taxonomy: tax.String(), domain: e.Domain, actor: e.Actor, first: e.ObservedAt}
			byKey[k] = g
			groups = append(groups, g)
		}
		g.n++
		g.selfUnexplained = g.selfUnexplained || e.SelfUnexplained
		if e.ObservedAt.Before(g.first) {
			g.first = e.ObservedAt
		}
	}
	out := make([]string, 0, len(groups))
	for i, g := range groups {
		if i == maxOtherActorNotes {
			out = append(out, fmt.Sprintf("other actors on %s: %d more group(s) not listed", host, len(groups)-i))
			break
		}
		why := ""
		if g.selfUnexplained {
			why = ", TG's own key with no matching TG execution"
		}
		out = append(out, fmt.Sprintf("%s on %s: %s %s x%d, first %s%s", g.taxonomy, host, g.domain, actorPrefix(g.actor), g.n,
			g.first.UTC().Format(time.RFC3339), why))
	}
	return out
}

const maxOtherActorNotes = 6

func actorPrefix(actor string) string {
	const keep = 28
	clean := strings.Map(func(r rune) rune {
		if r == ';' || unicode.IsControl(r) || r == utf8.RuneError {
			return '_'
		}
		return r
	}, actor)
	if len(clean) <= keep {
		return clean
	}
	cut := keep
	for cut > 0 && !utf8.RuneStart(clean[cut]) {
		cut--
	}
	return clean[:cut] + "..."
}

// taxonomySeverity orders the attribution taxonomies for the worst-of fold: an unsanctioned actor dominates a
// sanctioned one, which dominates the carve-out, which dominates TG's own identity, which dominates nothing seen.
func taxonomySeverity(t attribution.Taxonomy) int {
	switch t {
	case attribution.AttributedSuspicious:
		return 4
	case attribution.AttributedAuthorized:
		return 3
	case attribution.AuthorizedTest:
		return 2
	case attribution.AttributedSelf:
		return 1
	}
	return 0
}

// guestPowerWitness is the one domain that witnesses a guest being POWERED ON (TG-694). A stopped guest cannot start
// itself, and every way to start one — the Proxmox API, `pct`/`qm` on a node, a node's start-all, HA, a restore or
// rollback that starts it, a resume — writes a task into the PVE task log, which the pve reader reads (it is the
// authoritative domain for start-guest in TG-668's table, temporal/runner authoritativeActorDomains). The other
// domains cannot power a guest on: the journal records logins INSIDE the guest, AWX acts only through its
// inventory, NetBox is a record. So for a power-on heal their failure to read the subject — the NIC-less pool
// guests have no journal, no AWX host and no NetBox object — is noted, not blinding, PROVIDED the task log covered
// the subject. Positive evidence from any reader that did answer still counts (an operator's start, the injector's
// carve-out identity). Before this, every start-guest heal on a guest outside AWX or NetBox read `indeterminate`
// and could never be credited (live 2026-11-08: dc1tgpool02/03, terminus_record 7-12).
//
// The rule is for POWER-ON only (review of !1850): a running guest CAN stop or reboot itself from inside with no
// task, so a stop/shutdown/reboot class keeps the strict every-reader rule.
const guestPowerWitness = "pve"

// powerOnClasses are the op-classes whose goal is a guest powered on. Named, not derived from the family: the
// guest-lifecycle family also holds stop-guest, and an overlay may add reboot/shutdown classes to it.
var powerOnClasses = map[string]bool{"start-guest": true}

// isGuestPowerOn reports whether an op-class powers a guest on. An unknown class does not: it keeps the strict rule.
func isGuestPowerOn(opClass string) bool { return powerOnClasses[opClass] }
