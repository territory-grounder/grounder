import type { LedgerEntry } from "../../api/generated-client";
import type { GuardedState } from "../../lib/guarded-state";
import { a11yStatus } from "../../a11y/baseline";
import { chainStatusFrom, type LedgerVerification } from "./chain-status";

// Everything this panel shows is server-provided: the ledger entries and the LedgerVerifier's chain verdict.
// The ledger is an append-only audit trail — the console exposes NO mutating control over it in ANY mode
// (there is nothing to approve/veto/edit here), so canShowMutatingControl never gates a control in this
// panel. What the guarded state DOES gate is whether we render data at all: an auth or transport error fails
// closed to "restricted" — no entries and no verdict are fabricated in the browser (spec/010 T-010-5).
export interface LedgerData {
  entries: LedgerEntry[];
  verification: LedgerVerification;
}

export function LedgerView({ state }: { state: GuardedState<LedgerData> }) {
  if (state.status === "loading") {
    return (
      <section aria-label="Decision ledger">
        <div {...a11yStatus("Loading ledger")}>Loading ledger…</div>
      </section>
    );
  }

  if (state.status === "restricted") {
    const reason =
      state.reason === "auth" ? "not authorized to view the ledger" : "ledger currently unavailable";
    return (
      <section aria-label="Decision ledger">
        <div {...a11yStatus(`Ledger restricted: ${reason}`)}>Ledger unavailable — {reason}.</div>
      </section>
    );
  }

  const { entries, verification } = state.data;
  const chain = chainStatusFrom(verification);

  return (
    <section aria-label="Decision ledger">
      <div
        {...a11yStatus(`Ledger chain verification: ${chain.label}`)}
        data-testid="chain-status"
        data-status={chain.status}
        data-tampered={String(chain.tampered)}
      >
        <strong>{chain.label}</strong>
        {chain.tampered ? (
          <span> — the hash chain reported by the server is broken; entries may have been tampered with.</span>
        ) : null}
        {chain.detail ? <span> {chain.detail}</span> : null}
      </div>

      <table>
        <caption>Append-only decision ledger</caption>
        <thead>
          <tr>
            <th scope="col">Seq</th>
            <th scope="col">Decision</th>
            <th scope="col">Action</th>
            <th scope="col">Hash</th>
            <th scope="col">Prev hash</th>
          </tr>
        </thead>
        <tbody>
          {entries.map((entry) => (
            <tr key={entry.seq} data-testid="ledger-entry">
              <td>{entry.seq}</td>
              <td>{entry.decision}</td>
              <td>{entry.action_id}</td>
              <td>
                <code>{entry.hash}</code>
              </td>
              <td>
                <code>{entry.prev_hash}</code>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  );
}
