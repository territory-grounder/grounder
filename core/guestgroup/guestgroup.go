// Package guestgroup declares EXCLUSIVE guest groups (TG-654): sets of hypervisor guests that must never run
// at the same time, because together they overload the node they share.
//
// WHY. On a RAM-tight node an operator runs a swap: a default-ON guest is shut down to free memory, a big
// operator-managed guest runs for a while, then the big one is shut down and the default comes back. To a
// liveness detector every half of that dance reads "a guest went down": TG would heal the default back up
// while the big guest still runs (both ON, an OOM), and would heal the big guest back up after its
// deliberate shutdown. Measured 2026-11-07 12:08 NL: TG auto-approved restarting the default 35 s after the
// swap shut it down, stopped only by an unrelated API error.
//
// A group is one DEFAULT member (runs unless a partner does) and one or more PARTNERS (default OFF, started and
// stopped only by the operator). The valid states are: default ON with every partner OFF, or default OFF with
// exactly one partner ON. TG's part is in the detector (modules/ingest/pveliveness) and the start-guest
// actuator (modules/actuation/proxmox); this package only holds the declaration they share.
//
// The declaration is configuration, never code: estate guest names do not belong in the repository.
package guestgroup

import (
	"fmt"
	"slices"
	"strings"
)

// Group is one exclusive group: Default runs unless one of Partners does.
type Group struct {
	Default  string
	Partners []string
}

// Role is a guest's place in a group.
type Role int

const (
	// RoleNone: the guest is in no declared group (the zero value: today's behavior).
	RoleNone Role = iota
	// RoleDefault: the group's default member (ON unless a partner runs).
	RoleDefault
	// RolePartner: an operator-managed member whose default is OFF.
	RolePartner
)

// String renders a group in the declaration syntax ("default>partner|partner").
func (g Group) String() string { return g.Default + ">" + strings.Join(g.Partners, "|") }

// Parse reads a declaration: groups separated by ';', each "default>partner|partner". Whitespace around names
// is ignored and an empty or blank spec yields no groups (nil, nil). It rejects a group with no default or no
// partner, a default listed as its own partner, and a guest named twice anywhere: one guest in two groups would
// make its role ambiguous, so the declaration fails as a whole rather than being half-applied.
func Parse(spec string) ([]Group, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, nil
	}
	seen := map[string]bool{}
	claim := func(name string) error {
		if seen[name] {
			return fmt.Errorf("guestgroup: guest %q is named more than once", name)
		}
		seen[name] = true
		return nil
	}
	var out []Group
	for part := range strings.SplitSeq(spec, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		def, rest, ok := strings.Cut(part, ">")
		def = strings.TrimSpace(def)
		if !ok || def == "" {
			return nil, fmt.Errorf("guestgroup: group %q must read default>partner[|partner…]", part)
		}
		if err := claim(def); err != nil {
			return nil, err
		}
		g := Group{Default: def}
		for p := range strings.SplitSeq(rest, "|") {
			if p = strings.TrimSpace(p); p == "" {
				continue
			}
			if err := claim(p); err != nil {
				return nil, err
			}
			g.Partners = append(g.Partners, p)
		}
		if len(g.Partners) == 0 {
			return nil, fmt.Errorf("guestgroup: group %q names no partner", part)
		}
		out = append(out, g)
	}
	return out, nil
}

// Lookup returns the group a guest belongs to and its role in it. A guest in no group is RoleNone.
func Lookup(groups []Group, name string) (Group, Role) {
	name = strings.TrimSpace(name)
	for _, g := range groups {
		if g.Default == name {
			return g, RoleDefault
		}
		if slices.Contains(g.Partners, name) {
			return g, RolePartner
		}
	}
	return Group{}, RoleNone
}
