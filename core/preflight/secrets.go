// Package preflight is TG's fail-LOUD credential preflight (TG-113). It proves the process's REAL runtime
// user can actually resolve, read, and parse the SSH private key(s) the worker will use for native
// investigation + actuation — BEFORE the process advertises healthy.
//
// The silent-kill it exists to catch: the distroless worker runs as nonroot uid:gid 65532. When
// /secrets/one_key was ABSENT (a re-provision dropped it), or root-owned 0600 (the 65532 worker got
// permission-denied), ALL native SSH investigation + actuation was silently dead — yet the worker booted
// preflight-GREEN and advertised healthy (the failure surfaced only as misleading "hostkey"/"no logs").
// With mutation ON that is a live-safety gap.
//
// The fix is structural: CheckSSHKeys does a real os.ReadFile (via the file: SecretRef resolver) + a real
// ssh.ParsePrivateKey IN THIS PROCESS, so it runs as the process's actual uid. That is the whole point — a
// check that ran as root would falsely PASS exactly where the nonroot worker cannot read the file. It never
// echoes a byte of key material: every failure names only the reference and the resolve/parse reason.
//
// Provenance: [O] INV-13 (secrets are references, resolved in memory, never logged), INV-21/S8-5 (a control
// that cannot work must fail LOUD, never be left dark), P0-9 (fail-closed boot).
package preflight

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/territory-grounder/grounder/core/config"
)

// SSHKeyRef is one named SSH private-key SecretRef to preflight. Name is a human label safe to log; Ref is
// the reference (env:/file:/store:/…), never the key material it points to.
type SSHKeyRef struct {
	Name string
	Ref  config.SecretRef
}

// Failure records one configured SSH key reference that did not resolve, read, or parse as a private key.
// Ref is the reference string (safe to log); Reason is a redacted diagnostic that never contains material.
type Failure struct {
	Name   string
	Ref    string
	Reason string
}

// Report is the outcome of a CheckSSHKeys pass: the names of the refs that resolved+parsed cleanly (OK) and
// the ones that failed (Failures). A ref that was configured empty is skipped and counted in neither.
type Report struct {
	OK       []string
	Failures []Failure
}

// Configured reports how many non-empty SSH key references were actually checked (OK + failed).
func (r Report) Configured() int { return len(r.OK) + len(r.Failures) }

// Failed reports whether any configured SSH key reference is missing/unreadable/unparseable.
func (r Report) Failed() bool { return len(r.Failures) > 0 }

// Ready reports whether every configured SSH key reference resolved and parsed (the healthy state). It is
// true when nothing failed — including the vacuous case where no SSH key is configured at all.
func (r Report) Ready() bool { return len(r.Failures) == 0 }

// Summary renders a single-line, material-free description of the failures for a LOUD log/error line.
func (r Report) Summary() string {
	if len(r.Failures) == 0 {
		if len(r.OK) == 0 {
			return "no SSH key references configured"
		}
		return fmt.Sprintf("all %d configured SSH key ref(s) resolve+parse", len(r.OK))
	}
	parts := make([]string, 0, len(r.Failures))
	for _, f := range r.Failures {
		parts = append(parts, fmt.Sprintf("%s [%s]: %s", f.Name, f.Ref, f.Reason))
	}
	return fmt.Sprintf("%d SSH key ref(s) unusable — %s", len(r.Failures), strings.Join(parts, "; "))
}

// CheckSSHKeys resolves and PARSES each configured SSH key reference in-process (so it runs as the caller's
// real uid — a root-run check would falsely pass where the nonroot worker cannot read the file). For each
// ref it proves, in order, existence + readability (Resolve → os.ReadFile for file: refs) and validity
// (ssh.ParsePrivateKey) — catching the exact missing/permission/known-good-key failure modes. An empty ref
// is skipped (not configured). Every failure names only the REF and a redacted reason, never key material.
func CheckSSHKeys(refs []SSHKeyRef) Report {
	uid, gid := os.Getuid(), os.Getgid()
	var rep Report
	for _, k := range refs {
		ref := strings.TrimSpace(string(k.Ref))
		if ref == "" {
			continue // not configured — nothing to prove
		}
		material, err := k.Ref.Resolve()
		if err != nil {
			// The resolve error is a file-open / env-not-set diagnostic (never material); surface it with the
			// real uid:gid so a permission-denied on a root-owned key reads as itself, not a phantom hostkey.
			rep.Failures = append(rep.Failures, Failure{
				Name:   k.Name,
				Ref:    ref,
				Reason: fmt.Sprintf("did not resolve as uid:gid %d:%d (missing or unreadable): %v", uid, gid, err),
			})
			continue
		}
		if strings.TrimSpace(material) == "" {
			rep.Failures = append(rep.Failures, Failure{Name: k.Name, Ref: ref, Reason: "resolved empty (present but no key material)"})
			continue
		}
		if _, err := ssh.ParsePrivateKey([]byte(material)); err != nil {
			// Name the class of failure without echoing the material (INV-13). A truncated/corrupt key, a
			// public key mistakenly placed here, or an encrypted key with no passphrase all land here.
			rep.Failures = append(rep.Failures, Failure{Name: k.Name, Ref: ref, Reason: "did not parse as an SSH private key"})
			continue
		}
		rep.OK = append(rep.OK, k.Name)
	}
	return rep
}

// SSHKeyRefsFromEnv collects the SSH private-key references a worker is configured to use, from the known
// deployment env vars, deduplicated by reference value (so one_key shared across surfaces is checked once).
// get is the environment accessor (os.Getenv, or the worker's getenv bound to ""). It parses only the
// SSH-key field of each spec — never an api-token / become / basepath field — so it never tries to parse a
// non-key secret as a private key. The formats mirror the live parsers:
//
//   - TG_ACTUATION_SSH_KEY        : a direct SecretRef (the actuation identity; canonically file:/secrets/one_key)
//   - TG_SYSLOGNG_DEPLOYMENTS     : ';'-rows of  site|host|user|KEYREF|basepath      (KEYREF = field 3)
//   - TG_HOSTDIAG_DEPLOYMENTS     : ';'-rows of  site|hostglob|user|KEYREF           (KEYREF = field 3)
//   - TG_CREDENTIAL_NATIVE_RULES  : ';'-rows of  kind:pat|user|port|scheme|KEYREF|…  (KEYREF = field 4, ssh/netconf only)
func SSHKeyRefsFromEnv(get func(string) string) []SSHKeyRef {
	var out []SSHKeyRef
	seen := map[string]bool{}
	add := func(name, ref string) {
		ref = strings.TrimSpace(ref)
		if ref == "" || seen[ref] {
			return
		}
		seen[ref] = true
		out = append(out, SSHKeyRef{Name: name, Ref: config.SecretRef(ref)})
	}

	// 1. The actuation identity — the canonical one_key that the native SSH mutating runner authenticates with.
	add("actuation-ssh (TG_ACTUATION_SSH_KEY)", get("TG_ACTUATION_SSH_KEY"))

	// 2. syslog-ng device-log investigation reads: site|host|user|KEYREF|basepath.
	for _, f := range splitRows(get("TG_SYSLOGNG_DEPLOYMENTS")) {
		if len(f) >= 4 {
			add("syslogng-ssh ("+at(f, 1)+")", f[3])
		}
	}

	// 3. host-diagnostics investigation reads: site|hostglob|user|KEYREF.
	for _, f := range splitRows(get("TG_HOSTDIAG_DEPLOYMENTS")) {
		if len(f) >= 4 {
			add("hostdiag-ssh ("+at(f, 1)+")", f[3])
		}
	}

	// 4. Native credential rules: kind:pattern|user|port|scheme|KEYREF|…. The SSH key field applies only to
	//    the ssh/netconf schemes; an api/winrm rule carries a token/password field there instead, which must
	//    NOT be parsed as a private key.
	for _, f := range splitRows(get("TG_CREDENTIAL_NATIVE_RULES")) {
		if len(f) >= 5 {
			switch strings.ToLower(at(f, 3)) {
			case "ssh", "netconf":
				add("credential-rule "+at(f, 0)+" (TG_CREDENTIAL_NATIVE_RULES)", f[4])
			}
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// splitRows splits a ';'-separated deployment spec into rows, each further split into '|'-trimmed fields.
// Empty rows are dropped.
func splitRows(spec string) [][]string {
	var rows [][]string
	for _, row := range strings.Split(spec, ";") {
		if strings.TrimSpace(row) == "" {
			continue
		}
		f := strings.Split(row, "|")
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		rows = append(rows, f)
	}
	return rows
}

// at safely reads field i from a parsed row, returning "" when the row is too short.
func at(f []string, i int) string {
	if i < 0 || i >= len(f) {
		return ""
	}
	return f[i]
}
