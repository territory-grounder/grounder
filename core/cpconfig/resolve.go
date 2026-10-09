package cpconfig

import "context"

// Value is a resolved configuration knob: the registry Key plus the value that won and where it came from.
type Value struct {
	Key
	Value  string
	Source Source
}

// ConsoleStore returns operator overrides written through the console (Phase B). Phase A wires nil — no
// overrides exist — so the resolver falls straight through to env/default. An override is ever honored ONLY
// for a non-LAW, console-writable key (enforced in Resolve, not trusted from the store).
type ConsoleStore interface {
	Overrides(ctx context.Context) (map[string]string, error)
}

// Resolver layers the configuration sources and clamps LAW. Inputs are injected so the surface is
// oracle-testable with plain maps (CI has no live gate/env):
//   - Law:     the compiled LAW values, by key name (authoritative; never overridable)
//   - Env:     the boot env values for non-law keys, by key name
//   - Console: operator overrides (nil in Phase A; only honored for console-writable non-law keys)
type Resolver struct {
	Law     map[string]string
	Env     map[string]string
	Console ConsoleStore
}

// Resolve returns every registered knob's resolved value + source, in registry order. Precedence for a
// non-LAW key: console override (only if console-writable) → env → compiled default. A LAW key ALWAYS
// resolves to its compiled Law value with Source=law — no env or console entry can reach it.
func (r Resolver) Resolve(ctx context.Context) ([]Value, error) {
	var overrides map[string]string
	if r.Console != nil {
		o, err := r.Console.Overrides(ctx)
		if err != nil {
			return nil, err
		}
		overrides = o
	}
	reg := Registry()
	out := make([]Value, 0, len(reg))
	for _, k := range reg {
		v := Value{Key: k, Source: SourceDefault}
		switch {
		case k.Law:
			// LAW is pinned: it resolves to its compiled value regardless of any env/console entry.
			v.Value, v.Source = r.Law[k.Name], SourceLaw
		default:
			if k.ConsoleWritable && overrides != nil {
				if ov, ok := overrides[k.Name]; ok {
					v.Value, v.Source = ov, SourceConsole
					out = append(out, v)
					continue
				}
			}
			if ev, ok := r.Env[k.Name]; ok && ev != "" {
				v.Value, v.Source = ev, SourceEnv
			}
			// else: leaves the compiled default (empty) with Source=default
		}
		out = append(out, v)
	}
	return out, nil
}
