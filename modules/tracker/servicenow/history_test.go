package servicenow

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

// routedDoer answers per TABLE, so one oracle can drive the two-read shape (incident list, then journal)
// through the module's real request-building path.
type routedDoer struct {
	incidents string
	journal   string
	journalSt int
	seen      []string // full request URLs, in order
}

func (d *routedDoer) Do(req *http.Request) (*http.Response, error) {
	d.seen = append(d.seen, req.URL.String())
	body, status := "", 200
	switch {
	case strings.Contains(req.URL.Path, "sys_journal_field"):
		body = d.journal
		if d.journalSt != 0 {
			status = d.journalSt
		}
	case strings.Contains(req.URL.Path, "/table/incident"):
		body = d.incidents
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}

const incidentListJSON = `{"result":[
 {"sys_id":"aaa","number":"INC0010023","short_description":"dc1mealie01 unreachable","state":"6","opened_at":"2026-07-12 08:14:02"},
 {"sys_id":"bbb","number":"INC0009980","short_description":"dc1mealie01 disk full","state":"7","opened_at":"2026-06-02 21:40:00"}]}`

const journalJSON = `{"result":[
 {"value":"paging the on-call","element":"comments","sys_created_on":"2026-07-12 08:15:00"},
 {"value":"restarted the guest; the journal was the consumer","element":"work_notes","sys_created_on":"2026-07-12 09:02:00"}]}`

func historyFixture(t *testing.T, d *routedDoer) *Module {
	t.Helper()
	t.Setenv("TG_TEST_SN_TOKEN", "s3cr3t")
	return New("https://dev12345.service-now.com", testUsername, config.SecretRef("env:TG_TEST_SN_TOKEN"), WithHTTPClient(d))
}

// THE POINT OF THE WHOLE CAPABILITY: an established ServiceNow site's incident record, including the
// human discussion where the resolution is actually written.
func TestServiceNowHistoryReturnsIncidentsWithTheirJournal(t *testing.T) {
	d := &routedDoer{incidents: incidentListJSON, journal: journalJSON}
	got, err := historyFixture(t, d).SearchIncidents(context.Background(), "dc1mealie01", "device-down", 10)
	if err != nil {
		t.Fatalf("SearchIncidents: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 incidents, got %d", len(got))
	}
	// The human-readable number is what an engineer at this site recognises, not the sys_id.
	if got[0].ID != "INC0010023" {
		t.Errorf("want the readable incident number, got %q", got[0].ID)
	}
	if got[0].Filed.IsZero() || got[0].Filed.Format("2006-01-02") != "2026-07-12" {
		t.Errorf("opened_at was not parsed: %v", got[0].Filed)
	}
	// The resolution lives in a work note. A history that dropped the journal would look like it worked
	// and carry almost none of the value.
	joined := strings.Join(got[0].Comments, " | ")
	if !strings.Contains(joined, "restarted the guest") {
		t.Fatalf("the journal entry holding the resolution is missing: %q", joined)
	}
	// Work notes and customer comments carry different candour; a reader weighing a line must know which.
	if !strings.Contains(joined, "[work_notes]") || !strings.Contains(joined, "[comments]") {
		t.Errorf("journal entries are not labelled by stream: %q", joined)
	}
	// Oldest first: the resolution is written last, and reversing that buries it.
	if idx1, idx2 := strings.Index(joined, "paging"), strings.Index(joined, "restarted"); idx1 > idx2 {
		t.Error("journal is not oldest-first; the resolution no longer reads as the conclusion")
	}
}

// An alert-derived host must not be able to add a clause to the encoded query or change the table read.
func TestServiceNowHistorySanitizesTheEncodedQuery(t *testing.T) {
	d := &routedDoer{incidents: `{"result":[]}`, journal: `{"result":[]}`}
	_, err := historyFixture(t, d).SearchIncidents(context.Background(),
		"web01^state=1^ORDERBYnumber", "", 5)
	if err != nil {
		t.Fatalf("SearchIncidents: %v", err)
	}
	if len(d.seen) == 0 {
		t.Fatal("vacuity floor: no request was issued, so this asserted nothing")
	}
	req := d.seen[0]
	// The caret is the clause separator. Exactly one may survive — the ORDERBYDESC the module itself
	// appends — and none may come from the host.
	if strings.Count(req, "%5E") != 1 || strings.Contains(req, "state%3D1") {
		t.Fatalf("host text restructured the encoded query: %s", req)
	}
	if !strings.Contains(req, "table/incident") {
		t.Fatalf("the query no longer reads the incident table: %s", req)
	}
}

// A blank host would match the entire incident table and return unrelated tickets as this host's
// history, which reads as evidence.
func TestServiceNowHistoryRefusesABlankHost(t *testing.T) {
	d := &routedDoer{incidents: incidentListJSON}
	if _, err := historyFixture(t, d).SearchIncidents(context.Background(), "^=,", "", 5); err == nil {
		t.Fatal("a host that sanitizes to nothing was accepted")
	}
	if len(d.seen) != 0 {
		t.Fatalf("a refused search still issued %d request(s)", len(d.seen))
	}
}

// An unreadable tracker is an OUTAGE, never "this estate has no history" — the second would teach the
// agent that a site with a decade of incidents has none.
func TestServiceNowHistoryFailedReadIsAnErrorNotAnEmptyResult(t *testing.T) {
	d := &routedDoer{incidents: `{"result":[]}`}
	d.incidents = ""
	m := historyFixture(t, d)
	// A malformed body must not decode to an empty, confident "no incidents".
	if got, err := m.SearchIncidents(context.Background(), "web01", "", 5); err == nil {
		t.Fatalf("a malformed incident response returned %d incidents and no error", len(got))
	}
}

// A journal read that fails must not discard the incident: the summary and state are still real history.
// Losing the ticket entirely because its discussion could not be fetched trades a partial answer for none.
func TestServiceNowHistoryKeepsIncidentsWhenTheJournalFails(t *testing.T) {
	d := &routedDoer{incidents: incidentListJSON, journal: `{"error":"no acl"}`, journalSt: 403}
	got, err := historyFixture(t, d).SearchIncidents(context.Background(), "dc1mealie01", "", 10)
	if err != nil {
		t.Fatalf("a journal failure must not fail the whole search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 incidents despite the journal failing, got %d", len(got))
	}
	if len(got[0].Comments) != 0 {
		t.Errorf("a failed journal read must yield NO comments, not invented ones: %v", got[0].Comments)
	}
}

// A timestamp that cannot be parsed is UNKNOWN. Falling back to now() would make a decade-old ticket
// rank as the freshest precedent in the corpus.
func TestServiceNowHistoryUnparseableTimeIsZeroNotNow(t *testing.T) {
	if got := parseServiceNowTime("not a date"); !got.IsZero() {
		t.Fatalf("want zero time for an unparseable value, got %v", got)
	}
	if got := parseServiceNowTime(""); !got.IsZero() {
		t.Fatalf("want zero time for an absent value, got %v", got)
	}
}
