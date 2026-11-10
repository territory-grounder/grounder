// The FULL YouTrack surface (TG-238), beyond the four-verb tracker.Tracker contract.
//
// WHY THIS EXISTS. adapters/tracker.Tracker is deliberately four verbs — Open, Read, TransitionState,
// Comment — because the session lifecycle must never learn which backend it runs on (INV-18). That
// contract is exactly right for "annotate the issue you were handed" and is left untouched here: Jira and
// ServiceNow keep satisfying it unchanged, and nothing in this file widens it.
//
// But four verbs cannot express three things TG needs:
//
//  1. FILE ITS OWN WORK. There was no Create at all, so TG could annotate an issue a human opened and
//     never open one itself.
//  2. TAKE ORDERS. Read fetches ONE issue BY ID. With no search, TG could not discover work addressed to
//     it; something else always had to hand it an id.
//  3. REMEMBER. This is the load-bearing one. The predecessor's single biggest measured advantage is
//     incident memory — it recognizes a recurring incident where TG re-derives every one from scratch —
//     and its memory lives in YouTrack: ~18,220 logged actions with human comments and resolutions. With
//     no query path, TG could not read that history, so a head-to-head measured TG's few weeks of
//     session_triage against the predecessor's production lifetime. That is a CONFOUND of the same class
//     as running the two arms on different models: it conflates design quality with deployment age.
//     Equalizing the evidence available to both arms is what makes the comparison apples-to-apples.
//
// Everything here is therefore additive: a richer type set and the verbs YouTrack actually offers, built
// on the same authenticated `do` used by the four-verb path, so token handling (INV-13) has ONE
// implementation and no second auth story.
package youtrack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------------------------------
// Types. These are the RICH views — tracker.Issue stays the narrow correlation anchor the lifecycle uses.
// ---------------------------------------------------------------------------------------------------

// RichIssue is a YouTrack issue with everything TG can learn about it in one read.
type RichIssue struct {
	ID          string            `json:"id"`
	Readable    string            `json:"readable_id"`
	Summary     string            `json:"summary"`
	Description string            `json:"description"`
	Project     string            `json:"project"`
	Reporter    string            `json:"reporter"`
	Assignee    string            `json:"assignee"`
	Created     time.Time         `json:"created"`
	Updated     time.Time         `json:"updated"`
	Resolved    *time.Time        `json:"resolved,omitempty"`
	Tags        []string          `json:"tags,omitempty"`
	Fields      map[string]string `json:"fields,omitempty"`
	Comments    []Comment         `json:"comments,omitempty"`
	Links       []Link            `json:"links,omitempty"`
	Attachments []Attachment      `json:"attachments,omitempty"`
}

// Comment is one comment on an issue — for the history use-case this is the payload that matters, since a
// resolution is usually written by a human in a comment rather than in a field.
type Comment struct {
	ID      string    `json:"id"`
	Author  string    `json:"author"`
	Text    string    `json:"text"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated,omitempty"`
}

// Link is a typed relationship to another issue (relates / depends on / duplicates / subtask).
type Link struct {
	Type      string `json:"type"`
	Direction string `json:"direction"` // OUTWARD | INWARD | BOTH
	IssueID   string `json:"issue_id"`
}

// Attachment is a file on an issue.
type Attachment struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	MIME string `json:"mime_type,omitempty"`
	URL  string `json:"url,omitempty"`
}

// Project is a YouTrack project.
type Project struct {
	ID        string `json:"id"`
	ShortName string `json:"short_name"`
	Name      string `json:"name"`
}

// User is a YouTrack account.
type User struct {
	ID    string `json:"id"`
	Login string `json:"login"`
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
}

// WorkItem is a time-tracking entry.
type WorkItem struct {
	ID       string    `json:"id"`
	Author   string    `json:"author"`
	Text     string    `json:"text,omitempty"`
	Minutes  int       `json:"minutes"`
	Date     time.Time `json:"date"`
	TypeName string    `json:"type,omitempty"`
}

// NewIssue is a creation request. Project and Summary are required by YouTrack; everything else is
// optional and omitted from the payload when empty.
type NewIssue struct {
	Project     string            // short name (e.g. "TG") or internal id
	Summary     string            //
	Description string            //
	Fields      map[string]string // custom fields by NAME, e.g. {"Priority": "Major"}
	Tags        []string          //
}

// IssueUpdate carries the mutable parts of an issue. A nil/empty member is left untouched, so an update
// never silently blanks a field the caller did not mention.
type IssueUpdate struct {
	Summary     *string
	Description *string
	Fields      map[string]string
}

// ---------------------------------------------------------------------------------------------------
// Wire shapes. Kept unexported: YouTrack's JSON is deeply nested and its shape is not TG's problem.
// ---------------------------------------------------------------------------------------------------

type ytUser struct {
	ID    string `json:"id"`
	Login string `json:"login"`
	Name  string `json:"fullName"`
	Email string `json:"email"`
}

type ytFieldValue struct {
	Name     string `json:"name"`
	Login    string `json:"login"`
	FullName string `json:"fullName"`
	Text     string `json:"text"`
	Presel   string `json:"presentation"`
}

// ytRichField is a custom field whose value is kept RAW: unlike state.go's narrow ytCustomField (which
// only ever reads State.value.name), the rich read must survive every value shape YouTrack uses — enum
// element, user, text, number, or an array for a multi-value field.
type ytRichField struct {
	Name  string          `json:"name"`
	Value json.RawMessage `json:"value"`
}

type ytComment struct {
	ID      string  `json:"id"`
	Text    string  `json:"text"`
	Author  ytUser  `json:"author"`
	Created int64   `json:"created"`
	Updated int64   `json:"updated"`
	Deleted bool    `json:"deleted"`
	Issue   *ytBare `json:"issue,omitempty"`
}

type ytBare struct {
	ID       string `json:"id"`
	Readable string `json:"idReadable"`
}

type ytLink struct {
	Direction string                `json:"direction"`
	LinkType  struct{ Name string } `json:"linkType"`
	Issues    []ytBare              `json:"issues"`
}

type ytAttachment struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	MIME string `json:"mimeType"`
	URL  string `json:"url"`
}

type ytTag struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type ytRichIssue struct {
	ID          string                     `json:"id"`
	Readable    string                     `json:"idReadable"`
	Summary     string                     `json:"summary"`
	Description string                     `json:"description"`
	Created     int64                      `json:"created"`
	Updated     int64                      `json:"updated"`
	Resolved    *int64                     `json:"resolved"`
	Reporter    ytUser                     `json:"reporter"`
	Project     struct{ ShortName string } `json:"project"`
	Tags        []ytTag                    `json:"tags"`
	Fields      []ytRichField              `json:"customFields"`
	Comments    []ytComment                `json:"comments"`
	Links       []ytLink                   `json:"links"`
	Attachments []ytAttachment             `json:"attachments"`
}

// richFields is the field selector for a FULL issue read. YouTrack returns only what is asked for, so an
// incomplete selector is silently missing data rather than an error — which is why it lives in one
// constant instead of being spelled out per call site.
const richFields = "id,idReadable,summary,description,created,updated,resolved," +
	"reporter(id,login,fullName,email),project(shortName),tags(id,name)," +
	"customFields(name,value(name,login,fullName,text,presentation))," +
	"comments(id,text,created,updated,author(id,login,fullName))," +
	"links(direction,linkType(name),issues(id,idReadable))," +
	"attachments(id,name,size,mimeType,url)"

func ytTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

func userName(u ytUser) string {
	if u.Name != "" {
		return u.Name
	}
	return u.Login
}

// fieldText renders a custom field's value as text regardless of which of YouTrack's value shapes it
// arrived in (enum bundle element, user, text, number, or a bare scalar).
func fieldText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var v ytFieldValue
	if err := json.Unmarshal(raw, &v); err == nil {
		switch {
		case v.Name != "":
			return v.Name
		case v.FullName != "":
			return v.FullName
		case v.Login != "":
			return v.Login
		case v.Text != "":
			return v.Text
		case v.Presel != "":
			return v.Presel
		}
	}
	// Arrays (multi-value fields) and scalars.
	var arr []ytFieldValue
	if err := json.Unmarshal(raw, &arr); err == nil && len(arr) > 0 {
		parts := make([]string, 0, len(arr))
		for _, a := range arr {
			if n := (ytFieldValue{Name: a.Name, Login: a.Login, FullName: a.FullName, Text: a.Text, Presel: a.Presel}); true {
				switch {
				case n.Name != "":
					parts = append(parts, n.Name)
				case n.FullName != "":
					parts = append(parts, n.FullName)
				case n.Login != "":
					parts = append(parts, n.Login)
				case n.Text != "":
					parts = append(parts, n.Text)
				}
			}
		}
		return strings.Join(parts, ", ")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return strings.Trim(string(raw), `"`)
}

func (r ytRichIssue) toRich() RichIssue {
	out := RichIssue{
		ID: r.ID, Readable: r.Readable, Summary: r.Summary, Description: r.Description,
		Project: r.Project.ShortName, Reporter: userName(r.Reporter),
		Created: ytTime(r.Created), Updated: ytTime(r.Updated),
		Fields: map[string]string{},
	}
	if r.Resolved != nil && *r.Resolved > 0 {
		t := ytTime(*r.Resolved)
		out.Resolved = &t
	}
	for _, t := range r.Tags {
		out.Tags = append(out.Tags, t.Name)
	}
	for _, f := range r.Fields {
		txt := fieldText(f.Value)
		out.Fields[f.Name] = txt
		if strings.EqualFold(f.Name, "Assignee") && txt != "" {
			out.Assignee = txt
		}
	}
	for _, c := range r.Comments {
		if c.Deleted {
			continue
		}
		out.Comments = append(out.Comments, Comment{
			ID: c.ID, Author: userName(c.Author), Text: c.Text,
			Created: ytTime(c.Created), Updated: ytTime(c.Updated),
		})
	}
	for _, l := range r.Links {
		for _, iss := range l.Issues {
			id := iss.Readable
			if id == "" {
				id = iss.ID
			}
			out.Links = append(out.Links, Link{Type: l.LinkType.Name, Direction: l.Direction, IssueID: id})
		}
	}
	for _, a := range r.Attachments {
		out.Attachments = append(out.Attachments, Attachment{ID: a.ID, Name: a.Name, Size: a.Size, MIME: a.MIME, URL: a.URL})
	}
	return out
}

// ---------------------------------------------------------------------------------------------------
// Issues: search, read, create, update, delete.
// ---------------------------------------------------------------------------------------------------

// Search runs a YouTrack query and returns matching issues, newest first as YouTrack orders them.
//
// This is the verb that makes both "take orders" and "remember" possible: `query` is YouTrack's own query
// language, so a caller can ask for work addressed to TG ("project: TG State: Open tag: for-tg") or for
// the history behind a recurring incident ("project: IFR summary: mealie #Resolved").
//
// limit <= 0 means the backend default; the cap is applied by YouTrack via $top.
func (m *Module) Search(ctx context.Context, query string, limit int) ([]RichIssue, error) {
	q := url.Values{}
	q.Set("query", query)
	q.Set("fields", richFields)
	if limit > 0 {
		q.Set("$top", strconv.Itoa(limit))
	}
	body, err := m.do(ctx, http.MethodGet, "/api/issues?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var raw []ytRichIssue
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("youtrack: malformed search response: %w", err)
	}
	out := make([]RichIssue, 0, len(raw))
	for _, r := range raw {
		out = append(out, r.toRich())
	}
	return out, nil
}

// ReadFull returns one issue with comments, links, tags, attachments and custom fields.
func (m *Module) ReadFull(ctx context.Context, id string) (RichIssue, error) {
	if strings.TrimSpace(id) == "" {
		return RichIssue{}, fmt.Errorf("youtrack: empty issue id")
	}
	body, err := m.do(ctx, http.MethodGet, "/api/issues/"+url.PathEscape(id)+"?fields="+url.QueryEscape(richFields), nil)
	if err != nil {
		return RichIssue{}, err
	}
	var raw ytRichIssue
	if err := json.Unmarshal(body, &raw); err != nil {
		return RichIssue{}, fmt.Errorf("youtrack: malformed issue response: %w", err)
	}
	return raw.toRich(), nil
}

// Create files a new issue and returns it as stored.
func (m *Module) Create(ctx context.Context, in NewIssue) (RichIssue, error) {
	if err := m.guardWrite(); err != nil {
		return RichIssue{}, err
	}
	if strings.TrimSpace(in.Project) == "" {
		return RichIssue{}, fmt.Errorf("youtrack: create requires a project")
	}
	if strings.TrimSpace(in.Summary) == "" {
		return RichIssue{}, fmt.Errorf("youtrack: create requires a summary")
	}
	payload := map[string]any{
		"project": map[string]string{"shortName": in.Project},
		"summary": in.Summary,
	}
	if in.Description != "" {
		payload["description"] = in.Description
	}
	if len(in.Fields) > 0 {
		payload["customFields"] = customFieldPayload(in.Fields)
	}
	body, err := m.do(ctx, http.MethodPost, "/api/issues?fields="+url.QueryEscape(richFields), payload)
	if err != nil {
		return RichIssue{}, err
	}
	var raw ytRichIssue
	if err := json.Unmarshal(body, &raw); err != nil {
		return RichIssue{}, fmt.Errorf("youtrack: malformed create response: %w", err)
	}
	created := raw.toRich()
	// Tags are a separate endpoint in YouTrack; applying them here keeps Create a single call for callers.
	for _, t := range in.Tags {
		if err := m.AddTag(ctx, issueRef(created), t); err != nil {
			return created, fmt.Errorf("youtrack: issue created but tag %q failed: %w", t, err)
		}
		created.Tags = append(created.Tags, t)
	}
	return created, nil
}

// Update changes summary, description and/or custom fields. Members left nil are untouched.
func (m *Module) Update(ctx context.Context, id string, up IssueUpdate) error {
	if err := m.guardWrite(); err != nil {
		return err
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("youtrack: empty issue id")
	}
	payload := map[string]any{}
	if up.Summary != nil {
		payload["summary"] = *up.Summary
	}
	if up.Description != nil {
		payload["description"] = *up.Description
	}
	if len(up.Fields) > 0 {
		payload["customFields"] = customFieldPayload(up.Fields)
	}
	if len(payload) == 0 {
		return nil // nothing asked for is not an error, and must not issue a blanking write
	}
	_, err := m.do(ctx, http.MethodPost, "/api/issues/"+url.PathEscape(id), payload)
	return err
}

// DeleteIssue removes an issue permanently.
func (m *Module) DeleteIssue(ctx context.Context, id string) error {
	if err := m.guardWrite(); err != nil {
		return err
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("youtrack: empty issue id")
	}
	_, err := m.do(ctx, http.MethodDelete, "/api/issues/"+url.PathEscape(id), nil)
	return err
}

// SetField sets one custom field by NAME (State, Priority, Assignee, Type, Subsystem, ...).
func (m *Module) SetField(ctx context.Context, id, field, value string) error {
	return m.Update(ctx, id, IssueUpdate{Fields: map[string]string{field: value}})
}

// customFieldPayload renders name/value pairs into YouTrack's custom-field shape. The $type is
// deliberately omitted: YouTrack resolves the field by name against the project's own schema, so TG never
// has to model which bundle a field belongs to — and cannot get that modelling wrong.
func customFieldPayload(fields map[string]string) []map[string]any {
	out := make([]map[string]any, 0, len(fields))
	for name, val := range fields {
		out = append(out, map[string]any{
			"name":  name,
			"value": map[string]string{"name": val},
		})
	}
	return out
}

func issueRef(i RichIssue) string {
	if i.Readable != "" {
		return i.Readable
	}
	return i.ID
}

// ---------------------------------------------------------------------------------------------------
// Comments.
// ---------------------------------------------------------------------------------------------------

// Comments returns an issue's comments oldest-first as YouTrack stores them. Deleted comments are omitted.
func (m *Module) Comments(ctx context.Context, id string) ([]Comment, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("youtrack: empty issue id")
	}
	body, err := m.do(ctx, http.MethodGet,
		"/api/issues/"+url.PathEscape(id)+"/comments?fields="+url.QueryEscape("id,text,created,updated,deleted,author(id,login,fullName)"), nil)
	if err != nil {
		return nil, err
	}
	var raw []ytComment
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("youtrack: malformed comments response: %w", err)
	}
	out := make([]Comment, 0, len(raw))
	for _, c := range raw {
		if c.Deleted {
			continue
		}
		out = append(out, Comment{ID: c.ID, Author: userName(c.Author), Text: c.Text,
			Created: ytTime(c.Created), Updated: ytTime(c.Updated)})
	}
	return out, nil
}

// UpdateComment edits an existing comment.
func (m *Module) UpdateComment(ctx context.Context, issueID, commentID, body string) error {
	if err := m.guardWrite(); err != nil {
		return err
	}
	if strings.TrimSpace(issueID) == "" || strings.TrimSpace(commentID) == "" {
		return fmt.Errorf("youtrack: empty issue or comment id")
	}
	_, err := m.do(ctx, http.MethodPost,
		"/api/issues/"+url.PathEscape(issueID)+"/comments/"+url.PathEscape(commentID),
		map[string]string{"text": body})
	return err
}

// DeleteComment removes a comment.
func (m *Module) DeleteComment(ctx context.Context, issueID, commentID string) error {
	if err := m.guardWrite(); err != nil {
		return err
	}
	if strings.TrimSpace(issueID) == "" || strings.TrimSpace(commentID) == "" {
		return fmt.Errorf("youtrack: empty issue or comment id")
	}
	_, err := m.do(ctx, http.MethodDelete,
		"/api/issues/"+url.PathEscape(issueID)+"/comments/"+url.PathEscape(commentID), nil)
	return err
}

// ---------------------------------------------------------------------------------------------------
// Links, tags, attachments.
// ---------------------------------------------------------------------------------------------------

// Links returns an issue's typed relationships.
func (m *Module) Links(ctx context.Context, id string) ([]Link, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("youtrack: empty issue id")
	}
	body, err := m.do(ctx, http.MethodGet,
		"/api/issues/"+url.PathEscape(id)+"/links?fields="+url.QueryEscape("direction,linkType(name),issues(id,idReadable)"), nil)
	if err != nil {
		return nil, err
	}
	var raw []ytLink
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("youtrack: malformed links response: %w", err)
	}
	var out []Link
	for _, l := range raw {
		for _, iss := range l.Issues {
			ref := iss.Readable
			if ref == "" {
				ref = iss.ID
			}
			out = append(out, Link{Type: l.LinkType.Name, Direction: l.Direction, IssueID: ref})
		}
	}
	return out, nil
}

// Link relates two issues. linkType is YouTrack's own name ("Relates", "Depends on", "Duplicates",
// "Subtask"); the command syntax is used because it resolves the type against the project's schema
// rather than requiring TG to know internal link-type ids.
func (m *Module) Link(ctx context.Context, fromID, linkType, toID string) error {
	if strings.TrimSpace(fromID) == "" || strings.TrimSpace(toID) == "" {
		return fmt.Errorf("youtrack: empty issue id")
	}
	if strings.TrimSpace(linkType) == "" {
		return fmt.Errorf("youtrack: empty link type")
	}
	return m.applyCommand(ctx, fromID, fmt.Sprintf("%s %s", linkType, toID))
}

// Unlink removes a typed relationship.
func (m *Module) Unlink(ctx context.Context, fromID, linkType, toID string) error {
	if strings.TrimSpace(fromID) == "" || strings.TrimSpace(toID) == "" {
		return fmt.Errorf("youtrack: empty issue id")
	}
	return m.applyCommand(ctx, fromID, fmt.Sprintf("remove %s %s", linkType, toID))
}

// Tags returns an issue's tags.
func (m *Module) Tags(ctx context.Context, id string) ([]string, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("youtrack: empty issue id")
	}
	body, err := m.do(ctx, http.MethodGet, "/api/issues/"+url.PathEscape(id)+"/tags?fields=id,name", nil)
	if err != nil {
		return nil, err
	}
	var raw []ytTag
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("youtrack: malformed tags response: %w", err)
	}
	out := make([]string, 0, len(raw))
	for _, t := range raw {
		out = append(out, t.Name)
	}
	return out, nil
}

// AddTag tags an issue.
func (m *Module) AddTag(ctx context.Context, id, tag string) error {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(tag) == "" {
		return fmt.Errorf("youtrack: empty issue id or tag")
	}
	return m.applyCommand(ctx, id, "tag "+tag)
}

// RemoveTag untags an issue.
func (m *Module) RemoveTag(ctx context.Context, id, tag string) error {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(tag) == "" {
		return fmt.Errorf("youtrack: empty issue id or tag")
	}
	return m.applyCommand(ctx, id, "remove tag "+tag)
}

// applyCommand runs a YouTrack command against one issue. Commands are how YouTrack expresses the
// operations whose targets are resolved by NAME against a project's schema (tags, links, field values),
// which is exactly what keeps TG from having to model internal bundle ids.
func (m *Module) applyCommand(ctx context.Context, issueID, command string) error {
	if err := m.guardWrite(); err != nil {
		return err
	}
	payload := map[string]any{
		"query":  command,
		"issues": []map[string]string{{"idReadable": issueID}},
	}
	_, err := m.do(ctx, http.MethodPost, "/api/commands", payload)
	return err
}

// Attachments lists the files on an issue.
func (m *Module) Attachments(ctx context.Context, id string) ([]Attachment, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("youtrack: empty issue id")
	}
	body, err := m.do(ctx, http.MethodGet,
		"/api/issues/"+url.PathEscape(id)+"/attachments?fields=id,name,size,mimeType,url", nil)
	if err != nil {
		return nil, err
	}
	var raw []ytAttachment
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("youtrack: malformed attachments response: %w", err)
	}
	out := make([]Attachment, 0, len(raw))
	for _, a := range raw {
		out = append(out, Attachment{ID: a.ID, Name: a.Name, Size: a.Size, MIME: a.MIME, URL: a.URL})
	}
	return out, nil
}

// Attach uploads a file to an issue. Multipart, not JSON — so it cannot go through `do`, and this is the
// one place a second request path exists; it resolves the token the same way (INV-13) so there is still
// only one auth story.
func (m *Module) Attach(ctx context.Context, issueID, name string, content []byte) (Attachment, error) {
	if err := m.guardWrite(); err != nil {
		return Attachment{}, err
	}
	if strings.TrimSpace(issueID) == "" || strings.TrimSpace(name) == "" {
		return Attachment{}, fmt.Errorf("youtrack: empty issue id or attachment name")
	}
	token, err := m.tokenRef.Resolve()
	if err != nil {
		return Attachment{}, fmt.Errorf("youtrack: resolve token: %w", err)
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", name)
	if err != nil {
		return Attachment{}, err
	}
	if _, err := fw.Write(content); err != nil {
		return Attachment{}, err
	}
	if err := w.Close(); err != nil {
		return Attachment{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		m.baseURL+"/api/issues/"+url.PathEscape(issueID)+"/attachments?fields=id,name,size,mimeType,url", &buf)
	if err != nil {
		return Attachment{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := m.http.Do(req)
	if err != nil {
		return Attachment{}, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Attachment{}, fmt.Errorf("youtrack: attach %s: status %d: %s", issueID, resp.StatusCode, strings.TrimSpace(string(out)))
	}
	var raw []ytAttachment
	if err := json.Unmarshal(out, &raw); err == nil && len(raw) > 0 {
		return Attachment{ID: raw[0].ID, Name: raw[0].Name, Size: raw[0].Size, MIME: raw[0].MIME, URL: raw[0].URL}, nil
	}
	var one ytAttachment
	if err := json.Unmarshal(out, &one); err != nil {
		return Attachment{}, fmt.Errorf("youtrack: malformed attach response: %w", err)
	}
	return Attachment{ID: one.ID, Name: one.Name, Size: one.Size, MIME: one.MIME, URL: one.URL}, nil
}

// ---------------------------------------------------------------------------------------------------
// Projects, users, work items.
// ---------------------------------------------------------------------------------------------------

// Projects lists the projects the token can see — the discovery step before TG can file anything.
func (m *Module) Projects(ctx context.Context) ([]Project, error) {
	body, err := m.do(ctx, http.MethodGet, "/api/admin/projects?fields=id,name,shortName&$top=500", nil)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		ShortName string `json:"shortName"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("youtrack: malformed projects response: %w", err)
	}
	out := make([]Project, 0, len(raw))
	for _, p := range raw {
		out = append(out, Project{ID: p.ID, ShortName: p.ShortName, Name: p.Name})
	}
	return out, nil
}

// Me returns the account the configured token authenticates as — the honest answer to "who does TG act
// as", which an operator needs before granting it write access.
func (m *Module) Me(ctx context.Context) (User, error) {
	body, err := m.do(ctx, http.MethodGet, "/api/users/me?fields=id,login,fullName,email", nil)
	if err != nil {
		return User{}, err
	}
	var u ytUser
	if err := json.Unmarshal(body, &u); err != nil {
		return User{}, fmt.Errorf("youtrack: malformed user response: %w", err)
	}
	return User{ID: u.ID, Login: u.Login, Name: userName(u), Email: u.Email}, nil
}

// Users searches accounts.
func (m *Module) Users(ctx context.Context, query string) ([]User, error) {
	q := url.Values{}
	q.Set("fields", "id,login,fullName,email")
	if query != "" {
		q.Set("query", query)
	}
	body, err := m.do(ctx, http.MethodGet, "/api/users?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var raw []ytUser
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("youtrack: malformed users response: %w", err)
	}
	out := make([]User, 0, len(raw))
	for _, u := range raw {
		out = append(out, User{ID: u.ID, Login: u.Login, Name: userName(u), Email: u.Email})
	}
	return out, nil
}

// WorkItems lists time-tracking entries on an issue.
func (m *Module) WorkItems(ctx context.Context, id string) ([]WorkItem, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("youtrack: empty issue id")
	}
	body, err := m.do(ctx, http.MethodGet,
		"/api/issues/"+url.PathEscape(id)+"/timeTracking/workItems?fields="+
			url.QueryEscape("id,text,duration(minutes),date,author(id,login,fullName),type(name)"), nil)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		ID       string `json:"id"`
		Text     string `json:"text"`
		Duration struct {
			Minutes int `json:"minutes"`
		} `json:"duration"`
		Date   int64  `json:"date"`
		Author ytUser `json:"author"`
		Type   struct {
			Name string `json:"name"`
		} `json:"type"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("youtrack: malformed work items response: %w", err)
	}
	out := make([]WorkItem, 0, len(raw))
	for _, w := range raw {
		out = append(out, WorkItem{ID: w.ID, Author: userName(w.Author), Text: w.Text,
			Minutes: w.Duration.Minutes, Date: ytTime(w.Date), TypeName: w.Type.Name})
	}
	return out, nil
}

// AddWorkItem logs time against an issue.
func (m *Module) AddWorkItem(ctx context.Context, id string, minutes int, text string) error {
	if err := m.guardWrite(); err != nil {
		return err
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("youtrack: empty issue id")
	}
	if minutes <= 0 {
		return fmt.Errorf("youtrack: work item requires positive minutes, got %d", minutes)
	}
	payload := map[string]any{
		"duration": map[string]any{"minutes": minutes},
		"text":     text,
	}
	_, err := m.do(ctx, http.MethodPost, "/api/issues/"+url.PathEscape(id)+"/timeTracking/workItems", payload)
	return err
}
