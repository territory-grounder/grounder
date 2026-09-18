package main

// BOOT CONFIGURATION RESOLUTION — the console's saved values become the values this process runs on.
//
// THE DEFECT THIS CLOSES (TG-260). The console's module dialogs published 115 writable fields. Saving one
// validated it against the registry, appended it to the governance ledger with the operator's rationale
// and name, and committed a row in Postgres. Exactly THREE of those 115 were then read by anything — the
// Matrix notifier's approvers, rooms and default_room, which are consulted through a holder at use time.
// The other 112 were write-only. Every remaining consumer resolved through getenv, which was os.LookupEnv
// and a compiled default, so a restart re-read the environment and the saved value never took effect. The
// dialogs promise "takes effect at next restart"; for 112 fields that promise could not be kept.
//
// THE SHAPE OF THE FIX. getenv is the single point every module setting passes through, so it becomes the
// resolution point: console override → environment → compiled default. One chokepoint rather than 112
// call-site edits, which also means a module added tomorrow is configurable from the database without
// anyone remembering to wire it.
//
// WHAT STAYS OUT OF THE DATABASE, STRUCTURALLY RATHER THAN BY CONVENTION:
//
//   - The DSN. This loader reads TG_DB_DSN with os.Getenv DIRECTLY, never through getenv. A database
//     cannot supply the address of the database it lives in, and expressing that as a plain os.Getenv
//     makes the cycle impossible to introduce later by accident.
//   - Every other bootstrap knob (listen addresses, Temporal, TLS paths). These are not module descriptor
//     fields, so no binding exists and no override can be served. Absence from the catalog IS the
//     exclusion; bootConfigForbiddenEnvKeys then guards the ones a descriptor might plausibly claim.
//   - Secret VALUES. catalog.EnvBindings derives from the same filter as catalog.ConfigKeys, which drops
//     desc.TypeSecretValue outright. A secret cannot become writable here without also becoming writable
//     there, and a test holds that line.
//   - LAW keys. cpconfig forces Law=false on every module key and the resolver never consults law from a
//     store. Nothing law-pinned has an EnvKey binding to begin with.
//
// FAILURE POSTURE. A config-plane outage must not take the worker down: an unreachable store, a missing
// DSN or a query error logs loudly and leaves every value resolving from the environment, which is exactly
// today's behaviour. The failure is announced, never silent — a worker running on stale env values while
// the operator believes their saved settings are live is the condition this file exists to prevent.

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/modules/catalog"
	"github.com/territory-grounder/grounder/modules/desc"
)

// bootConfig is the resolved console-override snapshot, keyed by the ENV key the binary reads. Nil until
// the loader installs it, which is the correct value for the window before a database could be reached.
type bootConfig struct {
	byEnvKey  map[string]string // env key → operator-saved value
	sources   map[string]string // env key → the config key it came from (for the boot report)
	ambiguous []string          // env keys claimed by 2+ config keys whose stored values DISAGREE
	invalid   []string          // stored values refused because they violate the field's declared shape
}

// bootConfigDisableEnv is the break-glass.
//
// WHY IT MUST EXIST. This change inverts precedence: a stored value now beats the environment. That
// creates a recovery trap the previous design did not have. Several boot consumers are fail-closed and
// call log.Fatalf on a value they cannot use — the credential engine, the capability reconcile, the
// configured-ingest gate. The ONLY writer that can correct a bad row is configwrite.ConfigWriteWorkflow,
// which executes inside this worker (core/httpapi/config_write.go:18-20), and the route is POST-only, so
// a row cannot even be deleted. A worker that will not boot therefore cannot be told to stop reading the
// value that stops it booting, and the operator is left doing surgery on Postgres by hand.
//
// Setting this in the deploy environment makes the process ignore the store entirely and resolve from env
// exactly as it did before TG-260. The environment is the one channel that is always reachable without a
// running worker, which is precisely why the escape hatch belongs there and nowhere else.
const bootConfigDisableEnv = "TG_CONFIG_IGNORE_STORE"

// bootCfg is an atomic pointer rather than a plain map because getenv is called from the composition root
// while it is still assembling, and later from activity goroutines. A plain map would be a data race that
// surfaces as a torn read under load and never in a test.
var bootCfg atomic.Pointer[bootConfig]

// bootConfigForbiddenEnvKeys are env keys no module descriptor may bind, because they are read before a
// database connection can exist or they name the connection itself. A binding on one of these is dropped
// here AND fails deploy/boot_config_test.go — this is the belt to that test's braces, so a descriptor
// added on a branch that skipped the test cannot make the worker unbootable.
var bootConfigForbiddenEnvKeys = map[string]bool{
	"TG_DB_DSN": true,
	// The plane-scoped DSNs (TG-164) are the same class as TG_DB_DSN and are excluded for the same reason:
	// they NAME the connection, so a value served from that connection would be a cycle. Worse here than for
	// TG_DB_DSN — the console writes through tg_runtime, so a console-supplied plane DSN would let the
	// un-split identity choose which identity the split worker authenticates as.
	"TG_DB_DSN_TRIAGE":      true,
	"TG_DB_DSN_ACTUATE":     true,
	"TG_TEMPORAL_HOSTPORT":  true,
	"TG_TEMPORAL_ADDR":      true,
	"TG_TEMPORAL_NAMESPACE": true,
	"TG_PUBLIC_ADDR":        true,
	"TG_ADMIN_ADDR":         true,
}

// bootConfigLoadTimeout bounds the boot-time config read. A control plane whose database is slow must
// still start; it starts on env values and says so.
const bootConfigLoadTimeout = 10 * time.Second

// installBootConfig loads the operator's saved module settings and installs them as the first layer
// getenv consults. Called at the very top of main, before ANY module is assembled, because the OpenBao
// delivery keys are read in main's opening lines and they are descriptor fields too.
//
// Every return path is non-fatal by design. The worst outcome is the behaviour that shipped before this
// existed — resolution straight from the environment — and it is always logged.
func installBootConfig(ctx context.Context) {
	// The break-glass is read from the environment DIRECTLY and checked first, so a store that cannot be
	// read past — a value that kills the boot — is always escapable without a running worker.
	if truthy(os.Getenv(bootConfigDisableEnv)) {
		log.Printf("module config: %s is set — the config store is IGNORED and every setting resolves "+
			"from the environment. Unset it once the offending value is corrected.", bootConfigDisableEnv)
		return
	}
	dsn := strings.TrimSpace(os.Getenv("TG_DB_DSN")) // DIRECT: never through getenv. See the file comment.
	if dsn == "" {
		log.Printf("module config: no TG_DB_DSN — every setting resolves from the environment; " +
			"values saved in the console cannot take effect")
		return
	}
	bindings, err := catalog.EnvBindings()
	if err != nil {
		log.Printf("module config: catalog unreadable (%v) — resolving from the environment only", err)
		return
	}

	ctx, cancel := context.WithTimeout(ctx, bootConfigLoadTimeout)
	defer cancel()

	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		log.Printf("module config: cannot reach the config store (%v) — resolving from the environment "+
			"only; settings saved in the console are NOT in effect", err)
		return
	}
	defer pool.Close()

	stored, err := db.NewCPConfigStore(pool).Overrides(ctx)
	if err != nil {
		log.Printf("module config: cannot read the config store (%v) — resolving from the environment "+
			"only; settings saved in the console are NOT in effect", err)
		return
	}

	cfg := resolveBootConfig(bindings, stored)
	bootCfg.Store(cfg)
	logBootConfig(cfg, len(bindings))
}

// resolveBootConfig folds the stored overrides onto the catalog's bindings. Split from the loader so it is
// testable with plain maps — no database, no environment.
func resolveBootConfig(bindings []catalog.EnvBinding, stored map[string]string) *bootConfig {
	cfg := &bootConfig{byEnvKey: map[string]string{}, sources: map[string]string{}}
	conflict := map[string]bool{}
	for _, b := range bindings {
		if bootConfigForbiddenEnvKeys[b.EnvKey] {
			continue
		}
		v, ok := stored[b.ConfigKey]
		if !ok || v == "" {
			continue // no override written, or an empty one the write path should never have accepted
		}
		// SHAPE CHECK BEFORE THE VALUE IS EVER SERVED. A row that violates the field's own declared
		// constraints is refused here rather than handed to a fail-closed consumer that would Fatal on it.
		// Refusing costs the operator a corrected save; serving it costs them a crash-looping worker they
		// cannot reach the config writer through.
		if why := bindingValueFault(b, v); why != "" {
			cfg.invalid = append(cfg.invalid, b.ConfigKey+": "+why)
			continue
		}
		// TWO DESCRIPTORS CAN DECLARE THE SAME ENV KEY. TG_DISCOVERY_KNOWN_HOSTS and TG_DISCOVERY_TIMEOUT
		// each appear on two, so ConfigKey→EnvKey is not injective. If both dialogs were saved with
		// DIFFERENT values there is no defensible winner: silently picking one would make one dialog lie
		// about what the process is doing. Refuse the key, keep the environment, and say so loudly.
		if prev, seen := cfg.byEnvKey[b.EnvKey]; seen && prev != v {
			conflict[b.EnvKey] = true
			continue
		}
		cfg.byEnvKey[b.EnvKey] = v
		cfg.sources[b.EnvKey] = b.ConfigKey
	}
	for k := range conflict {
		delete(cfg.byEnvKey, k)
		delete(cfg.sources, k)
		cfg.ambiguous = append(cfg.ambiguous, k)
	}
	sort.Strings(cfg.ambiguous)
	return cfg
}

// bindingValueFault reports why a stored value may not be served, or "" when it is well formed.
//
// It enforces desc.Field's Pattern, MaxLen and MaxItems — which, until this existed, were declared by 29
// descriptors and enforced by nothing — plus the parse each Type implies. It deliberately checks SHAPE
// only: whether a URL points somewhere useful is not knowable here, and pretending otherwise would just
// move the failure. The goal is narrower and achievable: no value reaches a fail-closed consumer in a
// form that consumer cannot parse.
func bindingValueFault(b catalog.EnvBinding, v string) string {
	if b.MaxLen > 0 && len(v) > b.MaxLen {
		return fmt.Sprintf("%d bytes exceeds the field's %d-byte limit", len(v), b.MaxLen)
	}
	// Pattern constrains each ENTRY for the list/map types and the whole value otherwise — see
	// desc.Field.Pattern. Splitting the same way the consumers do keeps the check honest.
	entries := []string{v}
	switch b.Type {
	case desc.TypeIDList, desc.TypeKVMap:
		entries = entries[:0]
		for _, e := range strings.Split(v, ",") {
			if e = strings.TrimSpace(e); e != "" {
				entries = append(entries, e)
			}
		}
		if b.MaxItems > 0 && len(entries) > b.MaxItems {
			return fmt.Sprintf("%d entries exceeds the field's limit of %d", len(entries), b.MaxItems)
		}
	}
	if b.Pattern != "" {
		re, err := regexp.Compile(b.Pattern)
		if err != nil {
			// A descriptor's own pattern failing to compile is a repo defect, not an operator error.
			// Refusing the value would punish the operator for it; the descriptor test is where this
			// belongs, so serve the value and say the constraint could not be applied.
			return ""
		}
		for _, e := range entries {
			if !re.MatchString(e) {
				return fmt.Sprintf("%q does not match the field's required form", truncateForLog(e))
			}
		}
	}
	switch b.Type {
	case desc.TypeDuration:
		if _, err := time.ParseDuration(strings.TrimSpace(v)); err != nil {
			return "not a duration (want forms like 30s, 5m, 2h)"
		}
	case desc.TypeBool:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on", "0", "false", "no", "off", "":
		default:
			return "not a boolean (want true/false)"
		}
	case desc.TypeURL:
		u, err := url.Parse(strings.TrimSpace(v))
		if err != nil || u.Scheme == "" || u.Host == "" {
			return "not an absolute URL (want scheme://host[:port])"
		}
	}
	return ""
}

// truncateForLog keeps a rejected value from turning the boot log into a dump. The value is an operator's
// own input on a non-secret field, but length is still not a reason to lose the rest of the log.
func truncateForLog(s string) string {
	const max = 60
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// truthy reads the break-glass the same way truthyEnv reads every other flag, but from a raw string so it
// can be applied to an os.Getenv result before the resolver exists.
func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// logBootConfig makes the resolution auditable from the boot log alone: which settings came from the
// console, and which were refused. "Is my saved value in effect?" must be answerable without reading code.
func logBootConfig(cfg *bootConfig, bindings int) {
	keys := make([]string, 0, len(cfg.byEnvKey))
	for k := range cfg.byEnvKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	log.Printf("module config: %d of %d settings resolved from the console (the rest from the environment)",
		len(keys), bindings)
	for _, k := range keys {
		// Values are NOT logged: a non-secret config field can still carry a URL with an embedded
		// credential, and the boot log is not a place to find out.
		log.Printf("module config:   %s ← %s", k, cfg.sources[k])
	}
	for _, k := range cfg.ambiguous {
		log.Printf("module config: REFUSED %s — it is ONE setting behind TWO dialogs (the descriptors say "+
			"so) and they were saved with different values; the environment stands. Set both to the same "+
			"value in the console.", k)
	}
	for _, why := range cfg.invalid {
		log.Printf("module config: REFUSED %s — the environment stands. Correct it in the console.", why)
	}
}

// bootConfigValue reports the operator-saved value for an env key, if one is in effect. getenv consults
// this before the environment.
func bootConfigValue(k string) (string, bool) {
	c := bootCfg.Load()
	if c == nil {
		return "", false
	}
	v, ok := c.byEnvKey[k]
	return v, ok
}
