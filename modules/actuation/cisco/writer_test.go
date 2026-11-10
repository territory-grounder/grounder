package cisco

import (
	"context"
	"testing"

	"github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/safety"
)

// fakeConfigRunner records the lines it was handed so a test can assert the WriteModule vetted them before the
// runner (the device) ever saw a config command.
type fakeConfigRunner struct {
	got   []string
	calls int
}

func (f *fakeConfigRunner) RunConfig(_ context.Context, lines []string) (actuation.Result, error) {
	f.calls++
	f.got = append([]string(nil), lines...)
	return actuation.Result{Stdout: []byte("applied")}, nil
}

func allow() []string { return []string{"interface ", "ip access-list "} }

// TestWriteModuleReadOnlyUnlessArmed: read-only at Shadow, with a nil gate, and with an empty allowlist —
// mutation ships OFF; only a test-only actuating chokepoint + a non-empty allowlist yields a write path.
func TestWriteModuleReadOnlyUnlessArmed(t *testing.T) {
	run := &fakeConfigRunner{}
	if !NewWriteModule(run, safety.NewReadOnlyChokepoint(), allow()).ReadOnly() {
		t.Error("must be read-only at Shadow")
	}
	if !NewWriteModule(run, nil, allow()).ReadOnly() {
		t.Error("must be read-only with no gate")
	}
	if !NewWriteModule(run, safety.NewActuatingChokepoint(), nil).ReadOnly() {
		t.Error("must be read-only with an empty allowlist (no write path)")
	}
	if NewWriteModule(run, safety.NewActuatingChokepoint(), allow()).ReadOnly() {
		t.Error("armed (actuating + allowlist) must NOT be read-only")
	}
}

// TestWriteModuleRefusesAtShadow: even reached directly, Exec refuses while the mode is out — the device never
// sees a config command.
func TestWriteModuleRefusesAtShadow(t *testing.T) {
	run := &fakeConfigRunner{}
	m := NewWriteModule(run, safety.NewReadOnlyChokepoint(), allow())
	if _, err := m.Exec(context.Background(), []string{"interface", "Gi0/1"}, nil); err == nil {
		t.Fatal("must refuse at Shadow")
	}
	if run.calls != 0 {
		t.Fatalf("the runner must not be reached at Shadow (calls=%d)", run.calls)
	}
}

// TestWriteModuleAppliesAllowedTypedLines: armed + allowlisted lines reach the runner exactly as assembled
// (argv is the first line; each stdin line is an additional one).
func TestWriteModuleAppliesAllowedTypedLines(t *testing.T) {
	run := &fakeConfigRunner{}
	m := NewWriteModule(run, safety.NewActuatingChokepoint(), allow())
	_, err := m.Exec(context.Background(), []string{"interface", "Gi0/1"}, []byte("ip access-list extended FOO\n\n"))
	if err != nil {
		t.Fatalf("armed allowlisted change: %v", err)
	}
	if run.calls != 1 || len(run.got) != 2 || run.got[0] != "interface Gi0/1" || run.got[1] != "ip access-list extended FOO" {
		t.Fatalf("runner got the wrong vetted lines: %+v", run.got)
	}
}

// TestWriteModuleFailsClosed: off-allowlist prefix, forbidden token (persist/reload/mode-escape/no), separator,
// and empty change are all refused BEFORE the runner.
func TestWriteModuleFailsClosed(t *testing.T) {
	cases := map[string]struct {
		argv  []string
		stdin string
	}{
		"off allowlist":     {[]string{"hostname", "evil"}, ""},
		"persist token":     {[]string{"interface", "Gi0/1"}, "copy running-config startup-config"},
		"reload token":      {[]string{"reload"}, ""},
		"config removal no": {[]string{"no", "interface", "Gi0/1"}, ""},
		"mode escape end":   {[]string{"interface", "Gi0/1"}, "end"},
		"pipe redirect":     {[]string{"interface", "Gi0/1 | redirect flash:x"}, ""},
		"empty change":      {[]string{}, "   "},
		// The REAL threat shape (review finding #2): a forbidden token / separator smuggled AFTER a line that
		// already MATCHES an allowed prefix — the prefix check passes, so these ISOLATE the token/separator
		// scans. Deleting either scan (or un-hardening the tokenizer) sends exactly one of these to the runner.
		"shutdown after prefix":   {[]string{"interface", "Gi0/1", "shutdown"}, ""},                             // token scan
		"persist after prefix":    {[]string{"interface", "Gi0/1"}, "ip access-list extended FOO write memory"}, // token scan
		"separator after prefix":  {[]string{"interface", "Gi0/1", "description", "x", ">", "flash:y"}, ""},     // separator scan only
		"punctuation-hidden verb": {[]string{"interface", "Gi0/1", "no,shutdown"}, ""},                          // hardened tokenizer only
	}
	for name, c := range cases {
		run := &fakeConfigRunner{}
		m := NewWriteModule(run, safety.NewActuatingChokepoint(), allow())
		if _, err := m.Exec(context.Background(), c.argv, []byte(c.stdin)); err == nil {
			t.Errorf("%s: must fail closed", name)
		}
		if run.calls != 0 {
			t.Errorf("%s: runner must not be reached (calls=%d)", name, run.calls)
		}
	}
}

// TestGuardConfigLinesRefusesWithoutAllowlist: an armed actuator with an EMPTY allowlist writes nothing —
// nothing declared, nothing permitted.
func TestGuardConfigLinesRefusesWithoutAllowlist(t *testing.T) {
	if err := guardConfigLines([]string{"interface Gi0/1"}, nil); err == nil {
		t.Fatal("empty allowlist must refuse every line")
	}
}

// TestBlankAllowlistEntryDoesNotWiden (review finding #1): a stray blank prefix (e.g. a trailing comma in an
// operator's CSV → strings.Split yields "") must NOT turn the allowlist into allow-all — strings.HasPrefix(x,
// "") is always true, so an un-normalized blank entry would admit every line. NewWriteModule drops blanks.
func TestBlankAllowlistEntryDoesNotWiden(t *testing.T) {
	run := &fakeConfigRunner{}
	m := NewWriteModule(run, safety.NewActuatingChokepoint(), []string{"interface ", "", "  "})
	if _, err := m.Exec(context.Background(), []string{"hostname", "evil-anything-goes"}, nil); err == nil {
		t.Fatal("a blank allowlist entry must not admit an off-prefix line (allow-all wildcard)")
	}
	if run.calls != 0 {
		t.Fatalf("runner reached despite an off-prefix line (calls=%d)", run.calls)
	}
	// An all-blank allowlist collapses to no genuine write path — ReadOnly stays true even armed.
	if !NewWriteModule(run, safety.NewActuatingChokepoint(), []string{"", "  "}).ReadOnly() {
		t.Error("an all-blank allowlist must leave the actuator read-only (no write path)")
	}
	// And the guard itself skips an empty prefix (belt for a WriteModule built without New).
	if err := guardConfigLines([]string{"hostname evil"}, []string{""}); err == nil {
		t.Fatal("guardConfigLines must not admit a line via an empty prefix")
	}
}

// TestPrefixSpacingIsLoadBearing: an operator prefix's OWN spacing is the word boundary they wrote. "interface "
// must admit `interface Gi0/1` and REFUSE `interfacex ...` — trimming the prefix silently widened every entry
// to a word-stem match, so a device object whose name merely STARTS with an allowed word became writable.
func TestPrefixSpacingIsLoadBearing(t *testing.T) {
	run := &fakeConfigRunner{}
	m := NewWriteModule(run, safety.NewActuatingChokepoint(), []string{"interface "})
	if _, err := m.Exec(context.Background(), []string{"interfacex", "description", "sneaky"}, nil); err == nil {
		t.Fatal(`"interface " must NOT admit an "interfacex ..." line (the trailing space is the boundary)`)
	}
	if run.calls != 0 {
		t.Fatalf("a word-stem match reached the device (calls=%d)", run.calls)
	}
	if _, err := m.Exec(context.Background(), []string{"interface", "Gi0/1"}, nil); err != nil {
		t.Fatalf(`"interface " must still admit "interface Gi0/1": %v`, err)
	}
}
