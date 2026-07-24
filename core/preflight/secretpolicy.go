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
}

// SecretViolation is one non-exempt reference resolving through a plaintext-bearing scheme.
type SecretViolation struct {
	Name   string
	Scheme string
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
		if scheme == "empty" || config.IsBackendScheme(e.Ref) {
			continue // an unset optional secret, or an already-compliant backend ref
		}
		// env: / file: / literal — plaintext-bearing.
		if e.Exempt {
			rep.Exempted = append(rep.Exempted, fmt.Sprintf("%s (%s)", e.Name, scheme))
			continue
		}
		rep.Violations = append(rep.Violations, SecretViolation{Name: e.Name, Scheme: scheme})
	}
	sort.Slice(rep.Violations, func(i, j int) bool { return rep.Violations[i].Name < rep.Violations[j].Name })
	sort.Strings(rep.Exempted)
	return rep
}

// EnforceSecretPolicy applies a policy to a report and returns a fatal error under enforce when there are
// violations, or nil otherwise. Under warn it returns nil (the caller logs rep.Violations); under off it is
// a no-op. The error names only references and schemes — never a secret value.
func EnforceSecretPolicy(policy SecretPolicy, rep SecretPolicyReport) error {
	if policy != PolicyEnforce || rep.Clean() {
		return nil
	}
	names := make([]string, 0, len(rep.Violations))
	for _, v := range rep.Violations {
		names = append(names, fmt.Sprintf("%s (%s:)", v.Name, v.Scheme))
	}
	return fmt.Errorf("secret policy=enforce: %d business secret(s) resolve through a plaintext-bearing scheme "+
		"instead of a backend (bao:/vault:/store:) — move them to a secret backend or add to the permanent "+
		"exemption set: %s", len(rep.Violations), strings.Join(names, ", "))
}
