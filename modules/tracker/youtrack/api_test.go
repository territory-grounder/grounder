package youtrack

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/config"
)

// Oracles for the FULL YouTrack surface (TG-238).
//
// These drive a real httptest server rather than a canned Doer on purpose: URL construction, query
// encoding, the multipart upload and the Authorization header are exactly the parts that break silently
// against a stub, and they are the parts a live YouTrack judges.

type capture struct {
	method, path, rawQuery, auth, ctype string
	body                                string
}

type fakeYT struct {
	t        *testing.T
	reqs     []capture
	routes   map[string]string // "METHOD /path" -> JSON response
	status   int
	lastBody string
}

func newFakeYT(t *testing.T, routes map[string]string) (*fakeYT, *Module, func()) {
	t.Helper()
	f := &fakeYT{t: t, routes: routes}
	srv := httptest.NewServer(f)
	// The token is resolved from its reference at CALL time (INV-13), so the oracle supplies it the same
	// way production does rather than injecting a pre-resolved string.
	t.Setenv("YT_TEST_TOKEN", "tok-123")
	m := New(srv.URL, config.SecretRef("env:YT_TEST_TOKEN"))
	return f, m, srv.Close
}

func (f *fakeYT) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.lastBody = string(b)
	f.reqs = append(f.reqs, capture{
		method: r.Method, path: r.URL.Path, rawQuery: r.URL.RawQuery,
		auth: r.Header.Get("Authorization"), ctype: r.Header.Get("Content-Type"), body: string(b),
	})
	if f.status != 0 {
		http.Error(w, "boom", f.status)
		return
	}
	if resp, ok := f.routes[r.Method+" "+r.URL.Path]; ok {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, resp)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, "{}")
}

func (f *fakeYT) req(t *testing.T, i int) capture {
	t.Helper()
	if i >= len(f.reqs) {
		t.Fatalf("expected at least %d request(s), got %d", i+1, len(f.reqs))
	}
	return f.reqs[i]
}

const oneRichIssue = `{
  "id":"2-14","idReadable":"IFR-1406","summary":"mealie down","description":"guest stopped",
  "created":1751328000000,"updated":1751331600000,"resolved":1751335200000,
  "reporter":{"id":"u1","login":"kp","fullName":"K P"},
  "project":{"shortName":"IFR"},
  "tags":[{"id":"t1","name":"incident"}],
  "customFields":[
    {"name":"State","value":{"name":"Resolved"}},
    {"name":"Priority","value":{"name":"Major"}},
    {"name":"Assignee","value":{"login":"kp","fullName":"K P"}},
    {"name":"Notes","value":{"text":"restarted the guest"}},
    {"name":"Sprints","value":[{"name":"S1"},{"name":"S2"}]}
  ],
  "comments":[
    {"id":"c1","text":"root cause: OOM","created":1751329000000,"author":{"login":"kp","fullName":"K P"}},
    {"id":"c2","text":"deleted one","created":1751329500000,"deleted":true,"author":{"login":"kp"}}
  ],
  "links":[{"direction":"OUTWARD","linkType":{"name":"Relates"},"issues":[{"id":"2-9","idReadable":"IFR-1400"}]}],
  "attachments":[{"id":"a1","name":"log.txt","size":42,"mimeType":"text/plain","url":"/x"}]
}`

// TestReadFullDecodesEveryValueShape is the oracle for the history use-case: an incident's memory lives in
// its comments and custom fields, and YouTrack renders field values in five different shapes. A decoder
// that handles only the enum shape silently returns empty strings for the rest — data loss that looks
// exactly like "the field was blank".
func TestReadFullDecodesEveryValueShape(t *testing.T) {
	f, m, done := newFakeYT(t, map[string]string{"GET /api/issues/IFR-1406": oneRichIssue})
	defer done()

	got, err := m.ReadFull(context.Background(), "IFR-1406")
	if err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if got.Readable != "IFR-1406" || got.Summary != "mealie down" || got.Project != "IFR" {
		t.Fatalf("core fields wrong: %+v", got)
	}
	if got.Fields["State"] != "Resolved" {
		t.Fatalf("enum value shape: want Resolved, got %q", got.Fields["State"])
	}
	if got.Fields["Assignee"] != "K P" || got.Assignee != "K P" {
		t.Fatalf("user value shape: want K P, got field=%q assignee=%q", got.Fields["Assignee"], got.Assignee)
	}
	if got.Fields["Notes"] != "restarted the guest" {
		t.Fatalf("text value shape: got %q", got.Fields["Notes"])
	}
	if got.Fields["Sprints"] != "S1, S2" {
		t.Fatalf("multi-value shape: want \"S1, S2\", got %q", got.Fields["Sprints"])
	}
	if len(got.Comments) != 1 || got.Comments[0].Text != "root cause: OOM" {
		t.Fatalf("comments: a DELETED comment must be omitted; got %+v", got.Comments)
	}
	if len(got.Links) != 1 || got.Links[0].Type != "Relates" || got.Links[0].IssueID != "IFR-1400" {
		t.Fatalf("links: %+v", got.Links)
	}
	if len(got.Attachments) != 1 || got.Attachments[0].Name != "log.txt" {
		t.Fatalf("attachments: %+v", got.Attachments)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "incident" {
		t.Fatalf("tags: %+v", got.Tags)
	}
	if got.Resolved == nil || !got.Resolved.Equal(time.UnixMilli(1751335200000).UTC()) {
		t.Fatalf("resolved timestamp: %v", got.Resolved)
	}
	if a := f.req(t, 0).auth; a != "Bearer tok-123" {
		t.Fatalf("every call must carry the resolved bearer token, got %q", a)
	}
}

// TestSearchSendsTheQueryAndDecodesResults is the oracle for BOTH remaining gaps — taking orders and
// reading history. Without it TG can only fetch an issue whose id it was already handed.
func TestSearchSendsTheQueryAndDecodesResults(t *testing.T) {
	f, m, done := newFakeYT(t, map[string]string{"GET /api/issues": "[" + oneRichIssue + "]"})
	defer done()

	got, err := m.Search(context.Background(), "project: IFR summary: mealie #Resolved", 25)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 || got[0].Readable != "IFR-1406" {
		t.Fatalf("search results: %+v", got)
	}
	r := f.req(t, 0)
	if !strings.Contains(r.rawQuery, "query=project%3A+IFR") {
		t.Fatalf("the query must reach YouTrack verbatim, got %q", r.rawQuery)
	}
	if !strings.Contains(r.rawQuery, "%24top=25") {
		t.Fatalf("limit must be sent as $top, got %q", r.rawQuery)
	}
	if !strings.Contains(r.rawQuery, "comments") {
		t.Fatalf("search must request comments — history without comments is not history: %q", r.rawQuery)
	}
}

// TestCreateFilesAnIssueWithFieldsAndTags is the oracle for TG filing its own work.
func TestCreateFilesAnIssueWithFieldsAndTags(t *testing.T) {
	f, m, done := newFakeYT(t, map[string]string{"POST /api/issues": oneRichIssue})
	defer done()

	got, err := m.Create(context.Background(), NewIssue{
		Project: "IFR", Summary: "mealie down", Description: "guest stopped",
		Fields: map[string]string{"Priority": "Major"}, Tags: []string{"incident"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.Readable != "IFR-1406" {
		t.Fatalf("create must return the stored issue, got %+v", got)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(f.req(t, 0).body), &payload); err != nil {
		t.Fatalf("create payload: %v", err)
	}
	if p, _ := payload["project"].(map[string]any); p == nil || p["shortName"] != "IFR" {
		t.Fatalf("project must be sent by short name: %v", payload["project"])
	}
	if payload["summary"] != "mealie down" || payload["description"] != "guest stopped" {
		t.Fatalf("summary/description missing: %v", payload)
	}
	if payload["customFields"] == nil {
		t.Fatal("custom fields must be sent on create")
	}
	// The tag is a second call (YouTrack models tags as a command), but the caller sees one Create.
	if len(f.reqs) != 2 || f.req(t, 1).path != "/api/commands" {
		t.Fatalf("tag must be applied via the command endpoint, got %d reqs", len(f.reqs))
	}
	if !strings.Contains(f.req(t, 1).body, "tag incident") {
		t.Fatalf("tag command body: %q", f.req(t, 1).body)
	}
}

// TestCreateRefusesIncompleteRequests keeps the failure local instead of shipping a 400 round-trip.
func TestCreateRefusesIncompleteRequests(t *testing.T) {
	_, m, done := newFakeYT(t, nil)
	defer done()
	if _, err := m.Create(context.Background(), NewIssue{Summary: "x"}); err == nil {
		t.Fatal("create without a project must fail")
	}
	if _, err := m.Create(context.Background(), NewIssue{Project: "IFR"}); err == nil {
		t.Fatal("create without a summary must fail")
	}
}

// TestUpdateNeverBlanksUnmentionedFields is the data-loss oracle. IssueUpdate uses pointers so that
// "not mentioned" and "set to empty" are different requests; a value-typed struct would silently blank a
// summary every time a caller only wanted to change a field.
func TestUpdateNeverBlanksUnmentionedFields(t *testing.T) {
	f, m, done := newFakeYT(t, nil)
	defer done()

	if err := m.Update(context.Background(), "IFR-1406", IssueUpdate{Fields: map[string]string{"State": "Open"}}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(f.req(t, 0).body), &payload); err != nil {
		t.Fatalf("update payload: %v", err)
	}
	if _, present := payload["summary"]; present {
		t.Fatalf("an update that did not mention summary must NOT send one: %v", payload)
	}
	if _, present := payload["description"]; present {
		t.Fatalf("an update that did not mention description must NOT send one: %v", payload)
	}

	// An empty update must issue NO request at all, rather than a write that clears everything.
	before := len(f.reqs)
	if err := m.Update(context.Background(), "IFR-1406", IssueUpdate{}); err != nil {
		t.Fatalf("empty update: %v", err)
	}
	if len(f.reqs) != before {
		t.Fatal("an empty update must not issue a write")
	}
}

// TestCommentsRoundTrip covers the comment lifecycle beyond the four-verb Comment sink.
func TestCommentsRoundTrip(t *testing.T) {
	list := `[{"id":"c1","text":"first","created":1751329000000,"author":{"login":"kp","fullName":"K P"}},
	          {"id":"c2","text":"gone","created":1751329500000,"deleted":true,"author":{"login":"kp"}}]`
	f, m, done := newFakeYT(t, map[string]string{"GET /api/issues/IFR-1406/comments": list})
	defer done()

	got, err := m.Comments(context.Background(), "IFR-1406")
	if err != nil {
		t.Fatalf("Comments: %v", err)
	}
	if len(got) != 1 || got[0].Author != "K P" || got[0].Text != "first" {
		t.Fatalf("comments: deleted must be omitted and author resolved; got %+v", got)
	}
	if err := m.UpdateComment(context.Background(), "IFR-1406", "c1", "edited"); err != nil {
		t.Fatalf("UpdateComment: %v", err)
	}
	if !strings.Contains(f.req(t, 1).body, "edited") || f.req(t, 1).path != "/api/issues/IFR-1406/comments/c1" {
		t.Fatalf("update comment routed wrong: %+v", f.req(t, 1))
	}
	if err := m.DeleteComment(context.Background(), "IFR-1406", "c1"); err != nil {
		t.Fatalf("DeleteComment: %v", err)
	}
	if f.req(t, 2).method != http.MethodDelete {
		t.Fatalf("delete comment must use DELETE, got %s", f.req(t, 2).method)
	}
}

// TestLinksTagsAndCommandsResolveByName pins the design choice that keeps TG out of YouTrack's internals:
// links and tags go through the command endpoint, so targets resolve by NAME against the project's own
// schema and TG never has to model internal bundle ids (which it would get wrong per-project).
func TestLinksTagsAndCommandsResolveByName(t *testing.T) {
	links := `[{"direction":"OUTWARD","linkType":{"name":"Depends on"},"issues":[{"idReadable":"IFR-1"}]}]`
	f, m, done := newFakeYT(t, map[string]string{
		"GET /api/issues/IFR-2/links": links,
		"GET /api/issues/IFR-2/tags":  `[{"id":"t1","name":"incident"},{"id":"t2","name":"synthetic"}]`,
	})
	defer done()

	got, err := m.Links(context.Background(), "IFR-2")
	if err != nil || len(got) != 1 || got[0].Type != "Depends on" || got[0].IssueID != "IFR-1" {
		t.Fatalf("Links: %v %+v", err, got)
	}
	tags, err := m.Tags(context.Background(), "IFR-2")
	if err != nil || len(tags) != 2 || tags[1] != "synthetic" {
		t.Fatalf("Tags: %v %+v", err, tags)
	}
	if err := m.Link(context.Background(), "IFR-2", "Relates", "IFR-3"); err != nil {
		t.Fatalf("Link: %v", err)
	}
	if !strings.Contains(f.reqs[len(f.reqs)-1].body, "Relates IFR-3") {
		t.Fatalf("link command: %q", f.reqs[len(f.reqs)-1].body)
	}
	if err := m.Unlink(context.Background(), "IFR-2", "Relates", "IFR-3"); err != nil {
		t.Fatalf("Unlink: %v", err)
	}
	if !strings.Contains(f.reqs[len(f.reqs)-1].body, "remove Relates IFR-3") {
		t.Fatalf("unlink command: %q", f.reqs[len(f.reqs)-1].body)
	}
	if err := m.RemoveTag(context.Background(), "IFR-2", "synthetic"); err != nil {
		t.Fatalf("RemoveTag: %v", err)
	}
	if !strings.Contains(f.reqs[len(f.reqs)-1].body, "remove tag synthetic") {
		t.Fatalf("remove tag command: %q", f.reqs[len(f.reqs)-1].body)
	}
}

// TestAttachUploadsMultipartWithTheSameAuth covers the ONE request path that cannot go through `do`.
func TestAttachUploadsMultipartWithTheSameAuth(t *testing.T) {
	f, m, done := newFakeYT(t, map[string]string{
		"POST /api/issues/IFR-1406/attachments": `[{"id":"a9","name":"triage.log","size":11,"mimeType":"text/plain"}]`,
	})
	defer done()

	got, err := m.Attach(context.Background(), "IFR-1406", "triage.log", []byte("hello world"))
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if got.ID != "a9" || got.Name != "triage.log" {
		t.Fatalf("attachment: %+v", got)
	}
	r := f.req(t, 0)
	if !strings.HasPrefix(r.ctype, "multipart/form-data") {
		t.Fatalf("upload must be multipart, got %q", r.ctype)
	}
	if r.auth != "Bearer tok-123" {
		t.Fatalf("the upload path must resolve the SAME token (INV-13), got %q", r.auth)
	}
	if !strings.Contains(r.body, "hello world") || !strings.Contains(r.body, "triage.log") {
		t.Fatal("multipart body must carry the file name and content")
	}
}

// TestProjectsUsersAndWorkItems covers the remaining surface an operator needs before granting write
// access — including "who does TG act as", which Me answers honestly.
func TestProjectsUsersAndWorkItems(t *testing.T) {
	_, m, done := newFakeYT(t, map[string]string{
		"GET /api/admin/projects":                         `[{"id":"0-1","name":"Infrastructure","shortName":"IFR"}]`,
		"GET /api/users/me":                               `{"id":"u1","login":"tg-bot","fullName":"TG Bot","email":"tg@x"}`,
		"GET /api/users":                                  `[{"id":"u1","login":"tg-bot","fullName":"TG Bot"}]`,
		"GET /api/issues/IFR-1406/timeTracking/workItems": `[{"id":"w1","text":"triage","duration":{"minutes":15},"date":1751329000000,"author":{"login":"kp"},"type":{"name":"Development"}}]`,
	})
	defer done()

	ps, err := m.Projects(context.Background())
	if err != nil || len(ps) != 1 || ps[0].ShortName != "IFR" {
		t.Fatalf("Projects: %v %+v", err, ps)
	}
	me, err := m.Me(context.Background())
	if err != nil || me.Login != "tg-bot" || me.Name != "TG Bot" {
		t.Fatalf("Me: %v %+v", err, me)
	}
	us, err := m.Users(context.Background(), "tg")
	if err != nil || len(us) != 1 {
		t.Fatalf("Users: %v %+v", err, us)
	}
	ws, err := m.WorkItems(context.Background(), "IFR-1406")
	if err != nil || len(ws) != 1 || ws[0].Minutes != 15 || ws[0].TypeName != "Development" {
		t.Fatalf("WorkItems: %v %+v", err, ws)
	}
	if err := m.AddWorkItem(context.Background(), "IFR-1406", 0, "x"); err == nil {
		t.Fatal("a work item with non-positive minutes must be refused locally")
	}
}

// TestEveryVerbRefusesAnEmptyIssueID stops a malformed id from becoming a request against a URL that
// happens to mean something else (e.g. /api/issues/ is the LIST endpoint — a DELETE there is not a no-op).
func TestEveryVerbRefusesAnEmptyIssueID(t *testing.T) {
	f, m, done := newFakeYT(t, nil)
	defer done()
	ctx := context.Background()

	checks := map[string]error{
		"ReadFull":      func() error { _, e := m.ReadFull(ctx, ""); return e }(),
		"Update":        m.Update(ctx, "", IssueUpdate{Summary: strptr("x")}),
		"DeleteIssue":   m.DeleteIssue(ctx, ""),
		"Comments":      func() error { _, e := m.Comments(ctx, ""); return e }(),
		"UpdateComment": m.UpdateComment(ctx, "", "c1", "x"),
		"DeleteComment": m.DeleteComment(ctx, "IFR-1", ""),
		"Links":         func() error { _, e := m.Links(ctx, ""); return e }(),
		"Tags":          func() error { _, e := m.Tags(ctx, ""); return e }(),
		"AddTag":        m.AddTag(ctx, "", "t"),
		"Attachments":   func() error { _, e := m.Attachments(ctx, ""); return e }(),
		"Attach":        func() error { _, e := m.Attach(ctx, "", "n", nil); return e }(),
		"WorkItems":     func() error { _, e := m.WorkItems(ctx, ""); return e }(),
		"AddWorkItem":   m.AddWorkItem(ctx, "", 5, "x"),
		"Link":          m.Link(ctx, "", "Relates", "IFR-2"),
	}
	for name, err := range checks {
		if err == nil {
			t.Errorf("%s must refuse an empty issue id locally", name)
		}
	}
	if len(f.reqs) != 0 {
		t.Fatalf("no request may leave the process for an empty id, got %d", len(f.reqs))
	}
}

// TestNonSuccessStatusIsAnError keeps failures loud: a 4xx/5xx that decoded to a zero value would look
// like "the issue has no comments" rather than "the call failed".
func TestNonSuccessStatusIsAnError(t *testing.T) {
	f, m, done := newFakeYT(t, nil)
	defer done()
	f.status = http.StatusForbidden

	if _, err := m.Search(context.Background(), "project: IFR", 10); err == nil {
		t.Fatal("a 403 must surface as an error, never as an empty result set")
	}
	if _, err := m.ReadFull(context.Background(), "IFR-1"); err == nil {
		t.Fatal("a 403 must surface as an error from ReadFull")
	}
}

func strptr(s string) *string { return &s }

// TestReadOnlyRefusesEveryWriteAndSendsNothing is the CONTAMINATION control.
//
// TG reads the shared YouTrack corpus to equalize incident memory against the predecessor — but the
// predecessor is DRIVEN BY those same issues and reads them. One TG comment on a live incident is
// readable by the other arm mid-campaign, and no later analysis can undo that. So the read-only posture
// must be a control, not a promise: every mutating verb refuses, and — the part that matters — NO REQUEST
// LEAVES THE PROCESS, so there is nothing for a server-side bug or a retry to turn into a write.
func TestReadOnlyRefusesEveryWriteAndSendsNothing(t *testing.T) {
	f, m, done := newFakeYT(t, map[string]string{"GET /api/issues/IFR-1406": oneRichIssue})
	defer done()
	m.readOnly = true
	ctx := context.Background()

	writes := map[string]error{
		"Create":          func() error { _, e := m.Create(ctx, NewIssue{Project: "IFR", Summary: "x"}); return e }(),
		"Update":          m.Update(ctx, "IFR-1406", IssueUpdate{Summary: strptr("x")}),
		"SetField":        m.SetField(ctx, "IFR-1406", "State", "Open"),
		"DeleteIssue":     m.DeleteIssue(ctx, "IFR-1406"),
		"UpdateComment":   m.UpdateComment(ctx, "IFR-1406", "c1", "x"),
		"DeleteComment":   m.DeleteComment(ctx, "IFR-1406", "c1"),
		"Link":            m.Link(ctx, "IFR-1406", "Relates", "IFR-1"),
		"Unlink":          m.Unlink(ctx, "IFR-1406", "Relates", "IFR-1"),
		"AddTag":          m.AddTag(ctx, "IFR-1406", "t"),
		"RemoveTag":       m.RemoveTag(ctx, "IFR-1406", "t"),
		"Attach":          func() error { _, e := m.Attach(ctx, "IFR-1406", "f", []byte("x")); return e }(),
		"AddWorkItem":     m.AddWorkItem(ctx, "IFR-1406", 5, "x"),
		"TransitionState": m.TransitionState(ctx, "IFR-1406", "resolved"),
		"Comment":         m.Comment(ctx, "IFR-1406", "hello"),
	}
	for name, err := range writes {
		if !errors.Is(err, ErrReadOnly) {
			t.Errorf("%s must refuse with ErrReadOnly, got %v", name, err)
		}
	}
	if len(f.reqs) != 0 {
		t.Fatalf("a read-only module must send NOTHING for writes, got %d request(s): %+v", len(f.reqs), f.reqs)
	}

	// Reads must still work — a read-only module that cannot read is useless for the memory it exists to supply.
	if _, err := m.ReadFull(ctx, "IFR-1406"); err != nil {
		t.Fatalf("read-only must still READ: %v", err)
	}
	if len(f.reqs) != 1 {
		t.Fatalf("the read must have issued exactly one request, got %d", len(f.reqs))
	}
}

// TG-490: CreateEntry (the adapters/tracker.EntryCreator capability) files through the SAME
// authenticated Create the console write path uses, maps the READABLE id as the correlation key,
// refuses empty project/summary, and — like every mutating verb — refuses outright on a
// read-only module (the deployment arms writes via TG_YOUTRACK_WRITES; the creator ticker's
// per-pass log makes a dark-armed misconfig visible, never silent).
func TestCreateEntryFilesThroughTheWritePath(t *testing.T) {
	f, m, done := newFakeYT(t, map[string]string{
		"POST /api/issues": `{"id":"3-77","idReadable":"TGOPS-12","summary":"[critical] web01: NginxDown"}`,
	})
	defer done()

	iss, err := m.CreateEntry(context.Background(), "TGOPS", "[critical] web01: NginxDown", "body")
	if err != nil {
		t.Fatalf("create entry: %v", err)
	}
	if iss.ID != "TGOPS-12" {
		t.Fatalf("the READABLE id is the correlation key, got %q", iss.ID)
	}
	if len(f.reqs) != 1 || f.reqs[0].method != "POST" || f.reqs[0].path != "/api/issues" {
		t.Fatalf("must file via POST /api/issues, got %+v", f.reqs)
	}
	if !strings.Contains(f.lastBody, `"shortName":"TGOPS"`) || !strings.Contains(f.lastBody, "NginxDown") {
		t.Fatalf("the payload must carry project+summary, got %s", f.lastBody)
	}

	if _, err := m.CreateEntry(context.Background(), " ", "s", ""); err == nil {
		t.Fatal("empty project must refuse")
	}
	if _, err := m.CreateEntry(context.Background(), "TGOPS", " ", ""); err == nil {
		t.Fatal("empty summary must refuse")
	}

	ro := New("http://unused", config.SecretRef("env:YT_TEST_TOKEN"), WithReadOnly())
	if _, err := ro.CreateEntry(context.Background(), "TGOPS", "s", ""); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("a read-only module must refuse entry creation with ErrReadOnly, got %v", err)
	}
}

// TG-490 round-3: SearchEntry — the resolver's load-bearing de-dup read. The drill pins the
// query SHAPE (project scope, the QUOTED incident key, and the EXPLICIT `sort by: created desc`
// the adopt-arm's found[0]-is-newest assumption rests on), the empty-id filtering, and the
// malformed-response error path. What no offline drill can pin — whether the live index answers
// a description-phrase query — is the arming step's e2e oracle, stated on the ticket.
func TestSearchEntryPinsQueryShapeAndOrdering(t *testing.T) {
	f, m, done := newFakeYT(t, map[string]string{
		"GET /api/issues": `[{"idReadable":"TGOPS-7","summary":"newest"},{"idReadable":"","summary":"dropped"},{"idReadable":"TGOPS-3","summary":"older"}]`,
	})
	defer done()

	found, err := m.SearchEntry(context.Background(), "TGOPS", "librenms-dc1-42")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(found) != 2 || found[0].ID != "TGOPS-7" || found[1].ID != "TGOPS-3" {
		t.Fatalf("ids mapped in response order with empty ids dropped, got %+v", found)
	}
	if len(f.reqs) != 1 {
		t.Fatalf("one request, got %d", len(f.reqs))
	}
	q, uerr := neturl.QueryUnescape(f.reqs[0].rawQuery)
	if uerr != nil {
		t.Fatal(uerr)
	}
	for _, must := range []string{"project: TGOPS", `"librenms-dc1-42"`, "sort by: created desc"} {
		if !strings.Contains(q, must) {
			t.Fatalf("the query must carry %q (the adopt-arm's newest-first rests on the sort), got %q", must, q)
		}
	}

	if _, err := m.SearchEntry(context.Background(), "", "k"); err == nil {
		t.Fatal("empty project must refuse")
	}
	if _, err := m.SearchEntry(context.Background(), "TGOPS", " "); err == nil {
		t.Fatal("empty incident key must refuse")
	}

	bad, mm, done2 := newFakeYT(t, map[string]string{"GET /api/issues": `{"not":"an array"}`})
	defer done2()
	_ = bad
	if _, err := mm.SearchEntry(context.Background(), "TGOPS", "k"); err == nil {
		t.Fatal("a malformed response must error (the resolver then HOLDS, never creates)")
	}
}
