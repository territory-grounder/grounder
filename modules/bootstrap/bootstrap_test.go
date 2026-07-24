package bootstrap_test

import (
	"context"
	"testing"

	actuation "github.com/territory-grounder/grounder/adapters/actuation"
	cmdb "github.com/territory-grounder/grounder/adapters/cmdb"
	ingest "github.com/territory-grounder/grounder/adapters/ingest"
	model "github.com/territory-grounder/grounder/adapters/model"
	notifier "github.com/territory-grounder/grounder/adapters/notifier"
	observability "github.com/territory-grounder/grounder/adapters/observability"
	tracker "github.com/territory-grounder/grounder/adapters/tracker"
	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/modules"
	"github.com/territory-grounder/grounder/modules/bootstrap"
	"github.com/territory-grounder/grounder/modules/ingest/librenms"
)

// TestNewRegistryDeclaresModelFamily proves the composition root actually populates the runtime registry —
// the manifest, empty before this package existed, now declares every model-provider capability.
func TestNewRegistryDeclaresModelFamily(t *testing.T) {
	reg, err := bootstrap.NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	want := []string{
		"model/anthropic", "model/deepseek", "model/mistral",
		"model/ollama", "model/openai", "model/zai",
	}
	set := map[string]bool{}
	for _, k := range reg.Manifest() {
		set[k] = true
	}
	for _, w := range want {
		if !set[w] {
			t.Errorf("capability manifest missing %q; got %v", w, reg.Manifest())
		}
	}
}

// TestModelProvidersResolveAsEnabledProviders proves INV-17's positive direction: a registered+enabled
// module HAS an execution path — it resolves to its concrete model.Provider adapter with a real identity.
func TestModelProvidersResolveAsEnabledProviders(t *testing.T) {
	reg, err := bootstrap.NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	for _, src := range []string{"zai", "deepseek", "mistral", "ollama", "anthropic", "openai"} {
		regn, err := reg.Resolve(modules.SurfaceModel, src)
		if err != nil {
			t.Fatalf("resolve %s: %v (a registered+enabled module must have an execution path, INV-17)", src, err)
		}
		p, ok := regn.Adapter.(model.Provider)
		if !ok {
			t.Fatalf("%s adapter is not a model.Provider", src)
		}
		if p.Name() != src {
			t.Errorf("provider name = %q, want %q", p.Name(), src)
		}
		if len(p.Models()) == 0 {
			t.Errorf("provider %s declares no models", src)
		}
	}
}

// TestNewRegistryDeclaresConfigFreeFamilies proves the composition root also declares the config-free
// ingest push-receivers and observability exporters, not just the model family.
func TestNewRegistryDeclaresConfigFreeFamilies(t *testing.T) {
	reg, err := bootstrap.NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	set := map[string]bool{}
	for _, k := range reg.Manifest() {
		set[k] = true
	}
	for _, w := range []string{
		"ingest/crowdsec", "ingest/prometheus-alertmanager",
		"observability/prometheus", "observability/grafana",
	} {
		if !set[w] {
			t.Errorf("capability manifest missing %q; got %v", w, reg.Manifest())
		}
	}
}

// TestIngestModulesResolveAsIngesters proves the ingest members resolve to their concrete ingest.Ingester
// adapter (INV-17 positive), with the source identity the registry keyed them under.
func TestIngestModulesResolveAsIngesters(t *testing.T) {
	reg, err := bootstrap.NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	for _, src := range []string{"crowdsec", "prometheus-alertmanager"} {
		regn, err := reg.Resolve(modules.SurfaceIngest, src)
		if err != nil {
			t.Fatalf("resolve ingest/%s: %v", src, err)
		}
		ing, ok := regn.Adapter.(ingest.Ingester)
		if !ok {
			t.Fatalf("ingest/%s adapter is not an ingest.Ingester", src)
		}
		if ing.SourceType() != src {
			t.Errorf("ingest source type = %q, want %q", ing.SourceType(), src)
		}
	}
}

// TestObservabilityModulesResolveAsExporters proves the observability members resolve to their concrete
// observability.Exporter adapter.
func TestObservabilityModulesResolveAsExporters(t *testing.T) {
	reg, err := bootstrap.NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	for _, src := range []string{"prometheus", "grafana"} {
		regn, err := reg.Resolve(modules.SurfaceObservability, src)
		if err != nil {
			t.Fatalf("resolve observability/%s: %v", src, err)
		}
		exp, ok := regn.Adapter.(observability.Exporter)
		if !ok {
			t.Fatalf("observability/%s adapter is not an observability.Exporter", src)
		}
		if exp.SourceType() != src {
			t.Errorf("observability source type = %q, want %q", exp.SourceType(), src)
		}
	}
}

// TestRegisterTrackersOnlyRegistersConfigured proves config-not-code: a tracker is a capability only where
// its endpoint is declared. An empty config registers nothing; a full config registers each configured
// tracker enabled, resolving to its concrete tracker.Tracker.
func TestRegisterTrackersOnlyRegistersConfigured(t *testing.T) {
	empty := modules.NewRegistry()
	if err := bootstrap.RegisterTrackers(empty, bootstrap.TrackerConfig{}); err != nil {
		t.Fatalf("empty RegisterTrackers: %v", err)
	}
	if len(empty.Manifest()) != 0 {
		t.Errorf("no tracker configured, but the registry declared %v", empty.Manifest())
	}

	reg := modules.NewRegistry()
	err := bootstrap.RegisterTrackers(reg, bootstrap.TrackerConfig{
		YouTrackURL:    "https://yt.example",
		JiraURL:        "https://jira.example",
		JiraEmail:      "ops@example",
		GitHubURL:      "https://api.github.example",
		GitHubOwner:    "org",
		GitHubRepo:     "infra",
		ServiceNowURL:  "https://sn.example",
		ServiceNowUser: "svc",
	})
	if err != nil {
		t.Fatalf("RegisterTrackers: %v", err)
	}
	for _, src := range []string{"youtrack", "jira", "github-issues", "servicenow"} {
		regn, rerr := reg.Resolve(modules.SurfaceTracker, src)
		if rerr != nil {
			t.Fatalf("resolve tracker/%s: %v", src, rerr)
		}
		trk, ok := regn.Adapter.(tracker.Tracker)
		if !ok {
			t.Fatalf("tracker/%s adapter is not a tracker.Tracker", src)
		}
		if trk.SourceType() != src {
			t.Errorf("tracker source type = %q, want %q", trk.SourceType(), src)
		}
	}
}

// TestRegisterNotifiersOnlyRegistersConfigured proves config-not-code for the approval-delivery surface: a
// notifier channel is a capability only where its endpoint is declared. An empty config registers nothing;
// a configured channel registers enabled and resolves to its concrete notifier.Notifier.
func TestRegisterNotifiersOnlyRegistersConfigured(t *testing.T) {
	empty := modules.NewRegistry()
	if err := bootstrap.RegisterNotifiers(empty, bootstrap.NotifierConfig{}); err != nil {
		t.Fatalf("empty RegisterNotifiers: %v", err)
	}
	if len(empty.Manifest()) != 0 {
		t.Errorf("no notifier configured, but the registry declared %v", empty.Manifest())
	}

	reg := modules.NewRegistry()
	err := bootstrap.RegisterNotifiers(reg, bootstrap.NotifierConfig{
		MatrixHomeserver:    "https://matrix.example",
		MatrixApprovers:     []string{"@ops:example"},
		SlackURL:            "https://slack.example",
		SlackApprovers:      []string{"U123"},
		TeamsURL:            "https://teams.example",
		TeamsConversation:   "conv-1",
		EmailSMTP:           "smtp.example:587",
		EmailFrom:           "tg@example",
		EmailTo:             []string{"oncall@example"},
		TwilioURL:           "https://twilio.example",
		TwilioSID:           "AC1",
		TwilioFrom:          "+1000",
		TwilioTo:            "+1999",
		MattermostURL:       "https://mm.example",
		MattermostApprovers: []string{"ops"},
	})
	if err != nil {
		t.Fatalf("RegisterNotifiers: %v", err)
	}
	for _, src := range []string{"matrix", "slack", "teams", "email", "twilio-sms", "mattermost"} {
		regn, rerr := reg.Resolve(modules.SurfaceNotifier, src)
		if rerr != nil {
			t.Fatalf("resolve notifier/%s: %v", src, rerr)
		}
		nfy, ok := regn.Adapter.(notifier.Notifier)
		if !ok {
			t.Fatalf("notifier/%s adapter is not a notifier.Notifier", src)
		}
		if nfy.SourceType() != src {
			t.Errorf("notifier source type = %q, want %q", nfy.SourceType(), src)
		}
	}
}

// TestRegisterConfiguredFamilies proves the endpoint-driven CMDB, observability, and ingest members register
// only where configured and resolve to their concrete adapters — completing the fleet's declarable set.
func TestRegisterConfiguredFamilies(t *testing.T) {
	reg := modules.NewRegistry()
	if err := bootstrap.RegisterCMDB(reg, "https://netbox.example", "env:NETBOX_TOKEN"); err != nil {
		t.Fatalf("RegisterCMDB: %v", err)
	}
	if err := bootstrap.RegisterConfiguredObservability(reg, bootstrap.ObservabilityConfig{
		OpenObserveEndpoint: "https://o2.example",
		LangfuseEndpoint:    "https://lf.example",
		HealthchecksURL:     "https://hc.example",
	}); err != nil {
		t.Fatalf("RegisterConfiguredObservability: %v", err)
	}
	if err := bootstrap.RegisterConfiguredIngest(reg, []librenms.Deployment{{Site: "nl", BaseURL: "https://ln.example", TokenRef: "env:LN"}}); err != nil {
		t.Fatalf("RegisterConfiguredIngest: %v", err)
	}

	regn, err := reg.Resolve(modules.SurfaceCMDB, "netbox")
	if err != nil {
		t.Fatalf("resolve cmdb/netbox: %v", err)
	}
	if _, ok := regn.Adapter.(cmdb.CMDB); !ok {
		t.Error("cmdb/netbox adapter is not a cmdb.CMDB")
	}
	for _, src := range []string{"openobserve", "langfuse", "healthchecks"} {
		if _, rerr := reg.Resolve(modules.SurfaceObservability, src); rerr != nil {
			t.Errorf("resolve observability/%s: %v", src, rerr)
		}
	}
	if r, rerr := reg.Resolve(modules.SurfaceIngest, "librenms"); rerr != nil {
		t.Errorf("resolve ingest/librenms: %v", rerr)
	} else if _, ok := r.Adapter.(ingest.Ingester); !ok {
		t.Error("ingest/librenms adapter is not an ingest.Ingester")
	}

	// Unconfigured => absent.
	none := modules.NewRegistry()
	_ = bootstrap.RegisterCMDB(none, "", "")
	_ = bootstrap.RegisterConfiguredObservability(none, bootstrap.ObservabilityConfig{})
	_ = bootstrap.RegisterConfiguredIngest(none, nil)
	if len(none.Manifest()) != 0 {
		t.Errorf("nothing configured, but the registry declared %v", none.Manifest())
	}
}

// TestParseLibrenmsDeployments proves config-not-code parsing: valid site|url|tokenref rows parse, malformed
// or URL-less rows are skipped (never a half-built deployment), and an empty spec yields none.
func TestParseLibrenmsDeployments(t *testing.T) {
	got := bootstrap.ParseLibrenmsDeployments("nl|https://ln-nl.example|env:LN_NL ; gr|https://ln-gr.example|env:LN_GR ; broken|only-two ; |https://no-site.example|env:X")
	if len(got) != 3 {
		t.Fatalf("want 3 valid deployments (2 full + 1 site-less-but-url-present), got %d: %+v", len(got), got)
	}
	if got[0].Site != "nl" || got[0].BaseURL != "https://ln-nl.example" || got[0].TokenRef != "env:LN_NL" {
		t.Errorf("unexpected first deployment: %+v", got[0])
	}
	if len(bootstrap.ParseLibrenmsDeployments("")) != 0 {
		t.Error("empty spec must yield no deployments")
	}
	if len(bootstrap.ParseLibrenmsDeployments("nourlrow|  |env:X")) != 0 {
		t.Error("a URL-less row must be skipped, not half-registered")
	}
}

// TestReconcileRefusesADivergentFleet proves the manifest is load-bearing: an exact match passes, an
// unexpected or missing capability fails closed, and an empty expected set is an opt-out no-op.
func TestReconcileRefusesADivergentFleet(t *testing.T) {
	live := []string{"model/zai", "ingest/crowdsec", "tracker/youtrack"}

	if err := bootstrap.Reconcile(live, []string{"tracker/youtrack", "model/zai", "ingest/crowdsec"}); err != nil {
		t.Errorf("exact match (order-insensitive) must pass, got: %v", err)
	}
	if err := bootstrap.Reconcile(live, nil); err != nil {
		t.Errorf("empty expected set is opt-out and must pass, got: %v", err)
	}
	// A capability live but not declared → refuse (supply-chain surprise / config slip).
	if err := bootstrap.Reconcile(live, []string{"model/zai", "ingest/crowdsec"}); err == nil {
		t.Error("an undeclared live capability (tracker/youtrack) must fail reconciliation")
	}
	// A capability declared but absent → refuse (a connector that failed to register).
	if err := bootstrap.Reconcile(live, []string{"model/zai", "ingest/crowdsec", "tracker/youtrack", "notifier/matrix"}); err == nil {
		t.Error("a missing declared capability (notifier/matrix) must fail reconciliation")
	}
}

// TestActuationFamilyDeclaredButHasNoExecutionPath is the load-bearing Phase-0/1 safety oracle: the
// estate-mutating actuators are DECLARED in the manifest (they exist, are known, are auditable) yet every
// one resolves to NO execution path because they ship disabled (INV-17). The dangerous surface is inert by
// construction while mutation is OFF — not by a runtime check an author must remember.
func TestActuationFamilyDeclaredButHasNoExecutionPath(t *testing.T) {
	reg, err := bootstrap.NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	set := map[string]bool{}
	for _, k := range reg.Manifest() {
		set[k] = true
	}
	for _, src := range []string{"ssh", "kubernetes", "proxmox", "mcp"} {
		if !set["actuation/"+src] {
			t.Errorf("actuation/%s must be DECLARED in the manifest; got %v", src, reg.Manifest())
		}
		if _, rerr := reg.Resolve(modules.SurfaceActuation, src); rerr == nil {
			t.Errorf("actuation/%s resolved to an execution path while disabled — INV-17 violated (Phase 0/1 is read-only)", src)
		}
	}
}

// TestDisabledActuatorRunnerFailsClosed proves the belt-and-suspenders guard: even if a wiring mistake
// enabled and resolved an actuator, its adapter is read-only in Phase 0/1 and its no-op runner refuses to
// execute a command — an accidental Exec errors rather than running anything.
func TestDisabledActuatorRunnerFailsClosed(t *testing.T) {
	reg, err := bootstrap.NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	// Force-enable ssh only within this test to reach its adapter; the composition root never enables it.
	if serr := reg.SetEnabled(modules.SurfaceActuation, "ssh", true); serr != nil {
		t.Fatalf("SetEnabled: %v", serr)
	}
	regn, err := reg.Resolve(modules.SurfaceActuation, "ssh")
	if err != nil {
		t.Fatalf("resolve enabled ssh: %v", err)
	}
	act, ok := regn.Adapter.(actuation.Actuator)
	if !ok {
		t.Fatalf("actuation/ssh adapter is not an actuation.Actuator")
	}
	if !act.ReadOnly() {
		t.Error("a Phase-0/1 actuator must report ReadOnly()==true")
	}
	if _, execErr := act.Exec(context.Background(), []string{"reboot"}, nil); execErr == nil {
		t.Error("the disabled actuator's no-op runner must refuse to execute a command (fail closed)")
	}
}

// TestRegisterModelProvidersRejectsDuplicateINV18 proves the boot registration fails closed on a duplicate
// (surface, source) rather than starting with an ambiguous provider set.
func TestRegisterModelProvidersRejectsDuplicateINV18(t *testing.T) {
	reg := modules.NewRegistry()
	if err := bootstrap.RegisterModelProviders(reg); err != nil {
		t.Fatalf("first registration: %v", err)
	}
	if err := bootstrap.RegisterModelProviders(reg); err == nil {
		t.Error("re-registering the model family must fail (INV-18: exactly one implementation per surface+source)")
	}
}

// TestUnregisteredModelHasNoExecutionPathINV17 proves the load-bearing negative: an unregistered provider
// resolves to no execution path.
func TestUnregisteredModelHasNoExecutionPathINV17(t *testing.T) {
	reg, err := bootstrap.NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if _, err := reg.Resolve(modules.SurfaceModel, "gemini"); err == nil {
		t.Error("an unregistered provider must have NO execution path (INV-17)")
	}
}

// TestParseAllowedUnits proves the SSH mutating actuator's unit allowlist is config-not-code: units come
// from an operator-declared string, blanks are skipped, and an empty spec yields no units (task #21).
func TestParseAllowedUnits(t *testing.T) {
	got := bootstrap.ParseAllowedUnits("nginx, caddy;  ; prometheus-node-exporter\ttailscaled")
	want := []string{"nginx", "caddy", "prometheus-node-exporter", "tailscaled"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	if len(bootstrap.ParseAllowedUnits("")) != 0 {
		t.Error("an empty spec must yield no allowed units")
	}
}

// TestBuildSSHActuatorInertWhileGateOff proves the Phase-2 SSH mutating actuator is built behind the process
// gate but stays inert (read-only) while mutation is off, and that an unconfigured actuator is not wired.
func TestBuildSSHActuatorInertWhileGateOff(t *testing.T) {
	gate := safety.NewReadOnlyChokepoint() // OFF (Phase 0/1)
	act := bootstrap.BuildSSHActuator(gate, "web01", "svc-agent", nil, []string{"nginx"}, nil)
	if act == nil {
		t.Fatal("a configured SSH mutating actuator must be built")
	}
	if !act.ReadOnly() {
		t.Error("gate OFF: the SSH mutating actuator must report read-only (inert)")
	}
	if bootstrap.BuildSSHActuator(gate, "", "svc-agent", nil, nil, nil) != nil {
		t.Error("an SSH actuator with no host must not be wired")
	}
	if bootstrap.BuildSSHActuator(gate, "web01", "", nil, nil, nil) != nil {
		t.Error("an SSH actuator with no identity must not be wired")
	}
}

// TestBuildEffectActuatorDefaultsReadOnly proves the interceptor's effect-leaf selection is safe by DEFAULT
// (HARD SAFETY REQ 4: the deployed worker behaves exactly as today — read-only) and stays inert even when the
// SSH seam is configured while mutation is off (#23 is the wiring, not the flip).
func TestBuildEffectActuatorDefaultsReadOnly(t *testing.T) {
	gate := safety.NewReadOnlyChokepoint() // OFF

	// DEFAULT (no SSH host/identity) ⇒ the read-only reference adapter — identical to today's posture:
	// read-only and NOT an execution_log recorder (there is nothing to record).
	def := bootstrap.BuildEffectActuator(gate, actuation.LocalRunner{}, bootstrap.EffectActuatorConfig{})
	if !def.ReadOnly() {
		t.Fatal("the default effect actuator must be read-only (today's posture)")
	}
	if _, ok := def.(actuate.ExecRecorder); ok {
		t.Fatal("the read-only reference actuator must NOT be an execution_log recorder")
	}

	// SSH host+identity declared but gate OFF + EMPTY allowlist ⇒ still read-only + refuses; the swap is a
	// behavioral no-op until the gate is flipped AND a unit is allowlisted. It DOES carry the execution_log
	// recorder hook the interceptor invokes only after a (gated) execute.
	seam := bootstrap.BuildEffectActuator(gate, actuation.LocalRunner{}, bootstrap.EffectActuatorConfig{
		SSHHost: "web01", SSHIdentity: "svc-agent", // no TG_ACTUATION_ALLOWED_UNITS declared
	})
	if !seam.ReadOnly() {
		t.Fatal("gate OFF + empty allowlist: the SSH effect leaf must still report read-only (inert)")
	}
	if _, ok := seam.(actuate.ExecRecorder); !ok {
		t.Fatal("the SSH effect leaf must be an execution_log recorder (INV-07) so Do records through the chokepoint")
	}
}
