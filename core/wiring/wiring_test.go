package wiring

import (
	"context"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

func fixedNow() time.Time { return now }

func noEnv(string) string { return "" }

func validBecause() Because {
	return Because{
		Reason:      "no notifier configured in this deployment",
		Consequence: "governance notices reach no operator",
		// Expiry rides the REAL clock, not the package's fixed `now`: declaration-time validation
		// compares against the wall clock, so a fixed-date expiry rots by calendar (this one first
		// expired 2026-08-31T12:00Z and redded every tree). Same lesson as the tg440 fixtures.
		Owner: "@owner", Ticket: "TG-239", Expiry: time.Now().Add(30 * 24 * time.Hour),
	}
}

type notice struct{}

// TestBindRefusesNilSink — liveness is DERIVED, never asserted.
//
// Killing mutation: add a `live bool` parameter to Bind and trust the caller. Any call site passing true
// beside a nil sink then reports LIVE, which is exactly the shape of the defect this package exists to
// prevent — deps.Notify read as wired for the whole life of the bug.
func TestBindRefusesNilSink(t *testing.T) {
	m := newFor(fixedNow, noEnv)
	var nilSink func(context.Context, notice) error
	got := Bind(m, SeamGovNotify, nilSink)
	if got != nil {
		t.Fatal("Bind must return the value unchanged")
	}
	if got := findingFor(t, m, SeamGovNotify); got == nil || got.State != DarkUnbound {
		t.Fatalf("a nil sink must record DarkUnbound, got %+v", got)
	}

	// And a real one must report live.
	m2 := newFor(fixedNow, noEnv)
	Bind(m2, SeamGovNotify, func(context.Context, notice) error { return nil })
	if got := findingFor(t, m2, SeamGovNotify); got != nil {
		t.Fatalf("a usable sink must yield no dark finding for its seam, got %+v", got)
	}
}

// TestBindRejectsATypedNilInterface covers the subtler nil: a non-nil interface holding a nil pointer is
// a classic Go trap, and a sink shaped that way panics at call time rather than at boot.
func TestBindRejectsATypedNilInterface(t *testing.T) {
	m := newFor(fixedNow, noEnv)
	var nilMap map[string]string
	Bind(m, SeamGovNotify, nilMap)
	if got := findingFor(t, m, SeamGovNotify); got == nil || got.State != DarkUnbound {
		t.Fatalf("a typed nil must record DarkUnbound, got %+v", got)
	}
}

// TestReportEnumeratesClosedSetNotRecordedMap is the invisibility oracle.
//
// Killing mutation: range over m.recorded instead of All(). That reintroduces the live bug in
// modules/telemetry (counts[key{surface, enabled}]++), where a series exists only for pairs that
// occurred — so a wholly dark surface emits NO series and is invisible rather than zero. Invisible is
// indistinguishable from healthy on every dashboard.
func TestReportEnumeratesClosedSetNotRecordedMap(t *testing.T) {
	m := newFor(fixedNow, noEnv) // nothing bound, nothing declared
	findings, samples := m.Report(now)

	if len(findings) != len(All()) {
		t.Fatalf("EVERY seam in the closed set must report when untouched: want %d, got %+v", len(All()), findings)
	}
	gov := findingFor(t, m, SeamGovNotify)
	if gov == nil || gov.State != DarkUnrecorded {
		t.Fatalf("an untouched seam must still report, got %+v", gov)
	}
	if !strings.Contains(gov.Consequence, "reach NO operator") {
		t.Fatalf("the finding must carry the SeamSpec consequence verbatim: %q", gov.Consequence)
	}
	if len(samples) != len(All()) {
		t.Fatalf("a sample per seam in the CLOSED SET: want %d, got %d", len(All()), len(samples))
	}
	for _, sm := range samples {
		if sm.Value != 1 || sm.Labels["state"] != "dark-unrecorded" {
			t.Fatalf("every untouched seam's sample must report dark, got %+v", sm)
		}
	}

	// A live seam must still emit a sample — at zero. "Dark" and "never reported" must be distinguishable.
	m2 := newFor(fixedNow, noEnv)
	Bind(m2, SeamGovNotify, func(context.Context, notice) error { return nil })
	_, s2 := m2.Report(now)
	if len(s2) != len(All()) {
		t.Fatalf("a sample per seam regardless of state: want %d, got %d", len(All()), len(s2))
	}
	var sawLiveZero bool
	for _, sm := range s2 {
		if sm.Labels["seam"] == string(SeamGovNotify) && sm.Value == 0 && sm.Labels["state"] == "live" {
			sawLiveZero = true
		}
	}
	if !sawLiveZero {
		t.Fatalf("a live seam must emit a 0-valued sample, got %+v", s2)
	}
}

// TestCriticalSeamRefusesAMereBecause is the anti-rubber-stamp oracle. gov.notify is Critical, so a
// well-formed waiver is NOT enough — an operator must name it in the environment, which forces the
// acceptance through compose (env-parity) and onto a gauge.
//
// Killing mutation: treat Critical like Normal. The unaccepted waiver then reports DeclaredDark with no
// error, and accepting a dark page path becomes a source edit that reads like ordinary wiring.
func TestCriticalSeamRefusesAMereBecause(t *testing.T) {
	m := newFor(fixedNow, noEnv)
	Absent[func(context.Context, notice) error](m, SeamGovNotify, validBecause())
	f, _ := m.Report(now)
	if len(f) < 2 {
		t.Fatalf("an unaccepted CRITICAL waiver must add an error finding, got %+v", f)
	}
	var sawRefusal bool
	for _, x := range f {
		if strings.Contains(x.Detail, AcceptDarkEnv) {
			sawRefusal = true
		}
	}
	if !sawRefusal {
		t.Fatalf("the refusal must name %s so an operator knows the only legitimate move: %+v", AcceptDarkEnv, f)
	}

	// Named in the environment: accepted, and it says so in its own state.
	m2 := newFor(fixedNow, func(k string) string {
		if k == AcceptDarkEnv {
			return "something.else, gov.notify"
		}
		return ""
	})
	Absent[func(context.Context, notice) error](m2, SeamGovNotify, validBecause())
	got := findingFor(t, m2, SeamGovNotify)
	if got == nil || got.State != AcceptedDark {
		t.Fatalf("a named CRITICAL seam must be AcceptedDark, got %+v", got)
	}
	_, s2 := m2.Report(now)
	for _, sm := range s2 {
		if sm.Labels["seam"] == string(SeamGovNotify) {
			if sm.Labels["state"] != "accepted-dark" || sm.Value != 1 {
				t.Fatalf("accepted-dark must still be DARK on the gauge, got %+v", sm)
			}
		}
	}
}

// TestBecauseMustBeCompleteAndPerishable — a waiver costs a sentence of thought and expires.
//
// Killing mutation: drop the Expiry checks. A five-year waiver then validates, and "expires" becomes
// decoration — the exact shape of a control that only ever passes.
func TestBecauseMustBeCompleteAndPerishable(t *testing.T) {
	for name, why := range map[string]Because{
		"no reason":      {Consequence: "c", Owner: "o", Ticket: "t", Expiry: now.Add(time.Hour)},
		"no owner":       {Reason: "r", Consequence: "c", Ticket: "t", Expiry: now.Add(time.Hour)},
		"no ticket":      {Reason: "r", Consequence: "c", Owner: "o", Expiry: now.Add(time.Hour)},
		"no consequence": {Reason: "r", Owner: "o", Ticket: "t", Expiry: now.Add(time.Hour)},
		"no expiry":      {Reason: "r", Consequence: "c", Owner: "o", Ticket: "t"},
		"expired":        {Reason: "r", Consequence: "c", Owner: "o", Ticket: "t", Expiry: now.Add(-time.Hour)},
		"beyond horizon": {Reason: "r", Consequence: "c", Owner: "o", Ticket: "t", Expiry: now.Add(400 * 24 * time.Hour)},
	} {
		t.Run(name, func(t *testing.T) {
			if err := why.validate(now); err == nil {
				t.Fatalf("%s must be rejected", name)
			}
		})
	}
	if err := validBecause().validate(now); err != nil {
		t.Fatalf("a complete, unexpired, in-horizon Because must validate: %v", err)
	}
}

// TestDarkReportIsEmptyOnACleanTree keeps the ledger free of no-op rows: the boot append must happen
// only when something is actually dark.
func TestDarkReportIsEmptyOnACleanTree(t *testing.T) {
	// Bind EVERY seam in the closed set: "clean" means the whole set is live, not one member of it.
	// When this test bound only gov.notify it began failing the moment escalation.page joined the set —
	// which is the closed-set property doing its job, not a defect.
	m := newFor(fixedNow, noEnv)
	Bind(m, SeamGovNotify, func(context.Context, notice) error { return nil })
	Bind(m, SeamEscalationPage, pagerLike{notify: func(context.Context, notice) error { return nil }})
	Bind(m, SeamLessonsFeed, func() {})
	Bind(m, SeamWikiCompile, func() {})
	Bind(m, SeamWorldDiscovery, func() {})
	Bind(m, SeamSuppression, func() {})
	Bind(m, SeamTrackerEntry, func() {})
	Bind(m, SeamTrackerImport, func() {})
	Bind(m, SeamDiscoveryService, func() {})
	Bind(m, SeamVoteInbound, func() {})
	Bind(m, SeamHostDiag, []struct{}{})
	Bind(m, SeamSyslogRead, []struct{}{})
	f, _ := m.Report(now)
	if got := DarkReport(f); got != "" {
		t.Fatalf("a fully-wired tree must produce no report, got %q", got)
	}
	m2 := newFor(fixedNow, noEnv)
	f2, _ := m2.Report(now)
	rep := DarkReport(f2)
	if !strings.Contains(rep, "gov.notify") || !strings.Contains(rep, "reach NO operator") {
		t.Fatalf("the report must name the seam and its consequence: %q", rep)
	}
}

// pagerLike mirrors cmd/worker's notifierPager: a struct that is always non-nil and whose method
// degrades to success when its inner sink is nil. This is the "wired but functionally dark" class.
type pagerLike struct {
	notify func(context.Context, notice) error `wiring:"required"`
}

// TestBindSeesAHoleInANonNilValue is increment 2's reason to exist.
//
// notifierPager{notify: nil} passes every nil check ever written: the struct is non-nil, the interface
// it satisfies is non-nil, and Page() returns nil — success — while reaching nobody. Worse, FireDue
// marks the queue row fired BEFORE paging, so the escalation is consumed and permanently lost with no
// error and no retry anywhere in the stack.
//
// Killing mutation: delete the firstNilRequiredField walk from Bind (or drop the `wiring:"required"`
// tag). The holed value then records LIVE, and the detector certifies the exact defect it exists to
// catch — the worst possible outcome for a control.
func TestBindSeesAHoleInANonNilValue(t *testing.T) {
	m := newFor(fixedNow, noEnv)
	Bind(m, SeamEscalationPage, pagerLike{notify: nil}) // non-nil struct, nil sink
	f, _ := m.Report(now)

	var found *Finding
	for i := range f {
		if f[i].Seam == SeamEscalationPage {
			found = &f[i]
		}
	}
	if found == nil || found.State != DarkUnbound {
		t.Fatalf("a non-nil value with a nil required field must record DarkUnbound, got %+v", f)
	}
	if !strings.Contains(found.Detail, "notify") || !strings.Contains(found.Detail, "functionally dark") {
		t.Fatalf("the finding must name the holed FIELD, not just the seam: %q", found.Detail)
	}

	// The same struct, filled: live.
	m2 := newFor(fixedNow, noEnv)
	Bind(m2, SeamEscalationPage, pagerLike{notify: func(context.Context, notice) error { return nil }})
	for _, x := range mustReport(t, m2) {
		if x.Seam == SeamEscalationPage {
			t.Fatalf("a filled required field must be LIVE, got %+v", x)
		}
	}
}

// TestUntaggedFieldsAreNotWalked keeps the walk honest: only fields a human deliberately marked
// required participate, so the mechanism cannot become a blanket nil-scan that fires on every optional
// dependency and trains everyone to ignore it.
func TestUntaggedFieldsAreNotWalked(t *testing.T) {
	type optional struct {
		Sink  func() error // deliberately untagged
		Other *int
	}
	m := newFor(fixedNow, noEnv)
	Bind(m, SeamEscalationPage, optional{})
	for _, x := range mustReport(t, m) {
		if x.Seam == SeamEscalationPage {
			t.Fatalf("untagged nil fields must NOT make a value dark, got %+v", x)
		}
	}
}

func mustReport(t *testing.T, m *Manifest) []Finding {
	t.Helper()
	f, _ := m.Report(now)
	return f
}

// findingFor returns the finding for one seam, or nil when that seam is live. Tests assert per-seam
// rather than on the whole slice so that adding a seam to the closed set does not falsely fail an
// unrelated oracle — while TestReportEnumeratesClosedSetNotRecordedMap still asserts the set-wide
// property deliberately.
func findingFor(t *testing.T, m *Manifest, s Seam) *Finding {
	t.Helper()
	f, _ := m.Report(now)
	for i := range f {
		if f[i].Seam == s {
			return &f[i]
		}
	}
	return nil
}

// TestLessonsSeamDistinguishesFrozenFromGrowing is the corpus-growth seam (MECH-201).
//
// Found dark in production 2026-08-01: TG_LESSONS_SOURCE_FILE was unset, the outer `if` had NO else, so
// nothing ran and NOTHING WAS LOGGED — while the boot line still read "corpus loaded — 670 prior
// incidents", which is indistinguishable from health. The corpus had silently frozen: every row 1-8 days
// old, no growth, and the recency channel with nothing to discriminate on.
//
// Killing mutation: delete the else branch in cmd/worker/main.go (restoring the shipped state). The seam
// then reports dark-unrecorded rather than declared-dark — still dark, but with no reason, no owner and
// no expiry, which is the difference between a known gap and a silent one.
func TestLessonsSeamDistinguishesFrozenFromGrowing(t *testing.T) {
	// Feed wired: live.
	m := newFor(fixedNow, noEnv)
	Bind(m, SeamLessonsFeed, func() { /* appendLessons */ })
	if got := findingFor(t, m, SeamLessonsFeed); got != nil {
		t.Fatalf("a bound lessons feed must be LIVE, got %+v", got)
	}

	// Feed unset and DECLARED: dark, but with a reason that expires. SeamLessonsFeed is Normal, not
	// Critical — nobody goes un-paged when the corpus freezes; TG just stops learning. Requiring an
	// operator env-var acceptance for that would be crying wolf on a slow problem.
	m2 := newFor(fixedNow, noEnv)
	Absent[struct{}](m2, SeamLessonsFeed, validBecause())
	got := findingFor(t, m2, SeamLessonsFeed)
	if got == nil || got.State != DeclaredDark {
		t.Fatalf("an unset feed must be DECLARED dark, got %+v", got)
	}
	if got.Critical {
		t.Fatal("the lessons feed is not a paging path — marking it Critical would cry wolf on a slow failure")
	}
	if !strings.Contains(got.Consequence, "confirmed-clean heals") {
		t.Fatalf("the finding must say what the dark feed actually costs, got %q", got.Consequence)
	}

	// And the shipped state — no branch recorded at all — must be distinguishable from a declared one.
	m3 := newFor(fixedNow, noEnv)
	if got := findingFor(t, m3, SeamLessonsFeed); got == nil || got.State != DarkUnrecorded {
		t.Fatalf("an unrecorded seam must report dark-unrecorded, got %+v", got)
	}
}
