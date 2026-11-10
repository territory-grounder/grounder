-- 0004 ledgers & registries — the remaining governed tables the actuator/verifier/suppression lanes write.
-- Provenance: [O] INV-10 (verdict), INV-12 (idempotent chat cursor), INV-19 (append-only), INV-20 (bi-temporal
-- suppression registry), spec/002/003/005/006 · [F] the predecessor action_verdict / escalation_queue /
-- discovered_scheduled_reboots / chat_events. DATA-MODEL §4.7-4.8. Completes the DDL for all 8 registered
-- schema tables. Append-only / bounded is enforced by the runtime role's privilege boundary (no UPDATE/DELETE
-- on the immutable tables), never a trigger. CHECK sets align with the core Go enum String() values.

-- The mechanical verdict log: one row per action, written ONLY by the deterministic verifier (INV-10). A
-- deviation never auto-resolves; the verdict enum is the 0001 baseline type.
CREATE TABLE action_verdict (
  action_id      text PRIMARY KEY,               -- one verdict per bound action (append-only, first-wins)
  plan_hash      text NOT NULL,
  verdict        verdict NOT NULL,               -- match | partial | deviation (0001 enum)
  target_host    text NOT NULL DEFAULT '',
  site           text NOT NULL DEFAULT '',
  schema_version int NOT NULL CHECK (schema_version > 0),
  created_at     timestamptz NOT NULL DEFAULT now()
);

-- The self-learning reboot-schedule registry: bi-temporal (validity window + mutable operational state),
-- keyed by (host, kind, cron) so two crons of the same kind never collide (the P1-10 fix). INV-20: a row
-- suppresses only while currently valid AND live-verified; last_verified_at carries the freshness stamp.
CREATE TABLE discovered_scheduled_reboots (
  host             text NOT NULL,
  kind             text NOT NULL,
  cron             text NOT NULL,
  state            text NOT NULL DEFAULT 'observing' CHECK (state IN ('observing', 'live', 'disabled')),
  observations     int NOT NULL DEFAULT 0,
  kill_switch      boolean NOT NULL DEFAULT false,
  valid_from       timestamptz,
  valid_until      timestamptz,
  last_verified_at timestamptz,
  schema_version   int NOT NULL CHECK (schema_version > 0),
  created_at       timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (host, kind, cron)
);

-- The dropped-escalation requeue lane: a requeue RE-ENTERS the gated pipeline (never a side channel). Rate
-- caps + eligibility live in code; the row records the attempt count, status, and next eligibility.
CREATE TABLE escalation_queue (
  seq            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  external_ref   text NOT NULL,
  attempts       int NOT NULL DEFAULT 0,
  status         text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'fired', 'stood_down')),
  eligible_at    timestamptz NOT NULL DEFAULT now(),
  schema_version int NOT NULL CHECK (schema_version > 0),
  created_at     timestamptz NOT NULL DEFAULT now()
);

-- The chat event stream: an IDEMPOTENT insert keyed UNIQUE(source_id, event_id), so a redelivered event is a
-- no-op — closing the predecessor's cross-room approval-misattribution / silent-event-loss class (INV-12).
CREATE TABLE chat_events (
  id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  source_id      text NOT NULL,
  event_id       text NOT NULL,
  room_id        text NOT NULL DEFAULT '',
  payload        jsonb NOT NULL DEFAULT '{}'::jsonb,
  schema_version int NOT NULL CHECK (schema_version > 0),
  created_at     timestamptz NOT NULL DEFAULT now(),
  UNIQUE (source_id, event_id)
);

-- The per-(source_id, room_id) durable cursor — NO global cursor (INV-12), so one lagging room never advances
-- another's position and no event is silently skipped across rooms.
CREATE TABLE chat_cursor (
  source_id     text NOT NULL,
  room_id       text NOT NULL,
  last_event_id text NOT NULL DEFAULT '',
  updated_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (source_id, room_id)
);
