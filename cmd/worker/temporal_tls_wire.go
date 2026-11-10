package main

import (
	"crypto/tls"
	"log"
	"os"

	"go.temporal.io/sdk/client"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/temporal/clienttls"
)

// temporalTLSFromEnv builds the client identity this worker presents to the Temporal frontend (TG-660 part a):
// TG_TEMPORAL_TLS_CA_REF / _CERT_REF / _KEY_REF resolved in-process (bao: in production), server name
// TG_TEMPORAL_TLS_SERVER_NAME (default "temporal"). The actuation plane receives the same four keys from its own
// TG_ACTUATE_TEMPORAL_TLS_* refs through the compose mapping, so each plane holds its own leaf.
//
// Read with os.Getenv DIRECTLY, not the console-resolving getter (exempted by name in boot_config_test.go): the
// identity this process dials the control plane with must not be choosable from the control plane.
//
// Unset ⇒ a plaintext dial and a boot line that says so. ANY ref set ⇒ all three must resolve and parse or this
// process refuses to boot: configured-for-mTLS-but-dialing-plaintext is the gap, not an acceptable fallback.
func temporalTLSFromEnv() *tls.Config {
	refs := clienttls.Refs{
		CA:         config.SecretRef(os.Getenv("TG_TEMPORAL_TLS_CA_REF")),
		Cert:       config.SecretRef(os.Getenv("TG_TEMPORAL_TLS_CERT_REF")),
		Key:        config.SecretRef(os.Getenv("TG_TEMPORAL_TLS_KEY_REF")),
		ServerName: os.Getenv("TG_TEMPORAL_TLS_SERVER_NAME"),
	}.Normalized()
	cfg, err := refs.Build(func(r config.SecretRef) (string, error) { return r.Resolve() })
	if err != nil {
		log.Fatalf("temporal: %v — refusing to boot rather than dial the frontend without the configured client identity (TG-660)", err)
	}
	log.Printf("temporal: %s", clienttls.Summary(cfg))
	return cfg
}

// temporalClientOptions is the one place the worker's Temporal dial options are assembled.
func temporalClientOptions(hostPort string) client.Options {
	return client.Options{HostPort: hostPort, ConnectionOptions: client.ConnectionOptions{TLS: temporalTLSFromEnv()}}
}
