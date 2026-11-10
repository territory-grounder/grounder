package db

import (
	"context"
	"fmt"
	"time"

	"github.com/territory-grounder/grounder/core/episodes"
)

// EpisodeReadStore loads the rows core/episodes attributes (TG-672, spec/025 REQ-2534): EVERY injected fault
// around the window — the lookback before it (an in-window alert whose owner was injected just before the window
// finds that owner, not the first in-window fault on its host), the lookback after it (a successor injected
// after the window still ends the last in-window fault's interval), and every campaign (a drill on the same host
// ends an interval too; the note prefix only selects which faults are REPORTED) — and the alerts, executions,
// credits and sessions over the same span. Read-only; every value is a bound parameter.
type EpisodeReadStore struct{ p *Pool }

// NewEpisodeReadStore returns the Postgres-backed loader.
func NewEpisodeReadStore(p *Pool) *EpisodeReadStore { return &EpisodeReadStore{p: p} }

// EpisodeScope is one load: the cohort window, an optional note PREFIX that selects which faults are reported as
// the cohort (the injector stamps `note`, e.g. a soak's tag; ownership always sees every fault), and the lookback
// around the window.
type EpisodeScope struct {
	Since, Until time.Time
	NotePrefix   string
	Lookback     time.Duration
}

// DefaultEpisodeLookback is wider than any ownership interval the injector arms (a 45-minute restore plus the
// 10-minute grace), so no owner of an in-window alert can be missed.
const DefaultEpisodeLookback = 2 * time.Hour

// Load reads the attribution inputs for sc. A fault's virtualisation kind comes from guest_config_baseline (the
// Proxmox config baseline keyed by vmid, which the injector records as fault_ref — preferred — else by guest
// name, newest row first); "" when the baseline never saw the guest — reported as unknown, never guessed.
func (s *EpisodeReadStore) Load(ctx context.Context, sc EpisodeScope) (episodes.Input, error) {
	in := episodes.Input{Since: sc.Since, Until: sc.Until, NotePrefix: sc.NotePrefix, Sessions: map[string]episodes.Session{}}
	if sc.Since.IsZero() || sc.Until.IsZero() || !sc.Until.After(sc.Since) {
		return in, fmt.Errorf("db: episode scope needs a fixed window with until after since (got %s .. %s)",
			sc.Since.UTC().Format(time.RFC3339), sc.Until.UTC().Format(time.RFC3339))
	}
	lookback := sc.Lookback
	if lookback <= 0 {
		lookback = DefaultEpisodeLookback
	}
	lb := fmt.Sprintf("%d seconds", int64(lookback/time.Second))

	fr, err := s.p.Pool.Query(ctx, `
		SELECT f.id, f.host, f.node, f.fault_type, f.injected_at, f.restore_due_at, f.restored_at, f.restore_state, f.note,
		       COALESCE((SELECT g.kind FROM guest_config_baseline g
		                  WHERE (f.fault_ref <> '' AND g.vmid::text = f.fault_ref) OR g.guest = f.host
		                  ORDER BY (f.fault_ref <> '' AND g.vmid::text = f.fault_ref) DESC, g.observed_at DESC LIMIT 1), '')
		  FROM injected_fault f
		 WHERE f.injected_at >= $1::timestamptz - $3::interval AND f.injected_at < $2::timestamptz + $3::interval
		 ORDER BY f.host, f.injected_at, f.id`, sc.Since, sc.Until, lb)
	if err != nil {
		return in, fmt.Errorf("db: episode faults: %w", err)
	}
	for fr.Next() {
		var f episodes.Fault
		var due, restored *time.Time
		if err := fr.Scan(&f.ID, &f.Host, &f.Node, &f.FaultType, &f.InjectedAt, &due, &restored, &f.RestoreState, &f.Note, &f.Virt); err != nil {
			fr.Close()
			return in, fmt.Errorf("db: episode fault scan: %w", err)
		}
		if due != nil {
			f.RestoreDueAt = due.UTC()
		}
		if restored != nil {
			f.RestoredAt = restored.UTC()
		}
		f.InjectedAt = f.InjectedAt.UTC()
		in.Faults = append(in.Faults, f)
	}
	fr.Close()
	if err := fr.Err(); err != nil {
		return in, fmt.Errorf("db: episode fault rows: %w", err)
	}

	ar, err := s.p.Pool.Query(ctx, `
		SELECT external_ref, host, alert_rule, received_at, observed_at, source_type, site
		  FROM ingest_alert
		 WHERE received_at >= $1::timestamptz - $3::interval AND received_at < $2::timestamptz + $3::interval`, sc.Since, sc.Until, lb)
	if err != nil {
		return in, fmt.Errorf("db: episode alerts: %w", err)
	}
	for ar.Next() {
		var a episodes.Alert
		var observed *time.Time
		if err := ar.Scan(&a.ExternalRef, &a.Host, &a.Rule, &a.ReceivedAt, &observed, &a.Source, &a.Site); err != nil {
			ar.Close()
			return in, fmt.Errorf("db: episode alert scan: %w", err)
		}
		a.ReceivedAt = a.ReceivedAt.UTC()
		if observed != nil {
			a.ObservedAt = observed.UTC()
		}
		in.Alerts = append(in.Alerts, a)
	}
	ar.Close()
	if err := ar.Err(); err != nil {
		return in, fmt.Errorf("db: episode alert rows: %w", err)
	}

	xr, err := s.p.Pool.Query(ctx, `
		SELECT ae.id, ae.external_ref, ae.action_id, ae.executed_at, COALESCE(ae.verdict::text, ''), ae.site, ae.target_host,
		       COALESCE(st.op_class, '')
		  FROM action_execution ae LEFT JOIN session_triage st ON st.external_ref = ae.external_ref
		 WHERE ae.executed_at >= $1::timestamptz - $3::interval AND ae.executed_at < $2::timestamptz + $3::interval
		 ORDER BY ae.executed_at, ae.id`, sc.Since, sc.Until, lb)
	if err != nil {
		return in, fmt.Errorf("db: episode executions: %w", err)
	}
	for xr.Next() {
		var x episodes.Execution
		if err := xr.Scan(&x.ID, &x.ExternalRef, &x.ActionID, &x.ExecutedAt, &x.Verdict, &x.Site, &x.TargetHost, &x.OpClass); err != nil {
			xr.Close()
			return in, fmt.Errorf("db: episode execution scan: %w", err)
		}
		x.ExecutedAt = x.ExecutedAt.UTC()
		in.Executions = append(in.Executions, x)
	}
	xr.Close()
	if err := xr.Err(); err != nil {
		return in, fmt.Errorf("db: episode execution rows: %w", err)
	}

	cr, err := s.p.Pool.Query(ctx, `
		SELECT external_ref, op_class, outcome, credited_at FROM graduation_credit
		 WHERE credited_at >= $1::timestamptz - $3::interval AND credited_at < $2::timestamptz + $3::interval
		 ORDER BY credited_at`, sc.Since, sc.Until, lb)
	if err != nil {
		return in, fmt.Errorf("db: episode credits: %w", err)
	}
	for cr.Next() {
		var c episodes.Credit
		if err := cr.Scan(&c.ExternalRef, &c.OpClass, &c.Outcome, &c.CreditedAt); err != nil {
			cr.Close()
			return in, fmt.Errorf("db: episode credit scan: %w", err)
		}
		c.CreditedAt = c.CreditedAt.UTC()
		in.Credits = append(in.Credits, c)
	}
	cr.Close()
	if err := cr.Err(); err != nil {
		return in, fmt.Errorf("db: episode credit rows: %w", err)
	}

	sr, err := s.p.Pool.Query(ctx, `
		SELECT external_ref, op_class, band, outcome, harness_fp, synthetic FROM session_triage
		 WHERE created_at >= $1::timestamptz - $3::interval AND created_at < $2::timestamptz + $3::interval`, sc.Since, sc.Until, lb)
	if err != nil {
		return in, fmt.Errorf("db: episode sessions: %w", err)
	}
	for sr.Next() {
		var ses episodes.Session
		if err := sr.Scan(&ses.ExternalRef, &ses.OpClass, &ses.Band, &ses.Outcome, &ses.HarnessFP, &ses.Synthetic); err != nil {
			sr.Close()
			return in, fmt.Errorf("db: episode session scan: %w", err)
		}
		in.Sessions[ses.ExternalRef] = ses
	}
	sr.Close()
	if err := sr.Err(); err != nil {
		return in, fmt.Errorf("db: episode session rows: %w", err)
	}
	return in, nil
}
