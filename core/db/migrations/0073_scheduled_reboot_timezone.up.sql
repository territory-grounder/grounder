-- 0073 (TG-225) — the durable learned-suppression registry must round-trip the schedule's TIMEZONE.
--
-- discovered_scheduled_reboots (migration 0004) carried every field of a learned reboot schedule EXCEPT the
-- IANA timezone its cron window is evaluated in. Schedule.Suppresses evaluates a DST-CORRECT window in that
-- zone, so persisting a learned schedule and RELOADING it on the next boot without the timezone would produce
-- a window in the wrong zone — suppression firing at the wrong wall-clock times, the dangerous direction. With
-- the timezone stored, a promoted lesson survives a restart with a correct window (the whole point of durable
-- persistence, TG-225). Backfill 'UTC' for any pre-existing row; new rows carry the real zone.
ALTER TABLE discovered_scheduled_reboots ADD COLUMN timezone text NOT NULL DEFAULT 'UTC';
