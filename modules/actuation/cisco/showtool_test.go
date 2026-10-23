package cisco

// Drills for the cisco-show agent tool (TG-85 read-tool slice). Each refusal direction is its own arm;
// the fake runner records the exact command line sent, so the fixed-argv property is asserted on the
// WIRE shape, not on intent.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/adapters/actuation"
)

type showFakeRunner struct {
	got  string
	out  string
	fail error
}

func (f *showFakeRunner) RunShow(_ context.Context, line string) (actuation.Result, error) {
	f.got = line
	if f.fail != nil {
		return actuation.Result{}, f.fail
	}
	return actuation.Result{Stdout: []byte(f.out)}, nil
}

func TestShowToolRunsACataloguedCommandExactly(t *testing.T) {
	fr := &showFakeRunner{out: "Interface GigabitEthernet0/0 up"}
	tool, err := NewShowTool([]ReadDevice{{ID: "fw01", Dev: Device{Host: "192.0.2.1", Identity: "tg"}, Platform: PlatformASA}},
		time.Second, func(Device) showRunner { return fr })
	if err != nil {
		t.Fatal(err)
	}
	cat, _ := NewCatalog(DefaultCatalog(), PlatformASA)
	name := cat.Names()[0]
	entry, _ := cat.Lookup(name)
	args := map[string]string{"device": "fw01", "command": name}
	if len(entry.Params) > 0 {
		args["arg"] = "inside"
	}
	res, err := tool.Invoke(context.Background(), args)
	if err != nil || !res.Success {
		t.Fatalf("catalogued command must run: err=%v res=%+v", err, res)
	}
	want := strings.Join(entry.Argv, " ")
	if !strings.HasPrefix(fr.got, want) {
		t.Fatalf("the wire line must start with the entry's EXACT argv: got %q want prefix %q", fr.got, want)
	}
	if !strings.Contains(res.Output, "read-only, catalogued") {
		t.Fatalf("output must name the discipline, got %q", res.Output)
	}
}

func TestShowToolRefusesEveryImprovisation(t *testing.T) {
	fr := &showFakeRunner{out: "x"}
	tool, err := NewShowTool([]ReadDevice{{ID: "fw01", Dev: Device{Host: "192.0.2.1", Identity: "tg"}, Platform: PlatformASA}},
		time.Second, func(Device) showRunner { return fr })
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		args map[string]string
		want string
	}{
		{"unknown device", map[string]string{"device": "core-rtr", "command": "whatever"}, "unknown device"},
		{"uncatalogued command", map[string]string{"device": "fw01", "command": "show-running-config-all"}, "not in fw01's asa catalog"},
		{"metacharacter arg", map[string]string{"device": "fw01", "command": mustParamEntry(t).Name, "arg": "eth0; reload"}, "refused"},
		{"help-trigger arg", map[string]string{"device": "fw01", "command": mustParamEntry(t).Name, "arg": "eth0?"}, "refused"},
		{"control-byte arg", map[string]string{"device": "fw01", "command": mustParamEntry(t).Name, "arg": "eth0\x08\x08rm"}, "refused"},
		{"oversize arg", map[string]string{"device": "fw01", "command": mustParamEntry(t).Name, "arg": strings.Repeat("a", 80)}, "refused"},
		{"arg on a no-param entry", map[string]string{"device": "fw01", "command": mustNoParamEntry(t).Name, "arg": "x"}, "takes no argument"},
		{"missing required arg", map[string]string{"device": "fw01", "command": mustParamEntry(t).Name}, "needs its"},
	}
	for _, c := range cases {
		fr.got = ""
		res, err := tool.Invoke(context.Background(), c.args)
		if err != nil {
			t.Fatalf("%s: refusals are results, never errors: %v", c.name, err)
		}
		if res.Success || !strings.Contains(res.Output, c.want) {
			t.Fatalf("%s: want refusal containing %q, got %+v", c.name, c.want, res)
		}
		if fr.got != "" {
			t.Fatalf("%s: NOTHING may reach the wire on a refusal; runner saw %q", c.name, fr.got)
		}
	}
}

func TestShowToolZeroDevicesRefusesConstruction(t *testing.T) {
	if _, err := NewShowTool(nil, time.Second, nil); err == nil {
		t.Fatal("zero devices must refuse construction — an empty-enum tool is a trap, not a no-op")
	}
}

func TestShowToolDeviceFailureIsAnObservationNotACrash(t *testing.T) {
	fr := &showFakeRunner{fail: context.DeadlineExceeded}
	tool, err := NewShowTool([]ReadDevice{{ID: "fw01", Dev: Device{Host: "192.0.2.1", Identity: "tg"}, Platform: PlatformASA}},
		time.Second, func(Device) showRunner { return fr })
	if err != nil {
		t.Fatal(err)
	}
	entry := mustParamEntry(t)
	res, err := tool.Invoke(context.Background(), map[string]string{"device": "fw01", "command": entry.Name, "arg": "inside"})
	if err != nil {
		t.Fatalf("a dead device is a finding, not a tool error: %v", err)
	}
	if res.Success || !strings.Contains(res.Output, "failed") {
		t.Fatalf("the failure must be reported as an unsuccessful read, got %+v", res)
	}
}

// mustParamEntry returns an ASA entry that takes a parameter; mustNoParamEntry one that takes none —
// pulled from the REAL catalog so the drill tracks its content instead of inventing fixtures.
func mustParamEntry(t *testing.T) ShowCommand {
	t.Helper()
	cat, _ := NewCatalog(DefaultCatalog(), PlatformASA)
	for _, n := range cat.Names() {
		if e, _ := cat.Lookup(n); len(e.Params) > 0 {
			return e
		}
	}
	t.Fatal("the ASA catalog has no parameterized entry — the drill needs one")
	return ShowCommand{}
}

func mustNoParamEntry(t *testing.T) ShowCommand {
	t.Helper()
	cat, _ := NewCatalog(DefaultCatalog(), PlatformASA)
	for _, n := range cat.Names() {
		if e, _ := cat.Lookup(n); len(e.Params) == 0 {
			return e
		}
	}
	t.Fatal("the ASA catalog has no zero-param entry — the drill needs one")
	return ShowCommand{}
}
