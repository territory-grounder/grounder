package main

// THE API PROCESS RESOLVES CONFIG THE SAME WAY THE WORKER DOES (TG-263).
//
// TG-260 made the WORKER read operator-saved module settings: console override → env → compiled default.
// The grounder kept reading env alone, so the two halves of TG could disagree about what TG is configured
// with — an operator saves `module.model.litellm.url`, restarts, and the worker uses the new value while
// the console still renders and uses the deployed one. "What is TG configured with?" acquired two answers.
//
// THE ESCALATION THIS MUST NOT OPEN, and the reason this file exists instead of a one-line reuse of the
// worker's resolver: the grounder runs the console's OWN AUTHENTICATION. `TG_LDAP_URLS` and
// `TG_LDAP_STARTTLS` are console-writable fields of the credsource/ldap dialog AND the feed for
// auth.NewLDAPAuthenticator, whose AdminGroup/OperatorGroup decide who may elevate. Resolving module keys
// generically here would let an operator editing a CONNECTOR's settings re-point the directory that
// authenticates them — a privilege escalation wearing a settings form.
//
// So the exclusion is not the worker's list plus a note. Every env key the grounder's authentication path
// reads is refused here, whether or not a descriptor claims it today, and a test walks the auth path to
// keep that list honest as the code moves.

import (
	"context"
	"log"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/modules/catalog"
)

// authEnvKeys is EVERY env key the grounder's authentication path consumes: session identity and
// lifetime, the operator/admin accounts and their credentials, and the whole LDAP directory binding
// including the group names that decide elevation.
//
// Two of these — TG_LDAP_URLS and TG_LDAP_STARTTLS — are console-writable connector fields TODAY. The
// rest are listed anyway: a descriptor added tomorrow could claim any of them, and the moment it did, a
// settings dialog would reach the console's own front door. Listing the whole surface makes that
// impossible by construction rather than by nobody having tried yet.
var authEnvKeys = map[string]bool{
	"TG_SESSION_KEY": true, "TG_SESSION_KEY_REF": true, "TG_SESSION_TTL": true,
	"TG_OPERATOR_NAME": true, "TG_OPERATOR_TOKEN": true, "TG_OPERATOR_TOKEN_REF": true,
	"TG_ADMIN_NAME": true, "TG_ADMIN_TOKEN": true, "TG_ADMIN_TOKEN_REF": true, "TG_ADMIN_TTL": true,
	"TG_LDAP_AUTH_ENABLED": true, "TG_LDAP_URLS": true, "TG_LDAP_CA": true, "TG_LDAP_STARTTLS": true,
	"TG_LDAP_AUTH_USER_DN_TEMPLATE": true, "TG_LDAP_AUTH_ADMIN_GROUP": true, "TG_LDAP_AUTH_OPERATOR_GROUP": true,
}

// bootstrapEnvKeys cannot come from the database that they open or bind. Same reasoning as the worker's:
// expressed as a list rather than a comment so the cycle is refused, not merely discouraged.
var bootstrapEnvKeys = map[string]bool{
	"TG_RUNTIME_DSN": true, "TG_MIGRATION_DSN": true, "TG_DB_DSN": true,
	"TG_PUBLIC_ADDR": true, "TG_ADMIN_ADDR": true,
	"TG_TEMPORAL_HOSTPORT": true, "TG_TEMPORAL_ADDR": true, "TG_TEMPORAL_NAMESPACE": true,
}

// grounderOverrides is the resolved console-override snapshot, keyed by env key. Nil until installed.
var grounderOverrides atomic.Pointer[map[string]string]

// installGrounderConfig loads the operator's saved module settings for THIS process. Non-fatal on every
// path: a config-plane outage degrades to env resolution — which is exactly the behaviour that shipped
// before — and says so.
func installGrounderConfig(ctx context.Context, dsn string) {
	if strings.TrimSpace(dsn) == "" {
		return
	}
	if strings.HasPrefix(strings.TrimSpace(dsn), "dyn:") {
		// A dyn: reference cannot resolve here — this read runs before the dyn: scheme is wired, by design
		// (saved overrides must apply to that wiring). Say so and degrade to env resolution, instead of
		// handing pgx a ref it reports as a cryptic keyword/value parse failure (TG-422 slice 2).
		log.Printf("module config: the config-store DSN is a dyn: reference, which cannot resolve before " +
			"the dyn: scheme is wired — resolving from the environment only; pin TG_DB_DSN to a static " +
			"login to restore console overrides (TG-422)")
		return
	}
	bindings, err := catalog.EnvBindings()
	if err != nil {
		log.Printf("module config: catalog unreadable (%v) — resolving from the environment only", err)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		log.Printf("module config: cannot reach the config store (%v) — resolving from the environment only", err)
		return
	}
	defer pool.Close()
	stored, err := db.NewCPConfigStore(pool).Overrides(ctx)
	if err != nil {
		log.Printf("module config: cannot read the config store (%v) — resolving from the environment only", err)
		return
	}

	out := map[string]string{}
	var refusedAuth []string
	conflict := map[string]bool{}
	for _, b := range bindings {
		if bootstrapEnvKeys[b.EnvKey] {
			continue
		}
		if authEnvKeys[b.EnvKey] {
			// Refused LOUDLY, and only when a row actually exists: an operator who saved this needs to
			// know it will not take effect here, and a reviewer needs to see that the door held.
			if _, present := stored[b.ConfigKey]; present {
				refusedAuth = append(refusedAuth, b.ConfigKey)
			}
			continue
		}
		v, ok := stored[b.ConfigKey]
		if !ok || v == "" {
			continue
		}
		// Two descriptors can share one env key (the discovery pair). Disagreement resolves to neither.
		if prev, seen := out[b.EnvKey]; seen && prev != v {
			conflict[b.EnvKey] = true
			continue
		}
		out[b.EnvKey] = v
	}
	for k := range conflict {
		delete(out, k)
	}
	grounderOverrides.Store(&out)

	keys := make([]string, 0, len(out))
	for k := range out {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	log.Printf("module config: %d of %d settings resolved from the console (the rest from the environment)",
		len(keys), len(bindings))
	sort.Strings(refusedAuth)
	for _, k := range refusedAuth {
		log.Printf("module config: REFUSED %s — it feeds this process's own authentication and can never be "+
			"set from a settings dialog (TG-263); the deployed value stands", k)
	}
	for k := range conflict {
		log.Printf("module config: REFUSED %s — two dialogs set it to different values; the environment stands", k)
	}
}

// grounderOverride reports the operator-saved value for an env key, if one is in effect here.
//
// THE EXCLUSION IS ENFORCED AGAIN HERE, not only when the snapshot is built. installGrounderConfig is one
// writer today, and a guard that lives only in a writer protects only what that writer wrote: a second
// caller, a refactor, or a future reload path would carry auth keys straight through to the console's own
// front door. Making it a property of the READ means every consumer is covered by construction. The
// oracle that found this had asserted a "second line" the code did not yet have.
func grounderOverride(k string) (string, bool) {
	if authEnvKeys[k] || bootstrapEnvKeys[k] {
		return "", false
	}
	m := grounderOverrides.Load()
	if m == nil {
		return "", false
	}
	v, ok := (*m)[k]
	return v, ok
}
