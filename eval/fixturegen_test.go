package eval

// The fixture RE-CAPTURE tool (env-gated; not a behavioral test). It rewrites corpus.json's tool_fixtures
// by replaying the captured worlds (fixtures_test.go) through the REAL hostdiag renderer and stamping the
// hand-captured LibreNMS lines below. Run it after a DELIBERATE tool-dialect change:
//
//	TG_EVAL_REGEN_FIXTURES=1 go test ./eval -run TestRegenerateToolFixtures
//
// then review the corpus.json diff and commit it WITH the dialect change. The byte-equality pin
// (TestFixtureShapeFaithfulHostdiag) is what makes skipping this impossible: a hostdiag wording change
// without a re-capture fails CI loud, so the fixture arm can never quietly drift into a dialect
// production no longer speaks.

import (
	"encoding/json"
	"os"
	"testing"
)

// librenmsFixtures is the hand-captured LibreNMS arm of each fixture-armed incident, written in the EXACT
// single-line Sprintf dialects of modules/ingest/librenms/tools.go (deviceStatusTool / activeAlertsTool /
// eventlogTool). Down-device incidents (eval-01/02) show status=DOWN with the up/down rule firing;
// service/resource incidents (eval-05/09/16/19) show the device UP with the incident's own rule firing —
// the fault lives INSIDE the host, which is what routes the agent to the hostdiag fixtures.
// fragment-pinned by TestCorpusFixtureArm (prefix/rule/severity/since shapes).
func librenmsFixtures() map[string]map[string]FixtureResult {
	return map[string]map[string]FixtureResult{
		"eval-01": {
			"get-device-status": {Success: true, Output: `LibreNMS device dc1bookwyrm01: status=DOWN os=debian type=server hardware="Standard PC (i440FX + PIIX, 1996)" uptime=397h12m45s last_polled=2026-07-30 06:55:02 sysName=dc1bookwyrm01`},
			"get-active-alerts": {Success: true, Output: "active LibreNMS alerts on dc1bookwyrm01: Devices up/down (severity=critical, since=2026-07-30 06:47:03)"},
			"get-device-eventlog": {Success: true, Output: `LibreNMS eventlog for dc1bookwyrm01 (most recent 3):
  [2026-07-30 06:52:11] alert: Issued critical alert for rule 'Devices up/down' to transport mail
  [2026-07-30 06:47:03] down: Device status changed to Down from icmp check
  [2026-07-12 03:41:57] up: Device status changed to Up from icmp check`},
		},
		"eval-02": {
			"get-device-status": {Success: true, Output: `LibreNMS device dc1calibre01: status=DOWN os=debian type=server hardware="Standard PC (i440FX + PIIX, 1996)" uptime=1122h8m33s last_polled=2026-07-30 07:00:14 sysName=dc1calibre01`},
			"get-active-alerts": {Success: true, Output: "active LibreNMS alerts on dc1calibre01: Devices up/down (severity=critical, since=2026-07-30 06:39:26)"},
			"get-device-eventlog": {Success: true, Output: `LibreNMS eventlog for dc1calibre01 (most recent 3):
  [2026-07-30 06:44:02] alert: Issued critical alert for rule 'Devices up/down' to transport mail
  [2026-07-30 06:39:26] down: Device status changed to Down from icmp check
  [2026-06-13 08:02:19] up: Device status changed to Up from icmp check`},
		},
		"eval-05": {
			"get-device-status": {Success: true, Output: `LibreNMS device dc1atlantis01: status=UP os=debian type=server hardware="Standard PC (i440FX + PIIX, 1996)" uptime=1467h3m9s last_polled=2026-07-30 07:10:41 sysName=dc1atlantis01`},
			"get-active-alerts": {Success: true, Output: "active LibreNMS alerts on dc1atlantis01: Service up/down (severity=critical, since=2026-07-30 06:58:37)"},
			"get-device-eventlog": {Success: true, Output: `LibreNMS eventlog for dc1atlantis01 (most recent 3):
  [2026-07-30 07:00:02] alert: Issued critical alert for rule 'Service up/down' to transport mail
  [2026-07-30 06:58:37] service: Service 'tcp-4141' status changed from OK to CRITICAL (connection refused)
  [2026-07-30 02:00:11] system: Unattended-upgrades run completed`},
		},
		"eval-09": {
			"get-device-status": {Success: true, Output: `LibreNMS device dc1calibre01: status=UP os=debian type=server hardware="Standard PC (i440FX + PIIX, 1996)" uptime=443h51m20s last_polled=2026-07-30 07:15:33 sysName=dc1calibre01`},
			"get-active-alerts": {Success: true, Output: "active LibreNMS alerts on dc1calibre01: Linux High Memory Usage, >= 90% in use (severity=warning, since=2026-07-30 06:20:44)"},
			"get-device-eventlog": {Success: true, Output: `LibreNMS eventlog for dc1calibre01 (most recent 3):
  [2026-07-30 06:25:01] alert: Issued warning alert for rule 'Linux High Memory Usage, >= 90% in use' to transport mail
  [2026-07-27 14:02:19] alert: Alert for rule 'Linux High Memory Usage, >= 90% in use' has been cleared
  [2026-07-11 19:44:08] up: Device status changed to Up from icmp check`},
		},
		"eval-16": {
			"get-device-status": {Success: true, Output: `LibreNMS device dc1cl01garbd01: status=UP os=debian type=server hardware="Standard PC (i440FX + PIIX, 1996)" uptime=2258h17m2s last_polled=2026-07-30 07:19:08 sysName=dc1cl01garbd01`},
			"get-active-alerts": {Success: true, Output: "active LibreNMS alerts on dc1cl01garbd01: Linux High Memory Usage, >= 90% in use (severity=warning, since=2026-07-30 06:33:12)"},
			"get-device-eventlog": {Success: true, Output: `LibreNMS eventlog for dc1cl01garbd01 (most recent 3):
  [2026-07-30 06:38:01] alert: Issued warning alert for rule 'Linux High Memory Usage, >= 90% in use' to transport mail
  [2026-07-24 09:11:36] alert: Alert for rule 'Linux High Memory Usage, >= 90% in use' has been cleared
  [2026-04-27 06:05:52] up: Device status changed to Up from icmp check`},
		},
		"eval-19": {
			"get-device-status": {Success: true, Output: `LibreNMS device dc1cl01iotarb01: status=UP os=debian type=server hardware="Standard PC (i440FX + PIIX, 1996)" uptime=2109h40m26s last_polled=2026-07-30 07:22:51 sysName=dc1cl01iotarb01`},
			"get-active-alerts": {Success: true, Output: "active LibreNMS alerts on dc1cl01iotarb01: Linux High Memory Usage, >= 90% in use (severity=warning, since=2026-07-30 06:41:58)"},
			"get-device-eventlog": {Success: true, Output: `LibreNMS eventlog for dc1cl01iotarb01 (most recent 3):
  [2026-07-30 06:46:02] alert: Issued warning alert for rule 'Linux High Memory Usage, >= 90% in use' to transport mail
  [2026-07-22 17:30:44] alert: Alert for rule 'Linux High Memory Usage, >= 90% in use' has been cleared
  [2026-05-04 11:58:31] up: Device status changed to Up from icmp check`},
		},
	}
}

// parentHostFixtures arms the start-guest incidents (eval-01/02) with an observation of their PARENT PVE
// host. The agent's start-guest skill gates on "guest down, its PVE host UP": the incident guest is DOWN, and
// in production the WHOLE estate is monitored, so the parent dc1pve03's UP status is directly observable
// in LibreNMS. The prior capture armed tool outputs for the incident guest only, never its parent — so the
// agent could not confirm the host was up and CORRECTLY escalated instead of proposing start-guest (the
// 2026-07-30 proposal_recall miss on both). These entries are keyed by full FixtureKey for a DIFFERENT host
// than the incident's, written in the EXACT single-line dialects the real librenms deviceStatusTool /
// activeAlertsTool emit (modules/ingest/librenms/tools.go): a healthy device renders status=UP, and a host
// with nothing firing renders the "no active ... alerts firing ... (device status=UP)" line.
func parentHostFixtures() map[string]map[string]FixtureResult {
	pveUp := func(host, uptime, lastPolled string) FixtureResult {
		return FixtureResult{Success: true, Output: `LibreNMS device ` + host + `: status=UP os=debian type=server hardware="PowerEdge R730" uptime=` + uptime + ` last_polled=` + lastPolled + ` sysName=` + host}
	}
	noAlerts := func(host string) FixtureResult {
		return FixtureResult{Success: true, Output: "no active LibreNMS alerts firing on " + host + " (device status=UP)"}
	}
	const pve = "dc1pve03" // both bookwyrm01 (eval-01) and calibre01 (eval-02) run_on dc1pve03 (estate_fixture.json)
	return map[string]map[string]FixtureResult{
		"eval-01": {
			FixtureKey("get-device-status", pve): pveUp(pve, "6042h13m50s", "2026-07-30 06:55:41"),
			FixtureKey("get-active-alerts", pve): noAlerts(pve),
		},
		"eval-02": {
			FixtureKey("get-device-status", pve): pveUp(pve, "6042h49m10s", "2026-07-30 07:01:22"),
			FixtureKey("get-active-alerts", pve): noAlerts(pve),
		},
	}
}

func TestRegenerateToolFixtures(t *testing.T) {
	if os.Getenv("TG_EVAL_REGEN_FIXTURES") == "" {
		t.Skip("set TG_EVAL_REGEN_FIXTURES=1 to rewrite corpus.json's tool_fixtures from the captured worlds")
	}
	corpus := mustCorpus(t)
	worlds := fixtureHostWorlds()
	lnms := librenmsFixtures()
	parents := parentHostFixtures()
	for i := range corpus {
		inc := &corpus[i]
		if inc.Expected != "propose" {
			continue
		}
		w, okk := worlds[inc.ExternalRef]
		if !okk {
			t.Fatalf("%s is expected-propose but has no captured world", inc.ExternalRef)
		}
		if w.host != inc.Host {
			t.Fatalf("%s: captured world host %s != corpus host %s (hostnames stay AS THE CORPUS HAS THEM)", inc.ExternalRef, w.host, inc.Host)
		}
		fx := map[string]FixtureResult{}
		for tool, fr := range lnms[inc.ExternalRef] {
			fx[FixtureKey(tool, inc.Host)] = fr
		}
		for _, check := range w.checks {
			res := renderRealHostdiag(t, check, w.host, w.runner)
			fx[FixtureKey(check, w.host)] = FixtureResult{Success: res.Success, Output: res.Output}
		}
		// Parent-host observations (start-guest incidents only): a DIFFERENT host than the incident's, keyed
		// by full FixtureKey — so the agent can confirm "guest down, PVE host up" and propose start-guest.
		for key, fr := range parents[inc.ExternalRef] {
			fx[key] = fr
		}
		inc.ToolFixtures = fx
	}
	b, err := json.MarshalIndent(corpus, "", "  ")
	if err != nil {
		t.Fatalf("marshal corpus: %v", err)
	}
	if err := os.WriteFile("corpus.json", append(b, '\n'), 0o644); err != nil {
		t.Fatalf("write corpus: %v", err)
	}
	// Round-trip through the validator so a regen can never write a corpus LoadCorpus refuses.
	if _, err := LoadCorpus("corpus.json"); err != nil {
		t.Fatalf("regenerated corpus fails validation: %v", err)
	}
	t.Logf("corpus.json tool_fixtures regenerated for %d incidents", len(worlds))
}
