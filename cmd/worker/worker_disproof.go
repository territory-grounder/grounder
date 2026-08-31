package main

// Disproof-path projection helpers, carved out of main()'s composition root (TG-501 LOC-debt paydown).
// disproofPaths turns each captured prediction-vs-reality deviation into ONE DisproofPath carrying its
// contradiction identity (TG-206a), so a decayed learned edge is attributable to the exact misprediction
// that disproved it; disproofHosts is the flat surprised-host set for the coarse decay pass. Deterministic
// (sorted) output. Behaviour is unchanged by the move; disproof_paths_test.go pins it.

import (
	"sort"

	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/falsify"
)

// disproofHosts collects the deduplicated, sorted hosts a captured verify DEVIATION contradicts — the
// surprise-hosts (observed, unpredicted) plus the rule-mismatch hosts (predicted host, unpredicted rule),
// both read off the typed core/verify.VerdictDetail the falsify Scorer captured into the discovery corpus.
// These feed the estate decay-on-disproof pass; only LEARNED edges incident to them decay, so a surprise
// host with no learned edge is a harmless no-op. Read-only over the snapshot — it never drains the corpus.
// disproofPaths keeps the (target, surprised) PAIRING that disproofHosts throws away (TG-206).
//
// core/falsify.DiscoveryRecord carries TargetHost beside SurpriseHosts — the host the prediction was made
// FROM and the hosts it was surprised BY — and collapsing both into one flat hostname set is what makes
// decay hit every learned edge incident to any surprise host from any capture. An edge between two hosts
// that were each surprised by DIFFERENT incidents was never on either mispredicted path, and decaying it
// punishes a link no prediction got wrong.
//
// Rule mismatches are paired to the same target for the same reason: a predicted-host/unpredicted-rule
// partial is a failure of the path from THAT target.
// One DisproofPath PER CAPTURE (not merged by target), carrying the capture's contradiction identity
// (DeviationKey + ActionID) so a decayed edge can be attributed to the exact misprediction that disproved it
// (TG-206a). The decay pass unions the target→surprise pairs across paths, so the set of edges that decay is
// identical to the merged-by-target form (part b) — only the per-edge attribution is added.
func disproofPaths(captured []falsify.CapturedDeviation) []estate.DisproofPath {
	out := make([]estate.DisproofPath, 0, len(captured))
	for _, cd := range captured {
		t := cd.Record.TargetHost
		if t == "" {
			continue
		}
		set := map[string]struct{}{}
		for _, h := range cd.Record.SurpriseHosts {
			if h != "" && h != t {
				set[h] = struct{}{}
			}
		}
		for _, m := range cd.Record.Mismatches {
			if m.Host != "" && m.Host != t {
				set[m.Host] = struct{}{}
			}
		}
		if len(set) == 0 {
			continue
		}
		sur := make([]string, 0, len(set))
		for h := range set {
			sur = append(sur, h)
		}
		sort.Strings(sur)
		out = append(out, estate.DisproofPath{
			Target: t, Surprised: sur,
			DeviationKey: cd.Record.DeviationKey(), ActionID: cd.Record.ActionID,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Target != out[j].Target {
			return out[i].Target < out[j].Target
		}
		return out[i].DeviationKey < out[j].DeviationKey
	})
	return out
}

func disproofHosts(captured []falsify.CapturedDeviation) []string {
	set := map[string]struct{}{}
	for _, cd := range captured {
		for _, h := range cd.Record.SurpriseHosts {
			if h != "" {
				set[h] = struct{}{}
			}
		}
		for _, m := range cd.Record.Mismatches {
			if m.Host != "" {
				set[m.Host] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}
