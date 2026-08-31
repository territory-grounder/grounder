// Package verify is the deterministic post-execution verifier: the SOLE author of the mechanical
// match / partial / deviation verdict and its typed breakdown. The acting LLM never adjudicates its own
// outcome — ComputeVerdictDetail is the single pure author (ComputeVerdict is its enum-only projection), and
// the prediction/verdict tables grant the model/session role no UPDATE/DELETE.
//
// Provenance: [O] INV-10 (a deterministic verifier is the sole verdict writer; deviation never
// auto-resolves), spec/002 REQ-103/REQ-103a/REQ-104 · [F] the predecessor infragraph.action_verdict logic,
// re-expressed under the typed spine (the ONLY verdict author, target-host excluded).
package verify

import (
	"fmt"
	"sort"
	"strings"

	"github.com/territory-grounder/grounder/core/knowledge"
	"github.com/territory-grounder/grounder/core/safety"
)

// Prediction is the committed machine consequence prediction the verifier diffs against. It names the
// hosts and (host,rule) pairs the infragraph model expected to cascade, plus the action's own target
// host and site so their alerts can be excluded from the causal-surprise test.
type Prediction struct {
	ActionID       string
	PlanHash       string
	TargetHost     string              // the action's own host — its alerting is the expected direct effect
	Site           string              // the action's site — the unit of causal scope
	PredictedHosts map[string]struct{} // hosts the prediction named as possibly cascading
	PredictedRules map[string]struct{} // "host\x00rule" pairs the prediction named
	// HostConfidence is the per-host path-product confidence the estate graph computed for each predicted
	// host (TG-189). ADDITIVE and strictly parallel to PredictedHosts: nothing reads it as membership, so
	// every existing gate, verdict and control comparison keeps operating on the SET exactly as before.
	//
	// WHY IT HAD TO BE CARRIED. estate.BlastRadius already computes a path-product confidence per host —
	// it decays multiplicatively along the dependency walk, so a host two hops out is genuinely less
	// certain than one hop out. core/predict then flattened that to a set, and verify/falsify never saw
	// it. The cost is that "we predicted 44 hosts and 3 alerted" cannot be scored: a model that says
	// "0.95 certain" about 44 hosts and one that says "0.12 certain" about the same 44 are
	// indistinguishable by the set alone, while they are enormously different models.
	//
	// A prediction may legitimately carry an EMPTY map — a flat non-estate graph has no confidence to
	// offer — so consumers must treat absence as "unscored", never as zero confidence. Brier() below is
	// explicit about that: it returns ok=false rather than a 0.0 that would read as a perfect score.
	HostConfidence map[string]float64
}

// Brier returns the Brier score of this prediction against the hosts that actually alerted, plus the
// number of hosts it was computed over.
//
// Brier = mean((confidence - outcome)^2) over every host the prediction named, where outcome is 1 when
// that host alerted and 0 when it did not. LOWER IS BETTER: 0.0 is a perfectly calibrated forecast, 0.25
// is what you get by saying 0.5 about everything, and 1.0 is confident and wrong every time.
//
// IT IS SCORED OVER THE PREDICTED SET, NOT THE ALERTING SET, and that asymmetry is deliberate: this
// measures whether the confidences we ATTACHED were honest. A host that alerted and was never predicted
// is a recall failure, already counted as RealFN by the control scorer — folding it in here would blend
// two different questions into one number.
//
// ok=false when there is nothing to score: no predicted hosts, or no confidences carried (a flat-graph
// model, or a pre-TG-189 row read back from the database). A caller must not render 0.0 in that case —
// "perfectly calibrated" and "never measured" are the two readings this project keeps confusing.
func (p Prediction) Brier(alerted map[string]struct{}) (score float64, n int, ok bool) {
	if len(p.PredictedHosts) == 0 || len(p.HostConfidence) == 0 {
		return 0, 0, false
	}
	var sum float64
	for h := range p.PredictedHosts {
		c, has := p.HostConfidence[h]
		if !has {
			continue // named without a confidence: unscorable, and counting it as 0 would be a fabrication
		}
		outcome := 0.0
		if _, did := alerted[h]; did {
			outcome = 1.0
		}
		d := c - outcome
		sum += d * d
		n++
	}
	if n == 0 {
		return 0, 0, false
	}
	return sum / float64(n), n, true
}

// RuleKey is the canonical key for a (host, rule) pair in PredictedRules. The NUL separator is
// collision-safe here because host and rule are ingest-validated slugs (core/ingest grammar rejects
// control characters, including NUL), so neither field can contain the separator byte.
func RuleKey(host, rule string) string { return host + "\x00" + rule }

// Summary renders the committed prediction as a compact, judge-readable line: its target host, the
// hosts it named as possibly cascading (sorted, deterministic), and how many (host,rule) pairs it
// named. It is the SINGLE source of the prediction's judgeable form — the eval harness and the live
// Runner both render the committed prediction through it, so the offline scorecard and the durable
// session_judgment rows score falsifiable_prediction over an identical string (TG-61).
func (p Prediction) Summary() string {
	hosts := make([]string, 0, len(p.PredictedHosts))
	for h := range p.PredictedHosts {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	return fmt.Sprintf("target=%s; predicted-cascade-hosts=%v; predicted-rule-pairs=%d", p.TargetHost, hosts, len(p.PredictedRules))
}

// ObservedAlert is one alert observed in the verification window after execution.
type ObservedAlert struct {
	Host string
	Rule string
	Site string
}

// RuleMismatch is one observed alert that landed on a PREDICTED host but carried a rule the committed
// prediction did not name — the host was foreseen, its specific failure mode was not. Each mismatch is a
// host-level partial trigger. Host and Rule are the observed alert's ingest-validated slugs.
type RuleMismatch struct {
	Host string
	Rule string
}

// VerdictDetail is the typed, structured result of ONE verification pass: the mechanical Verdict PLUS the
// evidence that produced it — the SurpriseHosts (in-scope, non-target observed hosts the prediction never
// named; each a deviation trigger) and the Mismatches (observed alerts on a predicted host but an
// unpredicted rule; each a partial trigger). Verify-time callers consume this instead of re-diffing the
// prediction against the observation to rediscover which hosts surprised and which rules mismatched.
//
// The Verdict is DERIVED from the two breakdown slices (deviation dominates partial dominates match), so it
// is byte-identical to ComputeVerdict for every input — ComputeVerdict is the enum-only projection of this
// single author (ComputeVerdictDetail), and the acting model still has no path to author it (INV-10). Both
// slices are deduplicated and sorted, so the detail is deterministic for a given (prediction, observation).
type VerdictDetail struct {
	Verdict       safety.Verdict
	SurpriseHosts []string       // sorted, deduplicated surprise hosts — the deviation triggers
	Mismatches    []RuleMismatch // sorted, deduplicated (host,rule) partials — predicted-host / unpredicted-rule triggers

	// SurpriseAlerts is the SAME deviation evidence as SurpriseHosts, carrying the RULE that actually fired.
	//
	// WHY THIS EXISTS (measured 2026-07-30, governance_ledger seq 6555): a mismatch recorded (host,rule) while a
	// surprise — the trigger that DEMOTES an op-class and TRIPS the breaker — recorded only a hostname. So the
	// most consequential verdict carried the thinnest evidence. Diagnosing one real deviation
	// (`surprise-hosts=[dc2lte01]`, which cost restart-container its AUTO level and ~80 clean runs) took six
	// external queries against two LibreNMS instances plus discovering that one of them stores local time —
	// work that host+rule together would have reduced to a single lookup. The alert turned out to be an
	// unrelated 59-second sensor flap on the OTHER site.
	//
	// SurpriseHosts is deliberately left byte-identical rather than replaced: falsify.DiscoveryRecord's
	// DeviationKey is the (target, site, sorted-surprise-hosts) signature that gates "reproduces >= N", and
	// core/estate decay keys its disproof hosts off the same list. Widening either would silently redefine a
	// promotion-gating identity. This field is PURELY additive evidence.
	SurpriseAlerts []SurpriseAlert // sorted, deduplicated (host,rule) deviation triggers
}

// SurpriseAlert is one observed alert on a non-target host the committed prediction never named — a deviation
// trigger, recorded WITH the rule that fired so the deviation is diagnosable after the fact. Host and Rule are
// the observed alert's ingest-validated slugs. One surprise host may carry several rules; each is its own row.
type SurpriseAlert struct {
	Host string
	Rule string
}

// AutoResolvable reports whether this detail's verdict permits auto-resolution — a convenience over the free
// AutoResolvable function. A deviation never auto-resolves (REQ-104, INV-10).
func (d VerdictDetail) AutoResolvable() bool { return AutoResolvable(d.Verdict) }

// Summary renders the detail as a compact, audit-/judge-readable line: the verdict, the surprise hosts (the
// deviation triggers), and the rule mismatches (the partial triggers). Deterministic (both lists are sorted).
// surprise-alerts is appended (never replacing surprise-hosts, which downstream string consumers and the
// judge corpus already parse) so the ledger line names the RULE that triggered the deviation — the field whose
// absence turned one real diagnosis into six external queries across two monitoring instances.
func (d VerdictDetail) Summary() string {
	return fmt.Sprintf("verdict=%s; surprise-hosts=%v; rule-mismatches=%v; surprise-alerts=%v",
		d.Verdict, d.SurpriseHosts, d.Mismatches, d.SurpriseAlerts)
}

// SiteAuthority reports the ESTATE-DERIVED site of a host — the closed host→site vocabulary the verdict's
// coincidental-cross-site filter keys on (spec/002 REQ-107; estate.Graph.SiteOf is the production
// implementation). known=false means the estate makes NO site claim for the host, and the verdict then never
// excludes it (fail closed — the predecessor's `_host_site()` posture: only a host whose site is KNOWN and
// provably different from the target's can be background noise; an unknown-site host might be a genuine
// tunnel cascade). It is NEVER derived from an alert's self-reported ingest `Site` label: that label is a
// free-form slug any deployment may stamp, and trusting it to downgrade a surprise would fail OPEN.
type SiteAuthority func(host string) (site string, known bool)

// ComputeVerdictDetail is the SOLE deterministic author of the mechanical verdict AND its structured
// breakdown, computed in ONE pass over the observed alerts (spec/002 REQ-103a). It diffs the observed alert
// set against the committed prediction with the same exclusion + fail-closed rules as the original verifier:
//
//  1. Exclude the action's own target-host alerts (a rebooted host alerting is the expected direct effect,
//     not a cascade surprise).
//  2. A SURPRISE host — an observed alert on a host the prediction never named — is collected as a
//     SurpriseHost and is a deviation trigger. The verifier does NOT trust an ingest-supplied `Site`
//     label to downgrade it: the label is a free-form ingest slug (any deployment may stamp its own), so a
//     real cascade to a host carrying a third-site label would otherwise be silently swallowed (fail-OPEN).
//     The predecessor derives site from the HOST IDENTITY (a closed nl/gr vocabulary; every other host is
//     site-less and never excluded), and TG restores exactly that mechanic through the estate-derived
//     SiteAuthority (REQ-107): the SCOPED author below excludes a surprise candidate ONLY when the authority
//     knows BOTH the candidate's site AND the target's site and they differ; an unknown-site host is NEVER
//     excluded. This entry point carries no authority, so it excludes nothing — the fully fail-closed floor.
//  3. A predicted host carrying an UNPREDICTED rule is collected as a RuleMismatch — a partial trigger —
//     UNLESS the observed rule is a FAMILY SIBLING of a rule the prediction named for that host (spec/002
//     REQ-108, keyed on the single family authority core/knowledge.CanonicalRule): the same physical fault
//     surfaces under several source rule spellings ("HostDown" vs "Devices-up/down"), and a predicted host
//     failing in the predicted WAY under another source's name is the prediction holding, not degrading.
//
// The verdict is then DERIVED from the collected breakdown — deviation (any surprise) dominates partial (any
// mismatch) dominates match (includes the quiet case where a healthy remediation fires no cascade). Collecting
// the FULL breakdown (rather than short-circuiting on the first surprise, as the enum-only path could) makes
// the detail complete for the caller while leaving the derived verdict byte-identical. pred.TargetHost /
// pred.Site are the action's own validated fields; an empty TargetHost only widens the exclusion, never hides
// a cascade on a named host, so the fail direction stays safe.
func ComputeVerdictDetail(pred Prediction, observed []ObservedAlert) VerdictDetail {
	return ComputeVerdictDetailWithBaseline(pred, observed, nil)
}

// ComputeVerdictDetailWithBaseline is ComputeVerdictDetail with a temporal BASELINE (TG-148): the estate's active
// alerts captured just BEFORE the action executed. An observed alert already present in `baseline` (keyed by
// host+rule) fired BEFORE this action, so it CANNOT be this action's cascade — it is excluded from the
// surprise/mismatch breakdown. Rationale: the post-state Observe is estate-WIDE, so without this an UNRELATED
// alert already firing on a host the prediction never named would be misread as a cascade SURPRISE and DEVIATE —
// force-Shadowing the canary AND demoting the op-class auto→approve on a SUCCESSFUL heal (the observed TG-148
// flywheel break). Only an alert that APPEARED since the action (in `observed` but not `baseline`) can be this
// action's causal effect, so only those are candidate surprises/mismatches. A nil/empty baseline reproduces the
// original estate-wide behavior EXACTLY (that is ComputeVerdictDetail). Fail direction stays safe: a slow cascade
// not yet visible in this immediate pass is caught by the settle-window reconcile that re-observes later; the
// target host is excluded regardless of the baseline.
func ComputeVerdictDetailWithBaseline(pred Prediction, observed, baseline []ObservedAlert) VerdictDetail {
	return ComputeVerdictDetailWithBaselines(pred, observed, baseline, nil)
}

// ComputeVerdictDetailWithBaselines is the full-context author: the TG-148 (host,rule) pair baseline PLUS a
// HOST-level pre-anomalous set — hosts that already held an OPEN incident (a raise with no recovery) when the
// action executed, read from the durable ingest ledger (db.AlertHistoryStore.OpenIncidentHosts).
//
// WHY A SECOND ARM (the 2026-07-28 false deviation, governance ledger 5153-5155): the pair baseline is a
// point-in-time snapshot of the SAME live monitoring surface as the post-state read. In the one second where
// that snapshot was unestablished and the post-read succeeded, a host carrying a stale uncleared alert —
// harness-stopped at 19:47, restarted at 20:07, alert never re-polled — was read as the cascade of a
// start-guest on a DIFFERENT host: verdict deviation, breaker tripped estate-wide, start-guest demoted
// auto→approve, actuation halted 1h49m. The host arm is drawn from TG's own database, so it does not share a
// failure mode with the monitoring HTTP surface, and it is anchored at-or-before the action's execution, so
// nothing the action itself caused can hide in it.
//
// Granularity is deliberate: an already-anomalous HOST is excluded entirely, even on a rule the pair baseline
// never saw — an open incident evolving its rule label is the same incident, not a new cascade. The residual
// (a real cascade landing on an already-broken host) stays invisible to THIS immediate pass and is caught by
// the settle-window reconcile after the incident clears; that fail direction is the one the reconcile already
// owns. Both nil sets reproduce ComputeVerdictDetail exactly. This is the nil-SiteAuthority projection of the
// scoped author below — no cross-site exclusion at all.
func ComputeVerdictDetailWithBaselines(pred Prediction, observed, baseline []ObservedAlert, preAnomalousHosts map[string]bool) VerdictDetail {
	return ComputeVerdictDetailScoped(pred, observed, baseline, preAnomalousHosts, nil)
}

// ComputeVerdictDetailScoped is the FULL-CONTEXT verdict author: the TG-148 (host,rule) pair baseline, the
// host-level pre-anomalous arm (REQ-106), AND the estate-derived coincidental-cross-site filter (REQ-107) —
// the predecessor's `_host_site()` mechanic restored on TG's own terms. A surprise CANDIDATE (an in-window,
// non-baselined alert on a host the prediction never named) is excluded from the deviation evidence ONLY when
// the SiteAuthority knows BOTH the candidate host's site AND the action target's site and they DIFFER — a
// closed, estate-derived vocabulary, never the alert's self-reported ingest label. A host whose site the
// estate does not know is NEVER excluded (fail closed: a genuine cross-site tunnel cascade, or any host
// outside the naming convention, still surfaces as a deviation). A nil authority excludes nothing —
// reproducing ComputeVerdictDetailWithBaselines exactly.
//
// WHY (verdict.go's own record, governance_ledger seq 6555): an unrelated 59-second sensor flap on the OTHER
// site (`dc2lte01`, during an action targeting an dc1 host) scored a DEVIATION, demoted
// restart-container auto→approve and discarded ~80 hands-off clean runs. The predecessor would have excluded
// it — both sites known, different — while still surfacing every unknown-site host. That asymmetry (exclude
// only what the estate can prove is elsewhere; surface everything else) is the entire mechanic.
//
// The filter deliberately applies ONLY to surprise candidates, never to alerts on PREDICTED hosts: a host the
// prediction named is inside the causal claim regardless of its site (a predicted cross-site cascade must
// keep scoring as predicted), so site exclusion can only ever REMOVE deviation evidence about hosts the
// model never implicated — it cannot hide a predicted cascade or manufacture a match out of a mismatch.
func ComputeVerdictDetailScoped(pred Prediction, observed, baseline []ObservedAlert, preAnomalousHosts map[string]bool, siteOf SiteAuthority) VerdictDetail {
	preexisting := make(map[string]struct{}, len(baseline))
	for _, b := range baseline {
		preexisting[RuleKey(b.Host, b.Rule)] = struct{}{} // (host,rule) alerts that fired BEFORE this action
	}
	// The predicted rule FAMILIES per host (REQ-108), keyed on the single family authority
	// (core/knowledge.CanonicalRule — the same map the novelty gate and the recovery belt match on, so one
	// vocabulary governs "is this the same condition" everywhere). Built once per pass; empty when the
	// prediction names no rules.
	famByHost := make(map[string]map[string]struct{}, len(pred.PredictedRules))
	for key := range pred.PredictedRules {
		if i := strings.IndexByte(key, 0); i >= 0 {
			h, r := key[:i], key[i+1:]
			fams := famByHost[h]
			if fams == nil {
				fams = map[string]struct{}{}
				famByHost[h] = fams
			}
			fams[knowledge.CanonicalRule(r)] = struct{}{}
		}
	}
	// The action target's estate-derived site, resolved once (REQ-107). Unknown ⇒ the cross-site filter is
	// inert for this whole pass — with no proven target site there is no "other" site to be on.
	targetSite, targetSiteKnown := "", false
	if siteOf != nil {
		targetSite, targetSiteKnown = siteOf(pred.TargetHost)
	}
	surpriseSet := make(map[string]struct{})
	surpriseAlertSet := make(map[SurpriseAlert]struct{}) // the SAME triggers, keeping the rule (diagnosability)
	mismatchSet := make(map[RuleMismatch]struct{})
	for _, a := range observed {
		if a.Host == pred.TargetHost {
			continue // expected direct effect of the action on its own host
		}
		if _, pre := preexisting[RuleKey(a.Host, a.Rule)]; pre {
			continue // pre-existing alert (fired BEFORE this action) — not this action's cascade (TG-148)
		}
		if preAnomalousHosts[a.Host] {
			continue // host already held an OPEN incident before this action — its alerting is that incident, not this cascade
		}
		if _, named := pred.PredictedHosts[a.Host]; !named {
			if targetSiteKnown {
				if s, known := siteOf(a.Host); known && s != targetSite {
					continue // coincidental cross-site alert — BOTH sites estate-known AND different (REQ-107); unknown is never excluded
				}
			}
			surpriseSet[a.Host] = struct{}{}                                         // surprise host — a deviation trigger (fail-closed; see rationale above)
			surpriseAlertSet[SurpriseAlert{Host: a.Host, Rule: a.Rule}] = struct{}{} // ...and WHICH rule fired
			continue
		}
		if _, named := pred.PredictedRules[RuleKey(a.Host, a.Rule)]; !named {
			if fams, ok := famByHost[a.Host]; ok {
				if _, sameFamily := fams[knowledge.CanonicalRule(a.Rule)]; sameFamily {
					continue // a family sibling of a predicted rule — the predicted failure mode under another source's spelling (REQ-108)
				}
			}
			mismatchSet[RuleMismatch{Host: a.Host, Rule: a.Rule}] = struct{}{} // predicted host, unpredicted rule (different family)
		}
	}
	d := VerdictDetail{
		SurpriseHosts:  sortedHosts(surpriseSet),
		SurpriseAlerts: sortedSurpriseAlerts(surpriseAlertSet),
		Mismatches:     sortedMismatches(mismatchSet),
	}
	// Derive the verdict from the breakdown: deviation dominates partial dominates match. This is the ONE
	// place the verdict decision lives (INV-10) — ComputeVerdict is its enum-only projection.
	switch {
	case len(d.SurpriseHosts) > 0:
		d.Verdict = safety.VerdictDeviation
	case len(d.Mismatches) > 0:
		d.Verdict = safety.VerdictPartial
	default:
		d.Verdict = safety.VerdictMatch
	}
	return d
}

// ComputeVerdict is the enum-only entry point retained for callers that need only the bare verdict. It is a
// thin projection of the single verdict author (ComputeVerdict == ComputeVerdictDetail's Verdict), so the
// deterministic verifier remains the sole writer of the mechanical verdict and the acting LLM never
// adjudicates its own outcome (REQ-103, INV-10). The returned verdict equals the detail's for every input —
// enum and detail can never disagree, because there is exactly one author.
func ComputeVerdict(pred Prediction, observed []ObservedAlert) safety.Verdict {
	return ComputeVerdictDetail(pred, observed).Verdict
}

// AutoResolvable reports whether a verdict permits auto-resolution. A deviation — or any verdict the
// verifier did not validly produce — never auto-resolves (REQ-104, INV-10). match and partial may.
func AutoResolvable(v safety.Verdict) bool {
	return v == safety.VerdictMatch || v == safety.VerdictPartial
}

// sortedHosts returns the set's members as a sorted slice (nil when empty), so a VerdictDetail's SurpriseHosts
// is deterministic regardless of map iteration order.
func sortedHosts(set map[string]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// sortedSurpriseAlerts returns the set's members sorted by (host, rule) (nil when empty), so a VerdictDetail's
// SurpriseAlerts is deterministic regardless of map iteration order — the same property the ledger line and
// the judge-readable Summary depend on.
func sortedSurpriseAlerts(set map[SurpriseAlert]struct{}) []SurpriseAlert {
	if len(set) == 0 {
		return nil
	}
	out := make([]SurpriseAlert, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		return out[i].Rule < out[j].Rule
	})
	return out
}

// sortedMismatches returns the set's members sorted by (host, rule) (nil when empty), so a VerdictDetail's
// Mismatches is deterministic regardless of map iteration order.
func sortedMismatches(set map[RuleMismatch]struct{}) []RuleMismatch {
	if len(set) == 0 {
		return nil
	}
	out := make([]RuleMismatch, 0, len(set))
	for m := range set {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		return out[i].Rule < out[j].Rule
	})
	return out
}
