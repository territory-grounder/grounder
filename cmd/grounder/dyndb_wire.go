package main

import (
	"context"
	"log"
	"os"
	"strings"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/credential/dyndb"
)

// wireDynDB arms the dyn: SecretRef scheme at the GROUNDER composition root (TG-422 slice 2), mirroring
// cmd/worker: OFF (TG_DYNDB_ADDR unset) is a logged no-op and every dyn: reference fails closed; enabled
// but misconfigured REFUSES the boot rather than fall back to a static password. The grounder needs its own
// wiring because it is the sole consumer of TG_MIGRATION_DSN and TG_RUNTIME_DSN (db.Migrate,
// ApplyPlaneGrants and the runtime pool all live here, not in the worker), so without this a dyn: DSN could
// never resolve in the process that uses it.
//
// Read via os.Getenv DIRECTLY, never the console-resolving getter: this is the credential path TO the
// database, and a database cannot supply the address of the store that mints its own login — the same
// circularity rule the DSNs themselves follow (cmd/worker/boot_config.go, cmd/grounder/boot_config.go).
func wireDynDB() (*dyndb.Provider, string) {
	addr := strings.TrimSpace(os.Getenv("TG_DYNDB_ADDR"))
	tmpl := os.Getenv("TG_DYNDB_DSN_TEMPLATE")
	if addr == "" {
		_, _ = dyndb.Register(false, dyndb.ProviderConfig{}, log.Printf)
		return nil, tmpl
	}
	eng, err := dyndb.New(dyndb.Config{
		BaseURL:  addr,
		Mount:    os.Getenv("TG_DYNDB_MOUNT"),
		TokenRef: config.SecretRef(os.Getenv("TG_DYNDB_TOKEN_REF")),
		CACert:   os.Getenv("TG_DYNDB_CA"),
	})
	if err != nil {
		log.Fatalf("dyndb: dynamic Postgres credentials are enabled (TG_DYNDB_ADDR) but the engine will not "+
			"construct — refusing to boot rather than fall back to static passwords (TG-422): %v", err)
	}
	p, err := dyndb.Register(true, dyndb.ProviderConfig{
		Engine:      eng,
		DSNTemplate: tmpl,
		RootCtx:     context.Background(),
	}, log.Printf)
	if err != nil {
		log.Fatalf("dyndb: %v (TG-422)", err)
	}
	return p, tmpl
}
