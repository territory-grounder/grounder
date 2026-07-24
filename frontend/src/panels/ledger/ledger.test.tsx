import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { MutationModeProvider } from "../../mode/mode-provider";
import { guardedReady } from "../../lib/guarded-state";
import type { LedgerEntry } from "../../api/generated-client";
import { LedgerView, type LedgerData } from "./ledger-view";

const entries: LedgerEntry[] = [
  { seq: 1, decision: "APPROVE", action_id: "act-1", hash: "h1", prev_hash: "genesis" },
  { seq: 2, decision: "AUTO", action_id: "act-2", hash: "h2", prev_hash: "h1" },
];

// Deliberately-inconsistent hashes: entry 2's prev_hash does NOT chain from entry 1's hash. A browser that
// recomputed the chain would flag this as broken. The panel must instead show what the SERVER's verifier
// said — proving the verdict is reflected, never computed in the browser.
const inconsistentEntries: LedgerEntry[] = [
  { seq: 1, decision: "APPROVE", action_id: "act-1", hash: "h1", prev_hash: "genesis" },
  { seq: 2, decision: "AUTO", action_id: "act-2", hash: "h2", prev_hash: "DOES-NOT-MATCH" },
];

function renderPanel(data: LedgerData, mutationEnabled: boolean) {
  return render(
    <MutationModeProvider mutationEnabled={mutationEnabled}>
      <LedgerView state={guardedReady(data, mutationEnabled, true)} />
    </MutationModeProvider>,
  );
}

describe("ledger view (T-010-5)", () => {
  it("displays the chain-verification status returned by the LedgerVerifier (server value, not computed in browser)", () => {
    // The server verifier reports verified:true even though the entry hashes do not chain — the panel
    // reflects the server's verdict rather than recomputing one from the (inconsistent) entries.
    renderPanel({ entries: inconsistentEntries, verification: { verified: true } }, false);

    const status = screen.getByTestId("chain-status");
    expect(status).toHaveAttribute("data-status", "verified");
    expect(status).toHaveAttribute("data-tampered", "false");
    expect(status).toHaveTextContent(/chain verified/i);

    // Every server-provided entry is rendered, unaltered.
    expect(screen.getAllByTestId("ledger-entry")).toHaveLength(2);
  });

  it("a broken chain is shown as tamper-detected", () => {
    renderPanel({ entries, verification: { verified: false, broken_at_seq: 2 } }, false);

    const status = screen.getByTestId("chain-status");
    expect(status).toHaveAttribute("data-status", "tampered");
    expect(status).toHaveAttribute("data-tampered", "true");
    expect(status).toHaveTextContent(/tamper detected/i);
    expect(status).toHaveTextContent(/tampered/i);
  });

  it("maps a bare server status string the same way (still not computed in browser)", () => {
    renderPanel({ entries, verification: "tamper-detected" }, false);
    expect(screen.getByTestId("chain-status")).toHaveAttribute("data-status", "tampered");
  });

  it("exposes no mutating control over the append-only ledger, even when operational and authorized", () => {
    renderPanel({ entries, verification: "verified" }, true);
    expect(screen.queryByRole("button")).toBeNull();
  });
});
