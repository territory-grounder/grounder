// Package bootstrap is the composition root that populates the runtime modules.Registry from the built
// connector fleet. Until it runs, the registry is never populated in the running binaries — so INV-17 ("a
// capability exists only if its module is registered AND enabled") and INV-18 ("exactly one implementation
// per surface+source type") are only enforced in acceptance tests, never at boot. The 31 connectors ship
// built and unit-tested but inert: nothing declares them, so the capability manifest is empty.
//
// This package is the missing declaration layer. It registers each connector family into the registry so
// the manifest reflects the real live set and INV-18 rejects a duplicate implementation at boot. Families
// are added as their configuration surfaces land; the members declared here are the ones that need no
// endpoint/secret configuration to declare — model-provider descriptors (the LiteLLM gateway routes),
// ingest push-receivers, and the config-free observability exporters. Config-taking members (a tracker's
// base URL + token, a notifier's approvers, an actuator's host) join via configuration-taking helpers as
// each surface migrates to registry-backed resolution.
//
// Provenance: [O] INV-17, INV-18, spec/008 (the connector fleet's runtime composition root).
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	actuation "github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/modules"
	"github.com/territory-grounder/grounder/modules/actuation/kubernetes"
	"github.com/territory-grounder/grounder/modules/actuation/mcp"
	"github.com/territory-grounder/grounder/modules/actuation/proxmox"
	"github.com/territory-grounder/grounder/modules/actuation/ssh"
	"github.com/territory-grounder/grounder/modules/cmdb/netbox"
	"github.com/territory-grounder/grounder/modules/ingest/crowdsec"
	"github.com/territory-grounder/grounder/modules/ingest/librenms"
	alertmanager "github.com/territory-grounder/grounder/modules/ingest/prometheus-alertmanager"
	"github.com/territory-grounder/grounder/modules/model/anthropic"
	"github.com/territory-grounder/grounder/modules/model/deepseek"
	"github.com/territory-grounder/grounder/modules/model/mistral"
	"github.com/territory-grounder/grounder/modules/model/ollama"
	"github.com/territory-grounder/grounder/modules/model/openai"
	"github.com/territory-grounder/grounder/modules/model/zai"
	"github.com/territory-grounder/grounder/modules/notifier/email"
	"github.com/territory-grounder/grounder/modules/notifier/matrix"
	"github.com/territory-grounder/grounder/modules/notifier/mattermost"
	"github.com/territory-grounder/grounder/modules/notifier/slack"
	"github.com/territory-grounder/grounder/modules/notifier/teams"
	twiliosms "github.com/territory-grounder/grounder/modules/notifier/twilio-sms"
	"github.com/territory-grounder/grounder/modules/observability/grafana"
	"github.com/territory-grounder/grounder/modules/observability/healthchecks"
	"github.com/territory-grounder/grounder/modules/observability/langfuse"
	"github.com/territory-grounder/grounder/modules/observability/openobserve"
	"github.com/territory-grounder/grounder/modules/observability/prometheus"
	githubissues "github.com/territory-grounder/grounder/modules/tracker/github-issues"
	"github.com/territory-grounder/grounder/modules/tracker/jira"
	"github.com/territory-grounder/grounder/modules/tracker/servicenow"
	"github.com/territory-grounder/grounder/modules/tracker/youtrack"
)

// familyMember pairs a source slug with its concrete surface adapter for registration under a given surface.
// The adapter is stored as the registration's typed Adapter for a surface-typed assertion at the call site.
type familyMember struct {
	sourceType string
	adapter    any
}

// registerFamily registers every member of a surface family, failing closed on the first duplicate
// (surface, source) pair (INV-18) so a misconfigured fleet aborts boot rather than starting ambiguous. A
// member registered with enabled=false is DECLARED but has NO execution path (INV-17) until it is enabled —
// the reference-module posture the actuation family ships in through Phase 0/1.
func registerFamily(reg *modules.Registry, surface string, members []familyMember, enabled bool) error {
	for _, m := range members {
		if err := reg.Register(modules.Registration{
			Surface:    surface,
			SourceType: m.sourceType,
			Capability: surface + "." + m.sourceType,
			Enabled:    enabled,
			Adapter:    m.adapter,
		}); err != nil {
			return fmt.Errorf("bootstrap: register %s/%s: %w", surface, m.sourceType, err)
		}
	}
	return nil
}

// modelProviders is the declarative model-provider family. Each is a backend the bundled LiteLLM gateway
// fronts over one OpenAI-compatible endpoint (INV-08); TG never calls a provider directly. They declare
// their default model ids via each package's New().
var modelProviders = []familyMember{
	{zai.SourceType, zai.New()},
	{deepseek.SourceType, deepseek.New()},
	{mistral.SourceType, mistral.New()},
	{ollama.SourceType, ollama.New()},
	{anthropic.SourceType, anthropic.New()},
	{openai.SourceType, openai.New()},
}

// configFreeIngest is the ingest family members that declare without endpoint/secret config: push receivers
// that normalize an incoming payload against the source's grammar (INV-04). librenms — which needs its
// per-deployment site/URL/token list — joins via a config-taking helper as the ingest surface migrates to
// registry-backed selection.
var configFreeIngest = []familyMember{
	{crowdsec.SourceType, crowdsec.New()},
	{alertmanager.SourceType, alertmanager.New()},
}

// configFreeObservability is the observability exporters that declare without endpoint/secret config
// (Prometheus scrape exposition and the Grafana dashboard provisioner). openobserve / langfuse /
// healthchecks — which need an endpoint and a secret ref — join via a config-taking helper.
var configFreeObservability = []familyMember{
	{prometheus.SourceType, prometheus.New()},
	{grafana.SourceType, grafana.New()},
}

// deniedRunner is the fail-closed argv runner the DISABLED actuation adapters carry: it never executes a
// command. A disabled adapter is never resolved (INV-17), so this is belt-and-suspenders — if a wiring
// mistake ever reached Exec on a Phase-0/1 actuator, it errors rather than running anything.
type deniedRunner struct{}

func (deniedRunner) Run(context.Context, []string, []byte) (actuation.Result, error) {
	return actuation.Result{}, errors.New("bootstrap: actuation is disabled in Phase 0/1 — no execution path")
}

// deniedMCPRunner is the same fail-closed guard for the MCP tool surface, whose runner takes a tool name.
type deniedMCPRunner struct{}

func (deniedMCPRunner) Run(context.Context, string, []string, []byte) (actuation.Result, error) {
	return actuation.Result{}, errors.New("bootstrap: mcp actuation is disabled in Phase 0/1 — no execution path")
}

// actuationModules is the estate-mutating family, declared DISABLED (see RegisterActuationDisabled). Each is
// constructed with empty host config and a fail-closed runner because a disabled adapter is never dialed; a
// Phase-2 enablement re-registers it with a real host-scoped runner behind the mutation gate.
var actuationModules = []familyMember{
	{ssh.Capability, ssh.New("", "", deniedRunner{})},
	{kubernetes.Capability, kubernetes.New("", deniedRunner{})},
	{proxmox.Capability, proxmox.New("", config.SecretRef(""))}, // native HTTP actuator, read-only (no gate) ⇒ inert
	{mcp.Capability, mcp.New(deniedMCPRunner{})},
}

// RegisterModelProviders registers the model-provider family (enabled).
func RegisterModelProviders(reg *modules.Registry) error {
	return registerFamily(reg, modules.SurfaceModel, modelProviders, true)
}

// RegisterConfigFreeIngest registers the ingest push-receivers that need no endpoint/secret to declare (enabled).
func RegisterConfigFreeIngest(reg *modules.Registry) error {
	return registerFamily(reg, modules.SurfaceIngest, configFreeIngest, true)
}

// RegisterConfigFreeObservability registers the observability exporters that need no endpoint/secret to declare (enabled).
func RegisterConfigFreeObservability(reg *modules.Registry) error {
	return registerFamily(reg, modules.SurfaceObservability, configFreeObservability, true)
}

// TrackerConfig carries the operator-declared issue-tracker endpoints (config-not-code). A tracker whose
// base URL is empty is NOT configured and is not registered — an unconfigured tracker is simply not a
// capability in this deployment (INV-17). Credential fields are secret REFERENCES (env:/file:), never
// literal secrets (INV-13); each maps to one of the four built tracker backends' constructors.
type TrackerConfig struct {
	YouTrackURL, YouTrackTokenRef string
	// The deployment's YouTrack State-field value names + the field name (config-not-code); empty ⇒ reference
	// default. The default bundle has no `Resolved` value, so a real project maps resolved onto e.g. `Fixed`.
	YouTrackStateInProgress, YouTrackStateResolved, YouTrackStateOpen, YouTrackStateField string
	JiraURL, JiraEmail, JiraTokenRef                                                      string
	// The deployment's Jira workflow transition ids (config-not-code); empty ⇒ the reference-workflow default.
	JiraTransitionInProgress, JiraTransitionResolved, JiraTransitionOpen string
	GitHubURL, GitHubOwner, GitHubRepo, GitHubTokenRef                   string
	ServiceNowURL, ServiceNowUser, ServiceNowTokenRef                    string
	// The deployment's ServiceNow incident.state codes (config-not-code); empty ⇒ out-of-box ITSM default.
	ServiceNowStateInProgress, ServiceNowStateResolved, ServiceNowStateOpen string
}

// RegisterTrackers registers each tracker whose base URL is configured (enabled). An unconfigured tracker is
// skipped, not registered disabled — it is absent from the capability set entirely. A duplicate (tracker,
// source) fails closed (INV-18).
func RegisterTrackers(reg *modules.Registry, cfg TrackerConfig) error {
	var members []familyMember
	if cfg.YouTrackURL != "" {
		members = append(members, familyMember{youtrack.SourceType, youtrack.New(cfg.YouTrackURL, config.SecretRef(cfg.YouTrackTokenRef),
			youtrack.WithStateNames(cfg.YouTrackStateInProgress, cfg.YouTrackStateResolved, cfg.YouTrackStateOpen),
			youtrack.WithStateFieldName(cfg.YouTrackStateField))})
	}
	if cfg.JiraURL != "" {
		members = append(members, familyMember{jira.SourceType, jira.New(cfg.JiraURL, cfg.JiraEmail, config.SecretRef(cfg.JiraTokenRef),
			jira.WithTransitions(cfg.JiraTransitionInProgress, cfg.JiraTransitionResolved, cfg.JiraTransitionOpen))})
	}
	if cfg.GitHubURL != "" {
		members = append(members, familyMember{githubissues.SourceType, githubissues.New(cfg.GitHubURL, cfg.GitHubOwner, cfg.GitHubRepo, config.SecretRef(cfg.GitHubTokenRef))})
	}
	if cfg.ServiceNowURL != "" {
		members = append(members, familyMember{servicenow.SourceType, servicenow.New(cfg.ServiceNowURL, cfg.ServiceNowUser, config.SecretRef(cfg.ServiceNowTokenRef),
			servicenow.WithStates(cfg.ServiceNowStateInProgress, cfg.ServiceNowStateResolved, cfg.ServiceNowStateOpen))})
	}
	return registerFamily(reg, modules.SurfaceTracker, members, true)
}

// NotifierConfig carries the operator-declared notifier endpoints and, crucially, each channel's approver
// set (config-not-code). A notifier whose primary endpoint is empty is not configured and is not registered.
// The approver lists are the human authorization roster for that channel (INV-12: a vote binds a decision
// only from a sender in this set); credentials are secret references (env:/file:), never literals (INV-13).
type NotifierConfig struct {
	MatrixHomeserver, MatrixTokenRef                           string
	MatrixApprovers                                            []string
	MatrixRooms                                                map[string]string // routed room name -> real room id/alias
	MatrixDefaultRoom                                          string            // where an unmapped route posts
	SlackURL, SlackTokenRef                                    string
	SlackApprovers                                             []string
	SlackChannels                                              map[string]string // routed channel name -> real channel id/name
	SlackDefaultChannel                                        string            // where an unmapped route posts
	TeamsURL, TeamsConversation, TeamsTokenRef                 string
	TeamsApprovers                                             []string
	EmailSMTP, EmailFrom                                       string
	EmailTo, EmailApprovers                                    []string
	EmailUser, EmailPasswordRef                                string // SMTP PLAIN auth; empty user ⇒ no-auth path
	TwilioURL, TwilioSID, TwilioFrom, TwilioTo, TwilioTokenRef string
	MattermostURL, MattermostTokenRef                          string
	MattermostApprovers                                        []string
	MattermostChannels                                         map[string]string
}

// RegisterNotifiers registers each notifier whose primary endpoint is configured (enabled). An unconfigured
// notifier is skipped entirely. A duplicate (notifier, source) fails closed (INV-18).
func RegisterNotifiers(reg *modules.Registry, cfg NotifierConfig) error {
	var members []familyMember
	if cfg.MatrixHomeserver != "" {
		members = append(members, familyMember{matrix.SourceType, matrix.New(cfg.MatrixHomeserver, config.SecretRef(cfg.MatrixTokenRef), cfg.MatrixApprovers,
			matrix.WithRooms(cfg.MatrixRooms), matrix.WithDefaultRoom(cfg.MatrixDefaultRoom))})
	}
	if cfg.SlackURL != "" {
		members = append(members, familyMember{slack.SourceType, slack.New(cfg.SlackURL, config.SecretRef(cfg.SlackTokenRef), cfg.SlackApprovers,
			slack.WithChannels(cfg.SlackChannels), slack.WithDefaultChannel(cfg.SlackDefaultChannel))})
	}
	if cfg.TeamsURL != "" {
		members = append(members, familyMember{teams.SourceType, teams.New(cfg.TeamsURL, cfg.TeamsConversation, config.SecretRef(cfg.TeamsTokenRef), cfg.TeamsApprovers)})
	}
	if cfg.EmailSMTP != "" {
		members = append(members, familyMember{email.SourceType, email.New(cfg.EmailSMTP, cfg.EmailFrom, cfg.EmailTo, cfg.EmailApprovers,
			email.WithAuth(cfg.EmailUser, config.SecretRef(cfg.EmailPasswordRef)))})
	}
	if cfg.TwilioURL != "" {
		members = append(members, familyMember{twiliosms.SourceType, twiliosms.New(cfg.TwilioURL, cfg.TwilioSID, cfg.TwilioFrom, cfg.TwilioTo, config.SecretRef(cfg.TwilioTokenRef))})
	}
	if cfg.MattermostURL != "" {
		members = append(members, familyMember{mattermost.SourceType, mattermost.New(cfg.MattermostURL, config.SecretRef(cfg.MattermostTokenRef), cfg.MattermostApprovers, cfg.MattermostChannels)})
	}
	return registerFamily(reg, modules.SurfaceNotifier, members, true)
}

// RegisterCMDB registers the NetBox CMDB reader if its endpoint is configured (enabled). NetBox is the
// authoritative entity source a payload's claimed fields are reconciled against before dispatch. (PVE is an
// estate topology source, not a CMDB resolver, so it is wired directly, not through this surface.)
func RegisterCMDB(reg *modules.Registry, netboxURL, netboxTokenRef string) error {
	if netboxURL == "" {
		return nil
	}
	return registerFamily(reg, modules.SurfaceCMDB, []familyMember{
		{netbox.SourceType, netbox.New(netboxURL, config.SecretRef(netboxTokenRef))},
	}, true)
}

// ObservabilityConfig carries the endpoint + secret-reference config for the observability exporters that
// need it (config-not-code). An exporter whose endpoint is empty is not configured and is not registered.
type ObservabilityConfig struct {
	OpenObserveEndpoint, OpenObserveTokenRef               string
	LangfuseEndpoint, LangfusePublicRef, LangfuseSecretRef string
	HealthchecksURL, HealthchecksCheckRef                  string
}

// RegisterConfiguredObservability registers the endpoint-driven observability exporters (openobserve,
// langfuse, healthchecks) that are configured. The config-free exporters (prometheus, grafana) declare via
// NewRegistry. A duplicate (observability, source) fails closed (INV-18).
func RegisterConfiguredObservability(reg *modules.Registry, cfg ObservabilityConfig) error {
	var members []familyMember
	if cfg.OpenObserveEndpoint != "" {
		members = append(members, familyMember{openobserve.SourceType, openobserve.New(cfg.OpenObserveEndpoint, config.SecretRef(cfg.OpenObserveTokenRef))})
	}
	if cfg.LangfuseEndpoint != "" {
		members = append(members, familyMember{langfuse.SourceType, langfuse.New(cfg.LangfuseEndpoint, config.SecretRef(cfg.LangfusePublicRef), config.SecretRef(cfg.LangfuseSecretRef))})
	}
	if cfg.HealthchecksURL != "" {
		members = append(members, familyMember{healthchecks.SourceType, healthchecks.New(cfg.HealthchecksURL, config.SecretRef(cfg.HealthchecksCheckRef))})
	}
	return registerFamily(reg, modules.SurfaceObservability, members, true)
}

// ParseLibrenmsDeployments parses the operator-declared LibreNMS deployment list —
// `site|baseurl|tokenref[|timezone]` rows separated by ';' (config-not-code; no URLs or token references
// compiled in). The optional 4th field is the IANA timezone the server renders its alert `$timestamp` in
// (e.g. "Europe/Athens"); omit it for a UTC server. A malformed or URL-less row is skipped; an empty spec
// yields no deployments. Both the grounder (ingest front door) and the worker (estate + ingest) declare
// LibreNMS from this one grammar.
func ParseLibrenmsDeployments(spec string) []librenms.Deployment {
	var out []librenms.Deployment
	for _, row := range strings.Split(spec, ";") {
		f := strings.Split(strings.TrimSpace(row), "|")
		if len(f) < 3 || strings.TrimSpace(f[1]) == "" {
			continue
		}
		d := librenms.Deployment{
			Site:     strings.TrimSpace(f[0]),
			BaseURL:  strings.TrimSpace(f[1]),
			TokenRef: strings.TrimSpace(f[2]),
		}
		if len(f) >= 4 {
			d.Timezone = strings.TrimSpace(f[3])
		}
		out = append(out, d)
	}
	return out
}

// RegisterConfiguredIngest registers the endpoint-driven ingest sources — today LibreNMS, whose per-site
// deployment list (site/URL/token, config-not-code) the caller parses. The config-free push-receivers
// (crowdsec, prometheus-alertmanager) declare via NewRegistry. No deployments means LibreNMS is not a
// capability in this deployment. (Its topology contribution to the estate graph is wired separately.)
func RegisterConfiguredIngest(reg *modules.Registry, librenmsDeployments []librenms.Deployment) error {
	if len(librenmsDeployments) == 0 {
		return nil
	}
	return registerFamily(reg, modules.SurfaceIngest, []familyMember{
		{librenms.SourceType, librenms.New(librenmsDeployments)},
	}, true)
}

// ParseAllowedUnits parses the operator-declared systemd-unit allowlist for the SSH mutating actuator —
// comma-, semicolon-, or whitespace-separated unit names (config-not-code, TG_ACTUATION_ALLOWED_UNITS; no
// units are compiled in). Blank entries are skipped. This is the ONLY source of the units a gated
// `restart-service` may target: the module never hardcodes a unit and never accepts an arbitrary one
// (task #21, REQ-822).
func ParseAllowedUnits(spec string) []string { return parseAllowlist(spec) }

// ParseAllowedContainers parses the operator-declared docker-container allowlist for the SSH mutating actuator
// (config-not-code, TG_ACTUATION_ALLOWED_CONTAINERS; no containers are compiled in). This is the ONLY source
// of the containers a gated `restart-container` may target — the module never hardcodes or accepts an
// arbitrary container name.
func ParseAllowedContainers(spec string) []string { return parseAllowlist(spec) }

// parseAllowlist splits an operator allowlist spec on commas, semicolons, or whitespace, dropping blanks.
func parseAllowlist(spec string) []string {
	var out []string
	for _, tok := range strings.FieldsFunc(spec, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\t'
	}) {
		if tok != "" {
			out = append(out, tok)
		}
	}
	return out
}

// BuildSSHActuator constructs the Phase-2 SSH MUTATING actuator behind the process mutation gate, or returns
// nil when it is not configured (no host or identity) — an unconfigured actuator is simply not wired. The
// reversible-op allowlist is the canary restart-service class; allowedUnits is the operator-declared unit
// allowlist (ParseAllowedUnits). Constructing it does NOT turn mutation on: while the gate is off (Phase
// 0/1) the returned module reports read-only and refuses every mutating call. This is the ready seam the
// mutation flip (#23) wires into the interceptor with a live ssh runner; #21 builds the wiring only.
func BuildSSHActuator(gate *safety.Chokepoint, host, identity string, run ssh.Runner, allowedUnits, allowedContainers []string) *ssh.Module {
	if strings.TrimSpace(host) == "" || strings.TrimSpace(identity) == "" {
		return nil
	}
	return ssh.New(host, identity, run, ssh.WithMutation(gate, allowedUnits, allowedContainers))
}

// EffectActuatorConfig carries the operator-declared SSH effect-leaf configuration for the interceptor's
// actuator (config-not-code: TG_ACTUATION_SSH_HOST / TG_ACTUATION_SSH_IDENTITY / TG_ACTUATION_ALLOWED_UNITS;
// no host, identity, or unit is compiled in). All empty (the default) ⇒ no SSH effect leaf is configured.
type EffectActuatorConfig struct {
	SSHHost, SSHIdentity, AllowedUnitsSpec, AllowedContainersSpec string
}

// BuildEffectActuator selects the interceptor's effect-leaf actuator. The DEFAULT — no SSH host+identity
// declared — is the read-only reference adapter, exactly the posture the worker ships today. WHERE an SSH
// host AND identity ARE declared it returns the gated SSH mutating actuator (BuildSSHActuator) with the
// operator-declared unit allowlist and the supplied local argv-only runner — but constructing it turns
// NOTHING on: while the process mutation gate is off (the whole of Phase 0/1) that module reports read-only
// and refuses every mutating call, and with an EMPTY allowlist even a (future) gate-on cannot resolve a unit.
// So the swap is behaviorally a no-op until the gate is flipped AND a unit is allowlisted — mutation stays
// OFF. The returned actuator also carries the execution_log recorder hook (ssh.Module implements
// actuate.ExecRecorder) the interceptor invokes only after a (gated) execute.
func BuildEffectActuator(gate *safety.Chokepoint, run ssh.Runner, cfg EffectActuatorConfig) actuation.Actuator {
	if m := BuildSSHActuator(gate, cfg.SSHHost, cfg.SSHIdentity, run, ParseAllowedUnits(cfg.AllowedUnitsSpec), ParseAllowedContainers(cfg.AllowedContainersSpec)); m != nil {
		return m
	}
	return actuation.LocalReadOnly{Cap: "actuation.local.readonly"}
}

// RegisterActuationDisabled DECLARES the actuation family but registers every member DISABLED, so INV-17
// gives each NO execution path: in Phase 0/1 (mutation OFF) the estate-mutating surface is inert BY
// CONSTRUCTION, not by a runtime check an author must remember. Enabling an actuator is a Phase-2 act that
// re-registers it with a real, host-scoped runner behind the proven mutation gate. The adapters here carry a
// fail-closed no-op runner (an accidental Exec on a disabled adapter errors, never runs a command) and empty
// host config, because a disabled adapter is never resolved and never dialed.
func RegisterActuationDisabled(reg *modules.Registry) error {
	return registerFamily(reg, modules.SurfaceActuation, actuationModules, false)
}

// NewRegistry builds a fresh registry populated with every connector family this composition root can
// declare without external configuration. Config-taking families (tracker/notifier/cmdb-endpoints/actuation
// and the config-driven ingest/observability members) register through configuration-taking helpers as they
// land. It returns the populated registry so the caller can log its Manifest and reconcile it against a
// signed set. It fails closed on the first duplicate registration (INV-18).
func NewRegistry() (*modules.Registry, error) {
	reg := modules.NewRegistry()
	for _, register := range []func(*modules.Registry) error{
		RegisterModelProviders,
		RegisterConfigFreeIngest,
		RegisterConfigFreeObservability,
		RegisterActuationDisabled,
	} {
		if err := register(reg); err != nil {
			return nil, err
		}
	}
	return reg, nil
}

// Reconcile compares the live capability manifest against an operator-declared expected set (config-not-code,
// TG_EXPECTED_CAPABILITIES). It fails closed on ANY divergence — a capability that is live but was not
// declared (a module enabled by a config slip or a supply-chain surprise) OR one declared but absent (a
// connector that failed to register) — so the running fleet can never silently differ from the intended one.
// This is the registry's "refuse a divergent live set" boot control (the manifest's reason for being).
// An empty expected set is a no-op: reconciliation is opt-in, and a deployment that does not pin its fleet
// simply logs what it declared.
func Reconcile(live, expected []string) error {
	if len(expected) == 0 {
		return nil
	}
	liveSet := make(map[string]bool, len(live))
	for _, c := range live {
		liveSet[c] = true
	}
	expSet := make(map[string]bool, len(expected))
	for _, c := range expected {
		expSet[c] = true
	}
	var unexpected, missing []string
	for _, c := range live {
		if !expSet[c] {
			unexpected = append(unexpected, c)
		}
	}
	for _, c := range expected {
		if !liveSet[c] {
			missing = append(missing, c)
		}
	}
	if len(unexpected) > 0 || len(missing) > 0 {
		sort.Strings(unexpected)
		sort.Strings(missing)
		return fmt.Errorf("module registry diverged from the declared capability set — unexpected: %v, missing: %v", unexpected, missing)
	}
	return nil
}
