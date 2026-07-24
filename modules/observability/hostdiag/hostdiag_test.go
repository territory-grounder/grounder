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
	// The runner saw FIXED argvs (df, then a two-level du, then journalctl) — never a shell command string (INV-02).
	if len(fr.calls) != 3 || fr.calls[0][0] != "df" || fr.calls[1][0] != "du" || fr.calls[2][0] != "journalctl" {
		t.Errorf("expected fixed df + du(2-level) + journalctl argvs, got %v", fr.calls)
	}
	// The du must drill two levels (attribution), not one (blind spot).
	if got := strings.Join(fr.calls[1], " "); !strings.Contains(got, "--max-depth=2") {
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
	// df failed once then retried (2 calls) + du ok (1) + journalctl ok (1) = 4 total.
	if len(fr.calls) != 4 {
		t.Errorf("expected 4 runner calls (df fail+retry, du ok, journalctl ok), got %d: %v", len(fr.calls), fr.calls)
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
	// One attempt per step, no retry: df (1) + du (1) + journalctl (1) = 3.
	if len(fr.calls) != 3 {
		t.Errorf("expected 3 runner calls (no retry under cancellation), got %d", len(fr.calls))
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
