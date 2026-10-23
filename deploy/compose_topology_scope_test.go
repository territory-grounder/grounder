package deploy

// THE ACTUATION PLANE'S LIBRENMS CREDENTIAL MUST BE TOPOLOGY-SCOPED (TG-337).
//
// The plane split withholds untrusted content from the process holding the estate's mutation keys. TG-331
// then had to grant that process a LibreNMS credential anyway, because the interceptor's host-match and
// blast-radius gates are evaluated against the estate graph and a mutation gate reasoning over an empty
// graph is a gate that cannot refuse. The credential it was granted was a FULL token — the same one that
// reads alert bodies and event-log lines, which is precisely the content the split exists to withhold.
//
// The estate side of the fix is a LibreNMS role holding device.view/viewAny/viewAll and nothing else, with
// its own OpenBao path. This guards the ONE line in the repo that decides whether the running process ever
// sees it. Both halves are needed and only this half can be tested here: a scoped token that the compose
// file never points the actuate plane at is a credential nobody uses.

import (
	"os"
	"strings"
	"testing"
)

// composeSource reads the deployment file the rule lives in. Split out so a rename fails loudly rather than
// turning the guards below into checks against an empty string.
func composeSourceForTopologyScope(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("docker-compose.yml")
	if err != nil {
		t.Fatalf("read docker-compose.yml: %v — this guard cannot assert anything about a file it cannot "+
			"open, and must fail rather than pass vacuously", err)
	}
	if len(raw) == 0 {
		t.Fatal("docker-compose.yml is empty")
	}
	return string(raw)
}

func TestActuationPlaneReadsTopologyScopedLibreNMSCredentials(t *testing.T) {
	src := composeSourceForTopologyScope(t)

	const scopedVar = "TG_LIBRENMS_DEPLOYMENTS_TOPOLOGY"
	if !strings.Contains(src, scopedVar) {
		t.Fatalf("%s is gone from docker-compose.yml. The actuation plane is then configured from the "+
			"triage plane's value, which carries FULL LibreNMS tokens — the process holding the estate's "+
			"mutation keys can read alert bodies and the event log again, and nothing else in the tree "+
			"would notice.", scopedVar)
	}

	// It must be the ACTUATE service that consumes it. Finding the string somewhere in the file proves
	// nothing about which process is configured by it.
	actuate := serviceBlock(t, src, "worker-actuate")
	if !strings.Contains(actuate, scopedVar) {
		t.Errorf("worker-actuate does not consume %s, so the scoped value is declared and unused — the "+
			"actuation plane still resolves the full triage tokens", scopedVar)
	}

	// And it must NOT be the triage plane's. Triage legitimately reads alert text; pointing it at a
	// topology-only token would break the alert pull and look like a LibreNMS outage.
	worker := serviceBlock(t, src, "worker")
	if strings.Contains(worker, scopedVar) {
		t.Errorf("the TRIAGE worker consumes %s. That plane reads alert bodies and the event log by "+
			"design; a topology-scoped token there returns 403 on /api/v0/alerts and the alert pull dies "+
			"with what looks like an upstream failure.", scopedVar)
	}
}

// The fallback must survive. Unset, this plane keeps its graph and loses only the credential scoping;
// EMPTY, it loses the graph — and a blast-radius gate with no graph cannot refuse anything, which is the
// failure TG-331 closed. A stricter-looking edit here trades this ticket's risk for a worse one.
func TestTheTopologyValueFallsBackRatherThanBlindingTheGate(t *testing.T) {
	actuate := serviceBlock(t, composeSourceForTopologyScope(t), "worker-actuate")
	const want = "${TG_LIBRENMS_DEPLOYMENTS_TOPOLOGY:-${TG_LIBRENMS_DEPLOYMENTS:-}}"
	if !strings.Contains(actuate, want) {
		t.Errorf("the actuate plane's topology value is not %q.\n"+
			"If it resolves to empty when the scoped variable is unset, the estate graph is absent and the "+
			"interceptor's host-match and blast-radius checks evaluate against nothing — they cannot "+
			"refuse. An unscoped token is the LESSER failure and the fallback is what keeps it that way.",
			want)
	}
}

// serviceBlock returns one service's stanza. Compose services sit at two-space indent, so a block runs to
// the next two-space key.
func serviceBlock(t *testing.T, src, name string) string {
	t.Helper()
	start := strings.Index(src, "\n  "+name+":\n")
	if start < 0 {
		t.Fatalf("service %q is not in docker-compose.yml — this guard is asserting over nothing", name)
	}
	rest := src[start+1:]
	for i := 1; i < len(rest); i++ {
		if rest[i-1] == '\n' && strings.HasPrefix(rest[i:], "  ") &&
			!strings.HasPrefix(rest[i:], "   ") && !strings.HasPrefix(rest[i:], "  #") &&
			strings.Contains(strings.SplitN(rest[i:], "\n", 2)[0], ":") {
			return rest[:i]
		}
	}
	return rest
}

// THE ACTUATION PLANE NEEDS THE STRUCTURED-INVENTORY SOURCES, AND ONLY THOSE (TG-346).
//
// Measured 2026-08-06, the first reading after tg_estate_edges shipped:
//
//	worker (triage)   392 edges / 369 nodes
//	worker-actuate     17 edges /  20 nodes     tg_estate_sources_failed = 0 on BOTH
//
// Nothing was erroring. The actuation plane seeded from LibreNMS alone, so the interceptor's host-match
// and blast-radius gates computed over 4% of the estate the triage plane models — on the plane that holds
// TG_PROXMOX_BASE_URL and actually starts and stops guests.
//
// This guard pins BOTH directions, because they are different mistakes: the structured sources must be
// present, and the untrusted-content probes must stay absent. "More topology on the actuation plane" is
// not the rule; the rule is credential_plane.go's split by PURPOSE.
func TestActuationPlaneCarriesStructuredInventoryButNotTheProbes(t *testing.T) {
	// COMMENT LINES STRIPPED. This guard tripped on its OWN explanation: the block above documents which
	// probes are deliberately NOT added, naming them, and a substring scan counted that prose as
	// configuration. Exactly the failure TG-241's category floor was built to prevent, walked into an hour
	// after building it. Prose is not code.
	actuate := stripYAMLComments(serviceBlock(t, composeSourceForTopologyScope(t), "worker-actuate"))

	// PRESENT: the structured inventory this plane may legitimately hold. NetBox is deliberately NOT here
	// — see the forbidden list below and the compose comment: its secret reference is classified
	// read-triage, and handing it to this process fails the boot.
	for _, want := range []string{"TG_LIBRENMS_DEPLOYMENTS"} {
		if !strings.Contains(actuate, want) {
			t.Errorf("worker-actuate does not consume %s. The blast-radius gate then reasons over a graph "+
				"missing that source — it finds no impact, which is indistinguishable from there being "+
				"none, and it under-refuses quietly rather than failing.", want)
		}
	}

	// ABSENT: the service-observing probes are on triagePlaneEnvKeys deliberately — an SSH session per
	// host is not structured inventory, and the plane guard refuses them here.
	// ABSENT, and TG_NETBOX_TOKEN_REF is on this list because I put it in the actuate block and it TOOK
	// THE WORKER DOWN: the plane split is enforced on the secret REFERENCE, not the env-key name, and
	// secret/data/tg/netbox is classified read-triage. TG_NETBOX_URL rides with it — a URL with no
	// resolvable token is a source that cannot seed.
	for _, forbidden := range []string{"TG_DISCOVERY_SYSTEMD_HOSTS", "TG_DISCOVERY_DOCKER_HOSTS", "TG_NETBOX_TOKEN_REF"} {
		if strings.Contains(actuate, forbidden) {
			t.Errorf("worker-actuate consumes %s, which is plane-scoped to triage on purpose. Adding "+
				"topology sources indiscriminately re-merges the two planes the deployment split — the "+
				"rule is the split by PURPOSE, not 'more graph is better'.", forbidden)
		}
	}
}

// stripYAMLComments drops whole-line comments so a guard scanning for configuration keys cannot match the
// prose that explains them.
func stripYAMLComments(block string) string {
	var b strings.Builder
	for _, line := range strings.Split(block, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}
