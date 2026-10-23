package hostdiag

// THE READ-ONLY GRAMMAR, RENDERED FOR THE HOST GUARD (TG-280).
//
// THE DEFECT THIS SERVES. The host-diagnostics lane reads every estate host as ROOT using
// /secrets/one_key — the unrestricted estate root key, mode 0640 root:65532, mounted readable into the
// worker. A worker compromise that reads that file drives a stock SSH client as root on the whole fleet,
// with every TG gate bypassed, because TG's gates only bind commands TG itself constructs. It is also
// AWX's estate credential, so its blast radius is not "the hosts TG reads" — it is everything.
//
// The remedy that actually holds is the same one the actuation lane already uses: constrain the KEY in
// the server's sshd with `restrict,command="…"`, so the key can run exactly the vetted grammar and
// nothing else. That guard needs the exact SSH_ORIGINAL_COMMAND strings this lane sends.
//
// WHY GENERATED, NEVER HAND-AUTHORED. tools/guardallow exists because the actuation allowlist WAS
// hand-authored, drifted from the op-class registry, and TG autonomously chose a `systemctl start` that
// cleared all six of its own gates and then died at the host with exit 42. A hand-copied list of these
// twelve commands would drift the same way, and the failure mode here is worse: hostdiag fails by
// returning a sentinel string that looks like a normal answer, so a drifted allowlist would make the
// agent silently blind rather than loudly broken — exactly TG-271, which ran for weeks unnoticed.
//
// So the catalogue is the single source, and the allowlist is a projection of it.

import "github.com/territory-grounder/grounder/modules/observability/syslogng"

// ReadOnlyWireCommands returns every SSH_ORIGINAL_COMMAND this module can send, in catalogue order,
// deduplicated. The rendering MUST match the runner's byte-for-byte — both go through
// syslogng.RemoteCommand, so there is one quoting implementation and the guard cannot disagree with the
// client about what was sent.
func ReadOnlyWireCommands() []string {
	seen := make(map[string]struct{}, 16)
	out := make([]string, 0, 16)
	for _, c := range checks {
		for _, s := range c.steps {
			cmd := syslogng.RemoteCommand(s.argv)
			if _, dup := seen[cmd]; dup {
				continue
			}
			seen[cmd] = struct{}{}
			out = append(out, cmd)
		}
	}
	return out
}

// ReadOnlyCheckCount reports how many named checks the catalogue holds. It exists for the generator's
// vacuity floor: an allowlist rendered from an empty catalogue would install cleanly and then deny every
// read, and this lane's failures are silent.
func ReadOnlyCheckCount() int { return len(checks) }
