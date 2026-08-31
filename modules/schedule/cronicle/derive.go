package cronicle

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/schedule"
)

// Provider is the read-only Cronicle scheduling provider: it reads the live schedule and derives the
// vendor-neutral core/schedule.Calendar, and it satisfies schedule.WindowGuard so the actuation path can
// DEFER an out-of-window change. Construct with NewProvider.
type Provider struct {
	client     *Client
	source     string        // non-secret provenance, e.g. "cronicle:demo01"
	defaultDur time.Duration // window length applied when a directive omits tg-duration
}

// compile-time proof the provider is the actuation-facing seam.
var _ schedule.WindowGuard = (*Provider)(nil)

// SkipRecord is one scheduler event the most recent derivation could not turn into a window (coverage
// observability), with a non-secret reason. A skip is never a whole-read failure — a bad event simply
// contributes no window (fail closed) while the good events still derive.
type SkipRecord struct {
	EventID string
	Title   string
	Reason  string
}

// ProviderConfig configures a Provider. Client is required. Source is a non-secret id for provenance.
// DefaultWindowDuration is applied to a tagged event whose directive omits tg-duration (0 => 1h).
type ProviderConfig struct {
	Client                *Client
	Source                string
	DefaultWindowDuration time.Duration
}

// NewProvider builds a Provider. It fails closed if the client is nil.
func NewProvider(cfg ProviderConfig) (*Provider, error) {
	if cfg.Client == nil {
		return nil, fmt.Errorf("cronicle: provider requires a client")
	}
	src := cfg.Source
	if src == "" {
		src = SourceType
	}
	dur := cfg.DefaultWindowDuration
	if dur <= 0 {
		dur = defaultWindowDuration
	}
	return &Provider{client: cfg.Client, source: "cronicle:" + src, defaultDur: dur}, nil
}

// Snapshot reads the live schedule and derives the Calendar. Every call re-reads the schedule from the
// system-of-record (no cached copy — INV-05). On any read failure it returns the error; MaintenanceWindow
// converts that into a fail-closed-safe unreadable Calendar.
func (p *Provider) Snapshot(ctx context.Context) (schedule.Calendar, []SkipRecord, error) {
	events, err := p.client.Schedule(ctx)
	if err != nil {
		return schedule.Calendar{Readable: false, Source: p.source, Note: readClass(err)}, nil, err
	}
	cal, skips := p.derive(events)
	return cal, skips, nil
}

// MaintenanceWindow implements schedule.WindowGuard: it re-reads the live schedule and answers whether now
// is inside a sanctioned maintenance window for target. An UNREADABLE schedule yields the conservative
// OUTSIDE answer (REQ-1903) — it never assumes actuation is safe.
func (p *Provider) MaintenanceWindow(ctx context.Context, target string, now time.Time) (bool, string) {
	cal, _, err := p.Snapshot(ctx)
	if err != nil {
		// derive an unreadable calendar so the fail-closed-safe default reason is used.
		cal = schedule.Calendar{Readable: false, Source: p.source, Note: readClass(err)}
	}
	return cal.MaintenanceWindow(target, now)
}

// derive maps a set of Cronicle events onto the vendor-neutral Calendar: each operator-tagged event yields a
// window (maintenance/freeze), and every enabled recurring event yields an already-scheduled job.
func (p *Provider) derive(events []cronEvent) (schedule.Calendar, []SkipRecord) {
	cal := schedule.Calendar{Readable: true, Source: p.source}
	var skips []SkipRecord
	for _, ev := range events {
		if ev.Enabled == 0 {
			continue // a disabled event is inactive — neither a live window nor a running job
		}
		rec, hasTiming := timingToRecurrence(ev.Timing)
		loc := loadLoc(ev.Timezone)

		// every enabled event with a recurrence is an already-scheduled job (collision awareness).
		if hasTiming {
			cal.Jobs = append(cal.Jobs, schedule.ScheduledJob{
				Title:   ev.Title,
				EventID: ev.ID,
				Target:  targetScope("", ev.Target),
				Rec:     rec,
				Loc:     loc,
			})
		}

		// a window requires an operator directive in the event's notes (falling back to its title).
		dir := schedule.ParseDirective(ev.Notes)
		if !dir.Present {
			dir = schedule.ParseDirective(ev.Title)
		}
		if !dir.Present {
			continue // an untagged event is a plain job, not a window
		}
		if dir.Kind == schedule.KindUnspecified {
			skips = append(skips, SkipRecord{ev.ID, ev.Title, "tg-window directive has an unrecognised kind"})
			continue
		}
		if !hasTiming {
			skips = append(skips, SkipRecord{ev.ID, ev.Title, "tagged as a window but has no recurrence (on-demand event)"})
			continue
		}
		dur := dir.Duration
		if dur <= 0 {
			dur = p.defaultDur
		}
		if dur > maxWindowDuration {
			skips = append(skips, SkipRecord{ev.ID, ev.Title, fmt.Sprintf("window duration %s exceeds the %s cap", dur, maxWindowDuration)})
			continue
		}
		cal.Windows = append(cal.Windows, schedule.WindowRule{
			Kind:     dir.Kind,
			Target:   targetScope(dir.Target, ev.Target),
			Title:    ev.Title,
			EventID:  ev.ID,
			Rec:      rec,
			Duration: dur,
			Loc:      loc,
		})
	}
	return cal, skips
}

// timingToRecurrence maps Cronicle's timing object onto core/schedule.Recurrence. An absent/null timing
// (on-demand event) yields hasTiming=false.
func timingToRecurrence(t *cronTiming) (schedule.Recurrence, bool) {
	if t == nil {
		return schedule.Recurrence{}, false
	}
	return schedule.Recurrence{
		Years:    t.Years,
		Months:   t.Months,
		Days:     t.Days,
		Weekdays: t.Weekdays,
		Hours:    t.Hours,
		Minutes:  t.Minutes,
	}, true
}

// groupTargets are Cronicle server-group ids / catch-alls that map onto the whole estate ("*") rather than a
// concrete estate host.
var groupTargets = map[string]bool{"": true, "allgrp": true, "maingrp": true, "all": true}

// targetScope resolves a window/job's target scope: the operator's explicit tg-target wins; otherwise the
// event's Cronicle target is used, mapping a server-group id / catch-all onto the whole estate ("*").
func targetScope(directiveTarget, cronTarget string) string {
	if s := strings.TrimSpace(directiveTarget); s != "" {
		return s
	}
	if groupTargets[strings.TrimSpace(cronTarget)] {
		return "*"
	}
	return strings.TrimSpace(cronTarget)
}

// loadLoc loads an IANA timezone (the embedded tzdata makes this work on distroless). An empty or unknown
// timezone falls back to UTC so a window is still evaluable rather than silently dropped.
func loadLoc(tz string) *time.Location {
	if strings.TrimSpace(tz) == "" {
		return time.UTC
	}
	if loc, err := time.LoadLocation(tz); err == nil {
		return loc
	}
	return time.UTC
}

// readClass reduces a read error to a non-secret class for the Calendar.Note (never carries the API key or
// full URL/body).
func readClass(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "API error"):
		return "scheduler rejected the read (auth/API error)"
	case strings.Contains(s, "status "):
		return "scheduler returned a non-2xx status"
	case strings.Contains(s, "decode"):
		return "scheduler response was unparseable"
	default:
		return "scheduler unreachable"
	}
}

// Deployment is one operator-declared Cronicle instance (config-not-code). The env grammar is
// `id|baseurl|keyref[|defaultduration][|cacertpath]`, semicolon-separated rows.
type Deployment struct {
	ID          string
	BaseURL     string
	KeyRef      string // a SecretRef string (env:/file:/store:), never a literal secret
	DefaultDur  time.Duration
	CACertPath  string
	parseErrMsg string
}

// ParseDeployments parses the TG_CRONICLE_DEPLOYMENTS grammar into rows. A row missing id/baseurl/keyref is
// skipped (fail closed — a partial row grants no scheduler awareness rather than a broken client). It never
// returns key material.
func ParseDeployments(spec string) []Deployment {
	var out []Deployment
	for _, row := range strings.Split(spec, ";") {
		row = strings.TrimSpace(row)
		if row == "" {
			continue
		}
		f := strings.Split(row, "|")
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		if len(f) < 3 || f[0] == "" || f[1] == "" || f[2] == "" {
			continue
		}
		d := Deployment{ID: f[0], BaseURL: f[1], KeyRef: f[2]}
		if len(f) >= 4 && f[3] != "" {
			if pd, err := time.ParseDuration(f[3]); err == nil && pd > 0 {
				d.DefaultDur = pd
			}
		}
		if len(f) >= 5 {
			d.CACertPath = f[4]
		}
		out = append(out, d)
	}
	return out
}

// NewProviderFromDeployment builds a live Provider from a parsed Deployment row.
func NewProviderFromDeployment(d Deployment) (*Provider, error) {
	c, err := New(Config{BaseURL: d.BaseURL, KeyRef: config.SecretRef(d.KeyRef), CACertPath: d.CACertPath})
	if err != nil {
		return nil, err
	}
	return NewProvider(ProviderConfig{Client: c, Source: d.ID, DefaultWindowDuration: d.DefaultDur})
}
