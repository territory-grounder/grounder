package youtrack

import (
	"context"
	"fmt"
	"strings"

	tracker "github.com/territory-grounder/grounder/adapters/tracker"
)

// SearchIncidents implements the optional adapters/tracker.History capability: prior incidents for a
// host, with the human discussion that usually holds the actual resolution.
//
// This logic used to live in the composition root as a closure over the CONCRETE *youtrack.Module, which
// is why every other tracker backend was excluded from tracker history by construction. Moving it here
// makes it a capability the module ADVERTISES, so the worker can ask "does this tracker have history?"
// instead of "is this tracker YouTrack?".
func (m *Module) SearchIncidents(ctx context.Context, host, rule string, limit int) ([]tracker.HistoricalIncident, error) {
	host = querySafe(host)
	if host == "" {
		// A blank host would search the ENTIRE tracker and return unrelated incidents as this host's
		// history. Refusing is the only honest answer.
		return nil, fmt.Errorf("youtrack history: empty host after sanitization")
	}
	q := "summary: " + host
	if r := querySafe(rule); r != "" {
		q += " " + r
	}
	issues, err := m.Search(ctx, q, limit)
	if err != nil {
		return nil, err // an unreadable tracker is an outage, never "no history"
	}
	out := make([]tracker.HistoricalIncident, 0, len(issues))
	for _, i := range issues {
		hi := tracker.HistoricalIncident{
			ID:      i.Readable,
			Summary: i.Summary,
			State:   i.Fields["State"],
			Filed:   i.Created,
		}
		if hi.ID == "" {
			hi.ID = i.ID // the readable id is what a human recognises; the internal id is the fallback
		}
		for _, c := range i.Comments {
			hi.Comments = append(hi.Comments, c.Text)
		}
		out = append(out, hi)
	}
	return out, nil
}

// querySafe reduces a value to characters that cannot restructure a YouTrack query. Everything else
// becomes a space, which at worst BROADENS a search and can never turn a host name into a query operator.
func querySafe(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == ' ', r == '/':
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// The module satisfies the optional capability; a signature drift breaks the build here rather than
// silently falling out of the worker's type assertion and going dark.
var _ tracker.History = (*Module)(nil)
