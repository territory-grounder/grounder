import { endpoints, type PendingDecision } from "../../api/generated-client";
import { a11yButton, a11yLabel } from "../../a11y/baseline";

// The kinds of governed action an authorized approver may take on a pending decision. These map to the
// server-side action verbs; the console NEVER decides the outcome — it forwards the caller's intent to the
// control-plane, which enforces the band/verdict (spec/010).
export type DecisionActionKind = "approve" | "veto" | "handoff";

const ACTION_KINDS: readonly DecisionActionKind[] = ["approve", "veto", "handoff"];

export interface DecisionCardProps {
  decision: PendingDecision;
  // canAct is fully server-derived (console-level canShowMutatingControl AND this decision's
  // caller_can_act). The card NEVER computes authorization itself — it renders mutating controls only when
  // the console passes this true. In read-only mode or for a non-authorized caller this is false.
  canAct: boolean;
  onAction?: (decisionId: string, kind: DecisionActionKind) => void;
}

// decision-card renders one PendingDecision read-only: the proposed plan (its TWO-OR-MORE approaches), the
// prediction, and the reversibility signal. The reversibility/band meaning is carried by text + aria-label,
// never colour alone (spec/010 a11y). Approve/veto/handoff controls appear only when canAct is true.
export function DecisionCard({ decision, canAct, onAction }: DecisionCardProps) {
  // The approve/veto/handoff intent is forwarded to the served vote route (POST /v1/vote); the decision id
  // rides the request body (external_ref), not the URL. Repointed off the 404 /v1/decisions/{id}/action.
  const endpoint = endpoints.vote;
  const approaches = decision.plan.approaches;

  return (
    <article aria-label={`Pending decision ${decision.decision_id}`} data-band={decision.band}>
      <header>
        <span {...a11yLabel(`Action ${decision.action_id}`)}>{decision.action_id}</span>
        <span {...a11yLabel(`Band ${decision.band}`)}>{decision.band}</span>
      </header>

      {/* Proposed plan — the server's TWO-OR-MORE approaches, shown in order; the console never ranks them. */}
      <section aria-label={`Proposed plan for ${decision.decision_id}`}>
        <h3>Proposed plan</h3>
        <ol>
          {approaches.map((approach, i) => (
            // list semantics + the visible approach text carry the meaning; no live region needed
            <li key={i}>{approach}</li>
          ))}
        </ol>
      </section>

      {/* Prediction — the server's outcome forecast for this plan. */}
      <p>{decision.prediction}</p>

      {/* Reversibility signal — text label, not colour alone. */}
      <p>{decision.reversible ? "Reversible" : "Irreversible"}</p>

      {/* Mutating controls render ONLY when the server authorizes this caller on this decision AND mutation
          is operational (canAct, computed by the console from canShowMutatingControl). Each control is wired
          to the generated-client endpoint for THIS decision — the console never builds a URL by hand. */}
      {canAct ? (
        <div role="group" aria-label={`Decision actions ${decision.decision_id}`}>
          {ACTION_KINDS.map((kind) => (
            <button
              key={kind}
              type="button"
              {...a11yButton(`${kind} decision ${decision.decision_id}`, false)}
              data-endpoint={endpoint}
              data-action={kind}
              onClick={() => onAction?.(decision.decision_id, kind)}
            >
              {kind}
            </button>
          ))}
        </div>
      ) : null}
    </article>
  );
}
