package eval

// Oracles for the B4a fixture arm (fixtures.go). The load-bearing property under test is SHAPE
// FAITHFULNESS: a fixture-armed session must be indistinguishable IN DIALECT from a live one, or the eval
// measures an agent reading a language production never speaks. The strongest pin here is
// TestFixtureShapeFaithfulHostdiag, which replays each fixture's captured raw step outputs through the
// REAL hostdiag renderer (fake SSH runner, real formatting path) and requires the corpus text to match
// byte-for-byte — so any hostdiag rendering change forces a fixture re-capture (see fixturegen_test.go)
// instead of silently forking the dialect.

import (
	"context"
	"errors"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/modules/ingest/librenms"
	"github.com/territory-grounder/grounder/modules/observability/hostdiag"
	"github.com/territory-grounder/grounder/modules/observability/syslogng"
)

// ---- fakes for driving the REAL hostdiag/syslogng tool paths in CI (no live SSH) ----

// fakeRunner replays a captured host's raw step outputs, keyed by the joined fixed argv. err simulates an
// unreachable host (every read fails); an argv with no entry exits 127 ("may not apply on this host" —
// exactly how a Docker-less host answers the container step).
type fakeRunner struct {
	out map[string]syslogng.RunResult
	err error
}

func (f fakeRunner) Run(_ context.Context, _ syslogng.Server, argv []string) (syslogng.RunResult, error) {
	if f.err != nil {
		return syslogng.RunResult{}, f.err
	}
	if rr, ok := f.out[strings.Join(argv, " ")]; ok {
		return rr, nil
	}
	return syslogng.RunResult{ExitCode: 127}, nil
}

// staticResolver satisfies hostdiag.IdentityResolver with a fixed valid bundle (the captured estate's
// out-of-box root identity).
type staticResolver struct{ b credential.Bundle }

func (r staticResolver) Resolve(_ context.Context, _ credential.Target) (credential.Bundle, error) {
	return r.b, nil
}

func fixtureBundle(t *testing.T) credential.Bundle {
	t.Helper()
	b, err := credential.NewBundle(credential.BundleSpec{
		User: "root", Port: 22, Scheme: credential.SchemeSSH, SSHKeyRef: "env:TG_EVAL_FIXTURE_FAKE_KEY",
	})
	if err != nil {
		t.Fatalf("fixture bundle: %v", err)
	}
	return b
}

// renderRealHostdiag invokes the REAL hostdiag tool (real check catalogue, real rendering/synthesis path)
// over a fake runner — the exact pipeline production text flows through, minus the SSH transport.
func renderRealHostdiag(t *testing.T, tool, host string, r syslogng.Runner) agent.ToolResult {
	t.Helper()
	accs := []hostdiag.Access{{Site: "nl", HostGlob: "*", SSHUser: "root", KeyRef: "env:TG_EVAL_FIXTURE_FAKE_KEY"}}
	for _, tl := range hostdiag.NewTools(accs, r, staticResolver{fixtureBundle(t)}) {
		if tl.Name() != tool {
			continue
		}
		res, err := tl.Invoke(context.Background(), map[string]string{"host": host})
		if err != nil {
			t.Fatalf("real %s on %s: %v", tool, host, err)
		}
		return res
	}
	t.Fatalf("real hostdiag toolset has no %s", tool)
	return agent.ToolResult{}
}

// ---- the captured worlds behind corpus.json's hostdiag fixtures ----

// The fixed argvs of the hostdiag checks (joined form), mirroring hostdiag.go's step catalogue. These are
// fake-runner KEYS only — if hostdiag changes an argv, the fake returns 127 for it, the real render
// changes, and the byte-equality test below fails loud (the intended re-capture signal).
const (
	argvSvcFailed   = "systemctl --failed --no-legend --no-pager"
	argvSvcInactive = "systemctl list-units --type=service --state=inactive --no-legend --no-pager"
	argvSvcEnabled  = "systemctl list-unit-files --type=service --state=enabled --no-legend --no-pager"
	argvFree        = "free -m"
	argvPSMem       = "ps -eo pid,comm,%mem,rss --sort=-%mem --no-headers"
	argvUptime      = "uptime"
	argvPSCPU       = "ps -eo pid,comm,%cpu --sort=-%cpu --no-headers"
	argvDF          = "df -h"
	argvFindmnt     = "findmnt --noheadings --output SOURCE,FSTYPE,SIZE,USED,USE% /"
	argvDU          = "du -xh --max-depth=2 /"
	argvJournal     = "journalctl --disk-usage"
)

// benignInactive is the normally-inactive noise every healthy Debian host shows (oneshots/timers) — the
// haystack the derived down-services summary exists to see through.
const benignInactive = `  apt-daily.service                      loaded inactive dead Daily apt download activities
  apt-daily-upgrade.service              loaded inactive dead Daily apt upgrade and clean activities
  fstrim.service                         loaded inactive dead Discard unused blocks on filesystems from /etc/fstab
  logrotate.service                      loaded inactive dead Rotate log files
  systemd-tmpfiles-clean.service         loaded inactive dead Cleanup of Temporary Directories`

func ok(stdout string) syslogng.RunResult { return syslogng.RunResult{Stdout: []byte(stdout)} }

// upWorld is one reachable captured host: the target unit's state plus the host's resource reads.
type upWorld struct {
	unit, unitDesc string
	// cleanlyStopped: the unit is enabled+inactive (start-service form). Otherwise it is FAILED
	// (oom-kill / crash — restart-service form). Both land in the derived down-services list.
	cleanlyStopped             bool
	free, psMem, uptime, psCPU string
	dfRoot, findmnt, duApp     string
}

func (w upWorld) runner() fakeRunner {
	inactive := benignInactive
	failed := ""
	if w.cleanlyStopped {
		inactive = "  " + w.unit + strings.Repeat(" ", 39-len(w.unit)) + "loaded inactive dead " + w.unitDesc + "\n" + benignInactive
	} else {
		failed = "● " + w.unit + " loaded failed failed " + w.unitDesc
	}
	enabled := w.unit + strings.Repeat(" ", 37-len(w.unit)) + "enabled enabled\n" +
		`cron.service                         enabled enabled
snmpd.service                        enabled enabled
ssh.service                          enabled enabled
systemd-timesyncd.service            enabled enabled`
	return fakeRunner{out: map[string]syslogng.RunResult{
		argvSvcFailed:   ok(failed),
		argvSvcInactive: ok(inactive),
		argvSvcEnabled:  ok(enabled),
		argvFree:        ok(w.free),
		argvPSMem:       ok(w.psMem),
		argvUptime:      ok(w.uptime),
		argvPSCPU:       ok(w.psCPU),
		argvDF:          ok(w.dfRoot),
		argvFindmnt:     ok(w.findmnt),
		argvDU:          ok(w.duApp),
		argvJournal:     ok("Archived and active journals take up 184.0M in the file system."),
	}}
}

// hostWorld ties one corpus incident to the captured host state its hostdiag fixtures replay.
type hostWorld struct {
	host   string
	runner fakeRunner
	checks []string // the check-host-* tools captured for this incident
}

var allChecks = []string{"check-host-disk", "check-host-memory", "check-host-services", "check-host-load"}

// fixtureHostWorlds maps incident ref → captured world. Shared by the byte-equality shape test AND the
// env-gated regenerator (fixturegen_test.go), so the corpus text and the oracle can never disagree about
// what was captured — only about how the real renderer renders it, which is exactly the drift to catch.
func fixtureHostWorlds() map[string]hostWorld {
	unreachable := fakeRunner{err: errors.New("dial tcp: i/o timeout")}
	df := func(rootLine string) string {
		return `Filesystem      Size  Used Avail Use% Mounted on
udev            958M     0  958M   0% /dev
tmpfs           197M  600K  196M   1% /run
` + rootLine + `
tmpfs           982M     0  982M   0% /dev/shm
tmpfs           5.0M     0  5.0M   0% /run/lock`
	}
	du := func(appDir string) string {
		return "16K\t/lost+found\n1.9G\t/var\n1.1G\t/var/lib\n498M\t/var/log\n201M\t/var/cache\n1.2G\t/usr\n612M\t/usr/lib\n198M\t/usr/share\n" +
			appDir + "\n23M\t/etc\n5.1G\t/"
	}
	atlantis := upWorld{
		unit: "atlantis.service", unitDesc: "Atlantis workspace server", cleanlyStopped: true,
		free: `               total        used        free      shared  buff/cache   available
Mem:            1963         512         981           8         470        1311
Swap:            974           0         974`,
		psMem: `    198 systemd-journal   2.1   42180
    176 snmpd             0.9   18104
    154 rsyslogd          0.6   12820
      1 systemd           0.5   11876`,
		uptime: " 07:18:02 up 61 days,  4:02,  0 users,  load average: 0.08, 0.06, 0.01",
		psCPU: `    176 snmpd             0.3
    198 systemd-journal   0.1
      1 systemd           0.0`,
		dfRoot:  df("/dev/sda1        31G   11G   19G  37% /"),
		findmnt: "/dev/sda1 ext4   31G 10.6G   37%",
		duApp:   du("354M\t/opt\n298M\t/opt/atlantis"),
	}
	calibre := upWorld{
		unit: "calibre.service", unitDesc: "Calibre content server",
		free: `               total        used        free      shared  buff/cache   available
Mem:            3921        3572         109          11         240         181
Swap:            974         951          23`,
		psMem: `    684 python3          21.8  874216
    512 mariadbd          9.4  377040
    198 systemd-journal   2.2   88180
    176 snmpd             0.9   36104
      1 systemd           0.3   12876`,
		uptime: " 07:21:44 up 18 days, 11:37,  0 users,  load average: 0.62, 0.85, 0.79",
		psCPU: `    684 python3          14.2
    512 mariadbd          3.8
    198 systemd-journal   0.6
      1 systemd           0.0`,
		dfRoot:  df("/dev/sda1        62G   24G   36G  41% /"),
		findmnt: "/dev/sda1 ext4   62G 23.4G   41%",
		duApp:   du("9.8G\t/srv\n9.2G\t/srv/calibre-library"),
	}
	garbd := func(uptimeLine, freeUsed, swapUsed, garbdPct, garbdRSS string) upWorld {
		return upWorld{
			unit: "garbd.service", unitDesc: "Galera Arbitrator Daemon",
			free: `               total        used        free      shared  buff/cache   available
Mem:             963         ` + freeUsed + `          28           2          53          44
Swap:            487         ` + swapUsed + `          46`,
			// The failed garbd daemon IS the dominant memory consumer here — a leak drove the host past the
			// >=90% alert, so restart-service garbd is the memory-reclaiming remedy. Mirrors eval-09, whose
			// failed calibre.service maps to the python3 process holding 874MB (the top consumer). The prior
			// capture left the memory in journald/snmpd/rsyslogd, making restart-garbd fix nothing and a
			// stand-down correct — the internal inconsistency the 2026-07-30 recall miss was measuring.
			psMem: `    402 garbd            ` + garbdPct + `  ` + garbdRSS + `
    221 systemd-journal   3.4   33648
    183 snmpd             2.8   27692
    154 rsyslogd          1.9   18820
      1 systemd           1.2   11876`,
			uptime: uptimeLine,
			psCPU: `    221 systemd-journal   0.8
    183 snmpd             0.5
      1 systemd           0.1`,
			dfRoot:  df("/dev/sda1       7.8G  2.9G  4.5G  40% /"),
			findmnt: "/dev/sda1 ext4  7.8G  2.9G   40%",
			duApp:   du("88M\t/opt"),
		}
	}
	return map[string]hostWorld{
		"eval-01": {host: "dc1bookwyrm01", runner: unreachable, checks: allChecks},
		"eval-02": {host: "dc1calibre01", runner: unreachable, checks: allChecks},
		"eval-05": {host: "dc1atlantis01", runner: atlantis.runner(), checks: allChecks},
		"eval-09": {host: "dc1calibre01", runner: calibre.runner(), checks: allChecks},
		"eval-16": {host: "dc1cl01garbd01", runner: garbd(" 07:24:19 up 94 days,  1:12,  0 users,  load average: 1.24, 1.41, 1.18", "881", "441", "72.2", "712400").runner(), checks: allChecks},
		"eval-19": {host: "dc1cl01iotarb01", runner: garbd(" 07:26:53 up 87 days, 22:05,  0 users,  load average: 1.02, 1.19, 0.96", "894", "463", "73.6", "725600").runner(), checks: allChecks},
	}
}

// ---- the oracles ----

// The fixture arm's served tool names must be EXACTLY the network-backed names the real constructors
// register — a production tool rename that drifted past this list would fall out of the fixture set and go
// LIVE (dial out) on a fixture-armed incident.
func TestFixtureServedToolsMatchProduction(t *testing.T) {
	var real []string
	for _, tl := range librenms.NewTools([]librenms.Deployment{{Site: "nl", BaseURL: "https://unused.invalid", TokenRef: "env:X"}}, nil) {
		real = append(real, tl.Name())
	}
	for _, tl := range hostdiag.NewTools([]hostdiag.Access{{Site: "nl", HostGlob: "*", SSHUser: "root", KeyRef: "env:X"}}, fakeRunner{}, staticResolver{fixtureBundle(t)}) {
		real = append(real, tl.Name())
	}
	for _, tl := range syslogng.NewTools([]syslogng.Server{{Site: "NL", SSHHost: "dc1syslogng01", SSHUser: "root", KeyRef: "env:X"}}, fakeRunner{}) {
		real = append(real, tl.Name())
	}
	want := append([]string{}, fixtureServedTools...)
	sort.Strings(real)
	sort.Strings(want)
	if strings.Join(real, ",") != strings.Join(want, ",") {
		t.Fatalf("fixtureServedTools drifted from the real constructors:\n real: %v\n arm:  %v", real, want)
	}
}

func TestFixtureToolSetServesCapturedOutput(t *testing.T) {
	captured := "LibreNMS device dc1atlantis01: status=UP os=debian ..."
	inc := Incident{ExternalRef: "x", Expected: "propose", Host: "dc1atlantis01",
		AlertRule: "Service up/down", Severity: "critical", Summary: "s",
		ToolFixtures: map[string]FixtureResult{
			FixtureKey("get-device-status", "dc1atlantis01"): {Success: true, Output: captured},
		}}
	ts := NewFixtureToolSet(inc, estate.NewGraph())
	tl, okk := ts.Get("get-device-status")
	if !okk {
		t.Fatal("get-device-status must resolve on a fixture-armed toolset")
	}
	// The agent may call with any arg key and any host FORM — the librenms normalization rule applies.
	res, err := tl.Invoke(context.Background(), map[string]string{"target": "NLLEI01ATLANTIS01.example.net"})
	if err != nil || !res.Success || res.Output != captured {
		t.Fatalf("captured hit: err=%v success=%v out=%q", err, res.Success, res.Output)
	}
	if res.ID != "lnms-dev-dc1atlantis01" {
		t.Fatalf("fixture evidence id must speak the real tool's id dialect, got %q", res.ID)
	}
}

// A (tool, host) the capture does not cover answers in the FAMILY's honest production "no data" dialect —
// never an invented "fixture not found" that would tell the agent it is inside a replay.
func TestFixtureToolSetMissShapes(t *testing.T) {
	inc := Incident{ExternalRef: "x", Expected: "propose", Host: "dc1atlantis01",
		ToolFixtures: map[string]FixtureResult{FixtureKey("get-device-status", "dc1atlantis01"): {Success: true, Output: "y"}}}
	ts := NewFixtureToolSet(inc, estate.NewGraph())
	cases := []struct {
		tool, host, want string
	}{
		{"get-device-status", "dc1ghost99", "device dc1ghost99: not present in deployment nl"},
		{"get-device-eventlog", "dc1ghost99", "device dc1ghost99: not present in deployment nl"},
		{"get-active-alerts", "dc1ghost99", "device dc1ghost99: not present in deployment nl"},
		{"get-device-status", "", "no host provided (pass args.host)"},
		{"check-host-services", "dc1ghost99", "no resolvable SSH credential for dc1ghost99 — it is not covered by any credential rule/source (or the match is ambiguous), so I cannot investigate it directly"},
		{"check-host-disk", "", "refused: no host given"},
		{"get-host-logs", "dc1ghost99", "no syslog-ng log for dc1ghost99 via dc1syslogng01 (date today (today.log)): the device may not log there, or that day has no file"},
		{"search-host-logs", "dc1ghost99", "no syslog-ng log to search for dc1ghost99 via dc1syslogng01 (date today (today.log)): the device may not log there, or that day has no file"},
	}
	for _, c := range cases {
		tl, okk := ts.Get(c.tool)
		if !okk {
			t.Fatalf("%s must resolve", c.tool)
		}
		res, err := tl.Invoke(context.Background(), map[string]string{"host": c.host})
		if err != nil {
			t.Fatalf("%s(%q): %v", c.tool, c.host, err)
		}
		if res.Success {
			t.Fatalf("%s(%q): a miss must be Success=false", c.tool, c.host)
		}
		if res.Output != c.want {
			t.Fatalf("%s(%q) miss dialect:\n got  %q\n want %q", c.tool, c.host, res.Output, c.want)
		}
	}
}

// tripwireTransport fails the test on ANY http round-trip — the fixture arm must never dial out.
type tripwireTransport struct{ t *testing.T }

func (tw tripwireTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	tw.t.Fatalf("fixture arm dialed out over HTTP: %s %s", r.Method, r.URL)
	return nil, errors.New("dial-out")
}

// The no-live-network guarantee, enforced twice over: (a) STRUCTURALLY — every name on a fixture-armed
// toolset resolves either to a pure in-memory tool (incident context, estate graph) or to a fixtureTool
// (pure map lookup; no client, no runner, no resolver anywhere in the type), so there is no code path
// that CAN dial; and (b) as a TRIPWIRE — every tool is invoked (hit, miss, empty) with the process-default
// HTTP transport replaced by one that fails the test, so a regression that sneaks a default-client call
// into the arm trips immediately.
func TestFixtureToolSetNeverDialsOut(t *testing.T) {
	c := mustCorpus(t)
	var inc Incident
	for _, x := range c {
		if x.FixtureArmed() {
			inc = x
			break
		}
	}
	if inc.ExternalRef == "" {
		t.Fatal("corpus has no fixture-armed incident to exercise")
	}
	ts := NewFixtureToolSet(inc, estate.NewGraph())
	wantNames := append([]string{"get-device-context", "get-estate-context"}, fixtureServedTools...)
	sort.Strings(wantNames)
	if got := strings.Join(ts.Names(), ","); got != strings.Join(wantNames, ",") {
		t.Fatalf("fixture toolset surface: got %s want %s", got, strings.Join(wantNames, ","))
	}
	for _, name := range ts.Names() {
		tl, _ := ts.Get(name)
		switch name {
		case "get-device-context", "get-estate-context": // pure in-memory reads, real in both arms
		default:
			if _, isFixture := tl.(fixtureTool); !isFixture {
				t.Fatalf("%s on a fixture-armed toolset is %T, not a fixtureTool — it could reach the network", name, tl)
			}
		}
	}
	old := http.DefaultTransport
	http.DefaultTransport = tripwireTransport{t}
	defer func() { http.DefaultTransport = old }()
	for _, name := range ts.Names() {
		tl, _ := ts.Get(name)
		for _, host := range []string{inc.Host, "dc1ghost99", ""} {
			if _, err := tl.Invoke(context.Background(), map[string]string{"host": host}); err != nil {
				t.Fatalf("%s(%q): %v", name, host, err)
			}
		}
	}
}

// The corpus fixture plane fails closed like every other gate input: each failure mode below would
// otherwise be SILENT (a stand-down quietly replayed instead of live-grounded; a typo'd tool or an
// un-normalized key never matching, degrading the arm to all-miss shapes with no red anywhere).
func TestLoadCorpusRejectsBadFixtures(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"fixtures on a non-propose incident": `[{"external_ref":"x","expected":"stand-down","tool_fixtures":{"get-device-status|h1":{"success":true,"output":"y"}}}]`,
		"unknown tool name":                  `[{"external_ref":"x","expected":"propose","tool_fixtures":{"get-device-staus|h1":{"success":true,"output":"y"}}}]`,
		"key without a host":                 `[{"external_ref":"x","expected":"propose","tool_fixtures":{"get-device-status":{"success":true,"output":"y"}}}]`,
		"un-normalized host key":             `[{"external_ref":"x","expected":"propose","tool_fixtures":{"get-device-status|H1.example.net":{"success":true,"output":"y"}}}]`,
		"empty output":                       `[{"external_ref":"x","expected":"propose","tool_fixtures":{"get-device-status|h1":{"success":true,"output":"  "}}}]`,
	}
	for name, body := range cases {
		p := dir + "/bad.json"
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadCorpus(p); err == nil {
			t.Fatalf("%s must fail closed", name)
		}
	}
	good := `[{"external_ref":"x","expected":"propose","tool_fixtures":{"get-device-status|h1":{"success":true,"output":"y"}}}]`
	p := dir + "/good.json"
	if err := os.WriteFile(p, []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCorpus(p); err != nil {
		t.Fatalf("a well-formed fixture must load: %v", err)
	}
}

// Freshness only checks LIVE-armed expected-propose incidents; a fixture-armed one is stale-proof by
// construction and a non-propose label has nothing to go stale against.
func TestNeedsFreshnessCheck(t *testing.T) {
	fx := map[string]FixtureResult{FixtureKey("get-device-status", "h"): {Success: true, Output: "x"}}
	if !NeedsFreshnessCheck(Incident{Expected: "propose"}) {
		t.Fatal("a live-armed propose incident must be freshness-checked")
	}
	if NeedsFreshnessCheck(Incident{Expected: "propose", ToolFixtures: fx}) {
		t.Fatal("a fixture-armed incident must SKIP freshness — the captured world cannot drift")
	}
	if NeedsFreshnessCheck(Incident{Expected: "stand-down"}) || NeedsFreshnessCheck(Incident{Expected: "escalate"}) {
		t.Fatal("non-propose incidents were never freshness candidates")
	}
}

// Fixture-armed sessions COUNT in proposal_recall (they are the measurable propose supply — the arm's
// purpose) and their count is published on the scorecard alongside StaleExcluded, so recall always
// discloses how much of its supply was deterministic.
func TestAggregateCountsFixtureArmed(t *testing.T) {
	sc := Aggregate([]Session{
		{Ref: "f1", Expected: "propose", Proposed: true, FixtureArmed: true},
		{Ref: "f2", Expected: "propose", Proposed: false, FixtureArmed: true}, // a real miss — fixtures don't excuse it
		{Ref: "l1", Expected: "propose", Proposed: true},
		{Ref: "s1", Expected: "stand-down", Proposed: false},
	}, nil)
	if sc.ExpectedProposeN != 3 {
		t.Fatalf("fixture-armed sessions must stay IN the recall denominator: n=%d (want 3)", sc.ExpectedProposeN)
	}
	if sc.ProposalRecall != 0.67 {
		t.Fatalf("proposal_recall=%v (want 0.67 = 2/3 — the fixture-armed miss counts against the agent)", sc.ProposalRecall)
	}
	if sc.FixtureArmed != 2 {
		t.Fatalf("FixtureArmed=%d (want 2) — the deterministic-arm share must be disclosed", sc.FixtureArmed)
	}
}

// Pins the corpus's fixture-arm composition: EVERY expected-propose incident is fixture-armed (the
// deterministic propose supply — the 2026-07-30 trend record is what happens when it is not), each carries
// the librenms triple + check-host-services for ITS OWN host, the service-fault incidents name their target
// unit in the derived down-services form, and no stand-down/escalate incident is armed (their correctness
// IS live-groundedness; LoadCorpus also fails closed on that).
func TestCorpusFixtureArm(t *testing.T) {
	c := mustCorpus(t)
	units := map[string]string{ // ref → the down unit its check-host-services fixture must name
		"eval-05": "atlantis.service",
		"eval-09": "calibre.service",
		"eval-16": "garbd.service",
		"eval-19": "garbd.service",
	}
	armed := 0
	for _, inc := range c {
		if inc.Expected != "propose" {
			if inc.FixtureArmed() {
				t.Fatalf("%s (expected=%s) is fixture-armed — only the propose arm is deterministic", inc.ExternalRef, inc.Expected)
			}
			continue
		}
		armed++
		if !inc.FixtureArmed() {
			t.Fatalf("%s is expected-propose but NOT fixture-armed — its recall supply decays with the live estate (the 2026-07-30 trend failure)", inc.ExternalRef)
		}
		if NeedsFreshnessCheck(inc) {
			t.Fatalf("%s must not need a live freshness check", inc.ExternalRef)
		}
		for _, tool := range []string{"get-device-status", "get-active-alerts", "get-device-eventlog", "check-host-services"} {
			if _, okk := inc.ToolFixtures[FixtureKey(tool, inc.Host)]; !okk {
				t.Fatalf("%s lacks a %s fixture for %s", inc.ExternalRef, tool, inc.Host)
			}
		}
		status := inc.ToolFixtures[FixtureKey("get-device-status", inc.Host)]
		if !strings.HasPrefix(status.Output, "LibreNMS device "+inc.Host+": status=") {
			t.Fatalf("%s get-device-status fixture is off-dialect: %q", inc.ExternalRef, status.Output)
		}
		alerts := inc.ToolFixtures[FixtureKey("get-active-alerts", inc.Host)]
		if !strings.HasPrefix(alerts.Output, "active LibreNMS alerts on "+inc.Host+": ") ||
			!strings.Contains(alerts.Output, inc.AlertRule) ||
			!strings.Contains(alerts.Output, "(severity=") || !strings.Contains(alerts.Output, ", since=") {
			t.Fatalf("%s get-active-alerts fixture must show %q firing in the real dialect: %q", inc.ExternalRef, inc.AlertRule, alerts.Output)
		}
		events := inc.ToolFixtures[FixtureKey("get-device-eventlog", inc.Host)]
		if !strings.HasPrefix(events.Output, "LibreNMS eventlog for "+inc.Host+" (most recent ") {
			t.Fatalf("%s get-device-eventlog fixture is off-dialect: %q", inc.ExternalRef, events.Output)
		}
		// The incident's own story must be internally consistent: a down-device incident shows DOWN, a
		// service/resource incident shows the device UP (the fault is inside it).
		wantStatus := "status=UP"
		if inc.AlertRule == "Devices up/down" {
			wantStatus = "status=DOWN"
		}
		if !strings.Contains(status.Output, wantStatus) {
			t.Fatalf("%s device-status fixture must carry %s: %q", inc.ExternalRef, wantStatus, status.Output)
		}
		if unit, okk := units[inc.ExternalRef]; okk {
			svc := inc.ToolFixtures[FixtureKey("check-host-services", inc.Host)]
			if !svc.Success {
				t.Fatalf("%s check-host-services fixture must be a successful read", inc.ExternalRef)
			}
			for _, frag := range []string{
				"=== derived: down services (NOT running — restart candidates) ===",
				"systemd units NOT running (restart-service / start-service candidates — the unit name is the `unit` param):",
				unit,
			} {
				if !strings.Contains(svc.Output, frag) {
					t.Fatalf("%s check-host-services fixture lacks the real down-services form %q:\n%s", inc.ExternalRef, frag, svc.Output)
				}
			}
		}
	}
	if armed != 8 {
		t.Fatalf("fixture-armed propose incidents: got %d, want 8 — 6→8 is TG-533's deliberate confighash pair (a deliberate corpus change updates this pin)", armed)
	}
}

// The byte-equality dialect pin: every hostdiag fixture in the corpus must be EXACTLY what the real
// hostdiag renderer emits for that incident's captured step outputs. A wording/synthesis change in
// hostdiag.go fails this loud; the fix is a deliberate re-capture (TG_EVAL_REGEN_FIXTURES=1, see
// fixturegen_test.go), never a hand-tweak of corpus text.
func TestFixtureShapeFaithfulHostdiag(t *testing.T) {
	c := mustCorpus(t)
	byRef := map[string]Incident{}
	for _, inc := range c {
		byRef[inc.ExternalRef] = inc
	}
	for ref, w := range fixtureHostWorlds() {
		inc, okk := byRef[ref]
		if !okk {
			t.Fatalf("captured world %s has no corpus incident", ref)
		}
		if inc.Host != w.host {
			t.Fatalf("%s: captured world host %s != corpus host %s", ref, w.host, inc.Host)
		}
		for _, check := range w.checks {
			fx, okk := inc.ToolFixtures[FixtureKey(check, w.host)]
			if !okk {
				t.Fatalf("%s lacks the %s fixture its captured world declares", ref, check)
			}
			real := renderRealHostdiag(t, check, w.host, w.runner)
			if fx.Success != real.Success || fx.Output != real.Output {
				t.Fatalf("%s %s fixture drifted from the REAL renderer (re-capture with TG_EVAL_REGEN_FIXTURES=1):\n--- corpus (success=%v)\n%s\n--- real (success=%v)\n%s",
					ref, check, fx.Success, fx.Output, real.Success, real.Output)
			}
		}
	}
}
