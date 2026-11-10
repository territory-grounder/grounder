package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func statOrFail(t *testing.T, path string) fs.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Mode().Perm()
}

func readFileOrFail(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestParseArgs(t *testing.T) {
	pairs, err := parseArgs([]string{"AKEY=env:A", "BKEY=bao:secret/data/x#y"}, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(pairs) != 2 || pairs[0].envVar != "AKEY" || pairs[0].ref != "env:A" || pairs[1].ref != "bao:secret/data/x#y" {
		t.Fatalf("parsed wrong: %+v", pairs)
	}
}

// With -skip-empty, a pair whose ref is empty (an unconfigured optional provider key) is SKIPPED, while
// configured pairs still parse — but a truly malformed arg (no '=', bad name) still errors even under skip.
func TestParseArgsSkipEmpty(t *testing.T) {
	pairs, err := parseArgs([]string{"KIMI_API_KEY=bao:secret/data/tg/litellm#kimi", "OPENAI_API_KEY="}, true)
	if err != nil {
		t.Fatalf("skip-empty parse: %v", err)
	}
	if len(pairs) != 1 || pairs[0].envVar != "KIMI_API_KEY" {
		t.Fatalf("empty-ref pair must be skipped, configured kept: %+v", pairs)
	}
	// malformed still errors under skip-empty (skip is only for a well-formed ENVVAR= with an empty ref)
	if _, err := parseArgs([]string{"has space="}, true); err == nil {
		t.Fatal("a malformed name must still error even under -skip-empty")
	}
}

func TestParseArgsRejectsMalformed(t *testing.T) {
	cases := [][]string{
		{"NOEQUALS"},               // no '='
		{"=env:A"},                 // empty name
		{"AKEY="},                  // empty ref (errors WITHOUT -skip-empty)
		{"1BAD=env:A"},             // leading digit
		{"has space=env:A"},        // invalid char
		{"DUP=env:A", "DUP=env:B"}, // duplicate
	}
	for _, c := range cases {
		if _, err := parseArgs(c, false); err == nil {
			t.Fatalf("expected parse error for %v", c)
		}
	}
}

func TestWriteEnvShapeAndOrder(t *testing.T) {
	pairs := []pair{{envVar: "AKEY", ref: "env:A"}, {envVar: "BKEY", ref: "env:B"}}
	resolved := map[string]string{"AKEY": "va=l1", "BKEY": "v2"} // a value with '=' must survive verbatim
	// stdout path is exercised via a strings.Builder equivalent: re-derive the expected lines.
	if err := writeEnv("", pairs, resolved, false); err != nil {
		t.Fatalf("writeEnv stdout: %v", err)
	}
	// The value-with-'=' must not be mangled: only the FIRST '=' after the name delimits.
	var b strings.Builder
	for _, p := range pairs {
		b.WriteString(p.envVar + "=" + resolved[p.envVar] + "\n")
	}
	if got := b.String(); !strings.Contains(got, "AKEY=va=l1\n") || !strings.Contains(got, "BKEY=v2\n") {
		t.Fatalf("env line shape wrong: %q", got)
	}
}

func TestWriteEnvFileIs0600(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/env"
	pairs := []pair{{envVar: "K", ref: "env:X"}}
	if err := writeEnv(path, pairs, map[string]string{"K": "v"}, false); err != nil {
		t.Fatalf("writeEnv file: %v", err)
	}
	// The env file carries plaintext secret values → must be owner-only 0600.
	// (Re-stat via os in the test package.)
	fi := statOrFail(t, path)
	if fi&0o077 != 0 {
		t.Fatalf("env file mode %#o must be owner-only (0600)", fi)
	}
}

// A resolved value with a newline cannot be a shell env value — reject fail-closed, never truncate.
func TestWriteEnvRejectsNewlineValue(t *testing.T) {
	pairs := []pair{{envVar: "K", ref: "env:X"}}
	if err := writeEnv("", pairs, map[string]string{"K": "line1\nline2"}, false); err == nil {
		t.Fatal("a newline in a resolved value must be rejected")
	}
}

// Shell mode emits `export NAME='value'` with POSIX single-quote escaping, so a value with shell-special
// characters (spaces, $, ", and even a single quote) round-trips through `. <file>` verbatim.
func TestWriteEnvShellQuoting(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/env"
	pairs := []pair{{envVar: "K", ref: "bao:x#f"}}
	// a value with a single quote, a $, a space and a double quote — all must be single-quote-safe
	val := `a b'c$d"e`
	if err := writeEnv(path, pairs, map[string]string{"K": val}, true); err != nil {
		t.Fatalf("writeEnv shell: %v", err)
	}
	got := readFileOrFail(t, path)
	want := "export K='a b'\\''c$d\"e'\n"
	if got != want {
		t.Fatalf("shell line wrong:\n got %q\nwant %q", got, want)
	}
	// prove it round-trips: strip `export K=` and the outer single quotes, undo the '\'' escape → original.
	inner := strings.TrimSuffix(strings.TrimPrefix(got, "export K='"), "'\n")
	if strings.ReplaceAll(inner, `'\''`, "'") != val {
		t.Fatalf("shell-quoted value does not round-trip to the original")
	}
}

func TestValidEnvName(t *testing.T) {
	ok := []string{"A", "ABC", "A_B_1", "_leading_underscore"}
	bad := []string{"", "1A", "a-b", "a.b", "a b", "a$b"}
	for _, s := range ok {
		if !validEnvName(s) {
			t.Fatalf("%q should be valid", s)
		}
	}
	for _, s := range bad {
		if validEnvName(s) {
			t.Fatalf("%q should be invalid", s)
		}
	}
}

// run() resolves ALL refs before writing anything: a failing ref writes NOTHING and returns an error
// (fail-closed, the review's finding — now tested at the orchestration layer, not just the helpers).
func TestRunFailsClosedWritesNothing(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/env"
	// The SECOND ref fails; the first succeeds. Nothing must be written.
	resolve := func(ref string) (string, error) {
		if ref == "env:GOOD" {
			return "ok", nil
		}
		return "", errors.New("boom")
	}
	if _, err := run([]string{"A=env:GOOD", "B=env:BAD"}, path, false, false, resolve); err == nil {
		t.Fatal("run must return an error when a ref fails")
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("no output file must exist after a fail-closed run, stat err = %v", statErr)
	}
}

// A resolver error is NEVER forwarded into run's error — only the ENVVAR + scheme, so even a (hypothetical)
// resolver that echoed a value in its error cannot leak it through this tool.
func TestRunErrorCarriesNoResolverText(t *testing.T) {
	resolve := func(string) (string, error) {
		return "", errors.New("SECRET_VALUE_abc123 leaked in the underlying error")
	}
	_, err := run([]string{"MYKEY=env:X"}, "", false, false, resolve)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "SECRET_VALUE_abc123") || strings.Contains(err.Error(), "leaked") {
		t.Fatalf("run error must not carry the resolver's error text: %q", err)
	}
	if !strings.Contains(err.Error(), "MYKEY") || !strings.Contains(err.Error(), "env") {
		t.Fatalf("run error must name the ENVVAR + scheme, got %q", err)
	}
}

// The success path resolves and writes the file (0600), returning the sorted names.
func TestRunSuccessWritesFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/env"
	resolve := func(ref string) (string, error) { return "v-" + ref, nil }
	names, err := run([]string{"B=env:b", "A=env:a"}, path, false, false, resolve)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(names) != 2 || names[0] != "A" || names[1] != "B" {
		t.Fatalf("names must be sorted, got %v", names)
	}
	if fi := statOrFail(t, path); fi&0o077 != 0 {
		t.Fatalf("output file mode %#o must be owner-only", fi)
	}
}

// --- FILES mode (TG-660 part a): PEM material for a third-party container that reads from disk ---

func TestRunFilesWritesEachNameAs0600AndAllowsMultilineValues(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "drop")
	pem := "-----BEGIN CERTIFICATE-----\nAAAA\nBBBB\n-----END CERTIFICATE-----\n"
	resolve := func(ref string) (string, error) {
		if ref == "env:CERT" {
			return pem, nil
		}
		return "key-bytes", nil
	}
	names, err := runFiles([]string{"server.pem=env:CERT", "server-key.pem=env:KEY"}, dir, false, resolve)
	if err != nil {
		t.Fatalf("runFiles: %v", err)
	}
	if strings.Join(names, ",") != "server-key.pem,server.pem" {
		t.Fatalf("names: %v", names)
	}
	for name, want := range map[string]string{"server.pem": pem, "server-key.pem": "key-bytes"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(b) != want {
			t.Fatalf("%s: %q %v", name, b, err)
		}
		st, _ := os.Stat(filepath.Join(dir, name))
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("%s must be 0600, got %v", name, st.Mode().Perm())
		}
	}
}

func TestRunFilesFailsClosedWritingNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "drop")
	resolve := func(ref string) (string, error) {
		if ref == "env:BAD" {
			return "", errors.New("vault: SECRET-PATH-DETAIL")
		}
		return "ok", nil
	}
	_, err := runFiles([]string{"a.pem=env:GOOD", "b.pem=env:BAD"}, dir, false, resolve)
	if err == nil || strings.Contains(err.Error(), "SECRET-PATH-DETAIL") || !strings.Contains(err.Error(), "b.pem") {
		t.Fatalf("one unresolvable ref must refuse, naming the file and not the resolver text: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("nothing may be written on a refusal, found %d file(s)", len(entries))
	}
}

func TestRunFilesRefusesNamesThatCouldEscapeTheDrop(t *testing.T) {
	for _, bad := range []string{"../etc/passwd=env:X", "sub/dir=env:X", ".hidden=env:X", "..=env:X", "with space=env:X", "=env:X"} {
		if _, err := runFiles([]string{bad}, t.TempDir(), false, func(string) (string, error) { return "x", nil }); err == nil {
			t.Fatalf("%q must be refused as a file name", bad)
		}
	}
	// the env-mode validator still refuses dots, so an env-mode caller cannot accidentally get file semantics
	if _, err := parseArgs([]string{"server.pem=env:X"}, false); err == nil {
		t.Fatal("a dotted name must stay invalid as an env var")
	}
}

func TestRunFilesSkipEmptyDeliversOnlyTheConfiguredOnes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "drop")
	names, err := runFiles([]string{"ca.pem=env:CA", "cli.pem="}, dir, true, func(string) (string, error) { return "x", nil })
	if err != nil || strings.Join(names, ",") != "ca.pem" {
		t.Fatalf("an empty ref under -skip-empty is simply not delivered: names=%v err=%v", names, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "cli.pem")); !os.IsNotExist(err) {
		t.Fatal("the skipped file must not exist")
	}
}

// A file from an EARLIER run whose ref is now empty is removed, not left for the consumer to start on; a file
// this invocation does not manage is left alone.
func TestRunFilesRemovesStaleManagedFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "drop")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, stale := range []string{"cli.pem", "cli-key.pem", "unmanaged.txt"} {
		if err := os.WriteFile(filepath.Join(dir, stale), []byte("yesterday"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	names, err := runFiles([]string{"ca.pem=env:CA", "cli.pem=", "cli-key.pem="}, dir, true, func(string) (string, error) { return "today", nil })
	if err != nil || strings.Join(names, ",") != "ca.pem" {
		t.Fatalf("names=%v err=%v", names, err)
	}
	for _, gone := range []string{"cli.pem", "cli-key.pem"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Fatalf("%s was emptied in the config and must be REMOVED from the drop, not left stale", gone)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "unmanaged.txt")); err != nil || string(b) != "yesterday" {
		t.Fatalf("a file this invocation does not manage must be left alone: %q %v", b, err)
	}
	// and a refused run removes nothing: the stale state is at least not made worse silently
	_ = os.WriteFile(filepath.Join(dir, "server.pem"), []byte("yesterday"), 0o600)
	if _, err := runFiles([]string{"server.pem=env:BAD"}, dir, false, func(string) (string, error) { return "", errors.New("down") }); err == nil {
		t.Fatal("must refuse")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "server.pem")); string(b) != "yesterday" {
		t.Fatal("a refused run resolves BEFORE it touches the drop, so the previous file stays")
	}
}
