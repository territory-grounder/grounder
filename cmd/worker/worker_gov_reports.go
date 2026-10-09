package main

// Boot-time GOVERNANCE-REPORT helpers, carved out of main()'s composition root (TG-501 LOC-debt paydown).
// Each folds a boot-time configuration/wiring finding into ONE append-only governance-ledger row (or
// writes nothing when there is no gap), so a condition that silently withdraws autonomy is durable and
// served rather than a log line nobody retains. The APPEND GUARDS write through the narrow govAppender
// seam so the 'only append on a real gap' rule is unit-tested. Behaviour is unchanged by the move; the
// call sites (append*Report) stay in main().

import (
	"fmt"
	"strings"

	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/wiring"
)

// pveTLSFlagDisagreement reports whether the TWO env vars that both govern "skip TLS verification when
// talking to Proxmox" disagree, and which paths each one actually controls.
//
// THEY ARE READ BY THE SAME PROCESS FOR THE SAME DECISION:
//
//	TG_PVE_INSECURE      -> the estate reader and the PVE ACTOR-EVIDENCE reader
//	TG_PROXMOX_INSECURE  -> the actuation path
//
// and the pve-liveness detector follows WHICHEVER credential pair it resolved (TG-350), which is why that
// side is a parameter rather than a constant here. It used to be nailed to TG_PROXMOX_INSECURE in this
// comment and in the message; once the detector could read with the estate pair, a fixed attribution would
// have sent an operator to verify the wrong flag — the exact failure this report exists to prevent.
//
// So an operator who sets only one gets TLS skipped on half of TG's Proxmox conversations and enforced on
// the other half, against the same self-signed endpoint. The paths that still verify fail their requests,
// and the failure does not look like TLS: an actor-evidence reader that cannot reach its backend contributes
// NO evidence, which reads downstream as `unattributable` — indistinguishable from "nobody touched this
// host". That is a plausible reading of the standing "the PVE reader has returned zero rows" observation.
//
// THIS REPORTS AND CHANGES NOTHING. Making either flag imply the other would alter a security posture
// without being asked: OR-ing them would DISABLE verification on paths that currently verify, and requiring
// agreement would START verifying on paths that currently skip, breaking a working install at boot. Both are
// decisions for the operator, so the code states the disagreement and leaves the behaviour exactly as it is.
func pveTLSFlagDisagreement(pveInsecure, proxmoxInsecure bool, livenessFlagKey string) (disagree bool, detail string) {
	if pveInsecure == proxmoxInsecure {
		return false, ""
	}
	set, unset := "TG_PVE_INSECURE", "TG_PROXMOX_INSECURE"
	setPaths, unsetPaths := "estate reader + PVE actor-evidence reader", "actuation"
	if livenessFlagKey == "TG_PVE_INSECURE" {
		setPaths += " + pve-liveness detector"
	} else {
		unsetPaths += " + pve-liveness detector"
	}
	if proxmoxInsecure {
		set, unset = unset, set
		setPaths, unsetPaths = unsetPaths, setPaths
	}
	return true, fmt.Sprintf(
		"%s is TRUE (TLS verification skipped for: %s) while %s is FALSE/UNSET (TLS verification ENFORCED for: %s). "+
			"Both govern the same Proxmox endpoint from this same process. The enforcing paths will fail against a "+
			"self-signed endpoint, and a failed actor-evidence read contributes no evidence — which reads as "+
			"'unattributable', not as an error", set, setPaths, unset, unsetPaths)
}

// configGapReport folds the boot-time configuration findings into ONE governance-ledger reason, or reports
// that there is nothing to say.
//
// WHY THE LEDGER AND NOT ANOTHER LOG LINE. The three checks that produce these findings — carve-out host
// coverage, armed-reader identity gaps, and the Proxmox TLS flag disagreement — each detect a condition that
// silently withdraws autonomy or breaks half of TG's Proxmox traffic. All three reported only via log.Printf
// at boot, and NO HTTP surface carries worker stdout (core/httpapi/router.go serves /v1/events and
// /v1/ledger; the console's Logs·Evidence view is fed by the ledger and ingest alerts). So the findings
// existed only in `docker logs` on the host — invisible from every surface an operator actually opens, which
// is the opposite of what a diagnostic is for.
//
// The governance ledger is already served, already append-only and hash-chained (INV-19), and is already the
// audit surface for "why did the system behave this way". A finding placed there gets a timestamp and a chain
// position for free, and needs no new route.
//
// ONE ENTRY PER BOOT, AND ONLY WHEN THERE IS A GAP. The worker restarts on every deploy, so an unconditional
// append would spam an append-only ledger that cannot be pruned. A correctly-configured system writes
// nothing; a misconfigured one writes exactly one summarising row per boot, which is proportionate to a
// condition that persists until someone fixes the config.
//
// Withheld is deliberately FALSE. That flag means "autonomy was withheld for this decision" and feeds the
// withheld-rate metrics; a boot-time observation is not a per-action decision, and marking it withheld would
// inflate a governance number with a configuration note.
func configGapReport(uncoveredHosts []string, domainGaps []attribution.DomainConfigGap, tlsDetail string,
	expiryRisks []attribution.ExpiryRisk) (string, bool) {
	var parts []string
	if len(uncoveredHosts) > 0 {
		parts = append(parts, fmt.Sprintf("carve-outs do not cover %d allowlisted guest(s) [%s] — a harness cycle on an uncovered guest escalates to a human instead of resolving as authorized-test",
			len(uncoveredHosts), strings.Join(uncoveredHosts, " ")))
	}
	for _, g := range domainGaps {
		switch {
		case g.NoSelfActor && g.NoSanctioned:
			parts = append(parts, fmt.Sprintf("domain %q armed with NO self-actor and NO sanctioned principals — every actor there reads attributed-suspicious, INCLUDING TG's own actions. Self-identity is CODE (derived from that domain's credential at the composition root; only \"pve\" is wired), sanctioned principals are RULESET", g.Domain))
		case g.NoSelfActor:
			parts = append(parts, fmt.Sprintf("domain %q armed with NO self-actor — TG's OWN actions there read attributed-suspicious and escalate as a security event. Self-identity is CODE, not ruleset: derive it from that domain's credential at the composition root (only \"pve\" is wired)", g.Domain))
		case g.NoSanctioned:
			parts = append(parts, fmt.Sprintf("domain %q armed with NO sanctioned principals — every non-TG actor there reads attributed-suspicious", g.Domain))
		}
	}
	// ★ A BOUND THAT DEGRADES THE WRONG WAY IS WORSE THAN NO BOUND, because it fires on a date nobody is
	// watching. A carve-out whose actors are not sanctioned in its domain resolves attributed-suspicious once
	// the window closes — a SECURITY escalation on every ordinary admin action across its hosts — where the
	// requirement's stated safe direction is a stand-down. Sanctioning the actor is inert while the window is
	// open (the carve-out has precedence), so this is a gap with a remedy that costs nothing to apply early.
	for _, r := range expiryRisks {
		until := "no bound"
		if !r.ValidUntil.IsZero() {
			until = r.ValidUntil.Format("2006-01-02")
		}
		parts = append(parts, fmt.Sprintf(
			"carve-out %q (domain %q) expires %s and its actor(s) [%s] are NOT sanctioned in that domain — on "+
				"expiry they resolve attributed-suspicious (SECURITY escalation on every ordinary action there) "+
				"instead of attributed-authorized (stand-down); declare them as sanctioned principals now, which "+
				"changes nothing while the carve-out is valid",
			r.CarveOutID, r.Domain, until, strings.Join(r.Actors, " ")))
	}
	if tlsDetail != "" {
		parts = append(parts, "Proxmox TLS flags disagree — "+tlsDetail)
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, " | "), true
}

// govAppender is the narrow seam appendConfigGapReport writes through, so the APPEND GUARD itself is
// testable. Without it the "only append when there is a gap" rule lived inline in main() and nothing
// exercised it: a mutation that appended on every boot passed the whole suite, because the tests covered the
// pure report assembler and not the caller that decides whether to write.
type govAppender interface {
	Append(audit.GovDecision) (audit.LedgerEntry, error)
}

// appendWiringDarkReport writes ONE governance-ledger row naming every DARK wiring seam at boot, and
// writes nothing when every seam is live.
//
// It exists because a log line is not a record. The defect that motivated the wiring package was
// discovered by reading container logs by hand, days later: deps.Notify was nil, every governance notice
// degraded to log.Printf, and the only trace a judge-death page ever fired was a line in stdout that
// nothing watched and nothing retained. A ledger row is durable, hash-chained, and served.
func appendWiringDarkReport(l govAppender, findings []wiring.Finding) error {
	if l == nil {
		return nil
	}
	reason := wiring.DarkReport(findings)
	if reason == "" {
		return nil
	}
	if _, err := l.Append(audit.GovDecision{
		Decision: "wiring:dark-seam-at-boot",
		Reason:   reason,
		ActionID: "wiring-dark-report",
		// Withheld: a dark seam IS autonomy withheld — the one channel allowed to say no could not
		// reach anyone. Recording it any other way would file the outage as routine bookkeeping.
		Withheld: true,
	}); err != nil {
		return err
	}
	return nil
}

// appendConfigGapReport writes ONE governance-ledger row when — and only when — a boot-time config gap
// exists. Returns nil and writes nothing on a clean config or a nil ledger.
//
// The ledger is append-only and hash-chained and the worker restarts on every deploy, so an unconditional
// append would grow the audit spine with a row per boot for a system with nothing wrong, training readers to
// skip config rows. Best-effort by contract: an append failure is returned for logging and never blocks boot,
// because a diagnostic that can stop the control plane is a worse defect than the gap it reports.
func appendConfigGapReport(l govAppender, uncoveredHosts []string, domainGaps []attribution.DomainConfigGap, tlsDetail string,
	expiryRisks []attribution.ExpiryRisk) error {
	if l == nil {
		return nil
	}
	reason, any := configGapReport(uncoveredHosts, domainGaps, tlsDetail, expiryRisks)
	if !any {
		return nil
	}
	if _, err := l.Append(audit.GovDecision{
		Decision: "config:gap-at-boot",
		Reason:   reason,
		ActionID: "config-gap-report",
	}); err != nil {
		return err
	}
	return nil
}
