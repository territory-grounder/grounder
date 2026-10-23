package deploy

// THE CONTAINER-HARDENING PARITY GUARD (TG-289).
//
// WHY THIS EXISTS. A hardening pass (TG-153 gap #9) added `cap_drop: [ALL]`, `no-new-privileges` and a
// read-only rootfs to worker, grounder and litellm. It covered 3 of 13 services and it missed `console`
// — the ONLY service published on 0.0.0.0, i.e. the only one an unauthenticated host on the network can
// reach. So the posture inverted: the internal services were constrained and the front door was not.
// Live `docker inspect` on dc1tg01 read `RO=False CapDrop=None SecOpt=None` for console, with
// `root 1 nginx: master process`, three weeks after the pass was called done.
//
// Nothing caught it, and nothing COULD have: before this file, `grep -rn cap_drop --include=*.go` over the
// whole repo returned zero hits. No test anywhere read the hardening keys, so a service could lose its
// securityContext — or never receive one — with every gate still green. That is this repo's signature
// defect (a control that is built, green, and never actually checked), applied to the controls themselves.
//
// WHAT IT ASSERTS. The expected posture lives HERE, in Go, and the actual posture lives in
// docker-compose.yml; the test fails on any disagreement in either direction. Weakening a service fails
// it, and so does hardening one without recording that here — because a table that is allowed to drift
// weaker than reality stops describing anything and quietly becomes decoration.
//
// Every exemption carries a reason, and every reason below except litellm-secrets is an OBSERVED failure
// (the container was run under the posture and the error copied out), not a guess.
//
// It is pure-stdlib + yaml.v3 and does only a file read, so it is CI-runnable and deterministic. It sits
// in the non-governed deploy/ package for the reason given at the top of envparity_test.go.
//
// NOTE ON yaml.v3: `yaml.safe_load` in Python silently accepts a duplicate key and keeps the last one,
// which is how a compose file can lose a service's hardening block to a careless paste and still "parse".
// yaml.v3 rejects duplicate keys outright, so this guard also fails on that.

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// posture is the expected security posture of one compose service across the three axes compose exposes.
// why is MANDATORY whenever any axis is false: an exemption without a stated reason is how the first
// hardening pass ended up "covering" ten services it had never looked at.
type posture struct {
	capDropAll bool // cap_drop: ["ALL"]  — and nothing added back via cap_add
	noNewPriv  bool // security_opt: ["no-new-privileges:true"]
	readOnly   bool // read_only: true
	why        string
}

func (p posture) full() bool { return p.capDropAll && p.noNewPriv && p.readOnly }

// expectedPosture must name EVERY service in docker-compose.yml. A service present in compose but absent
// here fails the test: a new service has to state its posture, it cannot arrive unexamined.
var expectedPosture = map[string]posture{
	// --- fully hardened: all capabilities dropped, no privilege escalation, read-only rootfs ---
	"worker": {capDropAll: true, noNewPriv: true, readOnly: true},
	// The opt-in actuation-plane worker (TG-153). SAME binary, SAME posture as `worker` — and it is the
	// process that HOLDS the estate-mutating credential, so anything less here would put the weakest
	// container around the strongest key. It is behind the `split-planes` compose profile (not started by
	// default), which changes nothing about the posture it must declare: a service that is inert today is a
	// service somebody turns on later, and this guard exists because a service arrived unexamined once.
	"worker-actuate": {capDropAll: true, noNewPriv: true, readOnly: true},
	"grounder":       {capDropAll: true, noNewPriv: true, readOnly: true},
	"console":        {capDropAll: true, noNewPriv: true, readOnly: true},
	"prometheus":     {capDropAll: true, noNewPriv: true, readOnly: true},
	// TG-420 slice 1: the litellm->provider egress proxy (tinyproxy). Fully hardened, and MEASURED rather
	// than claimed (2026-08-14): built from a pinned Alpine base, it runs as a fixed non-root uid (so
	// tinyproxy never setuids and ALL caps can drop), binds 8888 (>1024, no CAP_NET_BIND_SERVICE) and only
	// makes outbound CONNECTs, and writes its log to stdout — nothing to the rootfs, so read_only holds. It
	// is behind the `egress-proxy` profile (not started by default); an inert service still declares its
	// posture here, for the same reason worker-actuate does.
	"tg-egress-proxy": {capDropAll: true, noNewPriv: true, readOnly: true},

	// The model sidecar, moved on-box by TG-413. Caps dropped and no-new-privileges, but NOT read_only:
	// the Claude CLI writes its own state (refreshed credentials, warm-pool workers) under $HOME, which is
	// a host volume rather than the rootfs — and the CLI additionally writes scratch to its rootfs during a
	// completion. Declared honestly rather than claimed: read_only here was not attempted, so it is listed
	// as writable rather than asserted as tested, which is the distinction this table exists to preserve.
	// It publishes NO host port and joins tg-backplane + tg-egress only (see sidecar_on_box_test.go).
	"sidecar": {capDropAll: true, noNewPriv: true, readOnly: false,
		why: "the Claude CLI writes state under $HOME and scratch to its rootfs during a completion; " +
			"read_only untested for it, so declared writable rather than claimed"},

	// --- capabilities dropped, rootfs still writable; each reason was measured on the dev box ---
	"litellm": {capDropAll: true, noNewPriv: true, readOnly: false,
		why: "python app writes to its own rootfs; holds every provider key so the caps still matter"},
	"alertmanager": {capDropAll: true, noNewPriv: true, readOnly: false,
		why: "writes data/ under its workdir; a tmpfs over an EXISTING image dir comes up root-owned and " +
			"this daemon ignores mode= there, so read_only needs a named volume (separate change)"},
	// TG-419's textfile exporter: runs nobody, reads one ro bind mount, writes nothing — full posture.
	"grafana": {capDropAll: true, noNewPriv: true, readOnly: false,
		why: "unpacks plugin state under /usr/share/grafana on the rootfs"},
	"temporal": {capDropAll: true, noNewPriv: true, readOnly: false,
		why: "auto-setup renders its config into /etc/temporal on every start"},
	"temporal-ui": {capDropAll: true, noNewPriv: true, readOnly: false,
		why: "writes ./config/docker.yaml into its workdir at start; pinned to uid 5000 so it no longer " +
			"needs CAP_DAC_OVERRIDE to do it, which is what let the caps be dropped at all"},

	// --- not hardened: dropping capabilities provably breaks these ---
	"postgres": {capDropAll: false, noNewPriv: false, readOnly: false,
		why: "root-then-switch entrypoint; under cap_drop ALL the image dies with " +
			"'failed switching to postgres: operation not permitted' and the database never starts"},
	"temporal-postgres": {capDropAll: false, noNewPriv: false, readOnly: false,
		why: "same image family and same measured failure as the postgres service"},
	"secrets-perms": {capDropAll: false, noNewPriv: false, readOnly: false,
		why: "chgrp/chmod on files it does not own IS its job; cap_drop ALL yields 'chgrp: Operation not " +
			"permitted', and its command swallows that error, so it would fail silently"},
	// The drop-directory chown one-shot (TG-413), same shape and same posture as `secrets-perms`: busybox,
	// root, exits immediately. It exists because tg-secretenv cannot chown its own output and the worker
	// image is distroless.
	"sidecar-perms": {capDropAll: false, noNewPriv: false, readOnly: false,
		why: "busybox one-shot as root to chown the secret drop; identical shape to secrets-perms"},

	// The sidecar's OpenBao init (TG-413). SAME posture and SAME weaker reason as litellm-secrets, which
	// it is modelled on: root writing a 0600 drop to a host tmpfs, unexercisable on the dev box because it
	// needs a reachable OpenBao and a resolvable ref. Recorded as unverified rather than dressed up.
	"sidecar-secrets": {capDropAll: false, noNewPriv: false, readOnly: false,
		why: "same shape and same unverified reason as litellm-secrets: root, 0600 drop, needs a live OpenBao"},

	"litellm-secrets": {capDropAll: false, noNewPriv: false, readOnly: false,
		why: "the one service that could not be exercised on the dev box (needs a reachable OpenBao); " +
			"unverified rather than known-safe, and recorded as the weaker reason it is"},
}

// serviceFloor is the vacuity floor for the enumeration itself. The stack has 13 services; if the parse
// ever hands back fewer, the walk broke and every assertion below would pass over an empty set. This is
// the failure mode the repo keeps rediscovering: a scan that silently stops matching reads as a pass.
const serviceFloor = 13

// fullyHardenedFloor is the vacuity floor for the posture table. If the "full" tier ever empties out,
// the table has been gutted and the guard is asserting nothing worth asserting.
const fullyHardenedFloor = 4

// KILLING MUTATION (executed 2026-08-04): delete the four hardening lines from the `console` service in
// deploy/docker-compose.yml — the exact state live docker inspect found on dc1tg01 — and this test
// goes RED with:
//
//	console: expected cap_drop ["ALL"], compose has [] | expected security_opt no-new-privileges:true,
//	compose has [] | expected read_only true, compose has false
//	  -> this service is PUBLISHED ON 0.0.0.0 and is therefore the most exposed container in the stack.
//
// Restored, it is green. That mutation is precisely the regression that stood in production for three
// weeks with every gate green.
func TestComposeHardeningParity(t *testing.T) {
	services := composeServices(t)

	if len(services) < serviceFloor {
		t.Fatalf("enumerated only %d service(s) from docker-compose.yml, expected at least %d — the "+
			"compose walk is broken, so this is an extraction failure, NOT a clean run", len(services), serviceFloor)
	}

	// A table entry naming a service that no longer exists means the guard silently stopped covering
	// something (a rename is the usual cause). Catch it before it becomes an unnoticed hole.
	var stale []string
	for name := range expectedPosture {
		if _, ok := services[name]; !ok {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("expectedPosture names %d service(s) that docker-compose.yml no longer defines: %s\n"+
			"Each is a rule that now matches NOTHING. Rename or delete the entry — do not leave it, or the "+
			"guard reports a pass for a service it never saw.", len(stale), strings.Join(stale, ", "))
	}

	fullyHardened := 0
	for _, name := range sortedKeys(services) {
		svc := services[name]
		want, declared := expectedPosture[name]
		if !declared {
			t.Errorf("service %q is defined in docker-compose.yml but has no entry in expectedPosture.\n"+
				"Add one stating its intended posture. A new service must not arrive unexamined — that is "+
				"exactly how `console` sat on 0.0.0.0 at docker defaults for three weeks.", name)
			continue
		}
		if want.full() {
			fullyHardened++
		}
		if !want.full() && len(strings.TrimSpace(want.why)) < 20 {
			t.Errorf("service %q claims a reduced posture with no usable reason (why=%q). State WHY, "+
				"with the observed failure if there is one.", name, want.why)
		}

		if diffs := comparePosture(want, svc); len(diffs) > 0 {
			t.Errorf("%s: %s\n  -> %s", name, strings.Join(diffs, " | "), consequence(name, svc))
		}

		// Nothing may be handed back after cap_drop: [ALL]; `cap_add` re-grants exactly what the drop
		// removed, and `privileged: true` re-grants all of it plus device access.
		if add := stringList(svc["cap_add"]); len(add) > 0 {
			t.Errorf("%s: cap_add %v — capabilities are added back after the drop. If a capability is "+
				"genuinely required, say so here rather than re-granting it quietly.", name, add)
		}
		if priv, _ := svc["privileged"].(bool); priv {
			t.Errorf("%s: privileged: true — this disables the entire container boundary.", name)
		}
	}

	if fullyHardened < fullyHardenedFloor {
		t.Errorf("only %d service(s) are declared fully hardened, expected at least %d — the posture table "+
			"has been hollowed out and this guard is no longer asserting anything meaningful",
			fullyHardened, fullyHardenedFloor)
	}
}

// KILLING MUTATION (executed 2026-08-04): change console's port line back to `"8080:80"` while leaving its
// hardening in place — the shape the service had before TG-289 — and this test still passes, because the
// posture is what it checks. Change it back to `"8080:8080"` and instead strip console's hardening and it
// goes RED naming console as an unhardened 0.0.0.0 service. Separately, rewriting every published port to
// `127.0.0.1:` form goes RED on the vacuity floor rather than reporting a vacuous pass.
//
// This is the invariant TG-289 actually turns on, expressed structurally rather than by service name: it
// keeps holding for whatever service is published next, which is the part a hardcoded name would miss.
func TestPubliclyPublishedServicesAreFullyHardened(t *testing.T) {
	services := composeServices(t)

	if len(services) < serviceFloor {
		t.Fatalf("enumerated only %d service(s), expected at least %d — extraction failure, not a pass",
			len(services), serviceFloor)
	}

	var public []string
	for _, name := range sortedKeys(services) {
		for _, p := range stringList(services[name]["ports"]) {
			if publishedOnAllInterfaces(p) {
				public = append(public, name)
				break
			}
		}
	}

	// VACUITY FLOOR. If this scan matches nothing the loop below is a no-op and the test reports a pass
	// while checking zero services. The stack publishes the console on 0.0.0.0; a run that finds no
	// public service has stopped parsing ports, not stopped exposing them.
	if len(public) == 0 {
		t.Fatalf("found no service published on 0.0.0.0 in docker-compose.yml. The console is published " +
			"there, so this scan has stopped matching and the check below would be vacuous.")
	}

	for _, name := range public {
		want, declared := expectedPosture[name]
		if !declared || !want.full() {
			t.Errorf("%s is published on 0.0.0.0 but expectedPosture does not require the full posture "+
				"for it. Anything reachable from the network gets cap_drop ALL + no-new-privileges + "+
				"read_only, or it does not get published.", name)
			continue
		}
		if diffs := comparePosture(posture{capDropAll: true, noNewPriv: true, readOnly: true}, services[name]); len(diffs) > 0 {
			t.Errorf("%s is the network-reachable front door and is NOT hardened: %s", name, strings.Join(diffs, " | "))
		}
	}
}

// comparePosture reports each axis where compose disagrees with the expectation, in both directions.
func comparePosture(want posture, svc map[string]any) []string {
	var diffs []string

	gotCaps := stringList(svc["cap_drop"])
	hasCapDropAll := len(gotCaps) == 1 && strings.EqualFold(gotCaps[0], "ALL")
	if want.capDropAll != hasCapDropAll {
		if want.capDropAll {
			diffs = append(diffs, fmt.Sprintf("expected cap_drop [\"ALL\"], compose has %v", gotCaps))
		} else {
			diffs = append(diffs, "compose drops ALL capabilities but expectedPosture does not require it — "+
				"tighten the table to match (and drop the stale exemption reason)")
		}
	}

	gotOpts := stringList(svc["security_opt"])
	hasNNP := false
	for _, o := range gotOpts {
		if strings.EqualFold(strings.TrimSpace(o), "no-new-privileges:true") {
			hasNNP = true
		}
	}
	if want.noNewPriv != hasNNP {
		if want.noNewPriv {
			diffs = append(diffs, fmt.Sprintf("expected security_opt no-new-privileges:true, compose has %v", gotOpts))
		} else {
			diffs = append(diffs, "compose sets no-new-privileges but expectedPosture does not require it — tighten the table")
		}
	}

	gotRO, _ := svc["read_only"].(bool)
	if want.readOnly != gotRO {
		if want.readOnly {
			diffs = append(diffs, fmt.Sprintf("expected read_only true, compose has %v", gotRO))
		} else {
			diffs = append(diffs, "compose sets read_only but expectedPosture does not require it — tighten the table")
		}
	}
	return diffs
}

// consequence spells out what the gap means in the estate, so a failure reads as a risk rather than as a
// diff. The 0.0.0.0 case is called out separately because that is the one TG-289 was filed about.
func consequence(name string, svc map[string]any) string {
	for _, p := range stringList(svc["ports"]) {
		if publishedOnAllInterfaces(p) {
			return "this service is PUBLISHED ON 0.0.0.0 and is therefore the most exposed container in " +
				"the stack: an RCE in it starts with full capabilities and a writable rootfs"
		}
	}
	return "a process that escapes into this container keeps the capabilities and the writable rootfs " +
		"that the rest of the stack has already given up"
}

// publishedOnAllInterfaces reports whether a compose port entry binds every host interface. Compose short
// syntax is [HOST_IP:][HOST_PORT:]CONTAINER_PORT[/PROTO]: with a host IP present the publish is scoped to
// that IP, and every scoped entry in this file uses 127.0.0.1. Without one, docker binds 0.0.0.0.
func publishedOnAllInterfaces(entry string) bool {
	spec := strings.TrimSpace(entry)
	if spec == "" {
		return false
	}
	if i := strings.Index(spec, "/"); i >= 0 {
		spec = spec[:i]
	}
	// IPv6 host IPs are bracketed, e.g. "[::1]:8080:8080"; treat any explicit IP as scoped.
	if strings.HasPrefix(spec, "[") {
		return false
	}
	// 3 fields => HOST_IP:HOST_PORT:CONTAINER_PORT (scoped). 1 or 2 => no host IP => 0.0.0.0.
	// "0.0.0.0:8080:8080" is spelled out explicitly by some authors, so honour that too.
	parts := strings.Split(spec, ":")
	if len(parts) >= 3 {
		return parts[0] == "0.0.0.0" || parts[0] == ""
	}
	return true
}

// composeServices parses docker-compose.yml and returns its `services` mapping. It walks a generic tree
// rather than a typed struct for the reason given in sealparity_test.go: compose lets services declare the
// same key in different shapes, and a typed field makes the whole document fail to parse over a service
// this guard does not even look at.
func composeServices(t *testing.T) map[string]map[string]any {
	t.Helper()
	raw, err := os.ReadFile("docker-compose.yml")
	if err != nil {
		t.Fatalf("read compose: %v", err)
	}
	var doc map[string]any
	// yaml.v3 rejects duplicate keys, which python's yaml.safe_load accepts silently. A pasted-over
	// service block therefore fails here instead of quietly discarding the earlier definition.
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse docker-compose.yml: %v\n"+
			"A duplicate key reports as 'already defined at line N' — python's yaml.safe_load would have "+
			"accepted that file and kept only the last block.", err)
	}
	services, ok := doc["services"].(map[string]any)
	if !ok || len(services) == 0 {
		t.Fatalf("docker-compose.yml has no readable `services:` mapping — the guard cannot see the "+
			"deployment and must not report a pass (got %T)", doc["services"])
	}
	out := make(map[string]map[string]any, len(services))
	for name, body := range services {
		svc, ok := body.(map[string]any)
		if !ok {
			t.Fatalf("service %q has an unreadable body (%T)", name, body)
		}
		out[name] = svc
	}
	return out
}

// stringList normalises a compose field that may be a YAML list or a single scalar into []string.
func stringList(v any) []string {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		return []string{x}
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			out = append(out, fmt.Sprint(item))
		}
		return out
	default:
		return []string{fmt.Sprint(x)}
	}
}

func sortedKeys(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
