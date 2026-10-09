package syslogng

import (
	"context"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/agent"
)

// fakeRunner records the routed server + remote argv and returns a canned result, so the whole tool path
// runs in CI with no live SSH (the injectable oracle seam).
type fakeRunner struct {
	result    RunResult
	err       error
	gotServer Server
	gotArgv   []string
	calls     int
}

func (f *fakeRunner) Run(_ context.Context, s Server, argv []string) (RunResult, error) {
	f.gotServer = s
	f.gotArgv = append([]string(nil), argv...)
	f.calls++
	return f.result, f.err
}

func testServers() []Server {
	return ParseServers("NL|dc1syslogng01|root|env:SYSLOGNG_SSH_KEY|/mnt/logs/syslog-ng;GR|dc2syslogng01|root|env:SYSLOGNG_SSH_KEY|/mnt/logs/syslog-ng")
}

func findTool(tools []agent.Tool, name string) agent.Tool {
	for _, t := range tools {
		if t.Name() == name {
			return t
		}
	}
	return nil
}

// ---- config parsing ----

func TestParseServers(t *testing.T) {
	got := ParseServers("NL|dc1syslogng01|root|env:SYSLOGNG_SSH_KEY|/mnt/logs/syslog-ng ; GR|dc2syslogng01|svc|file:/run/secrets/k| ; broken|only-two ; |nohost-missing-user|| ")
	if len(got) != 2 {
		t.Fatalf("want 2 valid servers, got %d: %+v", len(got), got)
	}
	nl := got[0]
	if nl.Site != "NL" || nl.SSHHost != "dc1syslogng01" || nl.SSHUser != "root" || string(nl.KeyRef) != "env:SYSLOGNG_SSH_KEY" || nl.BasePath != "/mnt/logs/syslog-ng" {
		t.Errorf("NL row parsed wrong: %+v", nl)
	}
	if nl.HostPrefix != "dc1" {
		t.Errorf("NL host prefix derived wrong: %q want dc1", nl.HostPrefix)
	}
	gr := got[1]
	if gr.BasePath != DefaultBasePath { // empty basepath field ⇒ default
		t.Errorf("GR empty basepath must default, got %q", gr.BasePath)
	}
	if gr.HostPrefix != "dc2" {
		t.Errorf("GR host prefix derived wrong: %q want dc2", gr.HostPrefix)
	}
	if len(ParseServers("")) != 0 {
		t.Error("empty spec must yield no servers")
	}
	// explicit 6th-field prefix override.
	ov := ParseServers("EX|logbox01|root|env:K|/logs|edgegw07")
	if len(ov) != 1 || ov[0].HostPrefix != "edgegw07" {
		t.Errorf("explicit prefix override failed: %+v", ov)
	}
}

// ---- routing: NL vs GR by prefix ----

func TestRoutingNLvsGR(t *testing.T) {
	servers := testServers()
	cases := []struct {
		host    string
		wantOK  bool
		wantSSH string
	}{
		{"dc1fw01", true, "dc1syslogng01"},
		{"dc1sw03", true, "dc1syslogng01"},
		{"dc1k8s-ctrlr01", true, "dc1syslogng01"},
		{"dc2sw01", true, "dc2syslogng01"},
		{"dc2fw01.example.net", true, "dc2syslogng01"},
		{"zzxxx01fw01", false, ""}, // known-shaped prefix, no configured server
		{"192.168.1.1", false, ""}, // an IP carries no site prefix
	}
	for _, c := range cases {
		h, err := validateHost(c.host)
		if err != nil {
			// only the IP is expected to still validate as a host string; routing then refuses it.
			h = normHost(c.host)
		}
		srv, ok, why := resolveServer(servers, h)
		if ok != c.wantOK {
			t.Errorf("%s: ok=%v want %v (why=%q)", c.host, ok, c.wantOK, why)
			continue
		}
		if ok && srv.SSHHost != c.wantSSH {
			t.Errorf("%s: routed to %q want %q", c.host, srv.SSHHost, c.wantSSH)
		}
		if !ok && why == "" {
			t.Errorf("%s: refusal must carry an honest reason", c.host)
		}
	}
}

func TestRoutingAmbiguous(t *testing.T) {
	// two servers claiming the same prefix ⇒ refuse rather than guess.
	servers := ParseServers("A|dc1syslogng01|root|env:K|/l;B|dc1syslogng02|root|env:K|/l")
	if _, ok, why := resolveServer(servers, "dc1fw01"); ok || !strings.Contains(why, "ambiguous") {
		t.Errorf("ambiguous routing must refuse, got ok=%v why=%q", ok, why)
	}
}

// ---- path construction + date handling ----

func TestLogPath(t *testing.T) {
	// default date ⇒ today.log current file.
	if p, label := logPath("/mnt/logs/syslog-ng", "dc1fw01", ""); p != "/mnt/logs/syslog-ng/dc1fw01/today.log" || !strings.Contains(label, "today") {
		t.Errorf("today path wrong: %q (%q)", p, label)
	}
	// explicit date ⇒ dated file under YYYY/MM.
	if p, label := logPath("/mnt/logs/syslog-ng", "dc2sw01", "2026-07-15"); p != "/mnt/logs/syslog-ng/dc2sw01/2026/07/dc2sw01-2026-07-15.log" || label != "2026-07-15" {
		t.Errorf("dated path wrong: %q (%q)", p, label)
	}
	// trailing slash on basepath is normalized.
	if p, _ := logPath("/mnt/logs/syslog-ng/", "dc1fw01", ""); p != "/mnt/logs/syslog-ng/dc1fw01/today.log" {
		t.Errorf("trailing-slash basepath not normalized: %q", p)
	}
}

// ---- REFUSAL oracles: host allowlist + traversal + injection ----

func TestValidateHostRejections(t *testing.T) {
	bad := []string{
		"../../etc/passwd",      // traversal
		"dc1fw01/../../etc", // embedded traversal + separator
		"..",                    // parent ref
		"-rf",                   // leading dash (flag-injection shape)
		".hidden",               // leading dot
		"host;rm -rf /",         // separator/space/metachars
		"host$(id)",             // command-substitution shape
		"a/b",                   // path separator
		"",                      // empty
	}
	for _, b := range bad {
		if h, err := validateHost(b); err == nil {
			t.Errorf("validateHost(%q) must reject, got %q", b, h)
		}
	}
	good := map[string]string{
		"dc1fw01":                     "dc1fw01",
		"GRSKG01SW01":                     "dc2sw01", // lowercased
		"dc1fw01.example.net": "dc1fw01", // domain stripped
	}
	for in, want := range good {
		if h, err := validateHost(in); err != nil || h != want {
			t.Errorf("validateHost(%q) = (%q, %v), want %q", in, h, err, want)
		}
	}
}

func TestValidateDate(t *testing.T) {
	if d, err := validateDate(""); err != nil || d != "" {
		t.Errorf("empty date must default: (%q,%v)", d, err)
	}
	if d, err := validateDate("2026-07-15"); err != nil || d != "2026-07-15" {
		t.Errorf("valid date rejected: (%q,%v)", d, err)
	}
	for _, bad := range []string{"2026-13-45", "2026/07/15", "15-07-2026", "2026-7-5", "../2026", "2026-07-15 rm"} {
		if _, err := validateDate(bad); err == nil {
			t.Errorf("validateDate(%q) must reject", bad)
		}
	}
}

func TestValidatePattern(t *testing.T) {
	if _, err := validatePattern("-v"); err == nil {
		t.Error("leading-dash pattern must be rejected (flag-injection shape)")
	}
	if _, err := validatePattern("line\none"); err == nil {
		t.Error("newline in pattern must be rejected")
	}
	if _, err := validatePattern(""); err == nil {
		t.Error("empty pattern must be rejected")
	}
	if _, err := validatePattern(strings.Repeat("x", 257)); err == nil {
		t.Error("over-long pattern must be rejected")
	}
	if p, err := validatePattern("%ASA-6-302014"); err != nil || p != "%ASA-6-302014" {
		t.Errorf("valid fixed-string pattern rejected: (%q,%v)", p, err)
	}
}

// ---- get-host-logs: happy path + argv + routing ----

const cannedASA = "Jul 15 12:00:01 dc1fw01 %ASA-6-302013: Built inbound TCP connection 1 for outside:192.0.2.9/443\n" +
	"Jul 15 12:00:02 dc1fw01 %ASA-6-302014: Teardown TCP connection 1 for outside:192.0.2.9/443 duration 0:00:01 bytes 12\n" +
	"Jul 15 12:00:03 dc1fw01 %ASA-4-106023: Deny tcp src outside:192.0.2.9/40000 dst inside:10.0.10.5/22 by access-group\n"

func TestGetHostLogsHappyPath(t *testing.T) {
	f := &fakeRunner{result: RunResult{ExitCode: 0, Stdout: []byte(cannedASA)}}
	tools := NewTools(testServers(), f)
	tl := findTool(tools, "get-host-logs")
	if tl == nil || !tl.ReadOnly() {
		t.Fatal("get-host-logs must exist and be read-only")
	}
	res, err := tl.Invoke(context.Background(), map[string]string{"host": "dc1fw01"})
	if err != nil {
		t.Fatalf("invoke error: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got %q", res.Output)
	}
	// routed to the NL server.
	if f.gotServer.SSHHost != "dc1syslogng01" {
		t.Errorf("must route to NL server, got %q", f.gotServer.SSHHost)
	}
	// FIXED argv, server-side bounded tail, today.log current file.
	wantArgv := []string{"tail", "-n", "200", "--", "/mnt/logs/syslog-ng/dc1fw01/today.log"}
	if strings.Join(f.gotArgv, " ") != strings.Join(wantArgv, " ") {
		t.Errorf("argv = %v want %v", f.gotArgv, wantArgv)
	}
	if !strings.Contains(res.Output, "%ASA-6-302014") {
		t.Errorf("output should carry the ASA lines: %q", res.Output)
	}
}

func TestGetHostLogsDatedPath(t *testing.T) {
	f := &fakeRunner{result: RunResult{ExitCode: 0, Stdout: []byte(cannedASA)}}
	tl := findTool(NewTools(testServers(), f), "get-host-logs")
	if _, err := tl.Invoke(context.Background(), map[string]string{"host": "dc2sw01", "date": "2026-07-15", "lines": "50"}); err != nil {
		t.Fatal(err)
	}
	wantArgv := []string{"tail", "-n", "50", "--", "/mnt/logs/syslog-ng/dc2sw01/2026/07/dc2sw01-2026-07-15.log"}
	if strings.Join(f.gotArgv, " ") != strings.Join(wantArgv, " ") {
		t.Errorf("dated argv = %v want %v", f.gotArgv, wantArgv)
	}
	if f.gotServer.SSHHost != "dc2syslogng01" {
		t.Errorf("must route to GR server, got %q", f.gotServer.SSHHost)
	}
}

func TestGetHostLogsLineCapClamped(t *testing.T) {
	f := &fakeRunner{result: RunResult{ExitCode: 0, Stdout: []byte(cannedASA)}}
	tl := findTool(NewTools(testServers(), f), "get-host-logs")
	if _, err := tl.Invoke(context.Background(), map[string]string{"host": "dc1fw01", "lines": "999999"}); err != nil {
		t.Fatal(err)
	}
	if f.gotArgv[2] != "1000" { // clamped to maxLines
		t.Errorf("lines must clamp to 1000, got %q", f.gotArgv[2])
	}
}

// ---- get-host-logs: REFUSALS never reach the runner ----

func TestGetHostLogsRefusals(t *testing.T) {
	f := &fakeRunner{result: RunResult{ExitCode: 0, Stdout: []byte("should not be read")}}
	tl := findTool(NewTools(testServers(), f), "get-host-logs")

	// (a) traversal host — refused before any runner call.
	res, _ := tl.Invoke(context.Background(), map[string]string{"host": "../../etc/passwd"})
	if res.Success || !strings.HasPrefix(res.Output, "refused:") {
		t.Errorf("traversal host must be refused, got %q", res.Output)
	}
	if f.calls != 0 {
		t.Errorf("a refused host must not reach the runner (calls=%d)", f.calls)
	}
	// (b) unknown host (no site match) — honest no-source, still no read.
	res2, _ := tl.Invoke(context.Background(), map[string]string{"host": "zzxxx01fw01"})
	if res2.Success || !strings.Contains(res2.Output, "no syslog-ng log source") {
		t.Errorf("unknown host must yield an honest no-source, got %q", res2.Output)
	}
	if f.calls != 0 {
		t.Errorf("an unroutable host must not reach the runner (calls=%d)", f.calls)
	}
	// the refusal must NOT leak a filesystem path.
	if strings.Contains(res.Output, "/mnt/") || strings.Contains(res2.Output, "/mnt/") {
		t.Error("a refusal must not leak a filesystem path")
	}
}

func TestGetHostLogsFileMissing(t *testing.T) {
	// tail non-zero ⇒ honest "no log file", no path/stderr leak.
	f := &fakeRunner{result: RunResult{ExitCode: 1, Stderr: []byte("tail: /mnt/logs/syslog-ng/dc1fw01/today.log: No such file")}}
	tl := findTool(NewTools(testServers(), f), "get-host-logs")
	res, _ := tl.Invoke(context.Background(), map[string]string{"host": "dc1fw01"})
	if res.Success || !strings.Contains(res.Output, "no syslog-ng log") {
		t.Errorf("missing file must be honest, got %q", res.Output)
	}
	if strings.Contains(res.Output, "/mnt/") {
		t.Errorf("must not leak the path/stderr: %q", res.Output)
	}
}

// ---- search-host-logs: happy path + server-side bounded grep ----

func TestSearchHostLogsHappyPath(t *testing.T) {
	matches := "Jul 15 12:00:02 dc1fw01 %ASA-6-302014: Teardown TCP connection 1\n"
	f := &fakeRunner{result: RunResult{ExitCode: 0, Stdout: []byte(matches)}}
	tl := findTool(NewTools(testServers(), f), "search-host-logs")
	if tl == nil || !tl.ReadOnly() {
		t.Fatal("search-host-logs must exist and be read-only")
	}
	res, err := tl.Invoke(context.Background(), map[string]string{"host": "dc1fw01", "pattern": "%ASA-6-302014"})
	if err != nil || !res.Success {
		t.Fatalf("search failed: %v %q", err, res.Output)
	}
	// server-side bounded fixed-string grep, pattern after `--`, no remote pipe/shell.
	wantArgv := []string{"grep", "-F", "-m", "500", "--", "%ASA-6-302014", "/mnt/logs/syslog-ng/dc1fw01/today.log"}
	if strings.Join(f.gotArgv, " ") != strings.Join(wantArgv, " ") {
		t.Errorf("search argv = %v want %v", f.gotArgv, wantArgv)
	}
	if !strings.Contains(res.Output, "302014") {
		t.Errorf("output should carry the match: %q", res.Output)
	}
}

func TestSearchHostLogsNoMatch(t *testing.T) {
	// grep exit 1 = no match, not an error.
	f := &fakeRunner{result: RunResult{ExitCode: 1, Stdout: nil}}
	tl := findTool(NewTools(testServers(), f), "search-host-logs")
	res, _ := tl.Invoke(context.Background(), map[string]string{"host": "dc1fw01", "pattern": "NEVERMATCHES"})
	if !res.Success || !strings.Contains(res.Output, "no lines matching") {
		t.Errorf("no-match must succeed honestly, got %q", res.Output)
	}
}

func TestSearchHostLogsRefusePattern(t *testing.T) {
	f := &fakeRunner{result: RunResult{ExitCode: 0, Stdout: []byte("x")}}
	tl := findTool(NewTools(testServers(), f), "search-host-logs")
	res, _ := tl.Invoke(context.Background(), map[string]string{"host": "dc1fw01", "pattern": "-rf"})
	if res.Success || !strings.HasPrefix(res.Output, "refused:") {
		t.Errorf("leading-dash pattern must be refused, got %q", res.Output)
	}
	if f.calls != 0 {
		t.Errorf("a refused pattern must not reach the runner (calls=%d)", f.calls)
	}
}

// ---- BOUNDING: a huge canned blob is capped in bytes AND lines ----

func TestSearchHostLogsBounded(t *testing.T) {
	// simulate a hot firewall file: many long matching lines returned by the runner.
	line := strings.Repeat("A", 400) + "\n"
	blob := strings.Repeat(line, 5000) // ~2 MB, 5000 lines
	f := &fakeRunner{result: RunResult{ExitCode: 0, Stdout: []byte(blob)}}
	tl := findTool(NewTools(testServers(), f), "search-host-logs")
	res, err := tl.Invoke(context.Background(), map[string]string{"host": "dc1fw01", "pattern": "A"})
	if err != nil || !res.Success {
		t.Fatalf("bounded search failed: %v", err)
	}
	if len(res.Output) > maxOutputBytes+1024 {
		t.Errorf("output not byte-bounded: %d bytes (cap %d)", len(res.Output), maxOutputBytes)
	}
	// the log-body line count must be capped to the hit cap.
	body := res.Output
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		body = body[i+1:] // drop the header line
	}
	if n := strings.Count(body, "\n") + 1; n > searchMaxHits {
		t.Errorf("output not line-bounded: %d lines (cap %d)", n, searchMaxHits)
	}
	if !strings.Contains(res.Output, "truncated") {
		t.Error("a bounded/truncated response must say so")
	}
}

func TestGetHostLogsByteBounded(t *testing.T) {
	// one pathologically long line (no newlines) must still be byte-capped.
	blob := strings.Repeat("A", 3*maxOutputBytes)
	f := &fakeRunner{result: RunResult{ExitCode: 0, Stdout: []byte(blob)}}
	tl := findTool(NewTools(testServers(), f), "get-host-logs")
	res, _ := tl.Invoke(context.Background(), map[string]string{"host": "dc1fw01"})
	if len(res.Output) > maxOutputBytes+256 {
		t.Errorf("output not byte-bounded: %d (cap %d)", len(res.Output), maxOutputBytes)
	}
	if !strings.Contains(res.Output, "truncated") {
		t.Error("a byte-truncated response must say so")
	}
}

// ---- read-only guarantee: both tools register into a read-only tool set ----

func TestToolsAreReadOnly(t *testing.T) {
	set := agent.NewReadOnlyToolSet()
	for _, tl := range NewTools(testServers(), &fakeRunner{}) {
		if !tl.ReadOnly() {
			t.Fatalf("%s is not read-only", tl.Name())
		}
		if err := set.Register(tl); err != nil {
			t.Fatalf("read-only tool %s must register: %v", tl.Name(), err)
		}
	}
	names := set.Names()
	if len(names) != 2 {
		t.Fatalf("want 2 registered tools, got %v", names)
	}
}

func TestNewToolsEmpty(t *testing.T) {
	if NewTools(nil, &fakeRunner{}) != nil {
		t.Error("no servers ⇒ no tools")
	}
}

// The production runner's own oracles (fail-closed key/known_hosts handling, the in-process SSH
// server end-to-end proof, and host-key rejection) live in native_test.go.
