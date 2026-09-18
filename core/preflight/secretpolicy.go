package preflight

import (
	"fmt"
	"sort"
	"strings"

	"github.com/territory-grounder/grounder/core/config"
)

// The secret-policy boot gate (spec/024 REQ-2400/2401/2409) inverts TG's default "plaintext allowed,
// backend optional" posture into a fail-closed "a fresh install can REFUSE plaintext" one. It classifies
// every process secret reference by scheme and, under `enforce`, refuses to boot when any non-exempt
// business secret resolves through a plaintext-bearing scheme (env:/file:/inline literal) rather than a
// real backend (bao:/vault:/store:/vw:/passbolt:). It NEVER resolves or logs a secret value — it inspects
// only the reference scheme.
//
// The permanent exemption set (REQ-2401) is caller-marked (SecretEntry.Exempt) and closed by construction:
// the substrate's own bootstrap credential (it cannot resolve from the substrate it authenticates) and the
// database connection strings needed before any resolver is wired. Everything else must be a backend under
// enforce.
//
// Provenance: [F] owner directive (no plaintext at rest) · [O] INV-13/INV-19/INV-21, spec/024 REQ-2400.

// SecretPolicy is the closed policy enumeration. The zero value is Off (behavior-preserving).
type SecretPolicy int

const (
	// PolicyOff preserves pre-feature behavior: the gate is a no-op. The default.
	PolicyOff SecretPolicy = iota
	// PolicyWarn logs each plaintext non-exempt reference and continues.
	PolicyWarn
	// PolicyEnforce fails the boot (fatal) on any plaintext non-exempt reference.
	PolicyEnforce
)

// ParseSecretPolicy parses the deployment control; an unknown or empty value is Off (the safe,
// behavior-preserving default — a policy typo never silently starts enforcing or stops the process).
func ParseSecretPolicy(s string) SecretPolicy {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "warn":
		return PolicyWarn
	case "enforce":
		return PolicyEnforce
	default:
		return PolicyOff
	}
}

func (p SecretPolicy) String() string {
	switch p {
	case PolicyWarn:
		return "warn"
	case PolicyEnforce:
		return "enforce"
	default:
		return "off"
	}
}

// PermanentExemptions is the CLOSED exemption set (REQ-2401), stated in CODE rather than in a comment.
//
// ★ WHY THIS EXISTS. `SecretEntry.Exempt` is set by whoever builds the entry list, and CheckSecretPolicy used
// to honour it unconditionally. The doc comment claimed the set was "closed by construction" — but nothing
// closed it: any caller could mark ANY secret exempt and the gate would permit plaintext for it. The
// 2026-07-29 audit put it plainly: `TG_ACTUATION_SSH_KEY` — the one key that mutates the estate — could be
// flipped to Exempt and the whole suite stayed green. The property lived in prose and in the discipline of
// whoever edited the caller, which is not a control.
//
// This matters today, not hypothetically: the live deployment runs TG_SECRET_POLICY=enforce, so the gate is
// what stands between a plaintext business secret and a refused boot.
//
// MEMBERSHIP IS DELIBERATELY NARROW, and each entry states why it CANNOT resolve from a backend:
//   - the OpenBao bootstrap credentials: they authenticate TO the backend, so they cannot come FROM it
//   - the seal keys: they unwrap what the backend hands back
//   - the CA certificates and public identifiers: public material, not secrets at all
//
// Adding a name here is a governed decision (REQ-2401) and must survive the question this set exists to ask:
// can this reference resolve from a backend? If yes, it is not exempt — it is unmigrated.
var PermanentExemptions = map[string]bool{
	// Substrate bootstrap — authenticate to OpenBao, so they cannot be stored in OpenBao.
	"TG_OPENBAO_TOKEN_REF":      true,
	"TG_OPENBAO_ROLE_ID_REF":    true,
	"TG_OPENBAO_SECRET_ID_REF":  true,
	"TG_OPENBAO_WRAP_TOKEN_REF": true,
	"TG_OPENBAO_JWT_REF":        true,
	// The CONSOLE WRITER's AppRole (TG-252). Same class and the same reason: it authenticates TO OpenBao,
	// so it cannot be stored in OpenBao. It is a SEPARATE identity from TG's own read credential above —
	// deliberately, because that one can read every operational secret and this one must not.
	"TG_OPENBAO_WRITER_ROLE_ID_REF":   true,
	"TG_OPENBAO_WRITER_SECRET_ID_REF": true,
	// Seal material — unwraps what the backend returns, so it cannot itself be behind the backend.
	"TG_SEAL_KEY_REF":           true,
	"TG_SEAL_TRANSIT_TOKEN_REF": true,
	// Public material — a CA certificate and public identifiers carry no secret at all.
	"TG_OPENBAO_CA":          true,
	"TG_LDAP_CA":             true,
	"TG_OIDC_CLIENT_ID_REF":  true,
	"TG_LANGFUSE_PUBLIC_REF": true,
}

// SecretEntry is one named process secret reference to police. Name is a human label safe to log; Ref is
// the reference (env:/file:/bao:/…), never the value. Exempt marks a member of the permanent exemption set
// (REQ-2401) — the only refs allowed to remain plaintext under enforce.
type SecretEntry struct {
	Name   string
	Ref    config.SecretRef
	Exempt bool
}

// SecretPolicyReport is the outcome of a policy pass: the plaintext non-exempt violations (each carrying
// only the name + scheme, never the value), plus the exempt-plaintext refs recorded for transparency.
type SecretPolicyReport struct {
	Violations []SecretViolation
	Exempted   []string // "name (scheme)" for exempt refs that are plaintext — allowed, but surfaced
	// Exemplar is one ALREADY-COMPLIANT reference from this same process, as "NAME=ref".
	//
	// It exists because the error this report produces used to be a diagnosis without an instruction.
	// TWENTY-ONE business secrets default to `env:<NAME>` in cmd/worker, and deploy/.env.example documents
	// eighteen of them that way — so on a deployment running policy=enforce, following the shipped
	// documentation is a fatal boot. The operator is then told "move them to a secret backend" and left to
	// invent the exact path themselves.
	//
	// The deployment almost always already contains the answer: another secret that IS on a backend. Citing
	// it turns "use a backend" into "use the one you are already using, like this". A reference is a
	// LOCATION, never a value — the same reason Exempted may carry names and schemes.
	Exemplar string

	// The reach of the process-env SHAPE scan (TG-284), carried so the boot log can distinguish "looked at
	// 300 variables, 61 secret-shaped, found nothing" from "looked at nothing". See EnvShapeReport.
	EnvScan EnvShapeReport
}

// SecretViolation is one non-exempt reference resolving through a plaintext-bearing scheme.
//
// UnclaimedExemption distinguishes the two ways a plaintext business secret gets past the gate. A plain
// violation is an operator who has not migrated a ref to a backend yet. An UNCLAIMED EXEMPTION is a caller
// asserting a permanent exemption it does not hold — the mechanism by which ANY secret could be excused, so
// it is named separately and never folded into the ordinary count.
//
// RawPlaintext marks the third way, the one the gate was blind to until TG-284: not a reference at all, but
// a secret-SHAPED process env var holding a raw credential VALUE. It is named separately because the
// remediation differs — an ordinary violation is repointed to a backend, a raw value must be MOVED to one
// and the plaintext variable deleted.
type SecretViolation struct {
	Name               string
	Scheme             string
	UnclaimedExemption bool
	RawPlaintext       bool
}

func (r SecretPolicyReport) Clean() bool { return len(r.Violations) == 0 }

// CheckSecretPolicy classifies each entry by scheme (never resolving a value). A reference is COMPLIANT
// when it resolves through a backend scheme (bao:/vault:/store:/vw:/passbolt:); a plaintext-bearing scheme
// (env:/file:/literal) is a VIOLATION unless the entry is Exempt. An empty ref is skipped (an unconfigured
// optional secret is not a plaintext violation — the feature is simply off). The report is deterministic
// (sorted by name).
func CheckSecretPolicy(entries []SecretEntry) SecretPolicyReport {
	var rep SecretPolicyReport
	for _, e := range entries {
		scheme := config.SchemeOf(e.Ref)
		if scheme == "empty" {
			continue // an unset optional secret — the feature is simply off, not a plaintext violation
		}
		if config.IsBackendScheme(e.Ref) {
			// Already compliant. Keep the FIRST one by name as the exemplar the error will cite, so an
			// operator is shown their own working pattern rather than a generic shape.
			if rep.Exemplar == "" || e.Name < strings.SplitN(rep.Exemplar, "=", 2)[0] {
				rep.Exemplar = e.Name + "=" + string(e.Ref)
			}
			continue
		}
		// env: / file: / literal — plaintext-bearing.
		if e.Exempt {
			if !PermanentExemptions[e.Name] {
				// ★ AN EXEMPTION IS NOT SOMETHING A CALLER MAY ASSERT. See PermanentExemptions.
				rep.Violations = append(rep.Violations, SecretViolation{
					Name: e.Name, Scheme: scheme, UnclaimedExemption: true,
				})
				continue
			}
			rep.Exempted = append(rep.Exempted, fmt.Sprintf("%s (%s)", e.Name, scheme))
			continue
		}
		rep.Violations = append(rep.Violations, SecretViolation{Name: e.Name, Scheme: scheme})
	}
	sort.Slice(rep.Violations, func(i, j int) bool { return rep.Violations[i].Name < rep.Violations[j].Name })
	sort.Strings(rep.Exempted)
	return rep
}

// CheckSecretPolicyWithEnv is the gate BOTH binaries boot on: the declared-reference pass (CheckSecretPolicy)
// plus the process-env SHAPE pass (CheckEnvShape) folded into one report.
//
// It exists because the two passes answer different questions and only their union is the question the
// operator actually asked. CheckSecretPolicy answers "is every secret I DECLARED pointed at a backend?" —
// which is green on a process holding two raw 32/64-char API tokens nobody declared (the live 2026-08-04
// worker). CheckEnvShape answers "is there a raw credential in this process at all?". Calling only the first
// is how a control ends up asserting the absence of what it cannot see.
//
// environ is os.Environ() at the call site — the REAL process environment, not a curated list, for the same
// reason preflight.CheckSSHKeys does its os.ReadFile in-process: a check run against a description of the
// environment passes exactly where the real one would fail.
func CheckSecretPolicyWithEnv(entries []SecretEntry, environ []string) SecretPolicyReport {
	rep := CheckSecretPolicy(entries)
	rep.EnvScan = CheckEnvShape(environ, entries)
	rep.Violations = append(rep.Violations, rep.EnvScan.Violations...)
	sort.Slice(rep.Violations, func(i, j int) bool { return rep.Violations[i].Name < rep.Violations[j].Name })
	return rep
}

// EnvScanSummary is the one-line boot record of the shape scan's REACH, for the log both binaries write
// whenever the policy is not off. It states what was LOOKED AT, not only what was found: a scan that has
// silently stopped matching prints "0 scanned" here, which no longer reads like a clean deployment. The
// excused names are printed in full because an exemption nobody can see is the defect this ticket fixed.
func (r SecretPolicyReport) EnvScanSummary() string {
	s := r.EnvScan
	out := fmt.Sprintf("env shape scan: %d var(s) scanned, %d secret-shaped, %d raw plaintext violation(s); "+
		"%d already policed by reference", s.Scanned, s.Shaped, len(s.Violations), len(s.Accounted))
	if len(s.InheritedExempt) > 0 {
		out += fmt.Sprintf(", %d exempt via bootstrap reference (%s)", len(s.InheritedExempt), strings.Join(s.InheritedExempt, ", "))
	}
	if len(s.Allowlisted) > 0 {
		out += fmt.Sprintf(", %d allowlisted as non-secret (%s)", len(s.Allowlisted), strings.Join(s.Allowlisted, ", "))
	}
	// The shape rule's blind spot, stated as its own sentence rather than folded into the counts above. It is
	// NOT a violation yet and the wording says so explicitly, because a reader who sees a credential named in
	// a gate's output will otherwise assume the gate refused it.
	if len(s.InlineURLCredential) > 0 {
		out += fmt.Sprintf(". SEPARATELY, %d var(s) carry a password INSIDE a URL (%s) — the name declares "+
			"no credential so the shape rule never examined them, and this gate has NOT refused them. Move "+
			"each behind a reference; promoting these to violations while enforce is live would refuse the "+
			"boot", len(s.InlineURLCredential), strings.Join(s.InlineURLCredential, ", "))
	}
	return out
}

// EnforceSecretPolicy applies a policy to a report and returns a fatal error under enforce when there are
// violations, or nil otherwise. Under warn it returns nil (the caller logs rep.Violations); under off it is
// a no-op. The error names only references and schemes — never a secret value.
func EnforceSecretPolicy(policy SecretPolicy, rep SecretPolicyReport) error {
	if policy != PolicyEnforce || rep.Clean() {
		return nil
	}
	names := make([]string, 0, len(rep.Violations))
	raw := 0
	for _, v := range rep.Violations {
		if v.RawPlaintext {
			// No trailing colon: a raw value has no scheme, and the text before its first colon must never
			// be printed (it would be a fragment of the credential). Name the SHAPE instead.
			raw++
			names = append(names, fmt.Sprintf("%s (raw value in process env)", v.Name))
			continue
		}
		names = append(names, fmt.Sprintf("%s (%s:)", v.Name, v.Scheme))
	}
	// The instruction, not just the diagnosis. Cite a reference this deployment ALREADY resolves through a
	// backend so the operator can copy its shape, and fall back to the generic form when there is none.
	how := "move them to a secret backend (e.g. bao:<kv-mount>/data/<prefix>/<name>#token) or add to the " +
		"permanent exemption set"
	if rep.Exemplar != "" {
		how = fmt.Sprintf("this deployment already resolves %s — give these the same treatment, or add to "+
			"the permanent exemption set", rep.Exemplar)
	}
	if raw > 0 {
		// A raw value needs a step the reference case does not: DELETING the plaintext variable. Repointing
		// the reference while the value stays in the env leaves the credential exactly where it was, which
		// is the state this gate was green over for months.
		how += fmt.Sprintf("; the %d raw value(s) must ALSO have their plaintext variable removed from the "+
			"deployment env — a reference repointed while the value stays behind moves nothing (or set "+
			"TG_SECRET_POLICY=warn while migrating)", raw)
	}
	return fmt.Errorf("secret policy=enforce: %d plaintext business secret(s) — %d declared reference(s) on a "+
		"plaintext-bearing scheme instead of a backend (bao:/vault:/store:) and %d raw credential value(s) in "+
		"the process env: %s — %s",
		len(rep.Violations), len(rep.Violations)-raw, raw, strings.Join(names, ", "), how)
}
