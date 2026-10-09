package hostdiag

// The host-diagnostics connector's console face (TG-265 family, owner directive 2026-08-03: "ALL MODULES
// CONFIG VIA THE UX/UI WITH SAVE AND TEST BUTTONS").
//
// Until this file, the connector that SSHes every alerting host was configured by two .env lines and had no
// dialog, no Save, no Test, and no visibility anywhere an operator looks. The cost of that arrived on
// 2026-08-03 (TG-271): its known_hosts covered 16 of the 38 estate hosts TG alerts on, so check-host-services
// failed on every PVE hypervisor, every k8s node and the router — 100% of the time, for weeks, silently. An
// operator who could have SEEN the config in the UX, pressed Test, and read "known_hosts refuses N of your
// hosts" would have caught it the day it broke.

import desc "github.com/territory-grounder/grounder/modules/desc"

// SourceType is this connector's identity on the observability surface.
const SourceType = "hostdiag"

// Descriptor declares the console dialog: what an operator can set, what each field risks, and what the
// TEST button truthfully does.
func Descriptor() desc.Descriptor {
	return desc.Descriptor{
		Surface:    "observability",
		SourceType: SourceType,
		Title:      "host diagnostics (read-only SSH)",
		Summary: "Gives the triage agent four read-only diagnostics (check-host-services / -disk / -memory / " +
			"-load) that SSH the ALERTING host itself and run fixed read-only commands, so a resource alert " +
			"is grounded in the host's own answer instead of escalated blind. There is no write path.",
		Fields: []desc.Field{
			{
				// AUTHORITY, exactly as the syslog-ng rows are: each row names a class of machines TG will
				// open authenticated SSH sessions to as the named user. Widening the glob widens the set of
				// hosts TG logs into — a trust-boundary change, not a settings tweak.
				Name: "deployments", EnvKey: "TG_HOSTDIAG_DEPLOYMENTS", Label: "Host allowlist",
				Help: "One row per site, ';'-separated: site|hostglob|sshuser|keyref — e.g. " +
					"\"dc1|dc1*|root|file:/secrets/one_key\". The glob gates WHICH alerting hosts the " +
					"agent may diagnose; keyref is a REFERENCE (env:/file:/store:) — never paste key material " +
					"here. A row missing a field is SKIPPED silently, so a typo costs that site its " +
					"diagnostics with no error anywhere. Empty means the agent has no host-diagnostics tools " +
					"at all.",
				Type: desc.TypeText, Security: desc.SecAuthority, Effect: desc.EffectRestart,
				MaxLen: 2048,
			},
			{
				// The gate every read passes: host-key verification is mandatory and fails CLOSED, so a host
				// missing from this file is a host the agent cannot diagnose — which is invisible at
				// configure time and surfaces as "unreachable" mid-incident. That asymmetry is why the TEST
				// probe reports the entry count instead of a bare ok.
				Name: "known_hosts", EnvKey: "TG_HOSTDIAG_KNOWN_HOSTS", Label: "known_hosts file",
				Help: "Path (on the worker) to the OpenSSH known_hosts file carrying each estate host's key, " +
					"e.g. /secrets/known_hosts. Unset ⇒ every diagnostic read is refused rather than made " +
					"unverified. A host ABSENT from this file fails exactly the same way at read time — " +
					"keep it covering every host the allowlist can match (grow it with `ssh-keyscan -t " +
					"ed25519 <host>` after verifying the key out-of-band).",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				MaxLen: 512,
			},
		},
		// No secret lane: the ssh key is a REFERENCE carried per row inside deployments, one per site.
		Test: desc.TestSpec{
			// The probe validates the two things that actually failed in production: every row's key
			// reference resolves and parses (the 0640-permissions class), and the known_hosts file is
			// present, parseable, and non-trivial (the 16-of-38-coverage class). It deliberately dials
			// NOTHING: this connector has no fixed target — its targets are whatever hosts alert next — so
			// any host it picked to dial would prove the wrong thing.
			Verb: "resolve and parse each row's SSH key reference, and open the known_hosts file the reads " +
				"verify against, reporting its entry count — no host is dialled and nothing is executed",
			Mutating: false,
		},
	}
}
