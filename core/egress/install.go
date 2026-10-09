package egress

// install.go — the SHARED half of installing the meter, extracted so a second process cannot get a
// almost-right copy of it (TG-324).
//
// WHY THIS EXISTS. The meter was built for the worker (TG-160) and installed only there. The grounder —
// which is on `tg-egress`, dials OpenBao over HTTPS for its own read credential and the console WRITER
// AppRole, and is the ONLY TG process on the published `tg-frontdoor` — installed nothing. Measured
// 2026-08-07: `grounder:8080/metrics` served 200 with zero `tg_egress_*` series. So the control was
// strongest on the least exposed process and absent on the most exposed one.
//
// The obvious repair — copy installEgressMeter into cmd/grounder — is how the empty-allowlist refusal
// below ends up existing twice and being fixed once. This function is the single decision; each
// composition root keeps only its own getenv calls.
//
// WHAT DELIBERATELY DID NOT MOVE. The `getenv("TG_EGRESS_ALLOW", …)` / `getenv("TG_EGRESS_MODE", …)`
// calls stay in the composition roots, because deploy/envparity_test.go finds a binary's env keys by
// reading the LITERAL first argument of a getenv call in a registered root file. Centralising the reads
// here would hide both keys from that guard for both binaries — and that guard is the one that fails CI
// when a process reads a variable compose never forwards, a gap this repo has shipped three times.

import (
	"net/http"
	"strings"
)

// InstallConfig is what a composition root must supply. Every field is the root's own answer; nothing is
// read from the environment here.
type InstallConfig struct {
	// Environ is the process's effective environment, INCLUDING any operator-saved module settings folded
	// on top. The allowlist is derived from it, so a connector configured entirely through the console
	// must appear here or its legitimate traffic is reported as exfil.
	Environ []string
	// Extra is the raw TG_EGRESS_ALLOW value: destinations the endpoint scan cannot derive (a redirect
	// target, a CDN, an egress proxy). Empty is normal.
	Extra string
	// ModeRaw is the raw TG_EGRESS_MODE value. Anything that is not "enforce" (case-insensitively) is
	// meter, which is the safe default: meter mode changes no request and no response, it only counts.
	ModeRaw string
	// Component names the process in the log lines ("worker", "grounder"), so two installs in one estate
	// are distinguishable in a boot log.
	Component string
	// Logf receives the boot narration. Required — a silent install is how "the meter is on" becomes
	// unfalsifiable.
	Logf func(string, ...any)
}

// Install compiles the allowlist, decides the mode, REPLACES http.DefaultTransport, and narrates what it
// did. It returns the meter so the caller can publish its snapshot on /metrics.
//
// Replacing http.DefaultTransport is the load-bearing line. Measured on this tree, 20+ outbound modules
// build their client as http.DefaultClient or &http.Client{Timeout: …} with no Transport of their own,
// so they resolve to http.DefaultTransport at call time. One install covers the process's whole HTTP
// egress surface INCLUDING connectors not written yet; a per-module hook would have to be remembered
// every time and would be forgotten once.
func Install(cfg InstallConfig) *Meter {
	logf := cfg.Logf
	if logf == nil {
		// Refusing to install would be worse than narrating nowhere — the caller asked for a control.
		logf = func(string, ...any) {}
	}

	declared := DeclaredDestinations(cfg.Environ)
	if extra := strings.TrimSpace(cfg.Extra); extra != "" {
		declared = append(declared, extra)
	}
	allow := NewAllowlist(declared)

	mode := ModeMeter
	if strings.EqualFold(strings.TrimSpace(cfg.ModeRaw), string(ModeEnforce)) {
		// THE GUARD THAT KEEPS AN OPERATOR FROM UNPLUGGING PRODUCTION. Enforcement against an EMPTY
		// allowlist refuses every outbound call the process makes — no model gateway, no estate, no
		// OpenBao — and it would do so silently from the operator's point of view, because "enforce" is
		// exactly what they asked for. An empty allowlist is never a considered decision; it means the
		// endpoint scan found nothing, which is a configuration fault, not a policy. Refuse to enforce
		// and say why, loudly, rather than take the network away on the strength of a typo.
		if allow.Size() == 0 {
			logf("EGRESS[%s]: TG_EGRESS_MODE=enforce was requested but the declared-destination allowlist "+
				"is EMPTY — refusing to enforce (would block EVERY outbound call including OpenBao). "+
				"Staying in meter mode. Declare destinations via the module endpoint settings or "+
				"TG_EGRESS_ALLOW, then re-enable.", cfg.Component)
		} else {
			mode = ModeEnforce
		}
	}

	m := NewMeter(http.DefaultTransport, allow, WithMode(mode), WithLogger(logf))
	// KILLING MUTATION (house rule 2). Deleting this one line leaves core/egress fully built, fully
	// unit-tested and simply not installed — which reproduces the pre-TG-160 world exactly, and is
	// precisely the state the grounder was in until TG-324.
	http.DefaultTransport = m

	if allow.Size() == 0 {
		// Not fatal — a stack with no connectors configured legitimately declares nothing — but it must
		// not read as healthy. Every outbound call will be counted as off-allowlist, and the operator
		// needs to know that is a property of the DECLARATION, not evidence of an intrusion.
		logf("EGRESS[%s]: meter installed with an EMPTY allowlist — every outbound destination will be "+
			"reported off-allowlist (tg_egress_allowlist_rules 0). This is a configuration signal, not an "+
			"alarm.", cfg.Component)
	} else {
		blocked := "NOT blocked"
		if mode == ModeEnforce {
			blocked = "BLOCKED"
		}
		logf("EGRESS[%s]: outbound meter installed over http.DefaultTransport in %s mode; %d declared "+
			"destinations derived from this deployment's own endpoint configuration (TG-160/TG-324). "+
			"Off-allowlist connections are COUNTED and NAMED in the log; they are %s.",
			cfg.Component, mode, allow.Size(), blocked)
	}
	return m
}
