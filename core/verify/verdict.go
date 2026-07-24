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
}

// AutoResolvable reports whether this detail's verdict permits auto-resolution — a convenience over the free
// AutoResolvable function. A deviation never auto-resolves (REQ-104, INV-10).
func (d VerdictDetail) AutoResolvable() bool { return AutoResolvable(d.Verdict) }

// Summary renders the detail as a compact, audit-/judge-readable line: the verdict, the surprise hosts (the
// deviation triggers), and the rule mismatches (the partial triggers). Deterministic (both lists are sorted).
func (d VerdictDetail) Summary() string {
	return fmt.Sprintf("verdict=%s; surprise-hosts=%v; rule-mismatches=%v", d.Verdict, d.SurpriseHosts, d.Mismatches)
}

// ComputeVerdictDetail is the SOLE deterministic author of the mechanical verdict AND its structured
// breakdown, computed in ONE pass over the observed alerts (spec/002 REQ-103a). It diffs the observed alert
// set against the committed prediction with the same exclusion + fail-closed rules as the original verifier:
//
//  1. Exclude the action's own target-host alerts (a rebooted host alerting is the expected direct effect,
//     not a cascade surprise).
//  2. A SURPRISE host — an observed alert on a host the prediction never named — is collected as a
//     SurpriseHost and is ALWAYS a deviation trigger. The verifier does NOT trust an ingest-supplied `Site`
//     label to downgrade it: the label is a free-form ingest slug (any deployment may stamp its own), so a
//     real cascade to a host carrying a third-site label would otherwise be silently swallowed (fail-OPEN).
//     The predecessor derives site from the HOST IDENTITY (a closed nl/gr vocabulary; every other host is
//     site-less and never excluded), so an unrecognized host is likewise never excluded there. TG has no such
//     host->site vocabulary (config-not-code), so it fails CLOSED: a surprise host is a deviation regardless
//     of any label it carries. (A precise coincidental-cross-site filter would key on an estate-derived /
//     operator-configured site vocabulary.)
//  3. A predicted host carrying an UNPREDICTED rule is collected as a RuleMismatch — a partial trigger.
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
	preexisting := make(map[string]struct{}, len(baseline))
	for _, b := range baseline {
		preexisting[RuleKey(b.Host, b.Rule)] = struct{}{} // (host,rule) alerts that fired BEFORE this action
	}
	surpriseSet := make(map[string]struct{})
	mismatchSet := make(map[RuleMismatch]struct{})
	for _, a := range observed {
		if a.Host == pred.TargetHost {
			continue // expected direct effect of the action on its own host
		}
		if _, pre := preexisting[RuleKey(a.Host, a.Rule)]; pre {
			continue // pre-existing alert (fired BEFORE this action) — not this action's cascade (TG-148)
		}
		if _, named := pred.PredictedHosts[a.Host]; !named {
			surpriseSet[a.Host] = struct{}{} // surprise host — a deviation trigger (fail-closed; see rationale above)
			continue
		}
		if _, named := pred.PredictedRules[RuleKey(a.Host, a.Rule)]; !named {
			mismatchSet[RuleMismatch{Host: a.Host, Rule: a.Rule}] = struct{}{} // predicted host, unpredicted rule
		}
	}
	d := VerdictDetail{
		SurpriseHosts: sortedHosts(surpriseSet),
		Mismatches:    sortedMismatches(mismatchSet),
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
// thin projection of the single verdict author ComputeVerdictDetail (ComputeVerdict == that detail's Verdict),
// so the deterministic verifier remains the sole writer of the mechanical verdict and the acting LLM never
// adjudicates its own outcome (REQ-103, INV-10). The returned verdict is byte-identical to the pre-detail
// implementation for every input.
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
