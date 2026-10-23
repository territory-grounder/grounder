package servicenow

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	tracker "github.com/territory-grounder/grounder/adapters/tracker"
)

// SearchIncidents implements the optional adapters/tracker.History capability against the ServiceNow
// Table API.
//
// Until 2026-08-01 tracker history was gated on a type assertion against the concrete YouTrack module,
// so a ServiceNow site — a tracker fully implementing the four-verb contract — was told "no tracker
// configured" and TG ran on its own weeks of session history while the site's own incident record, often
// years of it, sat one API call away. This is the half that makes an established ITSM estate legible to
// TG on day one.
//
// TWO READS, DELIBERATELY. ServiceNow keeps an incident's discussion in the JOURNAL (sys_journal_field),
// not on the incident record, so the resolution an engineer actually wrote — "restarted the guest, the
// journal was the consumer" — is invisible to a plain incident query. A history that returned incidents
// without their journal would look like it worked and carry almost none of the value.
const (
	// journalElements are the journal streams read per incident. `comments` is the customer-visible
	// stream and `work_notes` is the internal one; at most sites the actual resolution is written in
	// work_notes, so reading only comments would return the polite half of the conversation.
	journalComments  = "comments"
	journalWorkNotes = "work_notes"
	// maxJournalPerIncident bounds the journal read per incident. A long-running incident can carry
	// hundreds of entries, nearly all of them the alert re-firing rather than the answer.
	maxJournalPerIncident = 25
)

// incidentListEnvelope is the Table API's LIST response — a JSON array under "result", where the
// single-record read used an object. Reusing incidentEnvelope here would silently decode to a zero value.
type incidentListEnvelope struct {
	Result []incidentListRecord `json:"result"`
}

// incidentListRecord is the subset of incident fields history needs. Every Table API value is a string.
type incidentListRecord struct {
	SysID            string `json:"sys_id"`
	Number           string `json:"number"` // the human-readable id, e.g. INC0010023
	ShortDescription string `json:"short_description"`
	State            string `json:"state"`
	OpenedAt         string `json:"opened_at"`
}

// journalEnvelope is the sys_journal_field list response.
type journalEnvelope struct {
	Result []journalRecord `json:"result"`
}

type journalRecord struct {
	Value        string `json:"value"`
	Element      string `json:"element"`
	SysCreatedOn string `json:"sys_created_on"`
}

// SearchIncidents returns prior incidents whose short description mentions the host, newest first.
func (m *Module) SearchIncidents(ctx context.Context, host, rule string, limit int) ([]tracker.HistoricalIncident, error) {
	host = querySafe(host)
	if host == "" {
		// A blank term would match the whole incident table and return unrelated tickets as this host's
		// history — worse than returning nothing, because it reads as evidence.
		return nil, fmt.Errorf("servicenow history: empty host after sanitization")
	}
	if limit <= 0 {
		limit = 10
	}

	// ServiceNow encoded-query syntax: clauses joined by `^`, newest-first via ORDERBYDESC. The sanitizer
	// strips `^` and `=` among everything else, so a host name cannot introduce a clause of its own.
	clauses := []string{"short_descriptionLIKE" + host}
	if r := querySafe(rule); r != "" {
		// The rule is an OR-widening hint, not a requirement: site engineers do not file tickets titled
		// with the monitoring system's rule name, so ANDing it would usually return nothing at all.
		clauses = append(clauses, "ORshort_descriptionLIKE"+r)
	}
	q := url.Values{}
	q.Set("sysparm_query", strings.Join(clauses, "^")+"^ORDERBYDESCopened_at")
	q.Set("sysparm_fields", "sys_id,number,short_description,state,opened_at")
	q.Set("sysparm_limit", fmt.Sprint(limit))

	body, err := m.do(ctx, http.MethodGet, "/api/now/table/incident?"+q.Encode(), nil)
	if err != nil {
		return nil, err // an unreadable tracker is an outage, never "this estate has no history"
	}
	var env incidentListEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("servicenow history: malformed incident list response: %w", err)
	}

	out := make([]tracker.HistoricalIncident, 0, len(env.Result))
	for _, rec := range env.Result {
		hi := tracker.HistoricalIncident{
			ID:      firstNonEmpty(rec.Number, rec.SysID), // the number is what an engineer here recognises
			Summary: rec.ShortDescription,
			State:   rec.State,
			Filed:   parseServiceNowTime(rec.OpenedAt),
		}
		// A journal read that fails must not discard the incident: the summary and state are still real
		// history. The failure is visible as an absent discussion, not as an absent incident.
		if rec.SysID != "" {
			if cs, jerr := m.journal(ctx, rec.SysID); jerr == nil {
				hi.Comments = cs
			}
		}
		out = append(out, hi)
	}
	return out, nil
}

// journal reads an incident's discussion from sys_journal_field, oldest first — the order it was written,
// which is the order it reads as a narrative and which puts the resolution last.
func (m *Module) journal(ctx context.Context, sysID string) ([]string, error) {
	q := url.Values{}
	q.Set("sysparm_query", "element_id="+sysID+
		"^element="+journalComments+"^ORelement="+journalWorkNotes+"^ORDERBYsys_created_on")
	q.Set("sysparm_fields", "value,element,sys_created_on")
	q.Set("sysparm_limit", fmt.Sprint(maxJournalPerIncident))

	body, err := m.do(ctx, http.MethodGet, "/api/now/table/sys_journal_field?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var env journalEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("servicenow history: malformed journal response: %w", err)
	}
	// ORDERBY is requested but not trusted: an instance that ignores it would otherwise hand back an
	// arbitrary order, and the LAST entries are the ones a reader is told hold the resolution.
	sort.SliceStable(env.Result, func(i, j int) bool {
		return env.Result[i].SysCreatedOn < env.Result[j].SysCreatedOn
	})
	out := make([]string, 0, len(env.Result))
	for _, j := range env.Result {
		v := strings.TrimSpace(j.Value)
		if v == "" {
			continue
		}
		// The stream is labelled: a work note and a customer comment carry different candour, and a
		// reader deciding how much weight to give a line should know which one it is.
		out = append(out, "["+j.Element+"] "+v)
	}
	return out, nil
}

// parseServiceNowTime parses the Table API's timestamp form ("2026-07-30 14:22:11", UTC, no zone marker).
// An unparseable or absent value yields the ZERO time, which every consumer reads as UNKNOWN — never
// time.Now(), which would make a decade-old ticket rank as fresh precedent.
func parseServiceNowTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// querySafe reduces a value to characters that cannot restructure a ServiceNow encoded query. `^`, `=`,
// `,` and every other operator character become a space, so a hostile or merely odd host name can at
// worst broaden the search — it can never add a clause or change the table being read.
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
