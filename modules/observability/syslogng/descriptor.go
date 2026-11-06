package syslogng

import "github.com/territory-grounder/grounder/modules/desc"

// Descriptor publishes the syslog-ng investigation connector's configuration schema so the console can
// GENERATE its dialog rather than hand-render one that drifts from the binary.
//
// ONE FIELD, because one field is what the binary reads. cmd/worker/main.go calls ParseServers on
// TG_SYSLOGNG_DEPLOYMENTS at boot and registers the read-only tools from the result; everything else this
// connector needs — ssh user, key reference, base path, routing prefix — is a column INSIDE that row, not a
// separate env key. Splitting the row into invented per-column fields would render four inputs that no
// composition root reads.
//
// Effect is EffectRestart and that is the honest answer: the server list is parsed once at boot and the
// agent's tool set is built from it there, so a saved change is durable but inert until the worker restarts.
// A dialog claiming otherwise would report a new syslog server as reachable while the agent still had no
// tool for it.
//
// KNOWN GAP, stated so nobody has to rediscover it: every read also requires TG_SYSLOGNG_KNOWN_HOSTS (see
// KnownHostsEnv), and without it the native runner refuses to connect at all — fail closed, by design. It is
// NOT declared here because no composition root reads that literal: main.go passes the journal's known-hosts
// knob to NewNativeRunner and tools.go reads KnownHostsEnv from inside this package, so a field wired to it
// would be a control the binary never sees. It is called out in the help text instead.
func Descriptor() desc.Descriptor {
	return desc.Descriptor{
		Surface:    "observability",
		SourceType: SourceType,
		Title:      "syslog-ng device logs (read-only)",
		Summary: "Gives the triage agent two read-only tools that read a device host's syslog-ng log from " +
			"the site's syslog server over host-key-verified SSH. There is no write path.",
		Fields: []desc.Field{
			{
				// AUTHORITY, not an ordinary endpoint. Each row names a machine TG will open an authenticated
				// SSH session to, presenting the private key its keyref resolves to, and accept device-log
				// text back from. Adding a row extends the set of hosts TG authenticates to and the set of
				// sources whose text reaches the agent — a trust-boundary change, not a settings tweak, and
				// it must not render as a plain text box.
				Name: "deployments", EnvKey: "TG_SYSLOGNG_DEPLOYMENTS", Label: "Syslog servers",
				Help: "One row per site syslog server, ';'-separated: " +
					"site|sshhost|sshuser|keyref|basepath|prefix — e.g. " +
					"\"AA|syslog01.example|root|env:SYSLOGNG_SSH_KEY|/mnt/logs/syslog-ng\". basepath and " +
					"prefix are optional (prefix defaults to the leading site code of the ssh host, which is " +
					"how a device host is routed to its server). keyref is a REFERENCE (env:/file:/store:) — " +
					"never paste key material here, it would land in the config store in plaintext. A row " +
					"missing sshhost, sshuser or keyref is SKIPPED SILENTLY at boot, so a typo costs you that " +
					"site's logs with no error anywhere. Empty means the agent simply has no syslog tools. " +
					"Reads also need the worker's TG_SYSLOGNG_KNOWN_HOSTS set: without it every connection is " +
					"refused rather than made unverified.",
				Type: desc.TypeText, Security: desc.SecAuthority, Effect: desc.EffectRestart,
				MaxLen: 2048,
			},
			{
				// Promoted out of a sentence buried in the help text above ("Reads also need the worker's
				// TG_SYSLOGNG_KNOWN_HOSTS set") into a FIELD an operator can see, set and test — a dialog
				// that tells the operator to go edit an env file is the configuration UX failing at its one
				// job (TG-265).
				Name: "known_hosts", EnvKey: "TG_SYSLOGNG_KNOWN_HOSTS", Label: "known_hosts file",
				Help: "Path (on the worker) to the OpenSSH known_hosts file carrying each syslog server's " +
					"host key, e.g. /secrets/known_hosts. Unset \u21d2 every syslog read is refused rather " +
					"than made unverified; a server ABSENT from the file fails the same way at read time.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				MaxLen: 512,
			},
			{
				// THE PER-SESSION SEARCH BUDGET (TG-297). Declared for the same reason known_hosts above is:
				// cmd/worker/main.go reads this literal through the store-resolving envInt, so a value saved
				// here actually binds. A cap that could only be changed by editing a compose file is a cap
				// nobody tunes — and the failure mode of an untunable-but-too-low cap is an investigation
				// quietly refused its reads, which is precisely what this seam reports to the yield register.
				Name: "search_session_cap", EnvKey: SearchSessionCapEnv, Label: "Log searches per investigation",
				Help: "How many search-host-logs calls ONE investigation may make (default 12). Every other " +
					"bound on that tool is per-call, so without this a single session can ask the device's log " +
					"an unlimited number of yes/no questions — a confirmation oracle over its contents. " +
					"Exceeding the cap REFUSES the search and says so; it never returns an empty result, " +
					"because \"no matches\" and \"I did not look\" must not read the same. Blank or invalid " +
					"⇒ the default; there is no value that disables the cap.",
				Type: desc.TypeText, Security: desc.SecOrdinary, Effect: desc.EffectRestart,
				Pattern: `^[0-9]{1,5}$`, MaxLen: 5,
			},
		},
		// No secret lane. The ssh key is a REFERENCE carried per row inside the field above, and there can be
		// one per site — the lane holds a single value, so there is nothing here it could honestly write.
		Test: desc.TestSpec{
			// Read-only in the strict sense: the connector has no write path at all (INV-08 — returned log
			// text is an untrusted observation, never control flow). The probe stops at the handshake rather
			// than naming a command, because the handshake is what actually fails here — a key reference
			// that will not resolve, a wrong ssh user, or a server whose host key is not in known_hosts.
			Verb: "open a host-key-verified SSH session to each configured syslog server and close it — " +
				"checks the key reference, the ssh user and the host key; no log is read and nothing is written",
			Mutating: false,
		},
	}
}
