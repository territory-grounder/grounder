package deploy

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TG-366. The served console shipped a fixture connector fleet: 31 invented modules with invented
// endpoints, invented credential references, invented versions, health dots and poll intervals. Only 3
// of the 31 names existed. The live registry, read from the running control plane, is 45 declared
// capabilities and 43 registry entries.
//
// The live layer already refused to render it on the live path — "a plausible-looking connector list is
// exactly the thing an operator must not be shown here" — but fell back to the fixture on two paths: a
// non-live shell, and a live shell whose /v1/capabilities read had not landed. A fixture that is never
// allowed to render is not a preview, it is a hazard behind a banner.
//
// This guard is about ANY surface whose job is "what can this system reach and what is switched on".
// #secrets was retired the same way for the same reason (SEC_REFS = []).

// inventedConnectorNames are ids that appeared ONLY in the fixture — never in TG's real registry, which
// is verified live at /v1/capabilities. Any of them reappearing in the shipped console means a fabricated
// fleet has come back.
//
// Chosen from the fixture's own list, excluding the three that genuinely exist (librenms, netbox,
// youtrack) so this cannot fire on a legitimate mention of a real module.
var inventedConnectorNames = []string{
	"uptime-kuma", "graylog", "snmp-trap", "truenas", "gitlab-issues",
	"vllm-local", "prom-exporter", "grafana-annot", "healthz-probe", "mcp-exec",
}

func servedConsoleSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("console/v2/index.html")
	if err != nil {
		t.Fatalf("read the served console: %v", err)
	}
	if len(b) < 100_000 {
		t.Fatalf("VACUITY FLOOR: the served console is only %d bytes. Either the build is broken or this "+
			"guard is reading the wrong artifact — every assertion below would pass on a stub.", len(b))
	}
	return string(b)
}

// TestTheServedConsoleShipsNoInventedConnectorFleet is the finding itself.
func TestTheServedConsoleShipsNoInventedConnectorFleet(t *testing.T) {
	src := servedConsoleSource(t)

	var found []string
	for _, name := range inventedConnectorNames {
		// Quoted, so this matches a data entry rather than the word appearing in prose.
		if strings.Contains(src, `"`+name+`"`) {
			found = append(found, name)
		}
	}
	if len(found) > 0 {
		t.Errorf("the served console carries connector name(s) that exist ONLY in the retired fixture: %v\n"+
			"TG's real registry is read live from /v1/capabilities (45 declared, 22 enabled on 2026-08-06). "+
			"A surface whose job is \"what can this system reach\" must not answer from an invented estate — "+
			"see modules/modules/fixtures.txt and the identical retirement of SEC_REFS for #secrets.", found)
	}
}

// TestTheConnectorFixtureIsEmpty pins the mechanism rather than the symptom. Deleting ten names from the
// array would pass the test above while leaving twenty-one fabricated modules shipping.
func TestTheConnectorFixtureIsEmpty(t *testing.T) {
	src := servedConsoleSource(t)

	i := strings.Index(src, "const MODS_FLEET")
	if i < 0 {
		// Fine — the symbol may be gone entirely. But it must not have been replaced by another fleet
		// literal under a new name, so fall through to the endpoint check below.
		return
	}
	j := strings.Index(src[i:], "];")
	if j < 0 {
		t.Fatalf("MODS_FLEET is declared but never closed — the console build is malformed")
	}
	body := src[i : i+j]
	if strings.Contains(body, "{id:") {
		n := strings.Count(body, "{id:")
		t.Errorf("MODS_FLEET ships %d fixture connector entries. It must stay empty: the live layer renders "+
			"/v1/capabilities, and this array exists only as the fallback that put an invented estate on an "+
			"operator's screen.", n)
	}
}

// TestNoFabricatedEndpointOrCredentialRefShips is the shape rule. A future fixture under a different
// symbol name would defeat both tests above; an invented endpoint or credential reference is what makes
// any such fixture dangerous, whatever it is called.
func TestNoFabricatedEndpointOrCredentialRefShips(t *testing.T) {
	src := servedConsoleSource(t)

	// A .lan endpoint literal: this estate uses example.net, so a .lan URL in shipped console
	// data is fabricated by construction.
	lanEndpoint := regexp.MustCompile(`"https?://[a-z0-9.\-]+\.lan[:/][^"]*"`)
	if m := lanEndpoint.FindAllString(src, -1); len(m) > 0 {
		t.Errorf("the served console ships %d fabricated .lan endpoint literal(s): %v\n"+
			"Endpoints belong to live config, never to shipped console data.", len(m), m)
	}

	// A credential REFERENCE in shipped data. The real console reads references from /v1/secrets; one
	// baked into the artifact is invented, and it teaches an operator a key name that does not exist.
	credRef := regexp.MustCompile(`auth\s*:\s*\[\s*"(env|bao|vault):`)
	if m := credRef.FindAllString(src, -1); len(m) > 0 {
		t.Errorf("the served console ships %d fabricated credential reference(s) in module data: %v\n"+
			"Credential references are read live; a baked-in one names a key that does not exist.", len(m), m)
	}
}

// TestTheAlertsFixtureIsEmpty pins the ALERTS mechanism (TG-401), the exact counterpart of the connector
// fixture above. The served console shipped 8 fabricated alerts wearing estate-shaped hostnames AND invented
// CORRELATION provenance ("correlated · 3 alerts · same host + imagefs edge") — a claim about TG's reasoning
// on the surface an operator uses to judge it, and specifically the grouping the pve03 cascade proved TG
// cannot do (157→157, TG-376). The live layer only OVERWRITES ALERTS on a SUCCESSFUL /v1/alerts read, so a
// failed read left these showing as if live. It must stay empty: on success the live alerts render, on
// failure the surface shows honest-empty, never fabricated grouping.
func TestTheAlertsFixtureIsEmpty(t *testing.T) {
	src := servedConsoleSource(t)

	i := strings.Index(src, "const ALERTS")
	if i < 0 {
		return // gone entirely is fine; the shape rule below still guards against a rename.
	}
	j := strings.Index(src[i:], "];")
	if j < 0 {
		t.Fatalf("ALERTS is declared but never closed — the console build is malformed")
	}
	body := src[i : i+j]
	if strings.Contains(body, "{id:") {
		n := strings.Count(body, "{id:")
		t.Errorf("ALERTS ships %d fixture alert entries. It must stay empty: the live layer renders /v1/alerts "+
			"(index.html overwrites ALERTS on a successful read), and this array exists only as the fallback that "+
			"put fabricated alerts — with invented correlation provenance — on an operator's screen (TG-401).", n)
	}
}

// TestNoFabricatedAlertIdShips is the shape rule for the alerts class (TG-401), the counterpart of the
// endpoint/credential rule above: a future fixture under a different symbol name would defeat the emptiness
// test. The `al-<n>` alert-id namespace is invented BY CONSTRUCTION — a real admitted alert is a
// source-scoped external_ref (e.g. `librenms-dc1-184121`), never `al-9920` — so any such literal in the
// shipped artifact is fabricated alert data. The retired fixture paired these ids with real estate-shaped
// hostnames, which is exactly why the TG-366 shape rule (`.lan` endpoints) could not catch this class.
func TestNoFabricatedAlertIdShips(t *testing.T) {
	src := servedConsoleSource(t)

	fabricatedAlertID := regexp.MustCompile(`"al-\d{3,}"`)
	if m := fabricatedAlertID.FindAllString(src, -1); len(m) > 0 {
		t.Errorf("the served console ships %d fabricated alert-id literal(s) (al-<n>): %v\n"+
			"Alert ids are source-scoped external_refs read live from /v1/alerts; an `al-<n>` literal is invented "+
			"by construction, and in the retired fixture it carried invented correlation provenance TG demonstrably "+
			"cannot produce (TG-376, TG-401).", len(m), m)
	}
}
