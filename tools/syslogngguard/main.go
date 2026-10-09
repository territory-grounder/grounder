// Command tg-syslogng-guard is the forced command pinned to TG's syslog-ng READ key in authorized_keys on
// each syslog host (TG-305).
//
// WHY IT IS A BINARY AND NOT A SHELL SCRIPT. Its sibling deploy/actuation-guard/tg-actuator-guard is `sh`,
// and can be, because it matches SSH_ORIGINAL_COMMAND byte-for-byte against a vetted allowlist and only
// then evaluates it — by the time `eval` runs, the string is provably one of N known-good lines.
//
// This lane cannot work that way. Its two tools carry operator/agent-chosen values:
//
//	'tail' '-n' '<lines>' '--' '<path>'
//	'grep' '-F' '-m' '<hits>' '--' '<pattern>' '<path>'
//
// <pattern> is FREE TEXT, so the command set is not enumerable and the guard has to validate SHAPE instead.
// Shape validation means parsing the untrusted string BEFORE the gate — and doing that in shell requires
// either `eval` on attacker-controlled input (arbitrary execution) or a hand-rolled quote parser in POSIX
// sh (the kind of code this guard exists to protect against). So it is Go: parse, validate, then
// syscall.Exec an argv array. No shell is ever involved, so no metacharacter in <pattern> can mean anything.
//
// WHAT IT ENFORCES. Only the two read shapes above; the binary must be exactly `tail` or `grep`; numeric
// arguments must be digits within a cap; and the path must resolve INSIDE the configured base directory
// after symlink resolution. The pattern is never inspected and never needs to be — it reaches grep as one
// argv element, where it cannot become a flag (it sits after `--`) or a shell token (there is no shell).
//
// Exit 42 on refusal, matching tg-actuator-guard and tg-readonly-guard, so a caller can tell a refusal from
// an unreachable host. That distinction is load-bearing: these lanes report a failed read as
// "(host was unreachable or the read errored)", and TG-271/TG-300 are the record of what it costs when the
// agent cannot tell blind from quiet.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// errNoSuchLog is the sentinel for a request that was WELL-FORMED and lexically inside the log base, and
// whose file simply does not exist. It is reported with its own exit status so a caller can tell an estate
// FACT ("this host does not ship logs here") from a TG DEFECT ("TG built a request this guard rejects").
//
// Both used to exit 42, and the caller collapsed that into "the device may not log there, or that day has
// no file" — so a malformed argv from TG would have been reported to the agent, permanently and silently,
// as an observation about the estate. That is the blind-vs-quiet conflation TG-271/TG-300 exist about, and
// this guard's own header calls the refused/unreachable distinction load-bearing for exactly that reason
// (TG-363).
var errNoSuchLog = errors.New("no such log file")

const (
	refusedExit = 42
	// noSuchLogExit is DELIBERATELY not 42. A caller seeing 42 from an older guard still gets today's
	// honest hedge; a caller seeing 44 gets a definite estate fact. That asymmetry is what makes the
	// rollout safe in either order.
	noSuchLogExit = 44
	defaultBase   = "/mnt/logs/syslog-ng"
	maxTailLines  = 2000
	maxGrepHits   = 2000
)

// buildStamp identifies which tree this binary was built from, so a DEPLOYED copy can be compared with
// the source that is supposed to be on the host.
//
// WHY THIS EXISTS. On 2026-08-07 the TG-363 fix — the very exit-44 distinction above — was found merged
// on main since the previous morning and NOT DEPLOYED: both syslog hosts still ran the older build, so
// the defect was live in production while main carried a consumer expecting the new status. Nothing
// detected that for a day, and nothing could have: this is a stripped static binary, so there is no
// string in it to compare against the tree. A drift check over the sibling SHELL guards works by
// checksum; the one artifact that actually drifted is the one such a check structurally cannot see.
//
// Set at build time:  -ldflags "-X main.buildStamp=$(git rev-parse --short HEAD)"
var buildStamp = "unstamped"

// versionRequested reports whether this invocation is a LOCAL version probe rather than a real read.
//
// THE SSH CONDITION IS THE WHOLE POINT, not a detail. Under a forced command the client's request arrives
// in SSH_ORIGINAL_COMMAND and argv is fixed by authorized_keys — so an SSH caller cannot reach this path.
// But an operator editing authorized_keys COULD write
//
//	command="/usr/local/sbin/tg-syslogng-guard -version"
//
// and if argv alone decided this, every read on that host would print a stamp and exit 0: the guard
// disabled, silently, while reporting success. Requiring SSH_ORIGINAL_COMMAND to be ABSENT makes that
// misconfiguration fall through to the normal guard path, where an empty command is refused, instead of
// becoming an open door. Fail-safe, not fail-open.
func versionRequested(args []string, sshCommandSet bool) bool {
	if sshCommandSet || len(args) < 2 {
		return false
	}
	return args[1] == "-version" || args[1] == "--version"
}

func main() {
	base := envOr("TG_SYSLOGNG_GUARD_BASE", defaultBase)
	cmd, sshCommandSet := os.LookupEnv("SSH_ORIGINAL_COMMAND")

	if versionRequested(os.Args, sshCommandSet) {
		fmt.Println(buildStamp)
		os.Exit(0)
	}

	argv, err := parseQuoted(cmd)
	if err != nil {
		deny(cmd, err.Error())
	}
	if err := validate(argv, base); err != nil {
		if errors.Is(err, errNoSuchLog) {
			// NOT a refusal: the request was legal and the file is absent. Report it distinctly, and say
			// nothing about WHICH path — the caller already knows what it asked for, and an attacker does
			// not learn anything it could not learn by asking.
			fmt.Fprintln(os.Stderr, "tg-syslogng-guard: no such log file")
			os.Exit(noSuchLogExit)
		}
		deny(cmd, err.Error())
	}
	bin, err := lookPath(argv[0])
	if err != nil {
		deny(cmd, err.Error())
	}
	// No shell. argv is passed as a vector, so nothing in it can be re-interpreted.
	_ = syscall.Exec(bin, argv, os.Environ())
	deny(cmd, "exec failed")
}

func deny(cmd, reason string) {
	fmt.Fprintf(os.Stderr, "tg-syslogng-guard: refused — %s\n", reason)
	// The refused command is logged to stderr (which sshd carries to the client) but NOT to syslog: this
	// guard runs ON the syslog host, and writing a refused pattern into the log it protects would let a
	// caller inject chosen text into the corpus TG later reads.
	_ = cmd
	os.Exit(refusedExit)
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

// parseQuoted decodes the wire format TG's syslogng.RemoteCommand produces: every argv element wrapped in
// single quotes, with an embedded quote escaped as the classic '\” sequence.
//
// It is deliberately STRICT. Anything outside that grammar — an unquoted word, an unterminated quote, a
// stray character between elements — is refused rather than interpreted generously. A lenient parser here
// would be a second, subtly different implementation of shell quoting, and the gap between two such
// implementations is where a bypass lives.
func parseQuoted(s string) ([]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, fmt.Errorf("no command (interactive sessions are not permitted)")
	}
	var out []string
	i := 0
	for i < len(s) {
		for i < len(s) && s[i] == ' ' {
			i++
		}
		if i >= len(s) {
			break
		}
		if s[i] != '\'' {
			return nil, fmt.Errorf("argument %d is not single-quoted", len(out)+1)
		}
		i++ // opening quote
		var b strings.Builder
		closed := false
		for i < len(s) {
			if s[i] != '\'' {
				b.WriteByte(s[i])
				i++
				continue
			}
			// A quote: either the classic '\'' escape, or the end of this element.
			if strings.HasPrefix(s[i:], `'\''`) {
				b.WriteByte('\'')
				i += 4
				continue
			}
			i++ // closing quote
			closed = true
			break
		}
		if !closed {
			return nil, fmt.Errorf("unterminated quote in argument %d", len(out)+1)
		}
		// After a closing quote the only legal next byte is a space (or end of string). Anything else means
		// adjacent quoting like 'a'b — valid shell, NOT something TG emits, so refuse it.
		if i < len(s) && s[i] != ' ' {
			return nil, fmt.Errorf("argument %d has trailing junk after its closing quote", len(out)+1)
		}
		out = append(out, b.String())
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty command")
	}
	return out, nil
}

// validate admits exactly the two read shapes and nothing else.
func validate(a []string, base string) error {
	switch a[0] {
	case "tail":
		// tail -n <digits> -- <path>
		if len(a) != 5 || a[1] != "-n" || a[3] != "--" {
			return fmt.Errorf("tail must be exactly: tail -n <lines> -- <path>")
		}
		if err := checkNum(a[2], maxTailLines); err != nil {
			return fmt.Errorf("tail -n: %w", err)
		}
		return checkPath(a[4], base)
	case "grep":
		// grep -F -m <digits> -- <pattern> <path>
		if len(a) != 7 || a[1] != "-F" || a[2] != "-m" || a[4] != "--" {
			return fmt.Errorf("grep must be exactly: grep -F -m <hits> -- <pattern> <path>")
		}
		if err := checkNum(a[3], maxGrepHits); err != nil {
			return fmt.Errorf("grep -m: %w", err)
		}
		// a[5] is the PATTERN. It is deliberately not inspected: -F makes it a fixed string, it sits after
		// `--` so it cannot be read as a flag, and there is no shell for it to be a token in. Validating it
		// would add a parser without adding a guarantee.
		return checkPath(a[6], base)
	}
	return fmt.Errorf("only tail and grep are permitted, got %q", a[0])
}

func checkNum(s string, max int) error {
	n, err := strconv.Atoi(s)
	if err != nil || s != strconv.Itoa(n) {
		return fmt.Errorf("%q is not a plain decimal number", s)
	}
	if n < 1 || n > max {
		return fmt.Errorf("%d out of range 1..%d", n, max)
	}
	return nil
}

// checkPath refuses anything that resolves outside base, and distinguishes "inside base but absent" from
// "refused" (TG-363).
//
// ORDER IS THE SECURITY PROPERTY. Containment is checked LEXICALLY FIRST, before anything is said about
// existence — otherwise this becomes an oracle for whether arbitrary paths exist on the syslog host: a
// caller could probe /etc/shadow and learn the answer from the exit code. A path that does not clean into
// the base is refused with no existence information at all.
//
// Only once a path is lexically contained does absence get its own answer. And containment is then RE-checked
// after symlink resolution, because a symlink inside the log tree pointing at /etc/shadow passes every
// textual check ever written — that defence is unchanged.
func checkPath(p, base string) error {
	if p == "" {
		return fmt.Errorf("empty path")
	}
	if !filepath.IsAbs(p) {
		return fmt.Errorf("path %q is not absolute", p)
	}
	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return fmt.Errorf("log base %q is not readable: %v", base, err)
	}
	// (1) LEXICAL containment, on the cleaned strings. No filesystem question is asked yet.
	if !contained(realBase, filepath.Clean(p)) {
		return fmt.Errorf("path %q resolves outside %s", p, base)
	}
	// (2) Now — and only now — existence may be reported.
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: %s", errNoSuchLog, filepath.Base(p))
		}
		return fmt.Errorf("path %q does not resolve", p)
	}
	// (3) And containment again, on the RESOLVED path: a symlink inside the tree may point anywhere.
	if !contained(realBase, real) {
		return fmt.Errorf("path %q resolves outside %s", p, base)
	}
	return nil
}

// contained reports whether p sits at or under base. Both must already be cleaned/resolved by the caller.
func contained(base, p string) bool {
	rel, err := filepath.Rel(base, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// lookPath resolves the binary from a FIXED list of directories rather than $PATH: PATH is caller-influenced
// in some sshd configurations, and resolving `grep` to something a caller planted would defeat the guard.
func lookPath(name string) (string, error) {
	for _, d := range []string{"/usr/bin", "/bin", "/usr/local/bin"} {
		c := filepath.Join(d, name)
		if st, err := os.Stat(c); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return c, nil
		}
	}
	return "", fmt.Errorf("%q not found in the fixed binary path", name)
}
