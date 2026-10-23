// Package catalog is the CLOSED SET of module configuration descriptors.
//
// It is the one place that knows which modules publish a schema, and it exists for the same reason
// core/wiring keeps a closed seam set: a registry you range over tells you what is MISSING, while a
// registry assembled from whatever registered itself can only tell you what is present.
//
// The console generates its per-module dialogs from All(). A module absent here has no dialog — which is
// why deploy/module_descriptor_test.go fails the build for any module package that neither appears here
// nor is named in its explicit backlog list.
package catalog

import (
	"fmt"
	"sort"

	"github.com/territory-grounder/grounder/core/cpconfig"
	"github.com/territory-grounder/grounder/core/secretwrite"
	"github.com/territory-grounder/grounder/modules/desc"

	actuationAWXJob "github.com/territory-grounder/grounder/modules/actuation/awxjob"
	cmdbNetbox "github.com/territory-grounder/grounder/modules/cmdb/netbox"
	cmdbPVE "github.com/territory-grounder/grounder/modules/cmdb/pve"
	cmdbSlurpit "github.com/territory-grounder/grounder/modules/cmdb/slurpit"
	cmdbVsphere "github.com/territory-grounder/grounder/modules/cmdb/vsphere"
	credAnsible "github.com/territory-grounder/grounder/modules/credsource/ansible"
	credAWX "github.com/territory-grounder/grounder/modules/credsource/awx"
	credLDAP "github.com/territory-grounder/grounder/modules/credsource/ldap"
	credOIDC "github.com/territory-grounder/grounder/modules/credsource/oidctoken"
	credOpenBao "github.com/territory-grounder/grounder/modules/credsource/openbao"
	credPassbolt "github.com/territory-grounder/grounder/modules/credsource/passbolt"
	credSemaphore "github.com/territory-grounder/grounder/modules/credsource/semaphore"
	credVaultwarden "github.com/territory-grounder/grounder/modules/credsource/vaultwarden"
	discDocker "github.com/territory-grounder/grounder/modules/discovery/docker"
	discSystemd "github.com/territory-grounder/grounder/modules/discovery/systemd"
	ingestLibreNMS "github.com/territory-grounder/grounder/modules/ingest/librenms"
	ingestPVELiveness "github.com/territory-grounder/grounder/modules/ingest/pveliveness"
	knowledgeAWXPlaybooks "github.com/territory-grounder/grounder/modules/knowledge/awxplaybooks"
	modelLiteLLM "github.com/territory-grounder/grounder/modules/model/litellm"
	notifierEmail "github.com/territory-grounder/grounder/modules/notifier/email"
	notifierMatrix "github.com/territory-grounder/grounder/modules/notifier/matrix"
	notifierMattermost "github.com/territory-grounder/grounder/modules/notifier/mattermost"
	notifierSlack "github.com/territory-grounder/grounder/modules/notifier/slack"
	notifierTeams "github.com/territory-grounder/grounder/modules/notifier/teams"
	notifierTwilioSMS "github.com/territory-grounder/grounder/modules/notifier/twilio-sms"
	obsHealthchecks "github.com/territory-grounder/grounder/modules/observability/healthchecks"
	obsHostDiag "github.com/territory-grounder/grounder/modules/observability/hostdiag"
	obsLangfuse "github.com/territory-grounder/grounder/modules/observability/langfuse"
	obsOpenObserve "github.com/territory-grounder/grounder/modules/observability/openobserve"
	obsSyslogNG "github.com/territory-grounder/grounder/modules/observability/syslogng"
	trackerGitHub "github.com/territory-grounder/grounder/modules/tracker/github-issues"
	trackerJira "github.com/territory-grounder/grounder/modules/tracker/jira"
	trackerServiceNow "github.com/territory-grounder/grounder/modules/tracker/servicenow"
	trackerYouTrack "github.com/territory-grounder/grounder/modules/tracker/youtrack"
)

// descriptors is the registry. Adding a module here is what gives it a configuration dialog.
//
// The import aliases are surface-prefixed because the package names alone are ambiguous across surfaces —
// credsource/awx and actuation/awxjob are different modules with the same vendor, as are cmdb/pve and
// ingest/pveliveness. An alias that reads like the tree path is the difference between registering the
// AWX credential source and registering the AWX job launcher.
func descriptors() []desc.Descriptor {
	return []desc.Descriptor{
		actuationAWXJob.Descriptor(),
		cmdbNetbox.Descriptor(),
		cmdbPVE.Descriptor(),
		cmdbSlurpit.Descriptor(),
		cmdbVsphere.Descriptor(),
		credAnsible.Descriptor(),
		credAWX.Descriptor(),
		credLDAP.Descriptor(),
		credOIDC.Descriptor(),
		credOpenBao.Descriptor(),
		credPassbolt.Descriptor(),
		credSemaphore.Descriptor(),
		credVaultwarden.Descriptor(),
		discDocker.Descriptor(),
		discSystemd.Descriptor(),
		ingestLibreNMS.Descriptor(),
		ingestPVELiveness.Descriptor(),
		knowledgeAWXPlaybooks.Descriptor(),
		modelLiteLLM.Descriptor(),
		notifierEmail.Descriptor(),
		notifierMatrix.Descriptor(),
		notifierMattermost.Descriptor(),
		notifierSlack.Descriptor(),
		notifierTeams.Descriptor(),
		notifierTwilioSMS.Descriptor(),
		obsHealthchecks.Descriptor(),
		obsHostDiag.Descriptor(),
		obsLangfuse.Descriptor(),
		obsOpenObserve.Descriptor(),
		obsSyslogNG.Descriptor(),
		trackerGitHub.Descriptor(),
		trackerJira.Descriptor(),
		trackerServiceNow.Descriptor(),
		trackerYouTrack.Descriptor(),
	}
}

// All returns every declared descriptor, validated and deterministically ordered.
//
// Validation happens HERE rather than at each call site: a malformed descriptor is a dialog that would
// mislead an operator, and it should fail the test suite once rather than render wrongly forever.
func All() ([]desc.Descriptor, error) {
	out := descriptors()
	for _, d := range out {
		if err := d.Validate(); err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Surface != out[j].Surface {
			return out[i].Surface < out[j].Surface
		}
		return out[i].SourceType < out[j].SourceType
	})
	return out, nil
}

// Lookup returns one descriptor by (surface, sourceType).
func Lookup(surface, sourceType string) (desc.Descriptor, error) {
	all, err := All()
	if err != nil {
		return desc.Descriptor{}, err
	}
	for _, d := range all {
		if d.Surface == surface && d.SourceType == sourceType {
			return d, nil
		}
	}
	return desc.Descriptor{}, fmt.Errorf("catalog: no descriptor for %s/%s", surface, sourceType)
}

// ConfigKeys renders the catalog as control-plane configuration keys, so a console write of a connector
// setting is a REGISTERED, legality-checked write rather than an unknown key.
//
// A field becomes console-writable only when it has an env key (something the binary actually reads) and
// is not read-only. A secret VALUE is deliberately excluded: it travels its own lane to the secret
// backend and must never enter the config store, which is Postgres-backed and appears in ledger rows.
func ConfigKeys() []cpconfig.Key {
	all, err := All()
	if err != nil {
		return nil // a malformed catalog is a test failure, not a boot failure
	}
	return configKeysFrom(all)
}

// configKeysFrom is the filter, split out so it can be tested against a descriptor the catalog does not
// contain.
//
// THE ORDER OF THESE CHECKS IS LOAD-BEARING. The secret skip must come FIRST and must stand alone. It
// looked redundant while the only secret field also had no EnvKey — a mutation removing it stayed green,
// because the `EnvKey == ""` check happened to exclude the same field. That is exclusion by coincidence:
// give the token an env key (TG_MATRIX_TOKEN is a plausible one — it is the default ref's target) and the
// secret flows into a Postgres-backed, ledger-recorded store. The check that expresses the intent is the
// one that must hold.
func configKeysFrom(all []desc.Descriptor) []cpconfig.Key {
	var out []cpconfig.Key
	for _, d := range all {
		for _, f := range d.Fields {
			if f.Type == desc.TypeSecretValue {
				continue // secrets never enter the config store
			}
			if f.EnvKey == "" || f.Effect == desc.EffectReadOnly {
				continue // nothing to write, or display-only provenance
			}
			out = append(out, cpconfig.Key{
				Name:            ConfigKeyName(d.Surface, d.SourceType, f.Name),
				Description:     f.Label + " — " + f.Help,
				ConsoleWritable: true,
				// The descriptor's own constraints ride with the key so the WRITE path can refuse what
				// the field forbids, in the dialog, instead of the worker refusing it at boot (TG-262).
				Type:     string(f.Type),
				Pattern:  f.Pattern,
				MaxLen:   f.MaxLen,
				MaxItems: f.MaxItems,
				Help:     f.Help,
			})
		}
	}
	return out
}

// ConfigKeyName is the stable config-store name for one module field.
func ConfigKeyName(surface, sourceType, field string) string {
	return cpconfig.ModuleKeyPrefix + surface + "." + sourceType + "." + field
}

// EnvBinding ties one console-writable config key to the environment variable the binary actually reads.
//
// WHY THIS EXISTS. ConfigKeys() publishes what an operator may WRITE; it says nothing about what the
// process READS. Until this binding existed the two halves never met: 115 fields validated, appended to
// the ledger and committed a row, and 112 of them were then consulted by nothing, because every consumer
// resolved through os.LookupEnv. A write surface whose values reach no reader is a promise the product
// cannot keep (TG-260).
// It carries the field's SHAPE CONSTRAINTS as well as its two names, because those constraints have
// never been enforced anywhere. desc.Field.Pattern, MaxLen and MaxItems are declared by descriptors and
// read by nothing — the console write path checks only that a value is non-empty, short enough and free
// of control characters. That was harmless while no stored value reached the binary. Once one does, an
// unchecked row is a boot-time weapon: several consumers are fail-closed and call log.Fatalf on a value
// they cannot parse, and the only writer able to correct the row runs INSIDE the worker that is now
// crash-looping. The resolver therefore validates before it serves.
type EnvBinding struct {
	ConfigKey string // module.<surface>.<sourceType>.<field> — what the console writes
	EnvKey    string // TG_… — what cmd/worker reads

	Type     desc.FieldType // shape: url, duration, bool, idlist, kvmap, text
	Pattern  string         // per-ENTRY constraint for list/map types, whole-value otherwise
	MaxLen   int            // 0 = unbounded
	MaxItems int            // 0 = unbounded; list/map types only
}

// EnvBindings returns the ConfigKey→EnvKey binding for every console-writable field.
//
// It derives from configKeysFrom's EXACT filter rather than re-stating it, so the set of keys an operator
// can write and the set a resolver can serve cannot drift apart. A field excluded from one is excluded
// from the other by construction, which is what keeps secret VALUES out of both.
func EnvBindings() ([]EnvBinding, error) {
	all, err := All()
	if err != nil {
		return nil, err
	}
	return envBindingsFrom(all), nil
}

func envBindingsFrom(all []desc.Descriptor) []EnvBinding {
	out := make([]EnvBinding, 0, 128)
	for _, d := range all {
		for _, f := range d.Fields {
			if f.Type == desc.TypeSecretValue {
				continue // secrets never enter the config store — same exclusion as configKeysFrom
			}
			if f.EnvKey == "" || f.Effect == desc.EffectReadOnly {
				continue
			}
			out = append(out, EnvBinding{
				ConfigKey: ConfigKeyName(d.Surface, d.SourceType, f.Name),
				EnvKey:    f.EnvKey,
				Type:      f.Type,
				Pattern:   f.Pattern,
				MaxLen:    f.MaxLen,
				MaxItems:  f.MaxItems,
			})
		}
	}
	return out
}

// Lane implements core/secretwrite.LaneResolver: it answers "where does this module's secret live" from
// the module's own descriptor.
//
// The path is resolved HERE, from the compiled catalog, precisely so a caller can never submit one. A
// request names a module; a location is derived. A client that could name the path could overwrite any
// secret the writer's credential can reach.
func Lane(surface, sourceType string) (secretwrite.Lane, error) {
	d, err := Lookup(surface, sourceType)
	if err != nil {
		return secretwrite.Lane{}, fmt.Errorf("%w: %s/%s", secretwrite.ErrUnknownModule, surface, sourceType)
	}
	if d.Secret.KVPath == "" || d.Secret.Field == "" {
		return secretwrite.Lane{}, fmt.Errorf("%w: %s/%s", secretwrite.ErrNoSecretLane, surface, sourceType)
	}
	return secretwrite.Lane{
		Surface: d.Surface, SourceType: d.SourceType,
		KVPath: d.Secret.KVPath, Field: d.Secret.Field,
	}, nil
}

// LaneResolverFunc adapts Lane to the interface without core importing this package.
type LaneResolverFunc struct{}

func (LaneResolverFunc) Lane(surface, sourceType string) (secretwrite.Lane, error) {
	return Lane(surface, sourceType)
}

// undescribed names the module packages that publish no configuration schema, EACH WITH THE REASON.
//
// It lives here rather than in a test because BOTH the enforcement guard and the console read it. A test-
// only list would let the console show a tidy set of described modules while an operator had no way to
// learn what was missing — and "this is everything" versus "this is what someone got round to writing a
// form for" is precisely the distinction this list exists to preserve.
//
// WHY THE REASON IS A REQUIRED FIELD AND NOT A COMMENT. This started as a bare set of 40 package names
// under the word "backlog", and that framing was wrong in a way that would never have surfaced: a bare
// name cannot distinguish "nobody has written this dialog yet" from "this module has nothing to
// configure, and never will". Twenty-eight of those forty were the first kind and now have descriptors.
// The twelve below are the second kind — a console rendering them as pending work would misreport a
// finished surface as three-quarters done, forever, and every future reader would re-derive the same
// twelve answers from scratch. The reason is what makes the list shrinkable BY INSPECTION.
//
// Each entry states where the composition root constructs the module with no configuration, so the claim
// is checkable rather than asserted. A NEW module package is not in this list, so
// deploy/module_descriptor_test.go fails until it either publishes a descriptor or is deferred here
// deliberately, with a reason.
var undescribed = map[string]string{
	// A shared library, not a connector instance. There is no TG_VAULT_* key anywhere in the tree:
	// credsource/openbao is a thin wrapper (openbao.go New = vault.New) and the console's secret writer
	// builds a vault client from the control-plane TG_OPENBAO_WRITER_* keys. A descriptor here would
	// re-declare openbao's keys under a second module identity — two dialogs writing one set of values.
	"modules/credsource/vault": "shared library behind credsource/openbao; it has no configuration of its own",

	// Push receivers. bootstrap.go builds both with New() in the list named configFreeIngest, and their
	// only exported Option is WithClock (a test seam). The push credential is per-source and lives in the
	// Postgres `sources` table, not in the environment — so it is configured per sender, not per module.
	"modules/ingest/crowdsec":                "push-only receiver; its credential is per-source in the sources table, not module config",
	"modules/ingest/authlog":                 "push-only receiver (TG-315); the syslog-ng access it reads FROM is the collector's configuration, and this parser takes none of its own",
	"modules/ingest/prometheus-alertmanager": "push-only receiver; its credential is per-source in the sources table, not module config",
	"modules/ingest/otlp":                    "push-only receiver (TG-32 slice 1 = the OTLP-log adapter/normalizer only); the OTLP endpoint and its config dialog land with the receiver in slice 2",

	// Model providers. Each is a ~30-line declaration of provider identity plus default model ids,
	// hardcoded in New() and registered with no arguments. TG never dials a provider directly — the
	// LiteLLM gateway fronts them all over one OpenAI-compatible endpoint — so endpoints and credentials
	// live in LiteLLM's own configuration, which modules/model/litellm DOES describe. ollama is the one
	// that tempts a URL field, because a self-hosted server needs an address; that address is LiteLLM's,
	// and a TG_OLLAMA_URL would be a control wired to nothing.
	"modules/model/anthropic": "provider identity + default model ids only; endpoint and key live in LiteLLM's config",
	"modules/model/deepseek":  "provider identity + default model ids only; endpoint and key live in LiteLLM's config",
	"modules/model/mistral":   "provider identity + default model ids only; endpoint and key live in LiteLLM's config",
	"modules/model/ollama":    "provider identity + default model ids only; the server address is LiteLLM's config, not TG's",
	"modules/model/openai":    "provider identity + default model ids only; endpoint and key live in LiteLLM's config",
	"modules/model/zai":       "provider identity + default model ids only; endpoint and key live in LiteLLM's config",

	// Exporters built by New() with no arguments in the list named configFreeObservability. prometheus's
	// staleness window is reachable only through WithStalenessWindow, which no composition root calls;
	// grafana's baseline arrives through Provision(defs) in code. Note TG_GRAFANA_SA exists ONLY in the
	// served console's mock fixtures — no binary reads it, and declaring it would produce exactly the
	// control-wired-to-nothing defect this surface exists to remove.
	"modules/observability/grafana":    "drift comparator; its baseline arrives through Provision() in code, not through config",
	"modules/observability/prometheus": "exporter constructed with no arguments; its one tunable is not passed by any composition root",

	// Complete connector, not yet wired: cronicle.New is called only from its own tests and the spec-019
	// acceptance test. No composition root mentions it, so every field would be inert. It gets a
	// descriptor when something constructs it.
	"modules/schedule/cronicle": "connector is built but no composition root constructs it; fields would be inert until it is wired",
}

// Undescribed returns the packages with no dialog, sorted, for the console to render alongside the
// described modules.
func Undescribed() []string {
	out := make([]string, 0, len(undescribed))
	for p := range undescribed {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// UndescribedReason returns why a package publishes no schema, or "" if it is not on the list.
func UndescribedReason(pkg string) string { return undescribed[pkg] }

// IsUndescribed reports whether a module package publishes no configuration schema.
func IsUndescribed(pkg string) bool { _, ok := undescribed[pkg]; return ok }
