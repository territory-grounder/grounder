import { describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import { MutationModeProvider } from "../../mode/mode-provider";
import { guardedReady } from "../../lib/guarded-state";
import { endpoints, type PendingDecision } from "../../api/generated-client";
import { ApprovalConsole } from "./approval-console";

function makeDecision(overrides: Partial<PendingDecision> = {}): PendingDecision {
  return {
    decision_id: "dec-1",
    action_id: "act-1",
    band: "POLL_PAUSE",
    // A pending decision always carries TWO-OR-MORE approaches.
    plan: { approaches: ["drain node then cordon", "cordon then drain node"] },
    prediction: "reduces replica pressure by ~30% within 2m",
    reversible: true,
    caller_can_act: true,
    ...overrides,
  };
}

describe("approval console (T-010-3)", () => {
  it("shows the plan (2+ approaches), prediction and reversibility for a pending decision", () => {
    const decision = makeDecision();
    render(
      <MutationModeProvider mutationEnabled={false}>
        <ApprovalConsole state={guardedReady([decision], false, false)} />
      </MutationModeProvider>,
    );

    // The plan's two-or-more approaches are all shown, in order.
    expect(decision.plan.approaches.length).toBeGreaterThanOrEqual(2);
    for (const approach of decision.plan.approaches) {
      expect(screen.getByText(approach)).toBeInTheDocument();
    }

    // The prediction is shown.
    expect(screen.getByText(decision.prediction)).toBeInTheDocument();

    // The reversibility signal is carried by visible text (not colour alone).
    expect(screen.getByText("Reversible")).toBeInTheDocument();
  });

  it("lets an authorized approver in operational mode approve/veto/handoff, wired to the decision endpoint", () => {
    const onAction = vi.fn();
    const decision = makeDecision({ decision_id: "dec-42", caller_can_act: true });
    render(
      <MutationModeProvider mutationEnabled={true}>
        <ApprovalConsole state={guardedReady([decision], true, true)} onAction={onAction} />
      </MutationModeProvider>,
    );

    const approve = screen.getByRole("button", { name: "approve decision dec-42" });
    const veto = screen.getByRole("button", { name: "veto decision dec-42" });
    const handoff = screen.getByRole("button", { name: "handoff decision dec-42" });

    // Each control is wired to the served vote route (the decision id rides the request body).
    const expectedEndpoint = endpoints.vote;
    expect(approve).toHaveAttribute("data-endpoint", expectedEndpoint);
    expect(veto).toHaveAttribute("data-endpoint", expectedEndpoint);
    expect(handoff).toHaveAttribute("data-endpoint", expectedEndpoint);

    // And to a handler carrying the decision id + action kind.
    fireEvent.click(approve);
    expect(onAction).toHaveBeenCalledWith("dec-42", "approve");
    fireEvent.click(veto);
    expect(onAction).toHaveBeenCalledWith("dec-42", "veto");
    fireEvent.click(handoff);
    expect(onAction).toHaveBeenCalledWith("dec-42", "handoff");
  });

  it("shows NO approve/veto/handoff control for a decision the caller cannot act on", () => {
    const decision = makeDecision({ decision_id: "dec-7", caller_can_act: false });
    render(
      <MutationModeProvider mutationEnabled={true}>
        {/* Console is operational + authorized, so only the per-decision caller_can_act gate hides it. */}
        <ApprovalConsole state={guardedReady([decision], true, true)} onAction={vi.fn()} />
      </MutationModeProvider>,
    );

    // The read-only view is still there…
    expect(screen.getByText(decision.prediction)).toBeInTheDocument();
    // …but no mutating control for this caller.
    expect(screen.queryByRole("button", { name: "approve decision dec-7" })).toBeNull();
    expect(screen.queryByRole("button", { name: "veto decision dec-7" })).toBeNull();
    expect(screen.queryByRole("button", { name: "handoff decision dec-7" })).toBeNull();
  });

  it("shows NO control in read-only mode even for an authorized approver", () => {
    const decision = makeDecision({ decision_id: "dec-9", caller_can_act: true });
    render(
      <MutationModeProvider mutationEnabled={false}>
        <ApprovalConsole state={guardedReady([decision], false, true)} onAction={vi.fn()} />
      </MutationModeProvider>,
    );

    expect(screen.queryByRole("button", { name: "approve decision dec-9" })).toBeNull();
  });
});
