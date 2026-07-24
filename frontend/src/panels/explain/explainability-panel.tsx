import type { PendingDecision } from "../../api/generated-client";
import type { GuardedState } from "../../lib/guarded-state";
import { a11yStatus } from "../../a11y/baseline";

// A single retrieved (RAG) context chunk the orchestrator grounded on for this session.
export interface RetrievalChunk {
  chunk_id: string;
  source: string;
  snippet: string;
}

// One sampled point on the confidence trajectory the orchestrator recorded while grounding.
export interface ConfidencePoint {
  at: string;
  confidence: number;
}

// The server-captured explainability record for one grounding session (spec/010 T-010-6). Every field is
// authored by the control-plane / orchestrator; the console only reflects it. In particular the risk `band`
// reuses the contract enum so the console shows the SERVER's band verbatim — it never recomputes a band,
// its signals, or the execution class in browser code.
export interface ExplainabilitySession {
  retrieval: RetrievalChunk[];
  band: PendingDecision["band"];
  signals: string[];
  execution_class: string;
  confidence_trajectory: ConfidencePoint[];
  // ToolResult evidence IDs the orchestrator captured to back any auto-resolve.
  evidence_ids: string[];
}

// Explainability panel (T-010-6): a READ-ONLY view of one session's grounding evidence. It takes its data
// as a GuardedState prop (already-fetched server data) so it renders without any network access, and it
// exposes NO mutating control — there is no safety decision to enforce here, only server state to display.
// On a restricted (auth/transport/read-only) state it fails closed and fabricates no data.
export function ExplainabilityPanel({ state }: { state: GuardedState<ExplainabilitySession> }) {
  if (state.status === "loading") {
    return (
      <section {...a11yStatus("Explainability loading")}>
        <h2>Grounding evidence</h2>
        <p>Loading session evidence…</p>
      </section>
    );
  }

  if (state.status === "restricted") {
    return (
      <section {...a11yStatus(`Explainability unavailable (${state.reason})`)}>
        <h2>Grounding evidence</h2>
        <p>Explainability is unavailable: {state.reason}.</p>
      </section>
    );
  }

  const s = state.data;
  return (
    <section aria-label="Explainability">
      <h2>Grounding evidence</h2>

      <div>
        <h3>Retrieval context</h3>
        <ul>
          {s.retrieval.map((c) => (
            <li key={c.chunk_id}>
              <span>{c.source}</span>
              {": "}
              <span>{c.snippet}</span>
            </li>
          ))}
        </ul>
      </div>

      <div>
        <h3>Risk band</h3>
        <p>{s.band}</p>
        <ul aria-label="Risk band signals">
          {s.signals.map((sig) => (
            <li key={sig}>{sig}</li>
          ))}
        </ul>
      </div>

      <div>
        <h3>Execution class</h3>
        <p>{s.execution_class}</p>
      </div>

      <div>
        <h3>Confidence trajectory</h3>
        <ol>
          {s.confidence_trajectory.map((p) => (
            <li key={p.at}>
              {p.at}
              {": "}
              {p.confidence}
            </li>
          ))}
        </ol>
      </div>

      <div>
        <h3>Evidence IDs</h3>
        <ul aria-label="ToolResult evidence IDs list">
          {s.evidence_ids.map((id) => (
            <li key={id}>{id}</li>
          ))}
        </ul>
      </div>
    </section>
  );
}
