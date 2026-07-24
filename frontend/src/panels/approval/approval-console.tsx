import type { PendingDecision } from "../../api/generated-client";
import { canShowMutatingControl, type GuardedState } from "../../lib/guarded-state";
import { useMutationMode } from "../../mode/mode-provider";
import { a11yStatus } from "../../a11y/baseline";
import { DecisionCard, type DecisionActionKind } from "./decision-card";

export interface ApprovalConsoleProps {
  // Already-fetched server data, wrapped so an auth OR transport error fails closed to no data + no control.
  // The test controls this via props — the console never fetches on its own.
  state: GuardedState<PendingDecision[]>;
  onAction?: (decisionId: string, kind: DecisionActionKind) => void;
}

// approval-console lists the pending decisions the server returned (T-010-3). Approve/veto/handoff controls
// appear on a decision ONLY when mutation is operational AND the guarded state is server-authorized
// (canShowMutatingControl) AND that decision's own caller_can_act is true. The console reflects the server's
// authority — it never computes a band/verdict/floor/grant or fabricates a control (spec/010).
export function ApprovalConsole({ state, onAction }: ApprovalConsoleProps) {
  const operational = useMutationMode() === "operational";
  // Two independent gates that must agree: the provider's mode (read-only ALWAYS wins) and the guarded
  // state (ready AND server-operational AND authorized). Necessary but not sufficient — each decision ALSO
  // requires its own server-computed caller_can_act below.
  const consoleCanMutate = operational && canShowMutatingControl(state);

  if (state.status === "loading") {
    return (
      <section aria-label="Approval console">
        <p {...a11yStatus("Loading pending decisions")}>Loading pending decisions…</p>
      </section>
    );
  }

  if (state.status === "restricted") {
    // Fail closed: on auth/transport restriction show no decisions and no control.
    return (
      <section aria-label="Approval console">
        <p {...a11yStatus(`Approval console restricted: ${state.reason}`)}>
          No pending decisions available.
        </p>
      </section>
    );
  }

  const decisions = state.data;

  return (
    <section aria-label="Approval console">
      <h2>Pending decisions</h2>
      {decisions.length === 0 ? (
        <p {...a11yStatus("No pending decisions")}>No pending decisions.</p>
      ) : (
        <ul>
          {decisions.map((decision) => (
            <li key={decision.decision_id}>
              <DecisionCard
                decision={decision}
                // Per-decision gate: console-level canMutate AND this decision's server caller_can_act.
                canAct={consoleCanMutate && decision.caller_can_act}
                onAction={onAction}
              />
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
