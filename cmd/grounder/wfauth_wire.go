package main

import (
	"errors"
	"log"
	"os"
	"strings"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/temporal/wfauth"
)

// wireWorkflowAuth builds the SIGNER for the authorization envelope every admin-write workflow start and every
// approval vote carries to the worker (TG-660, deep-review F1 / pack 12). The grounder is the only process that
// authenticates humans; the envelope is how the worker tells its request from anything else on the backplane.
//
// Read with os.Getenv DIRECTLY, never the console-resolving getter: this key authenticates the console's OWN
// writes, so a console-supplied value would let the surface it protects choose the key it is checked with.
//
// UNSET ⇒ nil signer and a loud line. Every admin write and vote the grounder then forwards is refused by the
// worker as unsigned (fail closed) — the honest state for a deployment that has not provisioned the key, stated
// once here rather than discovered one 403 at a time. The ref itself is a BUSINESS secret for the preflight
// (secretEntries): under TG_SECRET_POLICY=enforce it must resolve from a backend (bao:), never plaintext env.
func wireWorkflowAuth() wfauth.EnvelopeSigner {
	ref := strings.TrimSpace(os.Getenv("TG_WORKFLOW_AUTH_KEY_REF"))
	if ref == "" {
		log.Print("workflow auth: NO KEY (TG_WORKFLOW_AUTH_KEY_REF unset) — every admin write and vote this process starts " +
			"is UNSIGNED and the worker refuses it (TG-660, fail closed); set the ref to a bao: secret on both processes")
		return (*wfauth.Signer)(nil)
	}
	secret, err := config.SecretRef(ref).Resolve()
	if err != nil {
		log.Fatalf("workflow auth: TG_WORKFLOW_AUTH_KEY_REF is set but does not resolve (%v) — refusing to boot rather than "+
			"start admin writes the worker will refuse (TG-660)", err)
	}
	key, err := wfauth.ParseKey(secret)
	if err != nil {
		log.Fatalf("workflow auth: %v — refusing to boot (TG-660)", err)
	}
	s, err := wfauth.NewSigner(key)
	if err != nil {
		log.Fatalf("workflow auth: %v (TG-660)", err)
	}
	log.Printf("workflow auth: signing every admin-write start and vote with key %s (TG-660)", s.KeyID())
	return s
}

// errWorkflowAuthUnconfigured is what a write backend returns when it has no signer: the request never leaves
// the grounder, so the operator reads the cause here instead of a worker-side refusal.
var errWorkflowAuthUnconfigured = errors.New("workflow auth: no signing key configured on the grounder (TG_WORKFLOW_AUTH_KEY_REF) — the write cannot be authorized (TG-660)")

// signFor mints the envelope for one request, or returns errWorkflowAuthUnconfigured when there is no key.
func signFor(sig wfauth.EnvelopeSigner, kind string, fields ...string) (wfauth.Envelope, error) {
	if sig == nil {
		return wfauth.Envelope{}, errWorkflowAuthUnconfigured
	}
	e, err := sig.Sign(kind, fields...)
	if err != nil {
		if errors.Is(err, wfauth.ErrNoKey) {
			return wfauth.Envelope{}, errWorkflowAuthUnconfigured
		}
		return wfauth.Envelope{}, err
	}
	return e, nil
}
