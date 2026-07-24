// Manifest timeline panel (spec/010 T-010-4). Renders the four manifest stages — predicted, approved,
// executed, verified — as ONE visual chain keyed by the single content-hashed action_id. A stage whose
// persisted record carries a DIFFERENT action_id is shown in a mismatch state; the console surfaces the
// divergence, it never silently reconciles it and it never recomputes the hash itself.
//
// This panel is a pure, read-only visualization of server-provided state. It exposes NO mutating control
// (no approve / veto / enable / disable / kill), so — per canShowMutatingControl — it renders none in any
// mode. Replaying the chain (see ./replay) is a read, not a mutation, so nothing here is gated by mutation
// mode or authorization; the panel just reflects what persistence recorded.
import { a11yLabel, a11yStatus } from "../../a11y/baseline";
import type { GuardedState } from "../../lib/guarded-state";
import { STAGE_ORDER, type ManifestData, type StageName, type StageRecord } from "./replay";

export interface ManifestTimelineProps {
  // Already-fetched server data, wrapped in the guarded state so loading / auth / transport failures all
  // fail closed to a restricted view rather than fabricating a chain in the browser.
  state: GuardedState<ManifestData>;
}

type StageStatus = "pending" | "recorded" | "mismatch";

function stageStatus(record: StageRecord | undefined, actionId: string): StageStatus {
  if (!record) return "pending";
  return record.action_id === actionId ? "recorded" : "mismatch";
}

function stageLabel(stage: StageName, status: StageStatus, actionId: string, record?: StageRecord): string {
  switch (status) {
    case "recorded":
      return `${stage} stage recorded for action ${actionId}`;
    case "mismatch":
      return `${stage} stage action_id mismatch: recorded ${record?.action_id ?? "unknown"}, expected ${actionId}`;
    case "pending":
      return `${stage} stage pending`;
  }
}

function Chain({ data }: { data: ManifestData }) {
  const { actionId, stages } = data;
  const byStage = new Map<StageName, StageRecord>(stages.map((r) => [r.stage, r]));
  return (
    <section
      data-testid="manifest-chain"
      data-action-id={actionId}
      {...a11yLabel(`Manifest chain for action ${actionId}`)}
    >
      <ol role="list" aria-label="manifest stages">
        {STAGE_ORDER.map((stage) => {
          const record = byStage.get(stage);
          const status = stageStatus(record, actionId);
          return (
            <li
              key={stage}
              data-testid={`stage-${stage}`}
              data-status={status}
              data-stage-action-id={record?.action_id ?? ""}
              {...a11yLabel(stageLabel(stage, status, actionId, record))}
            >
              <span data-testid={`stage-${stage}-name`}>{stage}</span>
              <span data-testid={`stage-${stage}-status`}>
                {status === "mismatch" ? "mismatch" : status}
              </span>
            </li>
          );
        })}
      </ol>
    </section>
  );
}

export function ManifestTimeline({ state }: ManifestTimelineProps) {
  if (state.status === "loading") {
    return <div {...a11yStatus("Loading manifest timeline")}>Loading manifest…</div>;
  }
  if (state.status === "restricted") {
    // Fail closed: an auth or transport failure shows a restricted notice, never a fabricated chain.
    return (
      <div data-testid="manifest-restricted" {...a11yStatus(`Manifest unavailable (${state.reason})`)}>
        Manifest unavailable
      </div>
    );
  }
  return <Chain data={state.data} />;
}
