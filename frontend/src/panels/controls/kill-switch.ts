// kill-switch display logic (spec/010, T-010-7).
//
// A kill-switch reflects the DISABLED state ONLY after the policy API confirms the write — it is NEVER
// shown disabled optimistically while a write is still in flight. The console mirrors server-confirmed
// state and fails closed: if the operator sees "disabled" they must be able to trust that the control
// plane actually applied the kill. A pending (unconfirmed) write is therefore rendered as "pending", not
// "disabled". This module is pure logic (no JSX) so the transition is unit-testable without a DOM.

export type KillSwitchDisplay = "enabled" | "pending" | "disabled";

// killSwitchDisplayState maps (write-in-flight, api-confirmed) to what the operator should SEE.
//   - apiConfirmed => "disabled"  (the API has acknowledged the write; only now is the kill authoritative)
//   - pendingWrite && !apiConfirmed => "pending"  (in flight — NOT yet disabled)
//   - otherwise => "enabled"
// apiConfirmed wins over pendingWrite: a confirmed write is disabled even if a flag lingers.
export function killSwitchDisplayState(pendingWrite: boolean, apiConfirmed: boolean): KillSwitchDisplay {
  if (apiConfirmed) return "disabled";
  if (pendingWrite) return "pending";
  return "enabled";
}

// isKillSwitchDisabled is true ONLY for the confirmed-disabled state — never for a pending write.
export function isKillSwitchDisabled(display: KillSwitchDisplay): boolean {
  return display === "disabled";
}
