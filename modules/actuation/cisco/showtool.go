package cisco

// The AGENT-FACING READ TOOL (TG-85, the slice catalog.go names): `cisco-show` surfaces the CLOSED
// show-command catalog to the model over an operator-declared device set. This is the pack's hands —
// until it, the whole read transport (interactive.go) and the catalog were built and model-invisible.
//
// The tool is the catalog's two controls made callable, and nothing more:
//   - the model picks a DEVICE from the operator-declared closed set and a COMMAND from the catalog's
//     closed name list — both rendered as Enum ParamSpecs, so an improvised device or command is refused
//     by the loop's arg validation before this code even runs, and refused AGAIN here (defense in depth:
//     a prompt-schema is steering, never the enforcement);
//   - the sent argv is the catalog entry's fixed vector plus its declared params only, re-checked through
//     Catalog.Admits (which itself re-checks RefuseCredentialBearing) — INV-02's fixed-argv discipline on
//     an interactive CLI, with the credential-revealing family refused BY NAME even if catalogued.
//
// Read-only BY CONSTRUCTION: the transport is RunShow (one command, capture to prompt, exit) — there is
// no config-mode path reachable from this type. Registration is env-gated at the composition root
// (TG_CISCO_READ_DEVICES unset ⇒ no devices ⇒ the tool never registers ⇒ the model's preamble is
// byte-identical to before this slice — the eval arms set no cisco env, so the change gate's arms stay
// unchanged by construction).

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/agent"
)

// ReadDevice is one operator-declared read-only diagnostic target: the connection profile plus the
// platform whose catalog subset it may run. The credential is a SecretRef resolved in memory at use
// (INV-13) — carried inside Device.KeyRef.
type ReadDevice struct {
	// ID is the stable name the model refers to (the tool's device enum member).
	ID string
	// Dev is the full connection profile (host, identity, key ref, known_hosts, jump, prompt).
	Dev Device
	// Platform selects which catalog entries this device admits (asa / ios / any).
	Platform Platform
}

// argTokenRE is the CLOSED shape of the one free argument: interface names, ACL names, peers, IPs —
// letters/digits and the network-naming punctuation, 1..64 chars. Everything else refuses.
var argTokenRE = regexp.MustCompile("^[A-Za-z0-9._/:-]{1,64}$")

// showRunner is the seam the tool drives — satisfied by *interactiveRunner; a test fake satisfies it
// without a PTY.
type showRunner interface {
	RunShow(ctx context.Context, commandLine string) (actuation.Result, error)
}

// NewShowTool builds the `cisco-show` agent tool over the declared devices. Fail-closed on every
// direction: zero devices refuses construction (the composition root treats that as "do not register",
// but a caller that gets here with none is a bug, not a silent no-op tool); an unknown catalog entry, an
// unknown device, an off-platform command, an undeclared extra param, and a credential-bearing argv are
// each refusals with the closed list named. newRunner is injectable for the drill; nil gets the real PTY
// transport.
func NewShowTool(devices []ReadDevice, timeout time.Duration, newRunner func(Device) showRunner) (agent.Tool, error) {
	if len(devices) == 0 {
		return nil, fmt.Errorf("cisco-show: zero devices declared — refusing to build a tool with an empty device enum")
	}
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	if newRunner == nil {
		newRunner = func(d Device) showRunner { return NewInteractiveRunner(d) }
	}
	byID := map[string]ReadDevice{}
	ids := make([]string, 0, len(devices))
	for _, d := range devices {
		id := strings.TrimSpace(d.ID)
		if id == "" || d.Dev.Host == "" {
			return nil, fmt.Errorf("cisco-show: a device needs a non-empty id and host (got id=%q host=%q)", d.ID, d.Dev.Host)
		}
		if _, dup := byID[id]; dup {
			return nil, fmt.Errorf("cisco-show: duplicate device id %q", id)
		}
		byID[id] = d
		ids = append(ids, id)
	}
	sort.Strings(ids)

	// One catalog per platform present, so an ASA-only command is not even offered against an IOS device.
	cats := map[Platform]*Catalog{}
	for _, d := range devices {
		if _, ok := cats[d.Platform]; ok {
			continue
		}
		c, err := NewCatalog(DefaultCatalog(), d.Platform)
		if err != nil {
			return nil, fmt.Errorf("cisco-show: catalog for platform %q: %w", d.Platform, err)
		}
		cats[d.Platform] = c
	}
	// The command enum is the UNION of the present platforms' names (each call re-checks against its own
	// device's catalog, so a name from the other dialect refuses with the right closed list).
	nameSet := map[string]bool{}
	for _, c := range cats {
		for _, n := range c.Names() {
			nameSet[n] = true
		}
	}
	names := make([]string, 0, len(nameSet))
	for n := range nameSet {
		names = append(names, n)
	}
	sort.Strings(names)

	return &showTool{byID: byID, ids: ids, cats: cats, names: names, timeout: timeout, newRunner: newRunner}, nil
}

type showTool struct {
	byID      map[string]ReadDevice
	ids       []string
	cats      map[Platform]*Catalog
	names     []string
	timeout   time.Duration
	newRunner func(Device) showRunner
}

func (t *showTool) Name() string   { return "cisco-show" }
func (t *showTool) ReadOnly() bool { return true }

func (t *showTool) Description() string {
	return "Run ONE catalogued read-only Cisco diagnostic (a closed show-command set) on a declared " +
		"ASA/IOS device and return its output. No other command can be run through this tool."
}

func (t *showTool) Params() []agent.ParamSpec {
	return []agent.ParamSpec{
		{Name: "device", Type: "string", Required: true, Enum: append([]string(nil), t.ids...),
			Example: t.ids[0], Description: "the declared device to read"},
		{Name: "command", Type: "string", Required: true, Enum: append([]string(nil), t.names...),
			Example: t.names[0], Description: "the catalogued diagnostic to run"},
		{Name: "arg", Type: "string", Required: false,
			Description: "the command's declared free parameter (an interface, an ACL name, a peer) — only for entries that take one; single token"},
	}
}

func (t *showTool) Invoke(ctx context.Context, args map[string]string) (agent.ToolResult, error) {
	dev, ok := t.byID[strings.TrimSpace(args["device"])]
	if !ok {
		return agent.ToolResult{Success: false,
			Output: fmt.Sprintf("unknown device %q — declared: %s", args["device"], strings.Join(t.ids, ", "))}, nil
	}
	cat := t.cats[dev.Platform]
	entry, ok := cat.Lookup(strings.TrimSpace(args["command"]))
	if !ok {
		return agent.ToolResult{Success: false,
			Output: fmt.Sprintf("command %q is not in %s's %s catalog — admitted: %s",
				args["command"], dev.ID, dev.Platform, strings.Join(cat.Names(), ", "))}, nil
	}
	argv := append([]string(nil), entry.Argv...)
	arg := strings.TrimSpace(args["arg"])
	switch {
	case len(entry.Params) == 0 && arg != "":
		return agent.ToolResult{Success: false,
			Output: fmt.Sprintf("%q takes no argument; drop arg", entry.Name)}, nil
	case len(entry.Params) > 0 && arg == "":
		return agent.ToolResult{Success: false,
			Output: fmt.Sprintf("%q needs its %s argument", entry.Name, entry.Params[0])}, nil
	case arg != "":
		// One single validated token, by ALLOWLIST (review 2026-08-25: the blacklist form missed `?` —
		// ASA/IOS context-help — and raw control bytes, which line-edit the device's input so the sent
		// command differs from the validated one). The argv stays a fixed vector plus exactly one
		// operator-subject word (INV-02); an allowlist is auditable where a blacklist only grows.
		if !argTokenRE.MatchString(arg) {
			return agent.ToolResult{Success: false,
				Output: fmt.Sprintf("argument %q refused: a single plain token matching %s is required", arg, argTokenRE.String())}, nil
		}
		argv = append(argv, arg)
	}
	// Defense in depth: the assembled vector back through the catalog's own admission (which re-checks the
	// credential-bearing floor). The enum validation upstream steers; THIS refuses.
	if err := cat.Admits(argv); err != nil {
		return agent.ToolResult{Success: false, Output: err.Error()}, nil
	}

	cctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	res, err := t.newRunner(dev.Dev).RunShow(cctx, strings.Join(argv, " "))
	if err != nil {
		// The error is the observation (a dead device is a diagnostic FINDING on a cisco incident) — the
		// tool reports it as an unsuccessful read, never as a loop-visible tool crash.
		return agent.ToolResult{Success: false,
			Output: fmt.Sprintf("%s on %s failed: %v", entry.Name, dev.ID, err)}, nil
	}
	return agent.ToolResult{Success: true,
		Output: fmt.Sprintf("%s on %s (read-only, catalogued):\n%s", strings.Join(argv, " "), dev.ID, res.Stdout)}, nil
}
