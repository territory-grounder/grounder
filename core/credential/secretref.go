package credential

import (
	"fmt"
	"strings"

	"github.com/territory-grounder/grounder/core/config"
)

// sealedSchemes are the reference schemes a bundle secret may use (REQ-1603). env:/file:/store: are the
// existing core/config schemes (#27 sealed store via RegisterStoreResolver); vault:/bao: are the OpenBao /
// HashiCorp Vault connector's KV v2 dereference schemes (REQ-1613, T-016-9); oidc: is the machine-plane
// OIDC client-credentials token minter (REQ-1619, T-016-14, modules/credsource/oidctoken) that mints a
// short-lived Bearer token at use time; ansible-vault: is the un-controlled-Ansible connector scheme
// (REQ-1612, T-016-8, modules/credsource/ansible) that natively decrypts a $ANSIBLE_VAULT payload at use
// time. All are accepted here as valid reference SHAPES so a bundle can carry them; the connector registers
// each scheme's resolver at composition (an unregistered scheme fails closed at Resolve, never an empty value).
var sealedSchemes = []string{"env:", "file:", "store:", "vault:", "bao:", "oidc:", "ansible-vault:"}

// requireSealed rejects a secret field that is not a sealed SecretRef — i.e. anything that is not one of the
// known reference schemes. This makes "no plaintext credential in a bundle" STRUCTURAL (REQ-1603, INV-13):
// a raw literal (a key, a token, a password) can never be placed in a Bundle, only a reference to one. An
// empty ref is allowed here (optional fields); NewBundle separately enforces the REQUIRED refs.
func requireSealed(ref config.SecretRef) error {
	s := string(ref)
	if s == "" {
		return nil // optional/unset field; requiredness is enforced per-scheme in NewBundle
	}
	for _, sc := range sealedSchemes {
		if strings.HasPrefix(s, sc) {
			return nil
		}
	}
	return fmt.Errorf("credential: secret field %q is not a sealed reference — supply env:/file:/store: (never a literal secret)", config.SecretRef(mask(s)))
}

// mask renders a rejected value safe to include in an error/log (never echo a suspected literal secret).
func mask(s string) string {
	if len(s) <= 4 {
		return "****"
	}
	return s[:2] + "…" + "****"
}

// Resolved is a bundle with its SecretRefs loaded into memory at USE TIME. It is produced only at the point
// of authentication and is never persisted, logged, or exported (INV-13). A failed SecretRef resolution
// returns an error and the resolver fails closed (REQ-1602) — the target is not reachable.
type Resolved struct {
	User   string
	Port   int
	Scheme ConnectionScheme

	SSHKey   string // resolved private key material (scheme ssh/netconf)
	APIToken string // resolved api token (scheme api)
	Become   string // resolved enable/become/password secret (may be empty)
}

// Resolve loads the bundle's secret references through the sealed store at use time (REQ-1603). Every
// non-empty SecretRef is resolved via core/config (env:/file:/store: + any registered store resolver). A
// resolution failure (unset env, missing file, unwired store) returns an error so the caller fails closed —
// there is no default or blank credential. b must be Valid; a zero/unresolved bundle refuses.
func (b Bundle) Resolve() (Resolved, error) {
	if !b.ok {
		return Resolved{}, ErrUnresolved
	}
	out := Resolved{User: b.user, Port: b.port, Scheme: b.scheme}
	var err error
	if b.sshKeyRef != "" {
		if out.SSHKey, err = b.sshKeyRef.Resolve(); err != nil {
			return Resolved{}, fmt.Errorf("credential: resolve ssh key: %w", err)
		}
	}
	if b.apiTokenRef != "" {
		if out.APIToken, err = b.apiTokenRef.Resolve(); err != nil {
			return Resolved{}, fmt.Errorf("credential: resolve api token: %w", err)
		}
	}
	if b.become != "" {
		if out.Become, err = b.become.Resolve(); err != nil {
			return Resolved{}, fmt.Errorf("credential: resolve become secret: %w", err)
		}
	}
	return out, nil
}
