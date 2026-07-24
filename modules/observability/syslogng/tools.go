package syslogng

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/agent"
)

// The agent's READ-ONLY syslog-ng investigation tools. They give triage the device-log window the
// predecessor's cisco-asa-specialist and triage-researcher had — read the firewall/switch/router's own
// syslog while diagnosing — which TG lacked entirely. Both tools are read-only (ReadOnly()=true; the
// ToolSet refuses a non-read-only tool), route a host to its site's server from config, validate every
// model-chosen arg against a strict allowlist, bound output at the server (`tail -n` / `grep -F -m`) and
// again in Go, and enforce a context timeout. A lookup that cannot be served returns
// ToolResult{Success:false} with an honest reason (the agent adapts) — never a Go error that aborts the
// session, and never a raw path leaked into TG's own logs. The returned log text is an untrusted
// observation (INV-08): nothing in it becomes control flow.

const (
	defaultLines   = 200     // get-host-logs default line count
	maxLines       = 1000    // get-host-logs hard cap
	searchMaxHits  = 500     // search-host-logs grep -m cap (server-side match bound)
	maxOutputBytes = 1 << 20 // 1 MiB response cap regardless of line count (one ASA line is long)
	defaultTimeout = 20 * time.Second
)

// toolBox is the shared read seam the two tools hang off.
type toolBox struct {
	servers []Server
	runner  Runner
	timeout time.Duration
}

// NewTools returns the read-only syslog-ng investigation tools bound to the configured servers + runner.
// With no servers it returns nil (the agent simply has no syslog-ng tools). A nil runner selects the
// production NATIVE in-process SSH runner (no subprocess — the worker image has no ssh binary), with
// mandatory host-key verification against the operator-declared known_hosts file (KnownHostsEnv);
// unset, every read refuses fail-closed rather than connecting unverified.
func NewTools(servers []Server, runner Runner) []agent.Tool {
	if len(servers) == 0 {
		return nil
	}
	if runner == nil {
		runner = NewNativeRunner(os.Getenv(KnownHostsEnv))
	}
	b := &toolBox{servers: servers, runner: runner, timeout: defaultTimeout}
	return []agent.Tool{getHostLogsTool{b}, searchHostLogsTool{b}}
}

// ---- get-host-logs ----
type getHostLogsTool struct{ b *toolBox }

func (getHostLogsTool) Name() string   { return "get-host-logs" }
func (getHostLogsTool) ReadOnly() bool { return true }

func (t getHostLogsTool) Invoke(ctx context.Context, args map[string]string) (agent.ToolResult, error) {
	raw := hostArg(args)
	res := agent.ToolResult{ID: "syslogng-logs-" + sanitizeID(raw), Tool: t.Name()}

	host, err := validateHost(raw)
	if err != nil {
		res.Output = fmt.Sprintf("refused: %v (host=%q)", err, raw)
		return res, nil
	}
	date, err := validateDate(args["date"])
	if err != nil {
		res.Output = fmt.Sprintf("refused: %v", err)
		return res, nil
	}
	lines := intArg(args, "lines", defaultLines)
	if lines < 1 {
		lines = defaultLines
	}
	if lines > maxLines {
		lines = maxLines
	}

	server, ok, why := resolveServer(t.b.servers, host)
	if !ok {
		res.Output = why
		return res, nil
	}
	p, label := logPath(server.BasePath, host, date)
	// Bounded at the server: only the last <lines> lines transit — never the whole (possibly 100s of MB) file.
	argv := []string{"tail", "-n", strconv.Itoa(lines), "--", p}

	cctx, cancel := context.WithTimeout(ctx, t.b.timeout)
	defer cancel()
	rr, runErr := t.b.runner.Run(cctx, server, argv)
	if runErr != nil {
		res.Output = fmt.Sprintf("log read failed for %s via %s (site %s): the syslog server was unreachable or the read errored", host, server.SSHHost, server.Site)
		return res, nil
	}
	if rr.ExitCode != 0 {
		// tail non-zero ⇒ the file is missing/unreadable. Do NOT leak the path or stderr.
		res.Output = fmt.Sprintf("no syslog-ng log for %s via %s (date %s): the device may not log there, or that day has no file", host, server.SSHHost, label)
		return res, nil
	}

	text, n, truncated := boundOutput(rr.Stdout, lines, true)
	var sb strings.Builder
	fmt.Fprintf(&sb, "syslog-ng logs for %s via %s [site %s] — last %d line(s), date %s", host, server.SSHHost, server.Site, n, label)
	if truncated {
		sb.WriteString(" (truncated to the response cap)")
	}
	sb.WriteString(":\n")
	sb.WriteString(text)
	res.Success = true
	res.Output = sb.String()
	return res, nil
}

// ---- search-host-logs ----
type searchHostLogsTool struct{ b *toolBox }

func (searchHostLogsTool) Name() string   { return "search-host-logs" }
func (searchHostLogsTool) ReadOnly() bool { return true }

func (t searchHostLogsTool) Invoke(ctx context.Context, args map[string]string) (agent.ToolResult, error) {
	raw := hostArg(args)
	res := agent.ToolResult{ID: "syslogng-search-" + sanitizeID(raw), Tool: t.Name()}

	host, err := validateHost(raw)
	if err != nil {
		res.Output = fmt.Sprintf("refused: %v (host=%q)", err, raw)
		return res, nil
	}
	pattern, err := validatePattern(patternArg(args))
	if err != nil {
		res.Output = fmt.Sprintf("refused: %v (pattern=%q)", err, patternArg(args))
		return res, nil
	}
	// date? / since? — either selects the day's file; default is the current today.log.
	dateRaw := args["date"]
	if strings.TrimSpace(dateRaw) == "" {
		dateRaw = args["since"]
	}
	date, err := validateDate(dateRaw)
	if err != nil {
		res.Output = fmt.Sprintf("refused: %v", err)
		return res, nil
	}
	hits := intArg(args, "lines", searchMaxHits)
	if hits < 1 {
		hits = searchMaxHits
	}
	if hits > searchMaxHits {
		hits = searchMaxHits
	}

	server, ok, why := resolveServer(t.b.servers, host)
	if !ok {
		res.Output = why
		return res, nil
	}
	p, label := logPath(server.BasePath, host, date)
	// Bounded at the server: `grep -F` is a FIXED-string scan (never a regex), `-m <hits>` stops after
	// <hits> matches, and the pattern is a distinct argv element after `--` (never a flag, never a shell
	// token). Only the matched lines — capped by -m — transit; the file itself never crosses the wire.
	argv := []string{"grep", "-F", "-m", strconv.Itoa(hits), "--", pattern, p}

	cctx, cancel := context.WithTimeout(ctx, t.b.timeout)
	defer cancel()
	rr, runErr := t.b.runner.Run(cctx, server, argv)
	if runErr != nil {
		res.Output = fmt.Sprintf("log search failed for %s via %s (site %s): the syslog server was unreachable or the read errored", host, server.SSHHost, server.Site)
		return res, nil
	}
	// grep exit: 0 = matches, 1 = no matches (not an error), >1 = grep/ssh error.
	if rr.ExitCode > 1 {
		res.Output = fmt.Sprintf("no syslog-ng log to search for %s via %s (date %s): the device may not log there, or that day has no file", host, server.SSHHost, label)
		return res, nil
	}

	text, n, truncated := boundOutput(rr.Stdout, hits, false)
	res.Success = true
	if n == 0 {
		res.Output = fmt.Sprintf("no lines matching %q for %s via %s [site %s], date %s (scanned up to %d matches)", pattern, host, server.SSHHost, server.Site, label, hits)
		return res, nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "syslog-ng matches for %q on %s via %s [site %s], date %s — %d line(s)", pattern, host, server.SSHHost, server.Site, label, n)
	if truncated {
		sb.WriteString(" (truncated to the response cap)")
	}
	sb.WriteString(":\n")
	sb.WriteString(text)
	res.Output = sb.String()
	return res, nil
}

// ---- shared helpers ----

// boundOutput enforces the byte AND line caps on a raw read. keepTail keeps the most-recent lines (a
// `tail` window ends with the newest); a `grep` window keeps the first matches. It returns the bounded
// text, its line count, and whether anything was dropped.
func boundOutput(raw []byte, maxLinesN int, keepTail bool) (text string, count int, truncated bool) {
	if len(raw) > maxOutputBytes {
		if keepTail {
			raw = raw[len(raw)-maxOutputBytes:]
		} else {
			raw = raw[:maxOutputBytes]
		}
		truncated = true
	}
	lines := strings.Split(string(raw), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1] // drop the empty tail from a trailing newline
	}
	if len(lines) > maxLinesN {
		if keepTail {
			lines = lines[len(lines)-maxLinesN:]
		} else {
			lines = lines[:maxLinesN]
		}
		truncated = true
	}
	return strings.Join(lines, "\n"), len(lines), truncated
}

// hostArg mirrors the LibreNMS/estate tools' argument convention so the agent uses one shape everywhere.
func hostArg(args map[string]string) string {
	for _, k := range []string{"host", "target", "device", "hostname"} {
		if v := strings.TrimSpace(args[k]); v != "" {
			return v
		}
	}
	return ""
}

// patternArg reads the search pattern under either key.
func patternArg(args map[string]string) string {
	for _, k := range []string{"pattern", "query"} {
		if v := args[k]; strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// intArg parses a positive-integer arg; a missing/blank/malformed value yields the default.
func intArg(args map[string]string, key string, def int) int {
	v := strings.TrimSpace(args[key])
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// sanitizeID keeps a ToolResult id printable and stable for the citation gate.
func sanitizeID(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	var b strings.Builder
	for _, r := range h {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "unnamed"
	}
	return b.String()
}
