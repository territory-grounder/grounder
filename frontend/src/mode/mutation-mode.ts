// The console's mutation mode mirrors the control-plane's mutation_enabled flag. It is READ-ONLY by
// default (Phase 0/1): until the server authoritatively reports mutation_enabled === true, the console
// exposes NO mutating control. The console NEVER decides this itself — it only reflects the server's value
// and fails closed on anything unknown (spec/010 REQ-6xx; INV-09 read-only foundation).
export type MutationMode = "read-only" | "operational";

export function modeFromServer(mutationEnabled: boolean | undefined | null): MutationMode {
  return mutationEnabled === true ? "operational" : "read-only";
}

export function isReadOnly(mode: MutationMode): boolean {
  return mode !== "operational";
}
