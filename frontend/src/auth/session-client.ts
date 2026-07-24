// The auth gate's API surface: check for an existing operator session, and mint one from a name +
// token. Same mechanism the live console (deploy/console/v2) already uses — GET /v1/whoami (cookie
// session check), POST /v1/session with X-TG-Operator + Authorization: Bearer headers (REQ-508). This
// module does not touch the auth MECHANISM; it only gives the gate a typed client to drive it.
import { endpoints } from "../api/generated-client";

export interface WhoAmI {
  source: string;
  mutation_enabled: boolean;
}

export type SessionCheckResult =
  | { status: "authenticated"; who: WhoAmI }
  | { status: "unauthenticated" }
  | { status: "network-error" };

export type SessionLoginResult =
  | { status: "ok"; who: WhoAmI }
  | { status: "invalid-credential" }
  | { status: "rate-limited" }
  | { status: "network-error" };

// checkSession asks whether the browser already holds a valid session cookie (e.g. after a page
// refresh). It never sends a credential — the cookie is the only thing tested.
export async function checkSession(): Promise<SessionCheckResult> {
  let res: Response;
  try {
    res = await fetch(endpoints.whoami, { credentials: "same-origin" });
  } catch {
    return { status: "network-error" };
  }
  if (res.status === 401) return { status: "unauthenticated" };
  if (!res.ok) return { status: "network-error" };
  const who = (await res.json().catch(() => null)) as WhoAmI | null;
  if (!who) return { status: "network-error" };
  return { status: "authenticated", who };
}

// login submits an operator name + token and, on success, resolves the identity the way checkSession
// would after the fact — the caller reveals the console from ONE verified identity, not two races.
export async function login(operatorName: string, operatorToken: string): Promise<SessionLoginResult> {
  let res: Response;
  try {
    res = await fetch(endpoints.session, {
      method: "POST",
      credentials: "same-origin",
      headers: { "X-TG-Operator": operatorName, Authorization: `Bearer ${operatorToken}` },
    });
  } catch {
    return { status: "network-error" };
  }
  if (res.status === 429) return { status: "rate-limited" };
  if (!res.ok) return { status: "invalid-credential" };
  const check = await checkSession();
  if (check.status !== "authenticated") return { status: "network-error" };
  return { status: "ok", who: check.who };
}
