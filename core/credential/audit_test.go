package credential

// ORACLE for the audited resolution seam (spec/016 REQ-1602/1604/1617): AuditedResolver resolves a covered
// host to its bundle and appends ONE non-secret credential_resolution event; an uncovered host fails closed
// (ErrUnresolved) with NO bundle and a recorded "unresolved" event; an equal-specificity overlap fails closed
// "ambiguous"; and the appended event NEVER carries key material or a full SecretRef value (only the ref
// SCHEME). A nil sink is a no-op (resolution still fails closed and returns bundles).

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

// memSink is an in-memory ResolutionSink (core/credential cannot import core/db — that would be a cycle).
type memSink struct{ events []ResolutionEvent }

func (m *memSink) AppendResolution(_ context.Context, ev ResolutionEvent) error {
	m.events = append(m.events, ev)
	return nil
}

const keyRefMarker = "file:/secrets/hostdiag_key"

// engineWith builds a SyncEngine whose one registered machine-plane source covers dc1* as root.
func engineWith(t *testing.T) *SyncEngine {
	t.Helper()
	b, err := NewBundle(BundleSpec{User: "root", Port: 22, Scheme: SchemeSSH, SSHKeyRef: config.SecretRef(keyRefMarker)})
	if err != nil {
		t.Fatalf("NewBundle: %v", err)
	}
	se := NewSyncEngine(nil)
	if err := se.RegisterSource(fakeSrc{id: "src-a", entries: []SourceEntry{{
		NativeID: "r1", Selector: Selector{Kind: KindHostGlob, Pattern: "dc1*"}, Bundle: b,
	}}}, 100); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := se.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	return se
}

type fakeSrc struct {
	id      string
	entries []SourceEntry
}

func (f fakeSrc) ID() string                                     { return f.id }
func (f fakeSrc) Plane() Plane                                   { return PlaneMachine }
func (f fakeSrc) Sync(context.Context) ([]SourceEntry, error)    { return f.entries, nil }

func TestAuditedResolver_ResolvedAppendsNonSecretEvent(t *testing.T) {
	sink := &memSink{}
	r := NewAuditedResolver(engineWith(t))
	r.SetSink(sink)

	b, err := r.Resolve(context.Background(), Target{Host: "dc1librespeed01"})
	if err != nil {
		t.Fatalf("Resolve(covered): %v", err)
	}
	if b.User() != "root" || string(b.SSHKeyRef()) != keyRefMarker {
		t.Errorf("resolved bundle = user %q keyref %q", b.User(), b.SSHKeyRef())
	}
	if len(sink.events) != 1 {
		t.Fatalf("expected exactly ONE audit event, got %d", len(sink.events))
	}
	ev := sink.events[0]
	if ev.Outcome != OutcomeResolved || ev.Target != "dc1librespeed01" || ev.Plane != PlaneMachine {
		t.Errorf("unexpected event: %+v", ev)
	}
	if ev.User != "root" || ev.Scheme != SchemeSSH || ev.KeyRefScheme != "file" || ev.Source != "src-a" {
		t.Errorf("event identity metadata wrong: %+v", ev)
	}

	// NO secret material and NO full ref value in the event — only the ref SCHEME ("file") is allowed.
	blob, _ := json.Marshal(ev)
	for _, leak := range []string{keyRefMarker, "/secrets/", "hostdiag_key"} {
		if strings.Contains(string(blob), leak) {
			t.Errorf("SECRET LEAK: audit event contains %q\n%s", leak, blob)
		}
	}
}

func TestAuditedResolver_UnresolvedFailsClosed(t *testing.T) {
	sink := &memSink{}
	r := NewAuditedResolver(engineWith(t))
	r.SetSink(sink)

	b, err := r.Resolve(context.Background(), Target{Host: "dc2unknown"})
	if !errors.Is(err, ErrUnresolved) {
		t.Fatalf("uncovered host must fail closed with ErrUnresolved, got %v", err)
	}
	if b.Valid() {
		t.Fatal("a refused resolution must return the ZERO (invalid) bundle — no default identity")
	}
	if len(sink.events) != 1 || sink.events[0].Outcome != OutcomeUnresolved {
		t.Fatalf("expected one 'unresolved' event, got %+v", sink.events)
	}
	if sink.events[0].User != "" || sink.events[0].KeyRefScheme != "" {
		t.Errorf("an unresolved event must carry no identity, got %+v", sink.events[0])
	}
}

func TestAuditedResolver_AmbiguousFailsClosed(t *testing.T) {
	// Two equal-specificity globs (both 7 fixed chars — a leading-* suffix glob and a trailing-* prefix glob)
	// both matching the host → ErrAmbiguous (fail closed).
	b, _ := NewBundle(BundleSpec{User: "root", Port: 22, Scheme: SchemeSSH, SSHKeyRef: config.SecretRef("env:K")})
	se := NewSyncEngine(nil)
	_ = se.RegisterSource(fakeSrc{id: "s", entries: []SourceEntry{
		{NativeID: "a", Selector: Selector{Kind: KindHostGlob, Pattern: "dc1*"}, Bundle: b},
		{NativeID: "b", Selector: Selector{Kind: KindHostGlob, Pattern: "*speed01"}, Bundle: b},
	}}, 100)
	if _, err := se.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	sink := &memSink{}
	r := NewAuditedResolver(se)
	r.SetSink(sink)

	if _, err := r.Resolve(context.Background(), Target{Host: "dc1librespeed01"}); !errors.Is(err, ErrUnresolved) {
		t.Fatalf("ambiguous match must fail closed (wraps ErrUnresolved), got %v", err)
	}
	if len(sink.events) != 1 || sink.events[0].Outcome != OutcomeAmbiguous {
		t.Fatalf("expected one 'ambiguous' event, got %+v", sink.events)
	}
}

func TestAuditedResolver_NilSinkStillResolves(t *testing.T) {
	r := NewAuditedResolver(engineWith(t)) // no SetSink
	if _, err := r.Resolve(context.Background(), Target{Host: "dc1a"}); err != nil {
		t.Fatalf("resolution must work without a sink: %v", err)
	}
	if _, err := r.Resolve(context.Background(), Target{Host: "nope"}); !errors.Is(err, ErrUnresolved) {
		t.Fatalf("still fail closed without a sink, got %v", err)
	}
}

// The resolver stamps the incident correlation id (external_ref) carried on the context onto the audit event,
// so the credential_resolution row joins the decision-tracer walk (spec/020 REQ-2015). A bare context stamps
// an empty ref — the correlation is simply absent, exactly as before.
func TestAuditedResolver_StampsExternalRefFromContext(t *testing.T) {
	sink := &memSink{}
	r := NewAuditedResolver(engineWith(t))
	r.SetSink(sink)

	if _, err := r.Resolve(WithExternalRef(context.Background(), "am-firedrill-x"), Target{Host: "dc1librespeed01"}); err != nil {
		t.Fatalf("Resolve (ctx ref): %v", err)
	}
	if _, err := r.Resolve(context.Background(), Target{Host: "dc1librespeed01"}); err != nil {
		t.Fatalf("Resolve (bare ctx): %v", err)
	}
	if len(sink.events) != 2 {
		t.Fatalf("want 2 events, got %d", len(sink.events))
	}
	if sink.events[0].ExternalRef != "am-firedrill-x" {
		t.Errorf("context external_ref not stamped onto the audit event: got %q, want am-firedrill-x", sink.events[0].ExternalRef)
	}
	if sink.events[1].ExternalRef != "" {
		t.Errorf("a bare-context resolution must carry an empty external_ref, got %q", sink.events[1].ExternalRef)
	}
}
