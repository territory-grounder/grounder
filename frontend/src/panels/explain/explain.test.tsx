import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { MutationModeProvider } from "../../mode/mode-provider";
import { guardedReady } from "../../lib/guarded-state";
import { ExplainabilityPanel, type ExplainabilitySession } from "./explainability-panel";

const session: ExplainabilitySession = {
  retrieval: [
    { chunk_id: "rag-1", source: "runbook://disk-pressure", snippet: "evict low-priority pods first" },
    { chunk_id: "rag-2", source: "kb://node-cordon", snippet: "cordon before drain" },
  ],
  band: "AUTO_NOTICE",
  signals: ["reversible=true", "blast_radius=node", "off_peak_window"],
  execution_class: "self-heal:reboot",
  confidence_trajectory: [
    { at: "2026-07-15T00:00:00Z", confidence: 0.42 },
    { at: "2026-07-15T00:01:00Z", confidence: 0.88 },
  ],
  evidence_ids: ["tr-aaa", "tr-bbb", "tr-ccc"],
};

describe("explainability panel (T-010-6)", () => {
  it("shows retrieval context, risk band + signals, execution class, confidence trajectory, and the ToolResult evidence IDs", () => {
    render(
      <MutationModeProvider mutationEnabled={false}>
        <ExplainabilityPanel state={guardedReady(session, false, false)} />
      </MutationModeProvider>,
    );

    // retrieval (RAG) context
    expect(screen.getByText("runbook://disk-pressure")).toBeInTheDocument();
    expect(screen.getByText("evict low-priority pods first")).toBeInTheDocument();
    expect(screen.getByText("kb://node-cordon")).toBeInTheDocument();

    // risk band + its signals (band shown verbatim from the server contract enum): the section carries a
    // "Risk band" heading and shows the server band value beneath it.
    expect(screen.getByRole("heading", { name: "Risk band" })).toBeInTheDocument();
    expect(screen.getByText("AUTO_NOTICE")).toBeInTheDocument();
    expect(screen.getByText("reversible=true")).toBeInTheDocument();
    expect(screen.getByText("blast_radius=node")).toBeInTheDocument();
    expect(screen.getByText("off_peak_window")).toBeInTheDocument();

    // execution class
    expect(screen.getByText("self-heal:reboot")).toBeInTheDocument();

    // confidence trajectory (both sampled points)
    expect(screen.getByText(/0\.42/)).toBeInTheDocument();
    expect(screen.getByText(/0\.88/)).toBeInTheDocument();

    // ToolResult evidence IDs that backed the auto-resolve
    expect(screen.getByText("tr-aaa")).toBeInTheDocument();
    expect(screen.getByText("tr-bbb")).toBeInTheDocument();
    expect(screen.getByText("tr-ccc")).toBeInTheDocument();
  });

  it("fails closed: renders no session evidence when the guarded state is restricted (even if operational)", () => {
    render(
      <MutationModeProvider mutationEnabled={true}>
        <ExplainabilityPanel state={{ status: "restricted", reason: "auth" }} />
      </MutationModeProvider>,
    );

    expect(screen.queryByText("self-heal:reboot")).not.toBeInTheDocument();
    expect(screen.queryByText("tr-aaa")).not.toBeInTheDocument();
    expect(screen.getByText(/unavailable/i)).toBeInTheDocument();
  });
});
