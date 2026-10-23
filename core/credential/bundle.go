package credential

import (
	"fmt"

	"github.com/territory-grounder/grounder/core/config"
)

// ConnectionScheme is how TG reaches a target. Fixed, closed vocabulary (REQ-1601): an unknown scheme is a
// construction error, never a silent default.
type ConnectionScheme string

const (
	SchemeSSH     ConnectionScheme = "ssh"
	SchemeAPI     ConnectionScheme = "api"
	SchemeWinRM   ConnectionScheme = "winrm"
	SchemeNetconf ConnectionScheme = "netconf"
)

func (s ConnectionScheme) valid() bool {
	switch s {
	case SchemeSSH, SchemeAPI, SchemeWinRM, SchemeNetconf:
		return true
	default:
		return false
	}
}

// Bundle is a resolved machine → host identity (REQ-1601): the credential TG authenticates a target with.
// Its secret-bearing fields (sshKeyRef, become, apiTokenRef) are core/config.SecretRef REFERENCES — never a
// plaintext value (INV-13, REQ-1603). The fields are UNEXPORTED and the only constructor is NewBundle, so a
// Bundle cannot be built with a struct literal from another package: an under-populated bundle is a
// construction error, and the ZERO VALUE is invalid (ok=false) — "no blank identity" is structural, not a
// runtime check that can be skipped.
type Bundle struct {
	ok bool // false for the zero value — an invalid/unresolved bundle; set only by NewBundle

	user   string
	port   int
	scheme ConnectionScheme

	sshKeyRef   config.SecretRef // required WHEN scheme == ssh
	apiTokenRef config.SecretRef // required WHEN scheme == api
	become      config.SecretRef // optional enable/become secret reference

	// provenance recorded on the resolved bundle (non-secret); set by the resolver, not part of identity.
	ruleID string
}

// BundleSpec is the required-field input to NewBundle. Constructing a valid Bundle requires every required
// field for the chosen scheme; a missing required field is a construction error (REQ-1601). Every secret is
// supplied as a SecretRef, so a plaintext credential cannot enter a bundle (REQ-1603).
type BundleSpec struct {
	User   string
	Port   int
	Scheme ConnectionScheme

	SSHKeyRef   config.SecretRef // the ssh private-key reference (required for scheme ssh)
	APITokenRef config.SecretRef // the api-token reference (required for scheme api)
	Become      config.SecretRef // optional enable/become secret reference
}

// NewBundle builds a Bundle from a fully-populated spec, or returns an error. It enforces (REQ-1601):
//   - user is non-empty;
//   - port is a valid TCP port (1..65535);
//   - scheme is a known ConnectionScheme;
//   - the scheme-appropriate secret reference is present (ssh → SSHKeyRef, api → APITokenRef);
//   - every SUPPLIED secret field is a sealed SecretRef (env:/file:/store:/…), never a plaintext literal
//     (REQ-1603, enforced by requireSealed in secretref.go).
//
// On any failure it returns the ZERO Bundle (ok=false) and a non-nil error — fail closed by construction.
func NewBundle(spec BundleSpec) (Bundle, error) {
	if spec.User == "" {
		return Bundle{}, fmt.Errorf("credential: bundle requires a user")
	}
	if spec.Port < 1 || spec.Port > 65535 {
		return Bundle{}, fmt.Errorf("credential: bundle requires a valid port (1..65535), got %d", spec.Port)
	}
	if !spec.Scheme.valid() {
		return Bundle{}, fmt.Errorf("credential: bundle has unknown connection scheme %q (use ssh/api/winrm/netconf)", spec.Scheme)
	}

	// Scheme-appropriate secret must be present.
	switch spec.Scheme {
	case SchemeSSH, SchemeNetconf:
		if spec.SSHKeyRef == "" {
			return Bundle{}, fmt.Errorf("credential: %s bundle requires an ssh key reference", spec.Scheme)
		}
	case SchemeAPI:
		if spec.APITokenRef == "" {
			return Bundle{}, fmt.Errorf("credential: %s bundle requires an api token reference", spec.Scheme)
		}
	case SchemeWinRM:
		// WinRM authenticates with a password reference carried in become (the enable/password slot).
		if spec.Become == "" {
			return Bundle{}, fmt.Errorf("credential: %s bundle requires a password reference (become)", spec.Scheme)
		}
	}

	// Every supplied secret field must be a sealed reference, never a plaintext literal (INV-13).
	for _, ref := range []config.SecretRef{spec.SSHKeyRef, spec.APITokenRef, spec.Become} {
		if err := requireSealed(ref); err != nil {
			return Bundle{}, err
		}
	}

	return Bundle{
		ok:          true,
		user:        spec.User,
		port:        spec.Port,
		scheme:      spec.Scheme,
		sshKeyRef:   spec.SSHKeyRef,
		apiTokenRef: spec.APITokenRef,
		become:      spec.Become,
	}, nil
}

// Valid reports whether b is a real resolved bundle. The zero Bundle is invalid — a caller must NEVER treat
// a zero/unresolved bundle as an identity (REQ-1602).
func (b Bundle) Valid() bool { return b.ok }

// Non-secret accessors — safe to log / persist as identity metadata (REQ-1617).
func (b Bundle) User() string             { return b.user }
func (b Bundle) Port() int                { return b.port }
func (b Bundle) Scheme() ConnectionScheme { return b.scheme }
func (b Bundle) RuleID() string           { return b.ruleID }

// SecretRef accessors return the REFERENCE (safe to log), never the secret value. Resolve them at use time
// through Resolved (secretref.go).
func (b Bundle) SSHKeyRef() config.SecretRef   { return b.sshKeyRef }
func (b Bundle) APITokenRef() config.SecretRef { return b.apiTokenRef }
func (b Bundle) BecomeRef() config.SecretRef   { return b.become }

// withRuleID returns a copy tagged with the matched rule's id (resolver provenance). Internal use.
func (b Bundle) withRuleID(id string) Bundle {
	b.ruleID = id
	return b
}
