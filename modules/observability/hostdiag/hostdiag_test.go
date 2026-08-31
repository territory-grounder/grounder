package hostdiag

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/modules/observability/syslogng"
)

// accessResolver is a lightweight IdentityResolver over the SAME hostdiag allowlist grammar the engine's
// native source uses — glob-match the host, return an ssh Bundle carrying the key REFERENCE (never material),
// else fail closed with ErrUnresolved. It exercises the real credential.Bundle type without pulling in the
// SyncEngine (an internal test cannot import credsource/native, which imports this package).
type accessResolver struct{ accs []Access }

func (r accessResolver) Resolve(_ context.Context, tgt credential.Target) (credential.Bundle, error) {
	for _, a := range r.accs {
		if globMatch(a.HostGlob, tgt.Host) {
			return credential.NewBundle(credential.BundleSpec{
				User: a.SSHUser, Port: 22, Scheme: credential.SchemeSSH, SSHKeyRef: a.KeyRef,
			})
		}
	}
	return credential.Bundle{}, credential.ErrUnresolved
}

// fakeRunner records the FIXED argvs it is asked to run (so the test proves no shell string is built) and
// returns canned stdout keyed by the command name.
type fakeRunner struct {
	calls [][]string
	out   map[string]string
}

func (f *fakeRunner) Run(_ context.Context, _ syslogng.Server, argv []string) (syslogng.RunResult, error) {
	cp := append([]string(nil), argv...)
	f.calls = append(f.calls, cp)
	if o, ok := f.out[argv[0]]; ok {
		return syslogng.RunResult{Stdout: []byte(o), ExitCode: 0}, nil
	}
	return syslogng.RunResult{ExitCode: 0}, nil
}

func toolByName(tools []agent.Tool, name string) agent.Tool {
	for _, t := range tools {
		if t.Name() == name {
			return t
		}
	}
	return nil
}

// The clone of the predecessor's `df`/`du` investigation: check-host-disk SSHes the alerting host and runs the
// FIXED read-only argvs, returning the observation the agent can ground a disk-full on.
func TestCheckHostDiskRunsFixedReadOnlyArgv(t *testing.T) {
	accs := ParseAccess("dc1|dc1*|root|file:/secrets/hostdiag_key")
	fr := &fakeRunner{out: map[string]string{
		"df":         "Filesystem      Size  Used Avail Use% Mounted on\n/dev/sda1       4.8G  4.5G  0.1G  98% /",
		"du":         "4.5G\t/\n3.2G\t/var/log\n0.9G\t/var/lib/docker",
		"journalctl": "Archived and active journals take up 3.1G in the file system.",
	}}
	tools := NewTools(accs, fr, accessResolver{accs})
	disk := toolByName(tools, "check-host-disk")
	if disk == nil {
		t.Fatal("check-host-disk tool must be present")
	}
	if !disk.ReadOnly() {
		t.Fatal("every host-diagnostics tool must be read-only")
	}
	res, err := disk.Invoke(context.Background(), map[string]string{"host": "dc1librespeed01"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got %+v", res)
	}
	if !strings.Contains(res.Output, "98%") {
		t.Errorf("df usage missing from output: %s", res.Output)
	}
	// Attribution: the deeper du names the actual consumer (/var/log) and the journal size is reported — the two
	// signals that let the agent attribute the space rather than guess a reboot.
	if !strings.Contains(res.Output, "/var/log") || !strings.Contains(res.Output, "journals take up") {
		t.Errorf("consumer attribution (deep du + journal usage) missing from output: %s", res.Output)
	}
	// The runner saw FIXED argvs — never a shell command string (INV-02). Asserted by PRESENCE rather than by
	// index, so adding a diagnostic step cannot break a test that is really about argv safety.
	want := map[string]bool{"df": false, "du": false, "journalctl": false, "findmnt": false}
	for _, c := range fr.calls {
		if len(c) == 0 {
			t.Fatalf("empty argv in %v", fr.calls)
		}
		if _, ok := want[c[0]]; !ok {
			t.Errorf("unexpected command %q — check-host-disk must stay a fixed read-only set", c[0])
		}
		want[c[0]] = true
	}
	for cmd, seen := range want {
		if !seen {
			t.Errorf("check-host-disk must run %q, got %v", cmd, fr.calls)
		}
	}
	// The du must drill two levels (attribution), not one (blind spot).
	var du []string
	for _, c := range fr.calls {
		if c[0] == "du" {
			du = c
		}
	}
	if got := strings.Join(du, " "); !strings.Contains(got, "--max-depth=2") {
		t.Errorf("du must run at --max-depth=2 for consumer attribution, got %q", got)
	}
}

// Access control: a host outside every allowlist rule is refused with an honest reason and NO SSH attempt.
func TestUnauthorizedHostRefusedWithoutSSH(t *testing.T) {
	accs := ParseAccess("dc1|dc1*|root|file:/k")
	fr := &fakeRunner{}
	tools := NewTools(accs, fr, accessResolver{accs})
	res, _ := toolByName(tools, "check-host-disk").Invoke(context.Background(), map[string]string{"host": "dc2unknown"})
	if res.Success {
		t.Fatal("a host outside the access rules must not succeed")
	}
	if len(fr.calls) != 0 {
		t.Fatalf("must NOT SSH to an unauthorized host, got calls %v", fr.calls)
	}
	if !strings.Contains(res.Output, "no resolvable SSH credential") {
		t.Errorf("expected a clean refusal reason, got %q", res.Output)
	}
}

func TestNoAccessRulesYieldNoTools(t *testing.T) {
	if NewTools(nil, &fakeRunner{}, accessResolver{}) != nil {
		t.Fatal("no access rules must yield no tools (the agent simply has none)")
	}
}

func TestAllChecksPresentAndReadOnly(t *testing.T) {
	tools := NewTools(ParseAccess("nl|*|root|file:/k"), &fakeRunner{}, accessResolver{ParseAccess("nl|*|root|file:/k")})
	for _, want := range []string{"check-host-disk", "check-host-memory", "check-host-services", "check-host-load"} {
		tl := toolByName(tools, want)
		if tl == nil {
			t.Errorf("missing tool %s", want)
			continue
		}
		if !tl.ReadOnly() {
			t.Errorf("%s must be read-only", want)
		}
	}
}

// svcRunner returns canned systemctl output keyed by SUBCOMMAND — the three check-host-services steps all
// share argv[0]="systemctl", so the base fakeRunner's argv[0] keying cannot tell them apart.
type svcRunner struct {
	calls                     [][]string
	failed, inactive, enabled string
	// containers is the canned `docker ps -a` inventory (name|state|status per line). dockerExit != 0 emulates a
	// host with no Docker installed (the real remote exits 127 with "docker: command not found").
	containers string
	dockerExit int
}

func (f *svcRunner) Run(_ context.Context, _ syslogng.Server, argv []string) (syslogng.RunResult, error) {
	f.calls = append(f.calls, append([]string(nil), argv...))
	joined := strings.Join(argv, " ")
	switch {
	case strings.Contains(joined, "--failed"):
		return syslogng.RunResult{Stdout: []byte(f.failed)}, nil
	case strings.Contains(joined, "list-unit-files"): // check BEFORE list-units
		return syslogng.RunResult{Stdout: []byte(f.enabled)}, nil
	case strings.Contains(joined, "list-units"):
		return syslogng.RunResult{Stdout: []byte(f.inactive)}, nil
	case argv[0] == "docker":
		return syslogng.RunResult{Stdout: []byte(f.containers), ExitCode: f.dockerExit}, nil
	}
	return syslogng.RunResult{}, nil
}

// derivedSection returns the synthesized "down services" summary block from a check-host-services result.
func derivedSection(output string) string {
	for _, p := range strings.Split(output, "\n\n=== ") {
		if strings.HasPrefix(p, "derived: down services") {
			return p
		}
	}
	return ""
}

func servicesTool(t *testing.T, sr *svcRunner) agent.Tool {
	t.Helper()
	accs := ParseAccess("dc1|dc1*|root|file:/secrets/one_key")
	tl := toolByName(NewTools(accs, sr, accessResolver{accs}), "check-host-services")
	if tl == nil {
		t.Fatal("check-host-services tool must be present")
	}
	return tl
}

// The grounded fix (2026-07-24 nginx-down): a service that is ENABLED yet FAILED-or-INACTIVE is surfaced by
// name as a restart candidate, while units that are inactive-but-NOT-enabled (normal) are not flagged.
func TestCheckHostServicesSurfacesEnabledButDownService(t *testing.T) {
	sr := &svcRunner{
		// A cleanly stopped nginx is NOT failed; redis crashed (failed). Both are enabled ⇒ both are down.
		failed: "redis.service loaded failed failed Redis data store",
		// A leading ● bullet on not-found/masked units is REAL systemctl output (grounded on the live host);
		// unitSet must strip it, and a not-found unit (never in the enabled set) must not be flagged.
		inactive: "● apparmor.service         not-found inactive dead apparmor.service\n" +
			"auth-rpcgss-module.service loaded inactive dead Kernel Module supporting RPCSEC_GSS\n" +
			"nginx.service              loaded inactive dead A high performance web server\n" +
			"systemd-fsck-root.service  loaded inactive dead File System Check on Root Device",
		enabled: "cron.service   enabled enabled\n" +
			"nginx.service  enabled enabled\n" +
			"redis.service  enabled enabled\n" +
			"ssh.service    enabled enabled",
	}
	res, err := servicesTool(t, sr).Invoke(context.Background(), map[string]string{"host": "dc1librespeed01"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	sec := derivedSection(res.Output)
	if sec == "" {
		t.Fatalf("a derived down-services summary must be emitted; got:\n%s", res.Output)
	}
	// Both the cleanly-stopped (inactive+enabled) and the crashed (failed+enabled) service are named.
	if !strings.Contains(sec, "nginx.service") || !strings.Contains(sec, "redis.service") {
		t.Errorf("both the enabled+inactive and enabled+failed services must be named down; got %q", sec)
	}
	// A unit that is inactive but NOT enabled is normal — it must NOT be flagged as down (incl. a ●-bulleted
	// not-found unit, whose glyph must be stripped without leaking it into the verdict).
	if strings.Contains(sec, "auth-rpcgss") || strings.Contains(sec, "systemd-fsck") || strings.Contains(sec, "apparmor") {
		t.Errorf("an inactive-but-not-enabled unit must not be flagged down; got %q", sec)
	}
	// The new enabled-baseline step actually ran, as a FIXED argv (no shell string) — INV-02.
	ran := false
	for _, c := range sr.calls {
		if strings.Join(c, " ") == "systemctl list-unit-files --type=service --state=enabled --no-legend --no-pager" {
			ran = true
		}
	}
	if !ran {
		t.Errorf("check-host-services must run the enabled-unit-files baseline step; calls=%v", sr.calls)
	}
}

// No false positive: when every enabled service is running, the summary says so rather than naming noise.
func TestCheckHostServicesNoFalsePositiveWhenAllRunning(t *testing.T) {
	sr := &svcRunner{
		failed:   "",
		inactive: "systemd-fsck-root.service loaded inactive dead File System Check on Root Device",
		enabled:  "nginx.service enabled enabled\nssh.service enabled enabled",
	}
	res, err := servicesTool(t, sr).Invoke(context.Background(), map[string]string{"host": "dc1librespeed01"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	sec := derivedSection(res.Output)
	if !strings.Contains(sec, "none") {
		t.Errorf("with every enabled service running, the summary must report none down; got %q", sec)
	}
	if strings.Contains(sec, "systemd-fsck") {
		t.Errorf("an inactive-but-not-enabled unit must not be flagged; got %q", sec)
	}
}

// A truncated source list must be flagged, not silently read as an authoritative verdict (HIGH review finding).
func TestDownServicesSummaryFlagsTruncation(t *testing.T) {
	out := map[string]string{
		svcEnabledLabel:  "nginx.service enabled enabled\n…(truncated to the response cap)",
		svcInactiveLabel: "nginx.service loaded inactive dead A high performance web server",
		svcFailedLabel:   "",
	}
	s := downServicesSummary(out)
	if !strings.Contains(s, "nginx.service") {
		t.Errorf("the partial verdict must still name what it found; got %q", s)
	}
	if !strings.Contains(s, "truncated") {
		t.Errorf("a truncated source list must be surfaced so the verdict is not read as complete; got %q", s)
	}
}

// A oneshot service enabled+inactive after it ran IS listed as a candidate (by design — the agent confirms
// before acting); this documents that acknowledged behavior so a future change to it is a conscious one.
func TestDownServicesSummaryListsEnabledInactiveOneshot(t *testing.T) {
	out := map[string]string{
		svcEnabledLabel:  "e2scrub_reap.service enabled enabled\nnginx.service enabled enabled",
		svcInactiveLabel: "e2scrub_reap.service loaded inactive dead Remove Stale...",
		svcFailedLabel:   "",
	}
	if s := downServicesSummary(out); !strings.Contains(s, "e2scrub_reap.service") {
		t.Errorf("an enabled+inactive unit is a candidate the agent then confirms; got %q", s)
	}
}

// Fail-safe: with no enabled-baseline captured (older systemd / a read gap), no verdict is fabricated.
func TestCheckHostServicesNoBaselineNoVerdict(t *testing.T) {
	sr := &svcRunner{failed: "", inactive: "nginx.service loaded inactive dead A high performance web server", enabled: ""}
	res, err := servicesTool(t, sr).Invoke(context.Background(), map[string]string{"host": "dc1librespeed01"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if sec := derivedSection(res.Output); sec != "" {
		t.Errorf("with no enabled baseline, no down-services verdict may be synthesized; got %q", sec)
	}
	// The raw inactive list is still surfaced so the agent is not left blind.
	if !strings.Contains(res.Output, "nginx.service") {
		t.Errorf("raw step output must still be present; got %s", res.Output)
	}
}

// flapRunner fails the first failNext calls with a deadline error, then behaves like fakeRunner — modelling the
// transient / budget-starved SSH read that made a groundable disk-full escalate in prod.
type flapRunner struct {
	fakeRunner
	failNext int
}

func (f *flapRunner) Run(ctx context.Context, srv syslogng.Server, argv []string) (syslogng.RunResult, error) {
	if f.failNext > 0 {
		f.failNext--
		f.calls = append(f.calls, append([]string(nil), argv...))
		return syslogng.RunResult{}, fmt.Errorf("syslogng: remote read on host:22 aborted by deadline: %w", context.DeadlineExceeded)
	}
	return f.fakeRunner.Run(ctx, srv, argv)
}

// A single transient SSH failure must NOT make the agent escalate a groundable disk-full: the read is retried
// once (idempotent, read-only) and still returns the df usage.
func TestCheckHostDiskRetriesOnceOnTransient(t *testing.T) {
	fr := &flapRunner{fakeRunner: fakeRunner{out: map[string]string{
		"df": "Filesystem      Size  Used Avail Use% Mounted on\n/dev/sda1       4.8G  4.5G  0.1G  98% /",
		"du": "4.5G\t/",
	}}, failNext: 1}
	disk := toolByName(NewTools(ParseAccess("dc1|dc1*|root|file:/k"), fr, accessResolver{ParseAccess("dc1|dc1*|root|file:/k")}), "check-host-disk")
	res, err := disk.Invoke(context.Background(), map[string]string{"host": "dc1librespeed01"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success after one transient failure + retry, got %+v", res)
	}
	if !strings.Contains(res.Output, "98%") {
		t.Errorf("df usage missing after retry: %s", res.Output)
	}
	// df failed once then retried (2 calls); every other step ran once. The point is the ONE retry, so it is
	// expressed as steps+1 rather than a literal that any new step would falsify.
	if want := len(diskSteps()) + 1; len(fr.calls) != want {
		t.Errorf("expected %d runner calls (one df retry, every step once), got %d: %v", want, len(fr.calls), fr.calls)
	}
}

// A parent context that is ALREADY cancelled must not trigger a retry (respect real cancellation) and yields no
// success — the agent gets an honest reason, not a detached SSH storm.
func TestNoRetryWhenParentCancelled(t *testing.T) {
	fr := &flapRunner{fakeRunner: fakeRunner{out: map[string]string{"df": "x"}}, failNext: 10}
	disk := toolByName(NewTools(ParseAccess("dc1|dc1*|root|file:/k"), fr, accessResolver{ParseAccess("dc1|dc1*|root|file:/k")}), "check-host-disk")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, _ := disk.Invoke(ctx, map[string]string{"host": "dc1librespeed01"})
	if res.Success {
		t.Fatal("a cancelled parent must not yield success")
	}
	// One attempt per step, NO retry — that is the property under test, so it is stated as exactly one call
	// per declared step rather than as a literal count.
	if want := len(diskSteps()); len(fr.calls) != want {
		t.Errorf("expected %d runner calls (one per step, no retry under cancellation), got %d", want, len(fr.calls))
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, "ok"},
		{context.DeadlineExceeded, "deadline"},
		{fmt.Errorf("syslogng: remote read on h:22 aborted by deadline: %w", context.DeadlineExceeded), "deadline"},
		{fmt.Errorf("syslogng: dial h:22: connection refused"), "dial"},
		{fmt.Errorf("syslogng: ssh handshake with h refused: ssh: unable to authenticate"), "auth-or-handshake"},
		// a file the worker can't READ (perms/missing) is a config fault, NOT a key change — must not be "hostkey"
		{fmt.Errorf("syslogng: known_hosts file /secrets/known_hosts is unusable (fail closed): open /secrets/known_hosts: permission denied"), "secrets-unreadable"},
		{fmt.Errorf("open /secrets/tg-syslog-ro: permission denied"), "secrets-unreadable"},
		// a genuine host-key rejection from the knownhosts callback IS "hostkey"
		{fmt.Errorf("syslogng: ssh handshake with h refused: knownhosts: key mismatch"), "hostkey"},
		{fmt.Errorf("some unexpected error"), "other"},
	}
	for _, c := range cases {
		if got := classify(c.err); got != c.want {
			t.Errorf("classify(%v)=%q want %q", c.err, got, c.want)
		}
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		glob, host string
		want       bool
	}{
		{"dc1*", "dc1librespeed01", true},
		{"dc1*", "dc2pve01", false},
		{"*", "anything", true},
		{"*pve01", "dc1pve01", true},
		{"exacthost", "exacthost", true},
		{"exacthost", "other", false},
	}
	for _, c := range cases {
		if got := globMatch(c.glob, c.host); got != c.want {
			t.Errorf("globMatch(%q,%q)=%v want %v", c.glob, c.host, got, c.want)
		}
	}
}

// CONTAINERS ARE SERVICES TOO (grounded 2026-07-27 on a real container-down).
//
// Measured on this estate: of the 13 pool guests, ZERO run their application as a systemd unit — every one
// runs plain Docker containers. So the three systemctl lists see `docker.service` UP and report nothing wrong
// while the application is down. The consequence was not a worse answer but NO answer: `restart-container`
// requires params.container, no tool in the agent's surface could name a container, and the op-class — built,
// policy-ruled, allowlisted and lockstep-bound — was structurally unproposable. Proven end-to-end: stopping
// the `mealie` container raised a LibreNMS Service-up/down alert in 2.5 min that TG could see and never act on.

// The core new behavior: a stopped container is named, with the exact string the `container` param needs.
func TestCheckHostServicesNamesStoppedContainerAsRestartCandidate(t *testing.T) {
	sr := &svcRunner{
		enabled:    "docker.service enabled enabled\nssh.service enabled enabled",
		inactive:   "",
		failed:     "",
		containers: "mealie|exited|Exited (0) 3 minutes ago\npostgres|running|Up 2 hours (healthy)\nwatchtower|running|Up 2 hours",
	}
	res, err := servicesTool(t, sr).Invoke(context.Background(), map[string]string{"host": "dc1mealie01"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	sec := derivedSection(res.Output)
	if !strings.Contains(sec, "mealie") {
		t.Fatalf("the stopped container must be named — without it restart-container cannot be proposed at all; got %q", sec)
	}
	if !strings.Contains(sec, "restart-container") {
		t.Errorf("the summary must name the op-class so the agent connects the finding to the action; got %q", sec)
	}
	if !strings.Contains(sec, "`container` param") {
		t.Errorf("the summary must say WHICH param the name fills, or the agent emits an incomplete proposal; got %q", sec)
	}
	// The running siblings must NOT be listed as candidates — a false candidate invites a needless restart.
	for _, ok := range []string{"postgres", "watchtower"} {
		if strings.Contains(sec, ok+" [") {
			t.Errorf("a RUNNING container must not be a restart candidate, but %q was listed; got %q", ok, sec)
		}
	}
}

// No false positive: a fully healthy container host must not produce a candidate.
func TestCheckHostServicesNoContainerCandidateWhenAllRunning(t *testing.T) {
	sr := &svcRunner{
		enabled:    "docker.service enabled enabled",
		containers: "mealie|running|Up 2 hours (healthy)\npostgres|running|Up 2 hours (healthy)",
	}
	res, err := servicesTool(t, sr).Invoke(context.Background(), map[string]string{"host": "dc1mealie01"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	sec := derivedSection(res.Output)
	if strings.Contains(sec, "restart-container candidates") {
		t.Errorf("no container is down, so no restart-container candidate may be claimed; got %q", sec)
	}
	if !strings.Contains(sec, "all 2 containers are running") {
		t.Errorf("a healthy container host should say so explicitly rather than stay silent; got %q", sec)
	}
}

// THE STRUCTURAL PROPERTY, and the reason the summariser was split in two. The systemd half returns "" when no
// enabled-baseline was captured (deliberate: never fabricate a verdict from the noisy lists alone). If the two
// halves shared that gate, a host whose systemd read degraded would ALSO lose its container answer — silently
// suppressing the one signal that names the fault. The two families are independently gated.
func TestContainerCandidateSurvivesAMissingSystemdBaseline(t *testing.T) {
	sr := &svcRunner{
		enabled:    "", // no should-run baseline: the systemd half must (correctly) stay silent
		inactive:   "nginx.service loaded inactive dead",
		containers: "mealie|exited|Exited (0) 1 minute ago",
	}
	res, err := servicesTool(t, sr).Invoke(context.Background(), map[string]string{"host": "dc1mealie01"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	sec := derivedSection(res.Output)
	if !strings.Contains(sec, "mealie") {
		t.Fatalf("a missing systemd baseline must NOT suppress the container finding; got %q", sec)
	}
	if strings.Contains(sec, "nginx.service") {
		t.Errorf("with no enabled baseline the systemd half must still fabricate nothing; got %q", sec)
	}
}

// A host with no Docker exits 127. That must read as "not applicable here", never as a finding and never as a
// false all-clear — the estate's Proxmox nodes and network gear are exactly this case.
func TestNoDockerHostMakesNoContainerClaim(t *testing.T) {
	sr := &svcRunner{
		enabled:    "ssh.service enabled enabled",
		dockerExit: 127, // "docker: command not found"
		containers: "",
	}
	res, err := servicesTool(t, sr).Invoke(context.Background(), map[string]string{"host": "dc1pve01"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	sec := derivedSection(res.Output)
	if strings.Contains(sec, "restart-container") || strings.Contains(sec, "containers are running") {
		t.Errorf("a host without Docker must make NO container claim in either direction; got %q", sec)
	}
	if !strings.Contains(res.Output, "may not apply on this host") {
		t.Errorf("the non-zero exit should render as the existing not-applicable note; got %q", res.Output)
	}
}

// MUTATION CONTROL. downContainersSummary is only load-bearing if removing its input turns the tests above RED.
// This asserts the dependency directly: identical state minus the container inventory yields no candidate. If
// this ever fails, the summary is naming containers from something other than the docker step and the tests
// above are not testing what they claim.
func TestMutationControl_WithoutTheDockerStepNoContainerIsEverNamed(t *testing.T) {
	withStep := map[string]string{
		svcEnabledLabel:    "docker.service enabled enabled",
		svcContainersLabel: "mealie|exited|Exited (0) 1 minute ago",
	}
	if s := downServicesSummary(withStep); !strings.Contains(s, "mealie") {
		t.Fatalf("baseline: the container must be named when the step ran; got %q", s)
	}
	withoutStep := map[string]string{
		svcEnabledLabel: "docker.service enabled enabled",
		// svcContainersLabel deliberately ABSENT — exactly the pre-change state of the world.
	}
	s := downServicesSummary(withoutStep)
	if strings.Contains(s, "mealie") || strings.Contains(s, "restart-container") {
		t.Fatalf("without the docker step nothing can name a container — if this passes, the guard is not "+
			"load-bearing and the container tests prove nothing; got %q", s)
	}
}

// The argv must stay a FIXED, shell-free vector (INV-02) and remain READ-ONLY. `docker ps` is an inventory
// read; anything mutating (start/stop/restart/rm/exec) must never appear in an observability check.
func TestContainerStepArgvIsFixedAndReadOnly(t *testing.T) {
	sr := &svcRunner{enabled: "docker.service enabled enabled", containers: "x|running|Up"}
	if _, err := servicesTool(t, sr).Invoke(context.Background(), map[string]string{"host": "dc1mealie01"}); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	var seen bool
	for _, argv := range sr.calls {
		if len(argv) == 0 || argv[0] != "docker" {
			continue
		}
		seen = true
		if got, want := strings.Join(argv, " "), "docker ps -a --format {{.Names}}|{{.State}}|{{.Status}}"; got != want {
			t.Errorf("the container step must be this exact fixed argv:\n got %q\nwant %q", got, want)
		}
		for _, banned := range []string{"start", "stop", "restart", "rm", "exec", "kill", "run"} {
			for _, el := range argv[1:] {
				if el == banned {
					t.Fatalf("an observability check must never carry the mutating verb %q: %v", banned, argv)
				}
			}
		}
		for _, el := range argv {
			for _, meta := range []string{";", "&&", "$(", "`", ">"} {
				if strings.Contains(el, meta) {
					t.Fatalf("argv element %q carries shell metacharacter %q — INV-02 forbids a shell-built command", el, meta)
				}
			}
		}
	}
	if !seen {
		t.Fatal("check-host-services must actually run the docker step")
	}
}

// diskSteps returns check-host-disk's declared steps, so retry tests assert "one call per step" instead of a
// literal that goes stale the moment a diagnostic is added.
func diskSteps() []step {
	for _, c := range checks {
		if c.name == "check-host-disk" {
			return c.steps
		}
	}
	panic("check-host-disk is not in the catalogue")
}

// THE DECIDING FACT MUST BE STATED, NOT BURIED.
//
// Measured over 96 injected disk-fill faults: TG cited the loopback constraint and correctly STOOD DOWN 63
// times — "No actuatable op-class is applicable to a loop-mounted root filesystem in an LXC" — and proposed an
// inapplicable disk-grow the other 33. `df -h` carried /dev/loopN in its Filesystem column every single time,
// so the fact was always present and was read two times in three. These tests pin it to its own line with an
// explicit verdict, the same technique that made restart-container proposable (MR !579).

func diskToolWith(t *testing.T, findmnt string) (agent.Tool, *fakeRunner) {
	t.Helper()
	fr := &fakeRunner{out: map[string]string{
		"df":         "Filesystem Size Used Avail Use% Mounted on\n" + findmnt + " 9.8G 9.4G 371M 97% /",
		"du":         "5.9G\t/var/tmp\n9.8G\t/",
		"journalctl": "Archived and active journals take up 979.8M in the file system.",
		"findmnt":    findmnt + " ext4 9.8G 9.4G 97%",
	}}
	accs := ParseAccess("dc1|dc1*|root|file:/k")
	return toolByName(NewTools(accs, fr, accessResolver{accs}), "check-host-disk"), fr
}

func diskDerived(output string) string {
	for _, p := range strings.Split(output, "\n\n=== ") {
		if strings.HasPrefix(p, "derived: can the root filesystem be GROWN?") {
			return p
		}
	}
	return ""
}

// A loop-mounted rootfs must say so, and must say that disk-grow is an ERROR rather than leaving the agent to
// infer it from a device path.
func TestLoopbackRootfsIsNamedAsUngrowable(t *testing.T) {
	tool, _ := diskToolWith(t, "/dev/loop16")
	res, err := tool.Invoke(context.Background(), map[string]string{"host": "dc1cloudbeaver01"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	sec := diskDerived(res.Output)
	if sec == "" {
		t.Fatalf("the disk check must emit a grow-applicability verdict; got:\n%s", res.Output)
	}
	if !strings.Contains(sec, "LOOPBACK") || !strings.Contains(sec, "CANNOT grow") {
		t.Fatalf("a loop-mounted rootfs must be named ungrowable IN WORDS, not just as /dev/loopN; got %q", sec)
	}
	// It must also point at the correct outcome, or the agent knows what it cannot do and not what it should.
	if !strings.Contains(sec, "STAND-DOWN") {
		t.Errorf("the verdict must name the correct outcome, not only the forbidden one; got %q", sec)
	}
}

// MUTATION CONTROL for the inverse: a real block device must NOT be declared ungrowable, or the fix would
// simply suppress disk-grow everywhere and call that an improvement.
func TestRealBlockDeviceIsNotDeclaredUngrowable(t *testing.T) {
	tool, _ := diskToolWith(t, "/dev/mapper/vg0-root")
	res, _ := tool.Invoke(context.Background(), map[string]string{"host": "dc1mealie01"})
	sec := diskDerived(res.Output)
	if strings.Contains(sec, "CANNOT grow") {
		t.Fatalf("an LVM-backed rootfs must not be declared ungrowable — that would suppress every legitimate "+
			"disk-grow and disguise it as a fix; got %q", sec)
	}
	if !strings.Contains(sec, "MAY be applicable") {
		t.Errorf("a growable rootfs should say so, hedged — got %q", sec)
	}
}

// An unreadable backing device answers UNKNOWN, never "growable". The failure being closed here is proposing
// an inapplicable remedy, so an unread constraint must not default to satisfied.
func TestUnreadableBackingDeviceIsUnknownNotGrowable(t *testing.T) {
	if got := diskRemedySummary(map[string]string{}); !strings.Contains(got, "UNKNOWN") {
		t.Fatalf("an unread backing device must be UNKNOWN, got %q", got)
	}
	if got := diskRemedySummary(map[string]string{rootBackingLabel: "   "}); !strings.Contains(got, "UNKNOWN") {
		t.Fatalf("a blank backing device must be UNKNOWN, got %q", got)
	}
}

// The match is on the DEVICE PATH, not a substring of the line — a volume merely named "loop" is not loopback.
func TestLoopbackMatchIsOnTheDevicePathNotASubstring(t *testing.T) {
	got := diskRemedySummary(map[string]string{rootBackingLabel: "/dev/mapper/loopback-data ext4 50G 10G 20%"})
	if strings.Contains(got, "CANNOT grow") {
		t.Fatalf("a device merely NAMED loopback is not loop-mounted; got %q", got)
	}
}

// The services summary keeps its own header — the header states what the block IS, and moving it into the
// check must not silently retitle an existing one.
func TestEachSummaryKeepsItsOwnHeader(t *testing.T) {
	for _, c := range checks {
		if c.synthesize != nil && strings.TrimSpace(c.summaryHeader) == "" {
			t.Errorf("check %q synthesizes a summary but declares no header — it would render as a bare "+
				"\"derived\" block that does not say what it contains", c.name)
		}
	}
}

// TG MUST NOT PROPOSE TO ACTUATE ITS OWN TEST HARNESS.
//
// Observed live: TG proposed `start-service` on `tg-restore-diskfill-104101701-1785159115.service` — a
// transient unit its OWN fault injector creates to discharge a restore obligation. It reached a sealed
// manifest and a human approval, and only the host-side allowed-units guard refused it. Had that unit been
// allowlisted, TG would have "healed" by firing its own restore and taken graduation credit for it.
func TestHarnessUnitsAreNotOfferedAsRemediationCandidates(t *testing.T) {
	// REALISTIC FIXTURE. The original listed the harness unit as ENABLED, which is what made the first version
	// of this test pass against a filter that could never fire in production: `tg-restore-*` is a systemd-run
	// TRANSIENT unit and is never in the enabled set. Getting the fixture wrong is how a test certifies a fix
	// that does not work.
	sr := &svcRunner{
		enabled:  "nginx.service enabled enabled",
		inactive: "tg-restore-diskfill-104101701-1785159115.service loaded inactive dead",
		failed:   "nginx.service loaded failed failed",
	}
	res, err := servicesTool(t, sr).Invoke(context.Background(), map[string]string{"host": "dc1openarchiver01"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	sec := derivedSection(res.Output)
	if strings.Contains(sec, "tg-restore-") {
		t.Fatalf("an injector restore unit must never be offered as a remediation candidate — TG would be "+
			"actuating its own harness and taking heal credit for it; got %q", sec)
	}
	// The REAL down service must survive: this must not become a filter that hides estate faults.
	if !strings.Contains(sec, "nginx.service") {
		t.Fatalf("a genuine down service must still be named — the harness filter must not swallow it; got %q", sec)
	}
	// The exclusion is DISCLOSED, not silent.
	if !strings.Contains(sec, "withheld 1 unit") {
		t.Errorf("a withheld candidate must be disclosed, or the list reads as complete when it is not; got %q", sec)
	}
}

// The prefix match must be anchored: an estate service whose name merely CONTAINS the marker is not harness.
func TestHarnessMatchIsAnchoredAtThePrefix(t *testing.T) {
	if isHarnessUnit("app-tg-restore-helper.service") {
		t.Error("a unit merely containing the marker is an estate service, not harness — hiding it would " +
			"suppress a real fault")
	}
	if !isHarnessUnit("tg-restore-diskfill-104101701-1785159115.service") {
		t.Error("the observed live unit must be recognised as harness")
	}
	if !isHarnessUnit("TG-Restore-Something.service") {
		t.Error("the match must be case-insensitive — systemd unit names are not reliably lowercase")
	}
}

// A FAILED UNIT IS A FAULT WHETHER OR NOT IT IS ENABLED.
//
// The enabled-∩ baseline exists to suppress one specific ambiguity: an enabled ONESHOT that ran and exited is
// inactive but healthy. That ambiguity does not apply to a FAILED unit — systemd is asserting it tried to run
// and could not. Filtering `failed` through the baseline dropped real faults (a unit started by a timer or as
// a dependency is failed-but-not-enabled) and then let the summary announce an all-clear over them.
func TestAFailedUnitOutsideTheEnabledBaselineIsStillReported(t *testing.T) {
	sr := &svcRunner{
		enabled:  "nginx.service enabled enabled",
		inactive: "",
		// Started by a timer, so it is NOT in the enabled list — but it FAILED.
		failed: "backup-sync.service loaded failed failed",
	}
	res, err := servicesTool(t, sr).Invoke(context.Background(), map[string]string{"host": "dc1mealie01"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	sec := derivedSection(res.Output)
	if !strings.Contains(sec, "backup-sync.service") {
		t.Fatalf("a FAILED unit must be reported even when it is not in the enabled baseline — systemd is "+
			"asserting it could not run; got %q", sec)
	}
	if strings.Contains(sec, "none —") {
		t.Fatalf("the summary announced an all-clear over a host with a failed unit on it; got %q", sec)
	}
}

// The all-clear must only be said when it is true, and must say what it actually checked.
func TestAllClearIsOnlySaidWhenNothingIsDown(t *testing.T) {
	sr := &svcRunner{enabled: "nginx.service enabled enabled", inactive: "", failed: ""}
	res, _ := servicesTool(t, sr).Invoke(context.Background(), map[string]string{"host": "dc1mealie01"})
	sec := derivedSection(res.Output)
	if !strings.Contains(sec, "no failed unit") {
		t.Errorf("the all-clear must state that it covers FAILED units too, not only enabled-and-running; got %q", sec)
	}
}

// An enabled+inactive oneshot is still reported as a candidate — the baseline logic must survive the
// restructure, or this becomes a fix that simply reports less.
func TestEnabledButInactiveUnitIsStillACandidate(t *testing.T) {
	sr := &svcRunner{
		enabled:  "nginx.service enabled enabled\nmealie.service enabled enabled",
		inactive: "mealie.service loaded inactive dead",
		failed:   "",
	}
	res, _ := servicesTool(t, sr).Invoke(context.Background(), map[string]string{"host": "dc1mealie01"})
	if sec := derivedSection(res.Output); !strings.Contains(sec, "mealie.service") {
		t.Fatalf("an enabled service that is not running must still be named; got %q", sec)
	}
}

// A unit both failed AND inactive must appear ONCE, not twice — a duplicated candidate reads as two faults.
func TestAUnitInBothListsIsReportedOnce(t *testing.T) {
	sr := &svcRunner{
		enabled:  "nginx.service enabled enabled",
		inactive: "nginx.service loaded inactive dead",
		failed:   "nginx.service loaded failed failed",
	}
	res, _ := servicesTool(t, sr).Invoke(context.Background(), map[string]string{"host": "dc1mealie01"})
	if n := strings.Count(derivedSection(res.Output), "nginx.service"); n != 1 {
		t.Fatalf("want nginx.service named exactly once, got %d occurrences", n)
	}
}
