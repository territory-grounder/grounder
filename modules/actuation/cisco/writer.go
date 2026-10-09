package cisco

// The WRITE half of the Cisco lane (TG-85), slice 1: the SAFETY STRUCTURE. This is a DISTINCT type from the
// read-only Module — never a flag flip on it (ADR-0012: a Cisco write lane raises the risk floor the read
// slice deliberately caps, so it is a separate, separately-gated actuator). It holds a mode chokepoint
// (ReadOnly() unless the mode permits actuation) and refuses at Shadow; even armed it admits ONLY typed config
// lines whose prefix is on an operator-declared allowlist (config-not-code, the awxjob.TemplateAllowlist /
// gitopsmr.RepoAllowlist analogue) — no free-form config, no mode-escape, no write-to-file, no separator. It
// depends on a ConfigRunner seam so the safety structure is complete and testable now, before the concrete
// config-mode PTY transport (slice 2) and the commit-confirmed rollback mechanic land. It ships DARK: nothing
// in cmd/worker wires it, and it is floored never-auto by the interceptor regardless.

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/safety"
)

// ConfigRunner drives a CONFIG-mode session: enter config, apply the typed config lines in order, exit, and
// return the transcript. Separate from the read-only Runner seam — the production implementation (a config-mode
// PTY runner) is slice 2; a recording fake exercises the WriteModule's gate + guard without a device now.
type ConfigRunner interface {
	RunConfig(ctx context.Context, lines []string) (actuation.Result, error)
}

// WriteModule is the WRITE Cisco actuator. It implements adapters/actuation.Actuator with a mode-gated
// ReadOnly() and a typed, allowlisted Exec. A nil gate, an empty allowlist, or the mode at Shadow all mean
// read-only / refuse — the actuator cannot write by omission.
type WriteModule struct {
	run     ConfigRunner
	gate    *safety.Chokepoint
	allowed []string // operator-declared allowed config-line PREFIXES (config-not-code); empty ⇒ refuse every line
	// ops (optional, TG-85 component 4) is the REVERSIBLE-OP registry: named changes that each carry a
	// declared undo. It is a SECOND, NARROWER admission path — ExecOp runs an op by name, so nothing
	// free-form travels it. Nil ⇒ only the prefix path exists (behaviour-preserving).
	ops *ReversibleRegistry
}

// WithReversibleOps returns a WriteModule that can also execute NAMED reversible ops. It does not widen the
// free-form path: the prefix allowlist and its guards are untouched, and a registered op is admitted by name
// rather than by text.
func (m *WriteModule) WithReversibleOps(r *ReversibleRegistry) *WriteModule {
	cp := *m
	cp.ops = r
	return &cp
}

// ExecOp applies a REGISTERED reversible op by name. It is the only path on which a leading `no` can reach a
// device, and only as lines an operator declared in advance — never as text a model composed.
//
// The mode chokepoint is re-checked here exactly as in Exec: a named op is still a mutation.
func (m *WriteModule) ExecOp(ctx context.Context, name string) (actuation.Result, error) {
	if m.run == nil {
		return actuation.Result{}, fmt.Errorf("cisco write: no config runner wired (fail closed)")
	}
	if !(m.gate != nil && m.gate.MayActuate()) {
		return actuation.Result{}, fmt.Errorf("cisco write: mode chokepoint does not permit actuation — refusing (mutation off)")
	}
	if m.ops == nil {
		return actuation.Result{}, fmt.Errorf("cisco write: no reversible-op registry wired — refusing %q (fail closed)", name)
	}
	op, ok := m.ops.Lookup(name)
	if !ok {
		return actuation.Result{}, fmt.Errorf("cisco write: %q is not a registered reversible op — admitted: %v", name, m.ops.Names())
	}
	return m.run.RunConfig(ctx, append([]string(nil), op.Forward...))
}

// InverseFor returns the declared compensating lines for a registered op — what a rollback sends. Read from
// the registration, never derived from the forward at rollback time.
func (m *WriteModule) InverseFor(name string) ([]string, error) {
	if m.ops == nil {
		return nil, fmt.Errorf("cisco write: no reversible-op registry wired")
	}
	return m.ops.InverseOf(name)
}

// NewWriteModule builds the write actuator. allowed is the operator's closed set of config-line prefixes the
// runbook may emit (e.g. "interface ", "ip access-list ") — the arm-live wiring supplies it from config, never
// a hardcoded set; empty leaves the actuator able only to refuse. The allowlist is FROZEN (copied) and
// NORMALIZED here: each prefix is trimmed and any that normalizes to empty is DROPPED — an empty prefix would
// make strings.HasPrefix match every line (allow-all), so a stray blank entry (a trailing comma in an
// operator's CSV, say) must not silently widen the gate. A list that is all-blank collapses to zero entries,
// and ReadOnly() then correctly reports no write path.
func NewWriteModule(run ConfigRunner, gate *safety.Chokepoint, allowed []string) *WriteModule {
	frozen := make([]string, 0, len(allowed))
	for _, p := range allowed {
		// Drop an all-whitespace entry (it would match every line), but keep the prefix's OWN spacing verbatim:
		// the trailing space in "interface " is load-bearing — it is what stops the prefix from also admitting
		// `interfacex ...`. Trimming it here silently widened every operator prefix to a word-stem match.
		if strings.TrimSpace(p) != "" {
			frozen = append(frozen, p)
		}
	}
	return &WriteModule{run: run, gate: gate, allowed: frozen}
}

// Capability reports the transport's capability slug (shared with the read module — same device plane).
func (m *WriteModule) Capability() string { return Capability }

// ReadOnly is true UNLESS a mode chokepoint is wired AND currently permits actuation AND the actuator has a
// genuine write path (a non-empty allowlist) — mirrors the proxmox/awx leaves. Mutation ships OFF, so this is
// true in every production and test path except one that constructs a test-only actuating chokepoint + allowlist.
func (m *WriteModule) ReadOnly() bool {
	return !(m.gate != nil && m.gate.MayActuate() && len(m.allowed) > 0)
}

// Exec applies a typed config change. argv is the config line's tokens; stdin (optional) carries ADDITIONAL
// typed config lines, one per line. It fails closed at Shadow (the mode gate re-check, defense in depth even if
// reached directly), refuses an empty change, and hands only guard-vetted lines to the runner. It NEVER
// persists (`write mem`/`copy run start`) or reloads — those are forbidden tokens; a change's durability and
// its rollback are the interceptor's + slice 2's concern, not a free-form device write here.
func (m *WriteModule) Exec(ctx context.Context, argv []string, stdin []byte) (actuation.Result, error) {
	if m.run == nil {
		return actuation.Result{}, fmt.Errorf("cisco write: no config runner wired (fail closed)")
	}
	// Mode chokepoint (defense in depth): a write NEVER fires while the mode is out, even reached directly.
	if !(m.gate != nil && m.gate.MayActuate()) {
		return actuation.Result{}, fmt.Errorf("cisco write: mode chokepoint does not permit actuation — refusing (mutation off)")
	}
	lines := assembleConfigLines(argv, stdin)
	if len(lines) == 0 {
		return actuation.Result{}, fmt.Errorf("cisco write: no config lines to apply (refusing an empty change)")
	}
	if err := guardConfigLines(lines, m.allowed); err != nil {
		return actuation.Result{}, err
	}
	return m.run.RunConfig(ctx, lines)
}

var _ actuation.Actuator = (*WriteModule)(nil)

// assembleConfigLines builds the ordered config-line list: argv (joined) is the first line; each non-blank
// line of stdin is an additional line. Blank/whitespace lines are dropped.
func assembleConfigLines(argv []string, stdin []byte) []string {
	var lines []string
	if first := strings.TrimSpace(strings.Join(argv, " ")); first != "" {
		lines = append(lines, first)
	}
	for ln := range strings.SplitSeq(string(stdin), "\n") {
		if s := strings.TrimSpace(ln); s != "" {
			lines = append(lines, s)
		}
	}
	return lines
}

// guardConfigLines admits a config change only if EVERY line starts with an operator-allowed prefix and
// carries no mutating/mode/persist token and no CLI separator. An empty allowlist refuses everything
// (fail-closed: an armed actuator with nothing declared cannot write). The forbidden-token and separator scans
// are the same defense-in-depth the read guard uses — so `write mem`, `copy run start`, `reload`, `no shutdown`
// smuggled past a prefix, or a `| redirect` exfil, are all refused even if a prefix matched.
func guardConfigLines(lines, allowed []string) error {
	if len(allowed) == 0 {
		return fmt.Errorf("cisco write: no config-line allowlist declared — refusing every write (fail closed)")
	}
	for _, line := range lines {
		if !hasAllowedPrefix(line, allowed) {
			return fmt.Errorf("cisco write: config line %q does not start with any operator-allowed prefix — refusing (config-not-code allowlist)", line)
		}
		// Defense in depth, and the REAL threat shape: a forbidden verb smuggled AFTER a matching prefix on
		// the same line (`interface Gi0/1 shutdown`, `ip access-list … | redirect tftp://host`). Tokenize on
		// whitespace AND CLI punctuation so a verb cannot hide behind a comma or paren (`no,shutdown`).
		for _, tok := range configTokens(line) {
			if writeForbiddenTokens[tok] {
				return fmt.Errorf("cisco write: refusing a config line with the forbidden token %q (%q) — no persist/reload/shutdown/mode-escape through this lane", tok, line)
			}
		}
		if strings.ContainsAny(line, "\n\r;&`$|<>") {
			return fmt.Errorf("cisco write: refusing a config line with a CLI separator or redirection: %q (fail closed)", line)
		}
	}
	return nil
}

// hasAllowedPrefix reports whether line starts with one of the operator's allowed prefixes. A prefix is matched
// with its OWN spacing intact — only an all-whitespace prefix is skipped (it would match everything, degrading
// the allowlist to allow-all; belt to NewWriteModule's normalization for a WriteModule built without it). The
// prefix is NOT trimmed: "interface " must not also admit `interfacex ...`, so the operator's trailing space is
// the word boundary they wrote it to be.
func hasAllowedPrefix(line string, allowed []string) bool {
	l := strings.ToLower(strings.TrimSpace(line))
	for _, p := range allowed {
		if strings.TrimSpace(p) == "" {
			continue
		}
		if strings.HasPrefix(l, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

// configTokens splits a config line into lowercase word tokens on whitespace AND CLI punctuation, so a
// forbidden verb cannot evade the exact-match blocklist by hiding behind a comma or paren. The separator scan
// already refuses `;|&<>` etc.; splitting on them here too is belt-and-suspenders for the token check.
func configTokens(line string) []string {
	fields := strings.FieldsFunc(line, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(",;|&()<>", r)
	})
	out := make([]string, len(fields))
	for i, f := range fields {
		out[i] = strings.ToLower(f)
	}
	return out
}

// writeForbiddenTokens are words a typed config line must NEVER contain, even AFTER a matching prefix: they
// persist (write/copy), disrupt (reload/boot/format/erase/delete), take an interface/line down
// (shutdown/shut — `core/safety`'s never-auto floor names interface-shutdown as network-catastrophic), escape
// config mode (end/exit/configure/enable/disable), or exfiltrate (the redirect/tee/append pipe-to-write
// family). `no` is refused too — a config REMOVAL is not an in-place update and routes to a higher-approval
// regime, not this lane. `terminal`/`debug` load or reconfigure the session, never the model. This blocklist
// is defense in depth BEHIND the operator prefix allowlist, not the primary control — a blocklist is
// structurally incomplete, so the load-bearing gate stays the closed operator prefix set + the mode chokepoint.
var writeForbiddenTokens = map[string]bool{
	"write": true, "copy": true, "reload": true, "boot": true, "format": true,
	"erase": true, "delete": true, "no": true, "end": true, "exit": true,
	"configure": true, "conf": true, "enable": true, "disable": true,
	"debug": true, "undebug": true, "terminal": true, "tclsh": true,
	"redirect": true, "tee": true, "append": true,
	"shutdown": true, "shut": true,
}
