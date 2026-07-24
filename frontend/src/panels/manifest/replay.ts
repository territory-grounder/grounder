// Manifest replay (spec/010 T-010-4). A manifest is ONE causal chain — predicted -> approved ->
// executed -> verified — bound to a single content-hashed action_id. replay() reconstructs that chain
// from PERSISTED governance records only: it reads via the injected fetcher (which the console derives
// from the generated client's manifest endpoint) and NEVER triggers a re-execution or any mutation.
// Reconstruction is a pure read; the timeline component renders whatever replay returns.
import type { ManifestStage } from "../../api/generated-client";

// The four manifest stages in causal order. Reusing ManifestStage["stage"] keeps this in lockstep with
// the single generated contract — we never introduce a competing enum.
export const STAGE_ORDER = ["predicted", "approved", "executed", "verified"] as const;
export type StageName = ManifestStage["stage"];

// A persisted governance record for one stage: the generated ManifestStage plus the content-hashed
// action_id under which the record was written. In an intact manifest every record carries the SAME
// action_id; a divergent id means the record was written against a different action (tamper / replay
// drift) and MUST surface as a mismatch — the console never silently reconciles it.
export interface StageRecord extends ManifestStage {
  action_id: string;
}

// The reconstructed chain the timeline renders: the single action_id it is keyed by, plus the stage
// records recovered from persistence (0..4), ordered canonically.
export interface ManifestData {
  actionId: string;
  stages: StageRecord[];
}

// A read-only fetcher for persisted stage records. It is the ONLY capability replay() is given, so
// replay is read-only by construction — there is no execute/mutate capability in scope to call.
export type FetchStageRecords = (actionId: string) => Promise<StageRecord[]>;

function orderStages(records: readonly StageRecord[]): StageRecord[] {
  return [...records].sort(
    (a, b) => STAGE_ORDER.indexOf(a.stage) - STAGE_ORDER.indexOf(b.stage),
  );
}

// replay reconstructs the manifest chain for actionId from persisted records. It performs a single
// read (fetchRecords) and returns the ordered chain. It NEVER re-executes: the function has no mutating
// capability and issues no write — a replay can only ever observe what already happened.
export async function replay(
  actionId: string,
  fetchRecords: FetchStageRecords,
): Promise<ManifestData> {
  const records = await fetchRecords(actionId);
  return { actionId, stages: orderStages(records) };
}
