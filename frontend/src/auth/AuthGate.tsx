import { useEffect, useState, type ReactNode } from "react";
import { checkSession, login } from "./session-client";
import { LoginScreen, type GateStatus } from "./LoginScreen";

// AuthGate wraps the whole app. Fail-closed rendering: `children` (the console — every panel, every
// data fetch, the demo/sample fixtures) is not returned by this component at all until a session is
// verified. There is no branch that mounts the console speculatively and covers it with a modal — the
// console literally does not exist in the tree pre-auth, so it cannot fetch, and its data cannot be
// visible behind anything (T-010 security fix: the previous console rendered live behind the login).
export function AuthGate({ children }: { children: ReactNode }) {
  const [status, setStatus] = useState<GateStatus>("checking");
  const [authenticated, setAuthenticated] = useState(false);
  const [errorMessage, setErrorMessage] = useState("");

  useEffect(() => {
    let cancelled = false;
    checkSession().then((result) => {
      if (cancelled) return;
      if (result.status === "authenticated") setAuthenticated(true);
      else setStatus("idle");
    });
    return () => {
      cancelled = true;
    };
  }, []);

  async function handleSubmit(operatorName: string, operatorToken: string) {
    setStatus("authenticating");
    setErrorMessage("");
    const result = await login(operatorName, operatorToken);
    if (result.status === "ok") {
      setAuthenticated(true);
      return;
    }
    setStatus("error");
    if (result.status === "invalid-credential") {
      setErrorMessage("unauthenticated — check the operator name and token");
    } else if (result.status === "rate-limited") {
      setErrorMessage("rate limited — wait a minute and retry");
    } else {
      setErrorMessage("network error — retry");
    }
  }

  if (authenticated) return <>{children}</>;

  return <LoginScreen status={status} errorMessage={errorMessage} onSubmit={handleSubmit} />;
}
