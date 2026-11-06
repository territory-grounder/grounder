package faultinjector

import "strings"

// GuestKind is which Proxmox CLI owns a guest: a container (`pct`) or a virtual machine (`qm`). TG-624 G3
// needs device-down on VMs: TG's start-guest heal has a qemu code path that no live run had ever exercised,
// and the injector could only stop containers.
//
// The kind is RESOLVED on the owning node at name-assert time (AssertGuest), never declared in the pool file:
// a declared kind can go stale exactly like a stale vmid, and the name-assert is already the one place that
// reads the guest's own config immediately before acting on it.
type GuestKind string

const (
	// KindLXC is the zero value on purpose: every pool line and every ledger row written before VMs were
	// supported is a container, so an unset kind must keep meaning `pct`.
	KindLXC GuestKind = ""
	// KindQemu is a virtual machine, driven with `qm`.
	KindQemu GuestKind = "qemu"
)

// cli is the Proxmox command that drives this kind of guest. `pct` and `qm` share the verbs this package
// uses (config, stop, start, status), so the kind changes only the binary.
func (k GuestKind) cli() string {
	if k == KindQemu {
		return "qm"
	}
	return "pct"
}

// qemuRefPrefix marks a VM in a device-down fault_ref. The restore of an obligation is driven from the LEDGER
// row alone (the reconciler may run after a restart, long after the deciding process is gone), so the row
// must carry the kind. A container's ref stays the bare vmid, which keeps every existing row readable.
const qemuRefPrefix = "qemu:"

// deviceRef is the fault_ref a device-down records: the bare vmid for a container, "qemu:<vmid>" for a VM.
func deviceRef(k GuestKind, vmid string) string {
	if k == KindQemu {
		return qemuRefPrefix + vmid
	}
	return vmid
}

// parseDeviceRef is deviceRef's inverse. Anything without the VM prefix is a container, so the rows of every
// campaign before TG-624 keep restoring with `pct start`.
func parseDeviceRef(ref string) (GuestKind, string) {
	if vmid, ok := strings.CutPrefix(ref, qemuRefPrefix); ok {
		return KindQemu, vmid
	}
	return KindLXC, ref
}
