package cisco

// REVERSIBLE-WRITE REGISTRY (TG-85 component 4): a named change that carries its own undo.
//
// The write lane's first admission path is a prefix allowlist over free-form config lines. That is the right
// floor for arbitrary text, and it is deliberately blunt: it refuses `no` and `shutdown` outright, because in
// free-form text those words are indistinguishable from a removal or an outage. The cost is that it also
// refuses the ONE shape that is genuinely safest — a change whose exact undo is declared up front.
//
// This is that shape, and it is NARROWER than the prefix path, not wider. Instead of "any line beginning with
// `interface `", an operator registers a NAMED op: this exact forward, this exact inverse, on this platform,
// under this op-class. The leaf then executes the op by NAME. Nothing free-form is admitted through this path,
// so `no shutdown` becomes expressible only as the declared inverse of a registered op — never as a line the
// model composed.
//
// FOUR RULES MAKE IT A CONTROL RATHER THAN A CONVENIENCE:
//
//  1. NO OP WITHOUT AN UNDO. An entry with no inverse cannot register. The whole point is that the compensating
//     action exists BEFORE the forward runs, not that someone will work one out afterwards.
//  2. THE INVERSE MUST NOT BE THE FORWARD. `start` whose rollback is another `start` is the trap this estate
//     has hit before: it reads as reversible and undoes nothing. Refused at registration.
//  3. THE NEVER-AUTO FLOOR CLAMPS AT THE LEAF. `interface-shutdown`, `no-interface`, `acl-delete`,
//     `route-delete`, `erase-startup-config` and their kin are on core/safety's non-configurable floor. This
//     leaf refuses to register them at all — the same clamp the kubernetes leaf applies to delete/drain, and
//     for the same reason: a leaf that can emit a floor op is one flag away from emitting it automatically.
//  4. PLATFORM IS PART OF THE IDENTITY. IOS says `no shutdown`, ASA says `no shut`. A registry built for one
//     dialect never yields the other's verb, so a wiring cannot send an ASA the wrong undo — which would leave
//     a change applied and un-revertable, the worst outcome of the four.
//
// The SET is deliberately EMPTY by default: which Cisco changes are safe to make on this estate is operator
// policy (config-not-code), not a judgement this package should ship. The mechanism is what was missing.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/territory-grounder/grounder/core/safety"
)

// ReversibleOp is one named change and its declared undo.
type ReversibleOp struct {
	Name     string   // the stable slug an operator config and an audit record refer to
	OpClass  string   // the op-class slug the governance stack knows this change by (floor-checked)
	Platform Platform // IOS and ASA spell the same undo differently; the dialect is part of the identity
	Forward  []string // the config lines that APPLY the change, in order
	Inverse  []string // the config lines that UNDO it, in order — declared before the forward may run
	Why      string   // what the change is for, so a registry review reads as intent
}

// ReversibleRegistry is a validated, indexed set of reversible ops for one platform.
type ReversibleRegistry struct {
	byName map[string]ReversibleOp
	plat   Platform
}

// NewReversibleRegistry validates and indexes an operator-declared op set. It fails closed on every shape that
// would let an irreversible or mis-dialected change look reversible.
func NewReversibleRegistry(ops []ReversibleOp, plat Platform) (*ReversibleRegistry, error) {
	if len(ops) == 0 {
		return nil, fmt.Errorf("cisco reversible: empty op set — a registry that admits nothing is a wiring error (fail closed)")
	}
	byName := make(map[string]ReversibleOp, len(ops))
	for i, op := range ops {
		name := strings.ToLower(strings.TrimSpace(op.Name))
		if name == "" {
			return nil, fmt.Errorf("cisco reversible: op %d has no name", i)
		}
		if _, dup := byName[name]; dup {
			return nil, fmt.Errorf("cisco reversible: duplicate op name %q", name)
		}
		class := strings.ToLower(strings.TrimSpace(op.OpClass))
		if class == "" {
			return nil, fmt.Errorf("cisco reversible: op %q declares no op_class — the governance stack could not classify it", name)
		}
		// RULE 3: the non-configurable floor, clamped at the leaf exactly as the kubernetes leaf clamps
		// delete/drain. A leaf that can emit a floor op is one flag away from emitting it automatically.
		if safety.IsNeverAuto(class) {
			return nil, fmt.Errorf("cisco reversible: op %q has op_class %q, which is on the non-configurable never-auto floor — this leaf refuses to register it at all (INV-09); no flag lifts that", name, class)
		}
		if len(op.Forward) == 0 {
			return nil, fmt.Errorf("cisco reversible: op %q has no forward lines", name)
		}
		// RULE 1: no op without an undo.
		if len(op.Inverse) == 0 {
			return nil, fmt.Errorf("cisco reversible: op %q declares NO inverse — a reversible op's compensating action must exist before the forward runs, not be worked out afterwards", name)
		}
		// RULE 2: the undo must actually undo.
		if sameLines(op.Forward, op.Inverse) {
			return nil, fmt.Errorf("cisco reversible: op %q declares an inverse identical to its forward — that reads as reversible and undoes nothing", name)
		}
		// Both sides must be sendable config: no separators, no persist/reload/mode-escape. The inverse is
		// the ONE sanctioned place a leading `no` may appear, which is exactly what makes it an inverse.
		if err := checkOpLines(name, "forward", op.Forward, false); err != nil {
			return nil, err
		}
		if err := checkOpLines(name, "inverse", op.Inverse, true); err != nil {
			return nil, err
		}
		if op.Platform != PlatformAny && plat != PlatformAny && op.Platform != plat {
			continue // RULE 4: another dialect's op is not this registry's
		}
		byName[name] = op
	}
	if len(byName) == 0 {
		return nil, fmt.Errorf("cisco reversible: no op applies to platform %q — the registry would admit nothing (fail closed)", plat)
	}
	return &ReversibleRegistry{byName: byName, plat: plat}, nil
}

// checkOpLines applies the write lane's line discipline to a registered op. allowLeadingNo permits an inverse
// to begin with `no` (a removal IS the undo of an addition); everything else — persist, reload, mode escape,
// shutdown, separators — is refused on both sides.
func checkOpLines(opName, side string, lines []string, allowLeadingNo bool) error {
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			return fmt.Errorf("cisco reversible: op %q %s line %d is blank", opName, side, i+1)
		}
		if strings.ContainsAny(line, "\n\r;&`$|<>") {
			return fmt.Errorf("cisco reversible: op %q %s line %d carries a CLI separator or redirection: %q", opName, side, i+1, line)
		}
		toks := configTokens(line)
		for j, tok := range toks {
			if allowLeadingNo && j == 0 && tok == "no" {
				continue // the sanctioned inverse form
			}
			if writeForbiddenTokens[tok] {
				return fmt.Errorf("cisco reversible: op %q %s line %d carries the forbidden token %q (%q)", opName, side, i+1, tok, line)
			}
		}
	}
	return nil
}

func sameLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(strings.TrimSpace(a[i]), strings.TrimSpace(b[i])) {
			return false
		}
	}
	return true
}

// Names returns the registered op names, sorted.
func (r *ReversibleRegistry) Names() []string {
	out := make([]string, 0, len(r.byName))
	for n := range r.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Lookup returns a registered op.
func (r *ReversibleRegistry) Lookup(name string) (ReversibleOp, bool) {
	op, ok := r.byName[strings.ToLower(strings.TrimSpace(name))]
	return op, ok
}

// InverseOf returns the declared compensating lines for a registered op — what a rollback would send. It is a
// pure lookup: the undo is read from the registration, never derived from the forward at rollback time, which
// is when deriving it is least likely to be right.
func (r *ReversibleRegistry) InverseOf(name string) ([]string, error) {
	op, ok := r.Lookup(name)
	if !ok {
		return nil, fmt.Errorf("cisco reversible: %q is not a registered op — admitted: %v", name, r.Names())
	}
	return append([]string(nil), op.Inverse...), nil
}
