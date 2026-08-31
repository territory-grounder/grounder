package main

import (
	"time"

	"github.com/territory-grounder/grounder/core/metrics"
	"github.com/territory-grounder/grounder/core/policy"
)

// THE WARN-DON'T-BLOCK SUBSYSTEM WAS DARK (TG-506, REQ-1517). policy.WarnFor computes non-blocking warnings
// about a PERMISSIVE operator posture — an allow-all rule, a removed deny-floor, Full-auto mode — that the
// code documents as feeding "the console banner + the audit trail". But WarnFor had ZERO production callers
// (deadcode-confirmed): the operator-owns-the-dial warnings never reached anyone, so an operator running an
// allow-all rule or Full-auto got no signal at all.
//
// Published here on the worker's OWN /metrics (which Prometheus actually scrapes), exactly as the TG-380
// decision stages and the pve-liveness register are. This makes the warnings VISIBLE; it changes NO decision
// — WarnFor is a pure read that never suppresses a proposal or alters a verdict (the propose duty is absolute,
// REQ-2609), and the constitutional never-auto floor beneath the operator layer is untouched. The console
// banner (the operator-UX surface) is a separate follow-on; this closes the observability half so a
// permissive posture is no longer silent.

// policyPostureWarningCodes is the CLOSED set of permissive-posture conditions, emitted EVERY scrape so a 0 is
// legible — "this condition is not active" is distinct from "the metric is dark", the denominator discipline
// TG-380 established. engine-forced-on / engine-disabled require the admin engine-toggle, which is not wired
// in this worker; they stay in the set as the honest frontier — always 0 until the toggle lands, never
// silently missing.
var policyPostureWarningCodes = []policy.WarnCode{
	policy.WarnAllowAllRule,
	policy.WarnFloorRemoved,
	policy.WarnFullAuto,
	policy.WarnEngineForcedOn,
	policy.WarnEngineDisabled,
}

// policyPostureWarningSamples renders the operator-posture warnings onto the scrape surface:
// tg_policy_posture_warning{code} is 1 when that permissive-posture condition is active for the current
// ruleset + mode, 0 when not. Every code in the closed set is emitted, so a quiet posture reads as an
// explicit row of zeros rather than absence — a permissive posture and a dark metric can never be confused.
func policyPostureWarningSamples(warns []policy.PolicyWarning, _ time.Time) []metrics.Sample {
	active := make(map[policy.WarnCode]bool, len(warns))
	for _, w := range warns {
		active[w.Code] = true
	}
	out := make([]metrics.Sample, 0, len(policyPostureWarningCodes))
	for _, c := range policyPostureWarningCodes {
		v := 0.0
		if active[c] {
			v = 1.0
		}
		out = append(out, metrics.Sample{
			Name: "tg_policy_posture_warning", Kind: metrics.Gauge,
			Help: "operator-posture permissive-condition warnings (REQ-1517, TG-506), non-blocking: 1=active " +
				"by code — allow-all-rule / floor-entry-removed / full-auto-mode / engine-forced-on / " +
				"engine-disabled. WarnFor is a pure read; it never suppresses a proposal or alters a verdict, " +
				"and never touches the constitutional never-auto floor. Until now it had no production caller, " +
				"so a permissive posture was silent.",
			Value:  v,
			Labels: map[string]string{"code": string(c)},
		})
	}
	return out
}
