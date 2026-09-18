package deploy

// THE EGRESS PARITY GUARD (TG-160).
//
// WHY THIS EXISTS. Until 2026-08-04 deploy/docker-compose.yml had NO `networks:` stanza at all. Every one
// of its fourteen services sat on the compose default bridge with full outbound NAT, so Postgres, Grafana,
// the Temporal UI and the busybox perms fixer could each open a socket to anywhere on the internet. On the
// deployed box `iptables -S DOCKER-USER` was the stock `-A DOCKER-USER -j RETURN`, the helm chart shipped
// no NetworkPolicy, and `grep -rn -w -i egress --include=*.go .` returned ZERO over the whole tree — while
// docs/THREAT-MODEL.md advertised an "egress" step in the interceptor chain. Nothing could have caught any
// of that, because no test anywhere read a network key.
//
// WHAT IT ASSERTS. The expected outbound posture lives HERE, in Go; the actual posture lives in
// docker-compose.yml; the test fails on any disagreement in EITHER direction. Granting a service egress
// fails it, and so does removing egress without recording that here — because a table allowed to drift
// weaker than reality stops describing anything and becomes decoration. This is the same doctrine as
// hardening_parity_test.go, applied to the network instead of the container.
//
// WHY "EITHER DIRECTION" MATTERS HERE SPECIFICALLY. Egress is granted by adding one word to a list. That
// is the cheapest possible edit and the one least likely to be noticed in review, and the services most
// likely to receive it (alertmanager, when someone wires a real pager; prometheus, when someone adds an
// off-host scrape target) are the ones nobody thinks of as security-relevant. Forcing the grant to be
// declared in two places makes it a decision instead of a side effect.
//
// THE TIER BOUNDARIES WERE MEASURED, not assumed — docker 20.10.24, dev box, 2026-08-04:
//   - internal:true → off-host outbound TIMES OUT; east-west and compose-name DNS keep working; and a
//     published `ports:` mapping does NOT serve (connection refused). That last one is why six services
//     that need no outbound still cannot be backplane-only.
//   - enable_ip_masquerade=false → published port serves (HTTP 200) AND outbound to the internet times
//     out and to a LAN neighbour fails. "Reachable, cannot reach out."
//   - BOTH restricted tiers still reach the DOCKER HOST itself (`nc -z` to the bridge gateway and to the
//     host's LAN IP on :22 both succeeded), because docker's isolation rules sit in FORWARD and
//     host-destined traffic goes through INPUT. Recorded here so nobody reads this guard as proving more
//     than it does.
//
// It is pure-stdlib + yaml.v3 over a file read, so it is CI-runnable and deterministic.

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	netBackplane = "tg-backplane" // internal: true — east-west only
	netFrontdoor = "tg-frontdoor" // publishable, no SNAT — reachable, cannot reach out
	netEgress    = "tg-egress"    // the only path off the box
)

// egressPosture is the expected network attachment of one compose service.
type egressPosture struct {
	frontdoor bool   // needs to serve a published port
	egress    bool   // needs to reach off the box
	why       string // MANDATORY whenever egress is true: WHAT it reaches, enumerated
}

// expectedEgress must name EVERY service in docker-compose.yml. A service present in compose but absent
// here fails the test: a new service has to state where it may send packets, it cannot arrive unexamined.
//
// Every service is implicitly on tg-backplane; that is asserted separately, because a service on NO
// network at all silently lands on the compose default bridge with full NAT — the exact pre-TG-160 state,
// reachable by simply forgetting a line.
var expectedEgress = map[string]egressPosture{
	// --- backplane only: no published port, no route off the box ---
	// postgres SERVES one published port for OpenBao's `database` secret engine (TG-422, armed 2026-08-22):
	// the engine dials in from the bao server to mint/revoke the per-lease Postgres logins. Frontdoor only —
	// reachable, no SNAT, still no path off the box. The bind is .env-parameterized, loopback by default,
	// so an unarmed deployment publishes nothing beyond the host.
	"postgres":          {frontdoor: true},
	"temporal-postgres": {},
	"secrets-perms":     {},

	// --- frontdoor: published, but nothing to say to the outside world ---
	// temporal is BACKPLANE-ONLY and unpublished (TG-327). It had frontdoor:true so its published :7233
	// could serve, and that second network took the control plane down on 2026-08-05: temporalio/auto-setup
	// defaults BIND_ON_IP to `hostname -i`, which is ambiguous when multi-homed, so temporal bound nothing
	// usable and the worker got `connection refused` on the correct backplane address. See
	// deploy/temporal_single_homed_test.go for the full mechanism and the guard that pins it.
	"temporal":    {},
	"temporal-ui": {frontdoor: true},
	"console":     {frontdoor: true},
	// PROMETHEUS EARNED EGRESS THE WAY THIS COMMENT PREDICTED IT WOULD (2026-08-06). The guard's header
	// names it by name — "prometheus, when someone adds an off-host scrape target" — and that is exactly what
	// happened when TG-231 added opus-sidecar. The grant was NOT made, so the scrape timed out after 10.0s on
	// every attempt (measured by raw IP as well as by name, while the same GET from the TG host returned 200
	// in 2ms), and SidecarDown — severity CRITICAL, the only such alert TG raises about its single brain —
	// fired all day against a healthy sidecar.
	//
	// This table could not have caught that: it compares the compose file against itself-declared-in-Go and
	// never reads prometheus.yml, so a TARGET added without the network it needs was outside its population
	// entirely. TestEveryOffHostScrapeTargetHasAPathToIt closes that.
	// EGRESS GIVEN BACK (TG-413/TG-324). It read `{frontdoor: true, egress: true}` with the reason
	// "scrapes the Opus sidecar at dc1claude01:8094 … the only target in prometheus.yml that is not a
	// compose service". That target moved on-box, so the sole justification evaporated — and the stale
	// scrape was worse than merely unnecessary: it stayed `health=up` against the OLD proxy after the
	// cutover, so every sidecar_* series described a machine serving no production traffic while the live
	// sidecar went unscraped.
	//
	// A grant whose stated reason has become false is worse than an undocumented one, because it reads as
	// reviewed. Prometheus has no remote_write and no other external target.
	"prometheus":   {frontdoor: true},
	"grafana":      {frontdoor: true},
	"alertmanager": {frontdoor: true},
	// The isolation-signal textfile exporter (TG-419): serves in-stack scrapes on tg-backplane and reads a
	// host bind mount ro. It sends nothing anywhere — no frontdoor, no egress, and the guard should red the
	// moment either appears.

	// --- egress: each one enumerated ---
	// The model sidecar (TG-413). ONE destination: api.anthropic.com, dialed by the Claude CLI on the
	// operator's subscription. It is deliberately NOT on tg-frontdoor — it publishes no host port and
	// nothing outside the stack may reach it; litellm talks to it over tg-backplane by service name.
	// Before TG-413 this traffic left a different machine entirely (the agent's dev workstation), and the
	// tg-egress tier existed on tg01 for exactly one target: reaching that box's :8094.
	"sidecar": {egress: true,
		why: "api.anthropic.com — the Claude CLI completing on the operator's subscription; no other destination"},

	// --- egress: each one enumerated ---
	"sidecar-perms": {},
	"sidecar-secrets": {egress: true,
		why: "resolves the sidecar's subscription token from OpenBao (TG_OPENBAO_ADDR), off-host — TG-413"},
	"litellm-secrets": {egress: true,
		why: "resolves the LiteLLM master key + every provider key from OpenBao (TG_OPENBAO_ADDR), off-host"},
	// NARROWED FOR TG-414. This grant used to name Anthropic among litellm's direct dials, but since
	// TG-413 the Anthropic hop is litellm -> on-box sidecar -> api.anthropic.com, and the `sidecar` entry
	// above holds that grant. litellm keeps egress for the OTHER providers it still dials directly.
	"litellm": {frontdoor: true, egress: true,
		why: "dials the non-Anthropic model providers directly (DeepSeek/Mistral/Z.AI; OpenAI/xAI/Kimi are " +
			"keyless dead rungs today, re-armable); the Anthropic hop goes via the on-box sidecar, which holds " +
			"that grant (TG-414). This is the hop the in-process Go meter structurally cannot see"},
	// TG-420 slice 1: the litellm->provider egress proxy. It is the DESTINATION of litellm's provider
	// traffic once armed (the future sole path out), so it needs the egress tier itself. Profile-gated
	// (`egress-proxy`) and unarmed by default, which changes nothing about the egress posture it must
	// declare — a service that is inert today is one somebody turns on later (see the worker-actuate note).
	"tg-egress-proxy": {egress: true,
		why: "the litellm->provider egress proxy (TG-420): CONNECTs to the model-provider API domains " +
			"(api.mistral.ai/api.deepseek.com/api.z.ai, derived from litellm-config.yaml); no published port"},
	"grounder": {frontdoor: true, egress: true,
		why: "OpenBao for its own read credential and the console WRITER AppRole, plus LDAPS to FreeIPA for " +
			"browser operator login. It does NOT reach the estate or any model provider"},
	"worker": {egress: true,
		why: "OpenBao, the estate (LibreNMS/NetBox/PVE/AWX/Semaphore/SSH/journald), the trackers " +
			"(YouTrack/Jira/GitHub/ServiceNow), the notifiers (Matrix/Slack/Teams/Mattermost/Twilio/SMTP), " +
			"the exporters (Langfuse/OpenObserve/healthchecks) and LDAPS — all declared as endpoint env"},
	"worker-actuate": {egress: true,
		why: "the actuation plane MUTATES the estate: SSH to target hosts, the PVE API, the AWX job lane, " +
			"plus OpenBao for its own AppRole"},
}

func TestComposeEgressPostureMatchesTheDeclaredTable(t *testing.T) {
	services := composeServices(t)

	// VACUITY FLOOR. Every assertion below is a comparison against a parsed document; if the parse yielded
	// nothing, every comparison passes and the guard reports a clean egress posture for a file it never
	// read. Two independent floors: the service set must be non-trivial, and the table must actually
	// restrict something (at least one backplane-only service and at least one egress service), because a
	// table where every service is on tg-egress is byte-equivalent to having no segmentation at all.
	if len(services) < 10 {
		t.Fatalf("only %d services parsed from docker-compose.yml — this guard would pass vacuously", len(services))
	}
	var restricted, permitted int
	for _, p := range expectedEgress {
		if p.egress {
			permitted++
		} else {
			restricted++
		}
	}
	if restricted == 0 {
		t.Fatal("VACUOUS TABLE: every service is expected to have egress, which is exactly the pre-TG-160 " +
			"posture (default bridge, full outbound NAT for everything). The guard would then be green " +
			"about a stack with no segmentation.")
	}
	if permitted == 0 {
		t.Fatal("VACUOUS TABLE: no service is expected to have egress. The worker cannot reach the estate " +
			"and litellm cannot reach a model provider, so this cannot be describing the real stack.")
	}

	for _, name := range sortedKeys(services) {
		want, declared := expectedEgress[name]
		if !declared {
			t.Errorf("service %q is in docker-compose.yml but not in expectedEgress. Declare where it may "+
				"send packets — a service that arrives unexamined is how every container on this stack ended "+
				"up with unrestricted outbound in the first place.", name)
			continue
		}
		got := serviceNetworks(t, name, services[name])

		// A service with NO networks lands on the compose default bridge: full outbound NAT, no isolation.
		// That is the failure mode a single deleted line reintroduces, so it is checked first and by name.
		if len(got) == 0 {
			t.Errorf("service %q declares NO `networks:` — it lands on the compose DEFAULT BRIDGE with full "+
				"outbound NAT, which is the exact pre-TG-160 posture this guard exists to prevent", name)
			continue
		}
		if !got[netBackplane] {
			t.Errorf("service %q is not on %s. Every service belongs on the backplane: it is where east-west "+
				"traffic lives, and omitting it means the service reaches its peers over a NAT-capable "+
				"bridge instead.", name, netBackplane)
		}
		if got[netEgress] != want.egress {
			if got[netEgress] {
				t.Errorf("service %q is on %s but the table says it needs NO outbound. Either it gained a "+
					"real off-host dependency — record it here WITH the destinations enumerated — or a "+
					"route off the box was granted without anyone deciding to.", name, netEgress)
			} else {
				t.Errorf("service %q is expected to have outbound (%s) but is NOT on %s. Its off-host "+
					"dependencies will fail: %s", name, netEgress, netEgress, want.why)
			}
		}
		if got[netFrontdoor] != want.frontdoor {
			t.Errorf("service %q frontdoor=%v, table says %v. MEASURED: a container whose only network is "+
				"internal cannot serve a published port, so a published service without %s is unreachable — "+
				"and an unpublished service on it is a bridge nobody needs.",
				name, got[netFrontdoor], want.frontdoor, netFrontdoor)
		}
		// A published port and no frontdoor is the outage this change could cause; catch it against the
		// FILE, not just against the table, so the two cannot be wrong together.
		if _, published := services[name]["ports"]; published && !got[netFrontdoor] && !got[netEgress] {
			t.Errorf("service %q publishes a port but is on neither %s nor %s — MEASURED on docker "+
				"20.10.24: the published port will not serve (connection refused)", name, netFrontdoor, netEgress)
		}
	}

	for _, name := range sortedTableKeys(expectedEgress) {
		if _, ok := services[name]; !ok {
			t.Errorf("expectedEgress names %q, which no longer exists in docker-compose.yml — a table that "+
				"describes services that are gone stops being read", name)
		}
	}
}

// TestEveryEgressGrantCarriesAnEnumeratedReason keeps the table honest. "why" is the difference between a
// declaration and a rubber stamp: an allowlist entry with no stated destination cannot be reviewed, and
// cannot be revoked later by anyone who was not in the room when it was added.
func TestEveryEgressGrantCarriesAnEnumeratedReason(t *testing.T) {
	var checked int
	for _, name := range sortedTableKeys(expectedEgress) {
		p := expectedEgress[name]
		if !p.egress {
			continue
		}
		checked++
		if strings.TrimSpace(p.why) == "" {
			t.Errorf("service %q is granted egress with no stated reason", name)
		}
		if len(p.why) < 30 {
			t.Errorf("service %q egress reason is too thin to review (%q) — name the destinations", name, p.why)
		}
	}
	if checked == 0 {
		t.Fatal("VACUOUS: no egress grant was examined, so this test asserts nothing")
	}
}

// TestEgressNetworkDefinitionsHaveTheMeasuredProperties pins the three tiers themselves. The per-service
// table is meaningless if `tg-backplane` quietly loses `internal: true` or `tg-frontdoor` regains
// masquerade: every service would still be on "the right network" and every one of them would have full
// outbound NAT again, with this guard green.
func TestEgressNetworkDefinitionsHaveTheMeasuredProperties(t *testing.T) {
	raw, err := os.ReadFile("docker-compose.yml")
	if err != nil {
		t.Fatalf("read compose: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse docker-compose.yml: %v", err)
	}
	nets, ok := doc["networks"].(map[string]any)
	if !ok || len(nets) == 0 {
		t.Fatalf("docker-compose.yml has no top-level `networks:` mapping (%T). Without it every service "+
			"is on the default bridge with full outbound NAT.", doc["networks"])
	}
	for _, want := range []string{netBackplane, netFrontdoor, netEgress} {
		if _, ok := nets[want]; !ok {
			t.Fatalf("network %q is not defined", want)
		}
	}

	back, _ := nets[netBackplane].(map[string]any)
	if v, _ := back["internal"].(bool); !v {
		t.Errorf("%s is not `internal: true`. MEASURED: internal is what installs docker's "+
			"`! -d <subnet> -i br-X -j DROP` isolation rule; without it the backplane is an ordinary "+
			"NAT bridge and postgres/grafana/temporal-ui can reach the internet again.", netBackplane)
	}
	front, _ := nets[netFrontdoor].(map[string]any)
	opts, _ := front["driver_opts"].(map[string]any)
	if fmt.Sprint(opts["com.docker.network.bridge.enable_ip_masquerade"]) != "false" {
		t.Errorf("%s does not disable ip masquerade. MEASURED: with SNAT off the published port still "+
			"serves (HTTP 200) while outbound to the internet times out; with SNAT on, every frontdoor "+
			"service — including console, the only one published on 0.0.0.0 — regains full outbound.",
			netFrontdoor)
	}
	// tg-egress must stay a PLAIN bridge. If it ever grew `internal: true` the services that genuinely
	// need outbound would lose it, which is the outage direction of this change.
	eg, _ := nets[netEgress].(map[string]any)
	if v, _ := eg["internal"].(bool); v {
		t.Errorf("%s is marked internal — the worker could not reach the estate and litellm could not "+
			"reach a model provider", netEgress)
	}
}

// serviceNetworks reads a service's `networks:` in either compose shape (a list of names, or a mapping of
// name → options) and returns them as a set.
func serviceNetworks(t *testing.T, name string, svc map[string]any) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	switch x := svc["networks"].(type) {
	case nil:
		return out
	case []any:
		for _, n := range x {
			out[fmt.Sprint(n)] = true
		}
	case map[string]any:
		for n := range x {
			out[n] = true
		}
	default:
		t.Fatalf("service %q has an unreadable `networks:` (%T)", name, svc["networks"])
	}
	return out
}

func sortedTableKeys(m map[string]egressPosture) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
