package main

// ORACLES FOR THE SYSLOG-NG FORCED COMMAND (TG-305).
//
// THE ASYMMETRY THIS REMOVES. hostdiag's key is pinned to 12 byte-exact commands by tg-readonly-guard. The
// syslog-ng key could not be, because `search-host-logs` sends a FREE-TEXT grep pattern — so until this
// existed that key carried `restrict` (no pty, no forwarding) but no `command=`, i.e. arbitrary commands as
// root on both syslog hosts. Two lanes that look alike had very different blast radii, which is exactly the
// kind of undocumented asymmetry that gets discovered during an incident.
//
// The parser is the security boundary here, so most of these tests are about REFUSING generously-shaped
// input rather than about the happy path.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// quote renders argv the way TG's syslogng.RemoteCommand does, so the fixtures are the real wire format.
func quote(argv ...string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(q, " ")
}

func logTree(t *testing.T) (base, file string) {
	t.Helper()
	base = t.TempDir()
	sub := filepath.Join(base, "dc1pve01")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	file = filepath.Join(sub, "messages.log")
	if err := os.WriteFile(file, []byte("line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return base, file
}

func mustParse(t *testing.T, s string) []string {
	t.Helper()
	a, err := parseQuoted(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return a
}

// The happy path, in the exact shapes the two tools emit. If these break, the lane goes blind — and this
// lane reports blindness as "host unreachable", which is how TG-300 hid for weeks.
func TestTheTwoRealReadShapesAreAdmitted(t *testing.T) {
	base, file := logTree(t)
	for _, argv := range [][]string{
		{"tail", "-n", "500", "--", file},
		{"grep", "-F", "-m", "200", "--", "Failed password", file},
	} {
		if err := validate(mustParse(t, quote(argv...)), base); err != nil {
			t.Errorf("refused a command TG actually sends %v: %v\n"+
				"Installed on the syslog hosts this makes the log-read lane silently blind.", argv, err)
		}
	}
}

// THE POINT OF THE WHOLE SHIM: a pattern full of shell metacharacters must be admitted unchanged, because
// there is no shell for it to be a token in. If this fails, the guard has become a pattern filter and the
// agent can no longer search for the things logs actually contain.
func TestAPatternFullOfMetacharactersIsAdmittedUntouched(t *testing.T) {
	base, file := logTree(t)
	for _, pat := range []string{
		`$(id)`, "`id`", `; rm -rf /`, `a|b`, `x>y`, `it's`, `--version`, `$PATH`, `\n`, `*`,
	} {
		argv := mustParse(t, quote("grep", "-F", "-m", "10", "--", pat, file))
		if err := validate(argv, base); err != nil {
			t.Errorf("refused pattern %q: %v — the agent cannot search real log content", pat, err)
		}
		if argv[5] != pat {
			t.Errorf("pattern %q round-tripped as %q — the parser is mangling the one field that must "+
				"survive byte-for-byte", pat, argv[5])
		}
	}
}

// KILLING MUTATION: drop the `a[0]` switch, or add a third case. RED — the guard's entire value is that a
// leaked key runs these two reads and nothing else.
func TestEverythingOtherThanTailAndGrepIsRefused(t *testing.T) {
	base, file := logTree(t)
	for _, argv := range [][]string{
		{"id"}, {"sh"}, {"bash", "-c", "id"}, {"cat", file}, {"tail", "-f", file},
		{"systemctl", "restart", "nginx"}, {"/bin/sh"}, {"env"},
	} {
		if err := validate(mustParse(t, quote(argv...)), base); err == nil {
			t.Errorf("ADMITTED %v — a leaked syslog key could run this as root on the log host", argv)
		}
	}
}

// KILLING MUTATION: drop checkPath, or compare textually instead of resolving symlinks. RED on the symlink
// case — a link inside the log tree pointing at /etc/shadow passes every textual check ever written.
func TestAPathOutsideTheLogTreeIsRefusedIncludingViaSymlink(t *testing.T) {
	base, file := logTree(t)
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	for name, p := range map[string]string{
		"absolute outside":    outside,
		"traversal":           filepath.Join(base, "..", filepath.Base(outside)),
		"symlink out of tree": link,
		"relative":            "messages.log",
	} {
		if err := validate(mustParse(t, quote("tail", "-n", "10", "--", p)), base); err == nil {
			t.Errorf("%s: ADMITTED %q — the key can read files outside the log tree", name, p)
		}
	}
	// The control: the real file must still work, or the check is just breaking the lane.
	if err := validate(mustParse(t, quote("tail", "-n", "10", "--", file)), base); err != nil {
		t.Fatalf("refused the genuine log file: %v", err)
	}
}

// Numeric bounds: a leaked key must not be able to pull an unbounded slice of the log in one call.
func TestNumericArgumentsMustBePlainAndBounded(t *testing.T) {
	base, file := logTree(t)
	for _, n := range []string{"0", "-5", "999999", "1e3", "10; id", "0x10", " 10", "+10", ""} {
		if err := validate(mustParse(t, quote("tail", "-n", n, "--", file)), base); err == nil {
			t.Errorf("ADMITTED -n %q", n)
		}
	}
	if err := validate(mustParse(t, quote("tail", "-n", "2000", "--", file)), base); err != nil {
		t.Errorf("refused the documented maximum: %v", err)
	}
}

// The parser is the boundary. It must be STRICT: a lenient parser is a second implementation of shell
// quoting, and the gap between two implementations is where a bypass lives.
//
// KILLING MUTATION: accept unquoted words, or stop checking for trailing junk after a closing quote. RED.
func TestTheParserRefusesAnythingOutsideTgsWireFormat(t *testing.T) {
	for _, bad := range []string{
		``, `   `, `tail -n 10`, `'tail' -n '10'`, `'tail`, `'tail' '-n`,
		`'tail''-n'`, `'a'b`, `"tail" "-n"`, `'tail' '-n' '10' '--' '/x'extra`,
	} {
		if a, err := parseQuoted(bad); err == nil {
			t.Errorf("parser ACCEPTED %q as %v — anything it accepts, it hands to validate(), and a "+
				"generous parser is how a bypass gets in", bad, a)
		}
	}
}

// An embedded quote must survive, because log patterns contain apostrophes and TG escapes them as '\”.
func TestAnEscapedQuoteRoundTrips(t *testing.T) {
	base, file := logTree(t)
	const pat = `it's a "quoted" thing`
	argv := mustParse(t, quote("grep", "-F", "-m", "5", "--", pat, file))
	if argv[5] != pat {
		t.Fatalf("pattern round-tripped as %q, want %q", argv[5], pat)
	}
	if err := validate(argv, base); err != nil {
		t.Fatalf("refused: %v", err)
	}
}

// VACUITY FLOOR: if quote() and the parser ever stop agreeing, every test above passes by exercising a
// format nothing sends. This pins them to each other on the real shapes.
func TestTheFixtureEncodingMatchesWhatTheParserExpects(t *testing.T) {
	argv := []string{"grep", "-F", "-m", "10", "--", `a'b c`, "/mnt/logs/syslog-ng/h/x.log"}
	got := mustParse(t, quote(argv...))
	if len(got) != len(argv) {
		t.Fatalf("round-trip produced %d element(s), want %d — the fixtures no longer resemble the wire "+
			"format, so every assertion in this file is testing a shape TG never sends", len(got), len(argv))
	}
	for i := range argv {
		if got[i] != argv[i] {
			t.Fatalf("element %d round-tripped as %q, want %q", i, got[i], argv[i])
		}
	}
}
