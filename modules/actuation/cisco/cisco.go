// Package cisco is the read-only interactive-SSH transport for Cisco IOS/ASA devices (TG-85). IOS/ASA
// expose no shell and no argv-exec channel — a command runs inside an interactive PTY session with a
// device prompt, so TG's argv-only native-ssh leaf (modules/actuation/ssh) cannot drive them. This package
// is that missing transport: a crypto/ssh PTY send/expect engine, mirroring the ssh leaf's fail-closed dial
// (host-key verified via core/sshhost, credential a config.SecretRef resolved in memory, a ctx watchdog
// bounding the whole exchange) but replacing the one-shot `sess.Run` with a prompt-driven send/expect loop.
//
// SLICE 1 IS READ-ONLY BY CONSTRUCTION (ADR-0012: high-risk network CLIs are re-author-only and floored at
// never-auto; a Cisco surface is permitted only vendor-official + READ-ONLY behind the interceptor). The
// Module's ReadOnly() is unconditionally true, it holds NO mutation gate and NO op-class registry, and Exec
// STRUCTURALLY refuses any command that is not a `show`/diagnostic read: the model can express nothing but a
// read through this leaf, so REQ-811's "the connector SHALL NOT expose an interactive shell" holds even
// though the wire is a PTY. Enable-mode, `configure terminal`/write paths, jump-host chaining, and per-target
// routing are DEFERRED to later slices — each raises the risk floor this slice deliberately caps at read-only.
//
// This slice ships DARK: the package compiles and is fully unit-tested (against an in-process fake device),
// but nothing in cmd/worker wires it, so no incident can reach it. Wiring + the selftest probe + the plane
// credential keys land with the arm-live slice, gated on a real device and the never-auto floor.
package cisco

import (
	"context"
	"fmt"
	"strings"

	"github.com/territory-grounder/grounder/adapters/actuation"
)

// Capability is the declared capability slug this transport provides.
const Capability = "cisco"

// Runner is the interactive-SSH send/expect seam. Exec builds the CLI command line and hands it here; the
// production implementation (interactiveRunner) opens the PTY, disables paging, runs the read, and captures
// the output. A recording fake in the tests exercises the Module's read-only enforcement without a device.
type Runner interface {
	// RunShow opens a session to the device, disables paging, sends the ONE read-only command line, captures
	// its output up to the next prompt, and returns it. It never sends a config/write command — the Module
	// has already refused anything but a read, and the runner sends exactly what it is given.
	RunShow(ctx context.Context, commandLine string) (actuation.Result, error)
}

// Module is the read-only Cisco actuator. It implements adapters/actuation.Actuator with an unconditional
// ReadOnly()==true (no mutation gate exists), and Exec admits only read-only `show`/diagnostic commands.
type Module struct {
	run Runner
	// cat (optional, TG-85 component 3) TIGHTENS admission from "any verb-guarded show" to "a catalogued
	// diagnostic". Nil ⇒ the slice-1 verb guard alone (behaviour-preserving). A wiring slice installs one
	// per device dialect; the credential floor below applies either way.
	cat *Catalog
}

// New builds the read-only Cisco transport over a Runner.
func New(run Runner) *Module { return &Module{run: run} }

// WithCatalog returns a Module that admits ONLY catalogued diagnostics. It is additive: the verb guard and the
// credential floor still run first, so a catalog can narrow what is admitted and can never widen it.
func (m *Module) WithCatalog(c *Catalog) *Module {
	cp := *m
	cp.cat = c
	return &cp
}

// Capability reports the transport's capability slug.
func (m *Module) Capability() string { return Capability }

// ReadOnly is UNCONDITIONALLY true for this slice (ADR-0012 never-auto read-only floor). There is no code
// path that constructs a mutating Cisco module; a future write lane would be a distinct, separately-gated
// type, never a flag flip on this one.
func (m *Module) ReadOnly() bool { return true }

// Exec runs one read-only Cisco command. argv is the command tokens (e.g. ["show", "access-list"]); stdin is
// ignored (a show has no stdin). It STRUCTURALLY refuses any non-read command before the runner is reached —
// the structural half of REQ-811 (no interactive shell reachable through this leaf) and ADR-0012 (read-only).
func (m *Module) Exec(ctx context.Context, argv []string, _ []byte) (actuation.Result, error) {
	if m.run == nil {
		return actuation.Result{}, fmt.Errorf("cisco: no runner wired (fail closed)")
	}
	if len(argv) == 0 {
		return actuation.Result{}, actuation.ErrEmptyArgv
	}
	if err := guardReadOnly(argv); err != nil {
		return actuation.Result{}, err
	}
	// THE CREDENTIAL FLOOR (TG-85 component 3), always on. `show` is a family, not a capability:
	// running-config/startup-config carry IPsec pre-shared keys, SNMP communities and password hashes, and
	// `show crypto ikev1 pre-shared-key` prints them outright. Reading a secret is not a mutation, so the
	// mutating-verb guard above does not refuse them — this does, catalog or no catalog.
	if why := RefuseCredentialBearing(argv); why != "" {
		return actuation.Result{}, fmt.Errorf("cisco: %s", why)
	}
	if m.cat != nil {
		if err := m.cat.Admits(argv); err != nil {
			return actuation.Result{}, err
		}
	}
	return m.run.RunShow(ctx, strings.Join(argv, " "))
}

var _ actuation.Actuator = (*Module)(nil)

// readOnlyVerbs is the CLOSED set of first tokens this transport admits — all read-only diagnostics on IOS
// and ASA. `show` covers the whole diagnostic surface the Cisco runbooks enumerate (show access-list, show
// nat, show xlate, show running-config …); ping/traceroute are read-only reachability diagnostics;
// packet-tracer is ASA's read-only path simulator (it changes nothing). Nothing here mutates config or state.
var readOnlyVerbs = map[string]bool{
	"show":          true,
	"ping":          true,
	"traceroute":    true,
	"packet-tracer": true,
}

// forbiddenTokens are mutating/mode-changing words that must NEVER appear ANYWHERE in a command line, even as
// an argument — defense in depth behind the verb allowlist, so a crafted `show` argument can never smuggle a
// write. Any of these fails the command closed. (`no` toggles config off; `clear` resets counters/sessions;
// `write`/`copy`/`reload`/`erase` persist or disrupt; `configure`/`conf`/`enable`/`disable` change mode;
// `debug`/`undebug` load the device; `terminal` is the paging control the RUNNER sends itself, never the model.)
var forbiddenTokens = map[string]bool{
	"configure": true, "conf": true, "enable": true, "disable": true,
	"write": true, "copy": true, "reload": true, "erase": true, "delete": true,
	"clear": true, "no": true, "debug": true, "undebug": true, "terminal": true,
	"boot": true, "format": true, "setup": true, "tclsh": true, "test": true,
	// The pipe-to-write family: `show <x> | redirect flash:file` / `| redirect tftp://host` / `| tee` /
	// `| append` WRITES a file or EXFILTRATES the running-config to an attacker-controlled host — a genuine
	// write reached through a `show` verb (Cisco's documented technique). The pipe character is refused by the
	// separator scan below; these verbs are the belt behind that suspenders, so a pipe smuggled some other way
	// still cannot name a write action.
	"redirect": true, "tee": true, "append": true,
}

// guardReadOnly admits only a read-only diagnostic command: the FIRST token must be an allowed read verb, and
// NO token anywhere may be a forbidden mutating/mode word. Case-insensitive. A command that passes is one the
// device cannot interpret as a write or a mode change.
func guardReadOnly(argv []string) error {
	verb := strings.ToLower(strings.TrimSpace(argv[0]))
	if !readOnlyVerbs[verb] {
		return fmt.Errorf("cisco: refusing %q — this read-only transport admits only %v (ADR-0012 never-auto read-only floor)", argv[0], sortedKeys(readOnlyVerbs))
	}
	for _, tok := range argv {
		t := strings.ToLower(strings.TrimSpace(tok))
		if forbiddenTokens[t] {
			return fmt.Errorf("cisco: refusing a command containing the mutating/mode token %q — this transport is read-only (ADR-0012)", tok)
		}
		// A CLI separator could chain a second command or a write past the verb check; refuse any control /
		// separator / redirection character in a token. `|` is the load-bearing one: `show x | redirect
		// flash:file` writes a file and `show run | redirect tftp://host` exfiltrates config — a WRITE through
		// a `show`. `>`/`<` are output/input redirection; ';'/'&' chain; newline injects; backtick/dollar are
		// shell substitution the device never sees but a mis-parse might.
		if strings.ContainsAny(tok, "\n\r;&`$|<>") {
			return fmt.Errorf("cisco: refusing a command with a shell/CLI separator or redirection in token %q — one read, no pipe-to-write (fail closed)", tok)
		}
	}
	return nil
}

// sortedKeys renders a set's keys for a stable error message.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// insertion order is non-deterministic; a tiny sort keeps the refusal message stable for tests/humans.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
