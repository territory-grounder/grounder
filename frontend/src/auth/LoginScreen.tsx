import { useEffect, useRef, useState, type FormEvent } from "react";
import "./auth-gate.css";

export type GateStatus = "checking" | "idle" | "authenticating" | "error";

export interface LoginScreenProps {
  status: GateStatus;
  errorMessage: string;
  onSubmit: (operatorName: string, operatorToken: string) => void;
}

// The full-screen, fail-closed sign-in — the ONLY thing this app ever renders while unauthenticated
// (see AuthGate). Themed after territorygrounder.com: dark instrument ground, the steel/indigo/teal
// epistemic palette, IBM Plex type, and the manifesto's own voice ("the agent is not allowed to act on
// a belief it has not checked"). The claim → verify → verdict ribbon is the product's own Grounding
// Ribbon signature, recast for the one claim this screen exists to check: an operator's identity.
export function LoginScreen({ status, errorMessage, onSubmit }: LoginScreenProps) {
  const [name, setName] = useState("");
  const [token, setToken] = useState("");
  const nameRef = useRef<HTMLInputElement>(null);
  const wasBusy = useRef(true);

  const busy = status === "checking" || status === "authenticating";
  const gate = busy ? "verifying" : status === "error" ? "error" : "idle";

  // Move focus to the operator-name field the moment the form becomes usable — the initial session
  // check resolving, or a failed attempt resolving back to an editable state.
  useEffect(() => {
    if (wasBusy.current && !busy) nameRef.current?.focus();
    wasBusy.current = busy;
  }, [busy]);

  function handleSubmit(e: FormEvent) {
    e.preventDefault();
    const trimmedName = name.trim();
    if (!trimmedName || !token) return;
    onSubmit(trimmedName, token);
  }

  return (
    <div className="tg-authgate" data-gate={gate}>
      <main className="ag-card" aria-label="Operator sign-in">
        <div className="ag-brand">
          <span className="ag-mark" aria-hidden="true">
            TG
          </span>
          <div className="ag-word">
            <b>Territory&nbsp;Grounder</b>
            <span>Operator Console</span>
          </div>
        </div>

        <p className="ag-eyebrow">Governed-autonomy SRE platform</p>
        <h1 className="ag-thesis">The agent is not allowed to act on a belief it has not checked.</h1>
        <p className="ag-desc">Sign in with your operator credential to reach the control plane.</p>

        <div className="ag-spine" aria-hidden="true">
          <div className="ag-stage">
            <span className="ag-node claim" />
            <span className="ag-wire claim-verify" />
          </div>
          <div className="ag-stage">
            <span className="ag-node verify" />
            <span className="ag-wire verify-verdict" />
          </div>
          <div className="ag-stage">
            <span className="ag-node verdict" />
          </div>
        </div>
        <div className="ag-caps" aria-hidden="true">
          <span>Claim</span>
          <span>Verify</span>
          <span>Verdict</span>
        </div>

        <form className="ag-form" onSubmit={handleSubmit} aria-label="Operator credentials">
          <label className="ag-field">
            <span className="ag-field-label">Operator name</span>
            <input
              ref={nameRef}
              name="username"
              autoComplete="username"
              autoCapitalize="none"
              autoCorrect="off"
              spellCheck={false}
              required
              disabled={busy}
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          </label>
          <label className="ag-field">
            <span className="ag-field-label">Operator token</span>
            <input
              name="password"
              type="password"
              autoComplete="current-password"
              required
              disabled={busy}
              value={token}
              onChange={(e) => setToken(e.target.value)}
            />
          </label>
          {/* Errors explain what's wrong, no apology (spec/010 voice) — a live region so a screen
              reader announces a failed attempt without moving focus off the field being corrected. */}
          <div className="ag-error" role="alert" aria-live="assertive">
            {errorMessage}
          </div>
          <button className="ag-submit" type="submit" disabled={busy}>
            {status === "authenticating" ? "Signing in…" : "Sign in"}
          </button>
        </form>

        <p className="ag-foot">Territory Grounder · the grounded instrument</p>
      </main>
    </div>
  );
}
