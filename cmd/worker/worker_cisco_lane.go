package main

// TG-85 arm-live routing slice (audit item 4): the Cisco write lane becomes CONSTRUCTIBLE — the last
// unreachable half of the write stack. Slice 4 built the operator surface (worker_cisco_write.go) and
// deliberately constructed nothing; this file is the construction + the regime binding it was waiting for:
//
//   - Each armed device policy becomes ONE per-device effect leaf: the config-mode PTY runner over the
//     device profile, wrapped by the WriteModule (prefix allowlist + forbidden tokens + the mode chokepoint)
//     and, when the operator declared reversible_ops, the ReversibleRegistry (named changes with declared
//     undos; the four registration rules and the never-auto floor clamp are enforced at REGISTRY
//     construction, so an op the floor forbids refuses the BOOT, not the rollback).
//   - The leaves hang off the cisco-interactive regime lane through the SAME per-target seam the fleet SSH
//     lane uses (REQ-1717): the action's target resolves to ITS declared device or refuses. Every leaf
//     carries ActuationHost() so the interceptor's host-match gate stays defense-in-depth.
//
// DORMANT BY DEFAULT, in layers, none of them this file's discretion: unconfigured or unarmed the lane
// carries the fail-closed pending leaf; armed, a leaf exists but nothing routes to it until an operator
// regime RULE selects cisco-interactive for a target; routed, the interceptor's chain still governs and the
// mode chokepoint refuses at Shadow; and the never-auto floor holds the op-classes regardless. Arming is a
// Phase-4, owner-present act — this slice only makes the key ABLE to arrive (the 2026-08-23 reachability
// rule: built + tested + boot-logged is not delivered until then).

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/regime"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/modules/actuation/cisco"
)

// ciscoHostBoundLeaf binds a constructed WriteModule to its device's host so the interceptor's host-match
// gate (REQ-1219) can refuse a target mismatch as defense-in-depth beneath the lane's own resolution.
type ciscoHostBoundLeaf struct {
	actuation.Actuator
	host string
}

func (l ciscoHostBoundLeaf) ActuationHost() string { return l.host }

// buildCiscoWriteLeaves constructs the per-device effect leaves from the parsed, armed policy set. Fail
// closed on every direction: a device whose reversible-op set violates a registration rule (a missing
// inverse, a floor-listed op_class, forward==inverse) refuses CONSTRUCTION — the wire site turns that into
// a fatal boot, because an operator who declared an op the floor forbids must learn it from the boot log,
// not from a rollback that cannot run. Returned keys are the canonical (lowercased, trimmed) device host
// AND device_id, so a regime-routed action may target either.
func buildCiscoWriteLeaves(policies map[string]ciscoWritePolicy, chokepoint *safety.Chokepoint) (map[string]actuation.Actuator, error) {
	leaves := make(map[string]actuation.Actuator)
	claim := func(key string, leaf actuation.Actuator, id string) error {
		k := strings.ToLower(strings.TrimSpace(key))
		if k == "" {
			return nil
		}
		if _, dup := leaves[k]; dup {
			return fmt.Errorf("cisco write lane: %q resolves to more than one declared device (second: %s) — an ambiguous target cannot be routed", k, id)
		}
		leaves[k] = leaf
		return nil
	}
	for id, pol := range policies {
		if len(pol.Allowed) == 0 && len(pol.Ops) == 0 {
			// A declared device with no write path at all: nothing to construct (declaring a device is not
			// the same as permitting a change on it). The preflight already reports it.
			continue
		}
		dev := cisco.Device{
			Host:         pol.Device.Host,
			Port:         pol.Device.Port,
			Identity:     pol.Device.Identity,
			KeyRef:       pol.Device.KeyRef,
			KnownHosts:   pol.Device.KnownHosts,
			LegacyCrypto: pol.Device.LegacyCrypto,
			PagerOffCmd:  pol.Device.PagerOffCmd,
			Prompt:       pol.Device.Prompt,
		}
		wm := cisco.NewWriteModule(cisco.NewConfigRunner(dev), chokepoint, pol.Allowed)
		if len(pol.Ops) > 0 {
			var plat cisco.Platform
			switch pol.PlatformName { // parse validated the vocabulary; the zero case is a wiring bug
			case "asa":
				plat = cisco.PlatformASA
			case "ios":
				plat = cisco.PlatformIOS
			default:
				return nil, fmt.Errorf("cisco write lane: device %s declares reversible_ops with platform %q — parse should have refused this", id, pol.PlatformName)
			}
			ops := make([]cisco.ReversibleOp, 0, len(pol.Ops))
			for _, o := range pol.Ops {
				ops = append(ops, cisco.ReversibleOp{
					Name:     o.Name,
					OpClass:  o.OpClass,
					Platform: plat,
					Forward:  append([]string(nil), o.Forward...),
					Inverse:  append([]string(nil), o.Inverse...),
					Why:      o.Why,
				})
			}
			reg, err := cisco.NewReversibleRegistry(ops, plat)
			if err != nil {
				return nil, fmt.Errorf("cisco write lane: device %s reversible-op registry refused: %w", id, err)
			}
			// The registry is CONSTRUCTED and validated here, but the named-op path (WriteModule.ExecOp)
			// is not yet reachable from the interceptor — adapters/actuation.Actuator exposes only Exec,
			// and nothing routes an op NAME today. Deliberate: the ExecOp route lands with the
			// commit-confirmed integration; until then an armed ops-only device has a validated registry
			// and a refusing free-form path (empty prefix allowlist ⇒ Exec admits nothing).
			wm = wm.WithReversibleOps(reg)
		}
		leaf := ciscoHostBoundLeaf{Actuator: wm, host: pol.Device.Host}
		if err := claim(pol.Device.Host, leaf, id); err != nil {
			return nil, err
		}
		if err := claim(id, leaf, id); err != nil {
			return nil, err
		}
	}
	return leaves, nil
}

// ciscoInteractiveLaneFor builds the cisco-interactive regime lane for this boot. Unconfigured, unarmed, or
// armed-but-unwritable it carries the fail-closed pending leaf (regime.ErrLaneNotWired on any reach); armed
// it resolves each action's target to ITS declared device's leaf through the per-target seam, refusing an
// undeclared target with the declared set named.
func ciscoInteractiveLaneFor(policies map[string]ciscoWritePolicy, chokepoint *safety.Chokepoint, armEnabled bool) (regime.Lane, error) {
	if len(policies) == 0 || !armEnabled || !ciscoWriteHasWritablePolicy(policies) {
		return regime.NewCiscoInteractiveLane(), nil
	}
	leaves, err := buildCiscoWriteLeaves(policies, chokepoint)
	if err != nil {
		return nil, err
	}
	declared := make([]string, 0, len(leaves))
	for k := range leaves {
		declared = append(declared, k)
	}
	sort.Strings(declared)
	return regime.NewCiscoInteractiveLane(regime.WithCiscoInteractiveActuatorFunc(
		func(_ context.Context, target string) (actuation.Actuator, error) {
			return ciscoResolveLeaf(leaves, declared, target)
		})), nil
}

// ciscoResolveLeaf is the lane's per-target resolution, EXTRACTED as a named function so an oracle can
// drive the shipped logic directly — the worker_ssh_lane.go precedent: a closure buried inside the lane is
// unreachable from any test, and a copy in a test proves nothing about what ships (the oracle-that-cannot-
// fail trap, caught live twice this week).
func ciscoResolveLeaf(leaves map[string]actuation.Actuator, declared []string, target string) (actuation.Actuator, error) {
	t := strings.ToLower(strings.TrimSpace(target))
	if t == "" {
		return nil, fmt.Errorf("cisco write lane: empty target — refusing to resolve an effect leaf")
	}
	leaf, ok := leaves[t]
	if !ok {
		return nil, fmt.Errorf("cisco write lane: %q is not a declared write device — declared: %s (an undeclared target is a governed refusal, never a fallback)", target, strings.Join(declared, ", "))
	}
	return leaf, nil
}
