package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/forensic"
)

func TestParseSince_DurationOrInstantOrRefused(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	// duration form: "72h" ⇒ 72h before now
	got, err := parseSince("72h", now)
	if err != nil {
		t.Fatalf("duration: %v", err)
	}
	if want := now.Add(-72 * time.Hour); !got.Equal(want) {
		t.Errorf("72h ⇒ %s, want %s", got, want)
	}
	// RFC3339 form
	got, err = parseSince("2026-08-06T00:00:00Z", now)
	if err != nil {
		t.Fatalf("rfc3339: %v", err)
	}
	if want := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("instant ⇒ %s, want %s", got, want)
	}
	// empty is refused — an unbounded window is a dump, not an answer
	if _, err := parseSince("  ", now); err == nil {
		t.Error("empty -since must be refused")
	}
}

// The renderer shows the ordered events AND the honesty footers — a truncated/partial read must be visibly
// partial, never mistakable for a complete narrative.
func TestRenderTimeline_ShowsEventsAndHonestyFooters(t *testing.T) {
	w := forensic.Window{
		From: time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 8, 6, 1, 0, 0, 0, time.UTC),
	}
	read := db.ForensicRead{
		Events: []forensic.Event{
			{At: time.Date(2026, 8, 6, 0, 5, 0, 0, time.UTC), Source: forensic.SourceIngest, Host: "dc1pve03", SubjectRef: "ext-1", Kind: "alert", Detail: "host down"},
			{At: time.Date(2026, 8, 6, 0, 6, 0, 0, time.UTC), Source: forensic.SourceLedger, SubjectRef: "ext-1", Kind: "classify:AUTO", Detail: "deny"},
		},
		Truncated: []string{"agent_step"},
		Dropped:   3,
	}
	var buf bytes.Buffer
	renderTimeline(&buf, w, "dc1pve03", read)
	out := buf.String()
	for _, want := range []string{
		"events=2",
		"blast radius:", "dc1pve03", // the host appears on its row + the blast-radius line
		"classify:AUTO: deny",       // a ledger decision, verbatim Kind + Detail
		"INCOMPLETE:", "agent_step", // the truncation honesty
		"3 event(s) dropped", // the dropped honesty
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q\n---\n%s", want, out)
		}
	}
}

// A clean, complete read prints NO INCOMPLETE / dropped footer (the honesty appears only when warranted), and
// an unscoped run renders as (all hosts).
func TestRenderTimeline_CleanReadHasNoPartialFooter(t *testing.T) {
	w := forensic.Window{From: time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)}
	var buf bytes.Buffer
	renderTimeline(&buf, w, "", db.ForensicRead{Events: []forensic.Event{
		{At: time.Date(2026, 8, 6, 0, 5, 0, 0, time.UTC), Source: forensic.SourceLedger, Kind: "x", Detail: "y"},
	}})
	out := buf.String()
	if strings.Contains(out, "INCOMPLETE") || strings.Contains(out, "dropped") {
		t.Errorf("a complete read must not print a partial footer:\n%s", out)
	}
	if !strings.Contains(out, "host=(all hosts)") {
		t.Errorf("no -host should render as (all hosts):\n%s", out)
	}
	if !strings.Contains(out, "window=[2026-08-06T00:00:00Z, now)") {
		t.Errorf("an open-ended -until should render 'now':\n%s", out)
	}
}
