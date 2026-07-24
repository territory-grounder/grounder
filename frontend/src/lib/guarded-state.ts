// GuardedState wraps a panel's async server data so that BOTH an authorization error and a transport error
// resolve to the RESTRICTIVE state — no data and no mutating control. The browser never fabricates data or
// a control on error; it fails closed. A mutating control renders ONLY when the panel is ready AND the
// server says mutation is operational AND the caller is authorized — the console reflects the server's
// authority, it never enforces a safety decision (band, verdict, floor, grant) in browser code (spec/010).
export type GuardedState<T> =
  | { status: "loading" }
  | { status: "ready"; data: T; canMutate: boolean }
  | { status: "restricted"; reason: "auth" | "transport" | "read-only" };

export function isAuthError(err: unknown): boolean {
  const s = (err as { status?: number } | null)?.status;
  return s === 401 || s === 403;
}

export function guardedReady<T>(data: T, mutationOperational: boolean, authorized: boolean): GuardedState<T> {
  return { status: "ready", data, canMutate: mutationOperational && authorized };
}

export function guardedFromError<T>(err: unknown): GuardedState<T> {
  return { status: "restricted", reason: isAuthError(err) ? "auth" : "transport" };
}

export function canShowMutatingControl<T>(state: GuardedState<T>): boolean {
  return state.status === "ready" && state.canMutate;
}
