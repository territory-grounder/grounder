package seal

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/territory-grounder/grounder/core/config"
)

// FromEnv builds the sealer the deployment is configured for, and says WHICH in a line an operator can read.
//
// ★ EXTRACTED SO TWO PROCESSES CANNOT DRIFT (TG-275). This construction lived in cmd/grounder's main
// package, so the worker had no way to reach it — and the consequence was not a style problem. The
// `store:` SecretRef scheme resolves against the sealed store, and RegisterStoreResolver was called ONLY
// in the grounder ("wired ONCE at composition (cmd/grounder)", core/config/config.go:24). The worker is
// where credentials are actually USED — hostdiag, syslog-ng, actuation, the AWX sync — so a secret written
// through the console landed in a store the consuming process could not read. `sealed_secret` held ZERO
// rows on a production deployment: an encrypted secret store, fully built, that nothing could use.
//
// Duplicating this into the worker would have made the two processes' seal configuration silently
// divergent — the class of bug where one half of a system encrypts with a key the other half does not
// have. One function, both roots.
//
// Returns (nil, "") when no sealer is configured. That is not an error: a deployment may legitimately run
// without a sealed store, and the store: scheme then stays fail-closed-unwired rather than resolving to an
// empty value.
func FromEnv(sealKeyRef config.SecretRef) (*Sealer, string) {
	// TRANSIT FIRST. When OpenBao Transit is configured the master key never exists in this process at all
	// — the DEK unwrap happens in OpenBao. That is strictly the stronger posture, so it wins over a local
	// master key whenever both are present, rather than depending on which is checked first.
	if key := strings.TrimSpace(os.Getenv("TG_SEAL_TRANSIT_KEY")); key != "" {
		addr := firstNonEmpty(os.Getenv("TG_SEAL_TRANSIT_ADDR"), os.Getenv("TG_OPENBAO_ADDR"))
		tokRef := firstNonEmpty(os.Getenv("TG_SEAL_TRANSIT_TOKEN_REF"), os.Getenv("TG_OPENBAO_TOKEN_REF"))
		w, err := NewTransitWrapper(TransitConfig{
			BaseURL: addr, KeyName: key, TokenRef: config.SecretRef(tokRef),
			Mount: os.Getenv("TG_SEAL_TRANSIT_MOUNT"), CACert: os.Getenv("TG_OPENBAO_CA"),
		})
		if err != nil {
			return nil, ""
		}
		s, err := NewSealer(w)
		if err != nil {
			return nil, ""
		}
		return s, "OpenBao Transit key " + key + " (master key never in this process)"
	}
	master, err := MasterKeyFromRef(sealKeyRef)
	if err != nil {
		return nil, ""
	}
	w, err := NewLocalWrapper(master)
	if err != nil {
		return nil, ""
	}
	s, err := NewSealer(w)
	if err != nil {
		return nil, ""
	}
	return s, "in-process master key " + string(sealKeyRef)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// StoreGetter reads a sealed blob by name. Satisfied by db.SealedSecretStore; declared here so the
// resolver below needs no dependency on core/db.
type StoreGetter interface {
	Get(ctx context.Context, name string) (Sealed, bool, error)
}

// StoreResolver returns the `store:` scheme resolver for a sealer and a store — the SAME function in every
// process, so the grounder and the worker cannot resolve a reference differently.
//
// A missing name is an ERROR, never an empty value: a `store:` reference that silently resolved to "" would
// hand a connector a blank credential and let it fail somewhere far from the cause.
func StoreResolver(s *Sealer, store StoreGetter) func(string) (string, error) {
	return func(name string) (string, error) {
		blob, found, err := store.Get(context.Background(), name)
		if err != nil {
			return "", err
		}
		if !found {
			return "", fmt.Errorf("sealed secret %q not found", name)
		}
		value, err := s.Open(name, blob)
		if err != nil {
			return "", err
		}
		return string(value), nil
	}
}
