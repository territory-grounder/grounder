package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/territory-grounder/grounder/core/persist"
	"github.com/territory-grounder/grounder/core/schema"
)

// ScheduledReboots is the pgx-backed persist.ScheduledRebootStore over discovered_scheduled_reboots
// (migration 0004). It is the durable twin of persist.MemScheduledReboots — both satisfy the same contract,
// so the suppression registry runs pure-Go in tests and durably under compose. The bi-temporal row is keyed
// (host, kind, cron) so two crons of the same kind never collide (P1-10); a re-register upserts.
type ScheduledReboots struct{ p *Pool }

// NewScheduledReboots returns a Postgres-backed scheduled-reboot registry.
func NewScheduledReboots(p *Pool) *ScheduledReboots { return &ScheduledReboots{p: p} }

var _ persist.ScheduledRebootStore = (*ScheduledReboots)(nil)

// srStateText maps the typed state to its stored text (aligned with persist.SRState.String()).
func srStateText(s persist.SRState) string {
	if s == persist.SRLive {
		return "live"
	}
	return "observing"
}

// srStateOf parses the stored text back to the typed state; an unknown value fails SAFE to observing (which
// never suppresses), never silently to live.
func srStateOf(text string) persist.SRState {
	if text == "live" {
		return persist.SRLive
	}
	return persist.SRObserving
}

// Register validates the validity window (fail-closed on an empty/inverted one), stamps the schema version,
// and upserts the row keyed (host, kind, cron). last_verified_at is stamped now() — a freshly registered row
// is fresh (INV-20). Returns the stamped row.
func (s *ScheduledReboots) Register(ctx context.Context, sr persist.ScheduledReboot) (persist.ScheduledReboot, error) {
	if sr.ValidFrom.IsZero() || sr.ValidUntil.IsZero() || !sr.ValidUntil.After(sr.ValidFrom) {
		return persist.ScheduledReboot{}, persist.ErrInvalidWindow
	}
	v, err := schema.Stamp(schema.TableDiscoveredScheduledReboots)
	if err != nil {
		return persist.ScheduledReboot{}, err
	}
	sr.SchemaVersion = v
	// On conflict PRESERVE the promotion state (state/observations/kill_switch are NOT in the UPDATE SET), so
	// a re-discovery never demotes a live schedule nor clears an operator's kill switch — mirroring the
	// predecessor's deliberate ON CONFLICT. timezone is ALSO preserved: the durable registry mirror (Save) is
	// the authority for correcting it, and a re-discovery of the same (host,kind,cron) re-observes the same
	// zone — so leaving it out of the SET keeps a known-good zone rather than letting a sweep that dropped the
	// tz silently rewrite a live window's zone (TG-225). Only the validity window / schema are refreshed.
	// RETURNING gives the AUTHORITATIVE stored row (the preserved state/timezone on a conflict, the inserted
	// values on a new row).
	var state string
	err = s.p.QueryRow(ctx, `
		INSERT INTO discovered_scheduled_reboots
		  (host, kind, cron, timezone, state, observations, kill_switch, valid_from, valid_until, last_verified_at, schema_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now(), $10)
		ON CONFLICT (host, kind, cron) DO UPDATE SET
		  valid_from = EXCLUDED.valid_from, valid_until = EXCLUDED.valid_until,
		  last_verified_at = now(), schema_version = EXCLUDED.schema_version
		RETURNING state, observations, kill_switch, valid_from, valid_until, timezone, last_verified_at`,
		sr.Host, sr.Kind, sr.Cron, sr.Timezone, srStateText(sr.State), sr.Observations, sr.KillSwitch,
		sr.ValidFrom, sr.ValidUntil, int(v)).Scan(&state, &sr.Observations, &sr.KillSwitch, &sr.ValidFrom, &sr.ValidUntil, &sr.Timezone, &sr.LastVerifiedAt)
	if err != nil {
		return persist.ScheduledReboot{}, fmt.Errorf("db: register scheduled reboot %s/%s: %w", sr.Host, sr.Kind, err)
	}
	sr.State = srStateOf(state)
	return sr, nil
}

// Save OVERWRITES the row keyed (host, kind, cron) with the authoritative registry state (TG-225) — unlike
// Register it DOES update state/observations/kill_switch, because the caller (the durable registry mirror) is
// the authority: a promote or demote must reach durability. last_verified_at is the registry's stamp (not
// now()), so freshness round-trips. A missing/inverted window is rejected (fail closed).
func (s *ScheduledReboots) Save(ctx context.Context, sr persist.ScheduledReboot) error {
	if sr.ValidFrom.IsZero() || sr.ValidUntil.IsZero() || !sr.ValidUntil.After(sr.ValidFrom) {
		return persist.ErrInvalidWindow
	}
	v, err := schema.Stamp(schema.TableDiscoveredScheduledReboots)
	if err != nil {
		return err
	}
	_, err = s.p.Exec(ctx, `
		INSERT INTO discovered_scheduled_reboots
		  (host, kind, cron, timezone, state, observations, kill_switch, valid_from, valid_until, last_verified_at, schema_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (host, kind, cron) DO UPDATE SET
		  timezone = EXCLUDED.timezone, state = EXCLUDED.state, observations = EXCLUDED.observations,
		  kill_switch = EXCLUDED.kill_switch, valid_from = EXCLUDED.valid_from, valid_until = EXCLUDED.valid_until,
		  last_verified_at = EXCLUDED.last_verified_at, schema_version = EXCLUDED.schema_version`,
		sr.Host, sr.Kind, sr.Cron, sr.Timezone, srStateText(sr.State), sr.Observations, sr.KillSwitch,
		sr.ValidFrom, sr.ValidUntil, sr.LastVerifiedAt, int(v))
	if err != nil {
		return fmt.Errorf("db: save scheduled reboot %s/%s: %w", sr.Host, sr.Kind, err)
	}
	return nil
}

// List returns every stored schedule — the boot rehydration source for the durable registry (TG-225).
func (s *ScheduledReboots) List(ctx context.Context) ([]persist.ScheduledReboot, error) {
	rows, err := s.p.Query(ctx, `
		SELECT host, kind, cron, timezone, state, observations, kill_switch, valid_from, valid_until, last_verified_at, schema_version
		FROM discovered_scheduled_reboots ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("db: list scheduled reboots: %w", err)
	}
	defer rows.Close()
	var out []persist.ScheduledReboot
	for rows.Next() {
		var (
			sr    persist.ScheduledReboot
			state string
			sv    int
		)
		if err := rows.Scan(&sr.Host, &sr.Kind, &sr.Cron, &sr.Timezone, &state, &sr.Observations, &sr.KillSwitch,
			&sr.ValidFrom, &sr.ValidUntil, &sr.LastVerifiedAt, &sv); err != nil {
			return nil, fmt.Errorf("db: list scheduled reboots scan: %w", err)
		}
		sr.State = srStateOf(state)
		sr.SchemaVersion = schema.Version(sv)
		if err := schema.CheckRow(schema.TableDiscoveredScheduledReboots, sr.SchemaVersion); err != nil {
			return nil, err // fail closed: never rehydrate the registry from a future/invalid-shaped row (TG-495)
		}
		out = append(out, sr)
	}
	return out, rows.Err()
}

// Get returns the most-recently-registered schedule for (host, kind), satisfying the store contract (which
// keys on host+kind); the DB row identity also carries cron.
func (s *ScheduledReboots) Get(ctx context.Context, host, kind string) (persist.ScheduledReboot, bool, error) {
	var (
		sr    persist.ScheduledReboot
		state string
		sv    int
	)
	err := s.p.QueryRow(ctx, `
		SELECT host, kind, cron, timezone, state, observations, kill_switch, valid_from, valid_until, last_verified_at, schema_version
		FROM discovered_scheduled_reboots WHERE host = $1 AND kind = $2
		ORDER BY created_at DESC LIMIT 1`, host, kind).
		Scan(&sr.Host, &sr.Kind, &sr.Cron, &sr.Timezone, &state, &sr.Observations, &sr.KillSwitch, &sr.ValidFrom, &sr.ValidUntil, &sr.LastVerifiedAt, &sv)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return persist.ScheduledReboot{}, false, nil
		}
		return persist.ScheduledReboot{}, false, fmt.Errorf("db: get scheduled reboot %s/%s: %w", host, kind, err)
	}
	sr.State = srStateOf(state)
	sr.SchemaVersion = schema.Version(sv)
	if err := schema.CheckRow(schema.TableDiscoveredScheduledReboots, sr.SchemaVersion); err != nil {
		return persist.ScheduledReboot{}, false, err // fail closed on a future/invalid schema_version (TG-495)
	}
	return sr, true, nil
}
