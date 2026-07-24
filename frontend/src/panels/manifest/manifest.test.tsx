import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { MutationModeProvider } from "../../mode/mode-provider";
import { guardedReady } from "../../lib/guarded-state";
import { ManifestTimeline } from "./manifest-timeline";
import { replay, STAGE_ORDER, type ManifestData, type StageRecord } from "./replay";

const ACTION = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
const ROGUE = "sha256:zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz";

function record(stage: StageRecord["stage"], actionId: string, at: string): StageRecord {
  return { stage, at, action_id: actionId };
}

function renderTimeline(data: ManifestData, mutationEnabled = false) {
  return render(
    <MutationModeProvider mutationEnabled={mutationEnabled}>
      <ManifestTimeline state={guardedReady(data, mutationEnabled, false)} />
    </MutationModeProvider>,
  );
}

describe("manifest timeline (T-010-4)", () => {
  it("renders the four stages as ONE chain keyed by the single action_id", () => {
    const stages = STAGE_ORDER.map((stage, i) => record(stage, ACTION, `2026-07-16T00:0${i}:00Z`));
    renderTimeline({ actionId: ACTION, stages });

    // exactly one chain, keyed by the single content-hashed action_id
    const chains = screen.getAllByTestId("manifest-chain");
    expect(chains).toHaveLength(1);
    expect(chains[0].getAttribute("data-action-id")).toBe(ACTION);

    // all four stages present, each bound to that same single action_id, none in a mismatch state
    for (const stage of STAGE_ORDER) {
      const el = screen.getByTestId(`stage-${stage}`);
      expect(el).toBeInTheDocument();
      expect(el.getAttribute("data-status")).toBe("recorded");
      expect(el.getAttribute("data-stage-action-id")).toBe(ACTION);
    }
    expect(screen.queryByText("mismatch")).not.toBeInTheDocument();
  });

  it("renders a mismatch state for a stage carrying a DIFFERENT action_id", () => {
    const stages: StageRecord[] = [
      record("predicted", ACTION, "2026-07-16T00:00:00Z"),
      record("approved", ACTION, "2026-07-16T00:01:00Z"),
      record("executed", ACTION, "2026-07-16T00:02:00Z"),
      record("verified", ROGUE, "2026-07-16T00:03:00Z"), // divergent content hash
    ];
    renderTimeline({ actionId: ACTION, stages });

    const verified = screen.getByTestId("stage-verified");
    expect(verified.getAttribute("data-status")).toBe("mismatch");
    expect(verified.getAttribute("data-stage-action-id")).toBe(ROGUE);
    // color is never the sole signal: a visible label AND the aria-label both carry "mismatch"
    expect(within(verified).getByText("mismatch")).toBeInTheDocument();
    expect(verified.getAttribute("aria-label")).toMatch(/mismatch/i);

    // the untampered stages stay recorded, not mismatched
    for (const stage of ["predicted", "approved", "executed"] as const) {
      expect(screen.getByTestId(`stage-${stage}`).getAttribute("data-status")).toBe("recorded");
    }
  });

  it("replay reconstructs the chain from records WITHOUT any re-execution (read-only)", async () => {
    // records arrive out of causal order; replay must reconstruct, not re-run
    const records: StageRecord[] = [
      record("verified", ACTION, "t3"),
      record("predicted", ACTION, "t0"),
      record("executed", ACTION, "t2"),
      record("approved", ACTION, "t1"),
    ];
    // A mock governance client. replay is handed ONLY the read capability, so it structurally cannot
    // mutate; the execute spy exists to prove it is never reached.
    const client = {
      fetchStageRecords: vi.fn(async (_id: string) => records),
      executeAction: vi.fn(async () => {
        throw new Error("replay must never execute");
      }),
    };

    const result = await replay(ACTION, client.fetchStageRecords);

    // reconstructed from persistence, ordered into the single causal chain
    expect(result.actionId).toBe(ACTION);
    expect(result.stages.map((s) => s.stage)).toEqual([
      "predicted",
      "approved",
      "executed",
      "verified",
    ]);

    // it ONLY read...
    expect(client.fetchStageRecords).toHaveBeenCalledTimes(1);
    expect(client.fetchStageRecords).toHaveBeenCalledWith(ACTION);
    // ...and NEVER executed / mutated
    expect(client.executeAction).not.toHaveBeenCalled();
  });
});
