package acceptance

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/persist"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/schema"
)

// recordingSignaler records escalation re-entries for the REQ-507 oracle.
type recordingSignaler struct{ refs []string }

func (s *recordingSignaler) SignalRequeue(_ context.Context, ref string) error {
	s.refs = append(s.refs, ref)
	return nil
}

// world is the per-scenario state for the spec/006 GREEN oracles.
type world struct {
	router    *auth.Router
	panicked  bool
	constrErr error

	// schema-version (REQ-505) oracle state.
	table     schema.Table
	stamped   schema.Version
	stampErr  error
	storedVer schema.Version
	readErr   error

	// ingest (REQ-502) oracle state.
	envelope  ingest.IncidentEnvelope
	pub       *ingest.RecordingPublisher
	batch     ingest.BatchResult
	pubBefore int // published count observed right after the chain ran, before PublishTriage
	pubAfter  int

	// audit/persist (REQ-503/506/507) oracle state.
	ledger     *audit.Ledger
	auditRow   audit.RiskAudit
	auditStamp audit.RiskAudit
	auditEntry audit.LedgerEntry
	auditErr   error
	sreg       *persist.MemScheduledReboots
	srIn       persist.ScheduledReboot
	srOut      persist.ScheduledReboot
	queue      *persist.EscalationQueue
	escSig     *recordingSignaler
	escFired   int
}

var ingestNow = time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

// didPanic reports whether f panicked.
func didPanic(f func()) (panicked bool) {
	defer func() {
		if recover() != nil {
			panicked = true
		}
	}()
	f()
	return false
}

// TestInterfaceContractsAcceptance runs the spec/006 acceptance feature. @pending scenarios (request-time
// auth behavior + generated contracts) are excluded; the executed set drives the real core/auth
// registration-time guarantees (REQ-501) and must pass strictly.
func TestInterfaceContractsAcceptance(t *testing.T) {
	suite := godog.TestSuite{
		Name:                "spec/006 interface-contracts",
		ScenarioInitializer: initializeScenario,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"."},
			Tags:     "~@pending",
			Strict:   true,
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("spec/006 acceptance scenarios failed")
	}
}

func initializeScenario(sc *godog.ScenarioContext) {
	w := &world{}

	// REQ-501/504 — the authenticated HTTP surface (core/httpapi over core/auth), bound in a companion
	// file so the HTTP harness + fakes stay separate from the pure-Go steps.
	registerHTTPAPISteps(sc)
	// REQ-501b — the generated wire contracts (tools/gencontracts), bound in a companion file.
	registerGenContractsSteps(sc)
	// REQ-508 — the browser operator session (login/read-only/logout/rate-limit), bound in a companion file.
	registerSessionSteps(sc)
	// REQ-520/522/523/524 — task #27: the LAW-clamped resolver, the admin step-up tier, ledgered
	// config writes, and sealed secrets, bound in a companion file.
	registerConfigSteps(sc)
	// REQ-525 — the Phase-2 canary safety plane: the runtime mutation kill-switch (MutationGate.Disable)
	// and the read-only /metrics exposition, bound in a companion file.
	registerSafetyPlaneSteps(sc)

	sc.Step(`^an auth router$`, func() error {
		w.router = auth.NewRouter(&auth.Verifier{})
		return nil
	})
	sc.Step(`^a route is registered with the auth-none method$`, func() error {
		w.panicked = didPanic(func() {
			w.router.Handle("/x", auth.AuthNone, func(http.ResponseWriter, *http.Request, auth.Principal) {})
		})
		return nil
	})
	sc.Step(`^route registration panics$`, func() error {
		if !w.panicked {
			return fmt.Errorf("registering a route with auth=none must panic (INV-01)")
		}
		return nil
	})

	sc.Step(`^a verifier is constructed with no nonce store$`, func() error {
		_, w.constrErr = auth.NewVerifier(nil, nil, time.Minute)
		return nil
	})
	sc.Step(`^construction returns the replay-protection-unconfigured error$`, func() error {
		if !errors.Is(w.constrErr, auth.ErrReplayProtectionUnconfigured) {
			return fmt.Errorf("expected ErrReplayProtectionUnconfigured, got %v", w.constrErr)
		}
		return nil
	})

	// REQ-505 — schema-version stamping and the fail-closed reader guard (core/schema).
	sc.Step(`^a writer inserting a row into a governed table$`, func() error {
		w.table = schema.TableSessionRiskAudit
		return nil
	})
	sc.Step(`^the row is persisted$`, func() error {
		w.stamped, w.stampErr = schema.Stamp(w.table)
		return nil
	})
	sc.Step(`^the row carries the current schema_version from the canonical registry$`, func() error {
		if w.stampErr != nil {
			return fmt.Errorf("stamping a governed table must succeed, got %v", w.stampErr)
		}
		want, err := schema.Current(w.table)
		if err != nil {
			return err
		}
		if w.stamped != want {
			return fmt.Errorf("stamped version %d != registry version %d for %s", w.stamped, want, w.table)
		}
		if w.stamped <= 0 {
			return fmt.Errorf("stamped version must be > 0 (DATA-MODEL §1.3), got %d", w.stamped)
		}
		return nil
	})

	sc.Step(`^a governed row whose stored schema_version exceeds the reader's compiled version$`, func() error {
		w.table = schema.TableSessionRiskAudit
		compiled, err := schema.Current(w.table)
		if err != nil {
			return err
		}
		w.storedVer = compiled + 1 // written by a future version the reader cannot interpret
		return nil
	})
	sc.Step(`^the reader decodes the row$`, func() error {
		w.readErr = schema.CheckRow(w.table, w.storedVer)
		return nil
	})
	sc.Step(`^the reader returns a SchemaVersionError instead of mis-reading the row$`, func() error {
		var sve schema.SchemaVersionError
		if !errors.As(w.readErr, &sve) {
			return fmt.Errorf("expected a SchemaVersionError for a future-versioned row, got %v", w.readErr)
		}
		return nil
	})

	// REQ-502 — the ingest event surface: normalize, run dedup/flap/burst/correlate in code, publish
	// triage.requested keyed by external_ref (core/ingest).
	sc.Step(`^an ingest adapter that received a provider alert$`, func() error {
		raw := ingest.NewRawEvent("prometheus-dc1", []byte(`{"provider":"payload"}`))
		raw.ExternalRef = "TG-4617"
		raw.AlertRule = "MeshBFDSessionDown"
		raw.Severity = "warning"
		raw.Host = "nl-frr01"
		raw.ObservedAt = ingestNow
		e, err := ingest.Normalize(raw, ingestNow)
		w.envelope = e
		return err
	})
	sc.Step(`^the adapter finishes normalization$`, func() error {
		w.pub = &ingest.RecordingPublisher{}
		res := ingest.NewPipeline().Process([]ingest.IncidentEnvelope{w.envelope}, ingestNow)
		_, err := ingest.PublishTriage(context.Background(), w.pub, res, ingestNow)
		return err
	})
	sc.Step(`^a triage\.requested event is published to the routing layer keyed by its external_ref$`, func() error {
		if len(w.pub.Events) != 1 {
			return fmt.Errorf("expected 1 published event, got %d", len(w.pub.Events))
		}
		ev := w.pub.Events[0]
		if ev.ExternalRef == "" || ev.ExternalRef != ev.Envelope.ExternalRef || ev.ExternalRef != w.envelope.ExternalRef {
			return fmt.Errorf("event not keyed by external_ref: event=%q envelope=%q", ev.ExternalRef, w.envelope.ExternalRef)
		}
		return nil
	})

	sc.Step(`^a burst of duplicate and flapping provider alerts$`, func() error {
		mk := func(ref, host string) ingest.IncidentEnvelope {
			raw := ingest.NewRawEvent("src", nil)
			raw.ExternalRef, raw.AlertRule, raw.Host, raw.Severity, raw.ObservedAt = ref, "R", host, "warning", ingestNow
			e, _ := ingest.Normalize(raw, ingestNow)
			return e
		}
		flap := mk("TG-1", "web01")
		// dup+flap on TG-1 (3 fires of one key) plus enough distinct incidents to trip burst.
		w.envelope = flap
		batch := []ingest.IncidentEnvelope{
			flap, flap, flap,
			mk("TG-2", "web02"), mk("TG-3", "web03"), mk("TG-4", "web04"), mk("TG-5", "web05"),
		}
		w.batch = ingest.NewPipeline().Process(batch, ingestNow)
		return nil
	})
	sc.Step(`^the ingest adapter processes them$`, func() error {
		// The chain has run (Process); nothing is published yet — capture that, then publish.
		w.pub = &ingest.RecordingPublisher{}
		w.pubBefore = len(w.pub.Events)
		n, err := ingest.PublishTriage(context.Background(), w.pub, w.batch, ingestNow)
		w.pubAfter = n
		return err
	})
	sc.Step(`^dedup, flap, burst, and correlate run in code before any triage\.requested is published$`, func() error {
		want := []string{ingest.StageDedup, ingest.StageFlap, ingest.StageBurst, ingest.StageCorrelate}
		if len(w.batch.Order) != len(want) {
			return fmt.Errorf("stage order %v != %v", w.batch.Order, want)
		}
		for i := range want {
			if w.batch.Order[i] != want[i] {
				return fmt.Errorf("stage %d = %q, want %q", i, w.batch.Order[i], want[i])
			}
		}
		if w.pubBefore != 0 {
			return fmt.Errorf("chain must run before any publish; saw %d published pre-publish", w.pubBefore)
		}
		// dedup collapsed the two repeats; flap/burst annotated; distinct groups published.
		dupes := 0
		for _, d := range w.batch.Decisions {
			if d.Duplicate {
				dupes++
			}
		}
		if dupes != 2 {
			return fmt.Errorf("dedup should collapse 2 repeats, collapsed %d", dupes)
		}
		if !w.batch.Decisions[0].Flapping || !w.batch.Decisions[0].InBurst {
			return fmt.Errorf("expected flapping+burst annotations on the repeated key")
		}
		if w.pubAfter == 0 {
			return fmt.Errorf("expected representatives to be published after the chain")
		}
		return nil
	})

	// REQ-503 — one required-field session_risk_audit row per classification, appended to the
	// hash-chained governance ledger (core/audit).
	sc.Step(`^a completed risk classification$`, func() error {
		w.ledger = audit.NewLedger()
		w.auditRow = audit.RiskAudit{
			ExternalRef:  "TG-4617",
			RiskLevel:    "low",
			Band:         safety.BandAuto,
			AutoApproved: true,
			ActionID:     "action-4617",
			PlanHash:     "plan-4617",
		}
		return nil
	})
	sc.Step(`^the decision is recorded$`, func() error {
		w.auditStamp, w.auditEntry, w.auditErr = audit.AppendRiskAudit(w.ledger, w.auditRow)
		return w.auditErr
	})
	sc.Step(`^exactly one required-field session_risk_audit row is appended to the hash-chained ledger$`, func() error {
		if w.ledger.Len() != 1 {
			return fmt.Errorf("expected exactly one row per classification, got %d", w.ledger.Len())
		}
		if w.auditStamp.SchemaVersion <= 0 {
			return fmt.Errorf("row must be schema-stamped, got %d", w.auditStamp.SchemaVersion)
		}
		if w.auditEntry.ActionID != w.auditRow.ActionID {
			return fmt.Errorf("ledger entry not bound to the action: %q != %q", w.auditEntry.ActionID, w.auditRow.ActionID)
		}
		if err := w.ledger.Verify(); err != nil {
			return fmt.Errorf("hash chain must verify: %v", err)
		}
		return nil
	})

	// REQ-506 — the bi-temporal discovered_scheduled_reboots registry with a kill switch (core/persist).
	sc.Step(`^a discovered scheduled-reboot schedule$`, func() error {
		w.sreg = persist.NewMemScheduledReboots()
		w.srIn = persist.ScheduledReboot{
			Host:       "nl-frr01",
			Cron:       "0 3 * * 0",
			Kind:       "os-patch",
			KillSwitch: false,
			ValidFrom:  ingestNow,
			ValidUntil: ingestNow.Add(90 * 24 * time.Hour),
		}
		return nil
	})
	sc.Step(`^the schedule is registered$`, func() error {
		var err error
		w.srOut, err = w.sreg.Register(context.Background(), w.srIn)
		return err
	})
	sc.Step(`^the row carries valid_from, valid_until, an observing or live state, and a kill_switch$`, func() error {
		if w.srOut.ValidFrom.IsZero() || w.srOut.ValidUntil.IsZero() || !w.srOut.ValidUntil.After(w.srOut.ValidFrom) {
			return fmt.Errorf("row must carry a valid bi-temporal window: %+v", w.srOut)
		}
		if w.srOut.State != persist.SRObserving && w.srOut.State != persist.SRLive {
			return fmt.Errorf("row must carry an observing-or-live state, got %v", w.srOut.State)
		}
		if w.srOut.State != persist.SRObserving {
			return fmt.Errorf("a freshly discovered schedule must default to observing (safe), got %s", w.srOut.State)
		}
		if w.srOut.SchemaVersion <= 0 {
			return fmt.Errorf("row must be schema-stamped, got %d", w.srOut.SchemaVersion)
		}
		return nil
	})

	// REQ-507 — the append-only escalation_queue that re-enters the gated pipeline via an authenticated
	// signal (core/persist).
	sc.Step(`^an unanswered approval poll whose session archived$`, func() error {
		w.queue = persist.NewEscalationQueue()
		return nil
	})
	sc.Step(`^a delayed re-check row is enqueued and later fires$`, func() error {
		ctx := context.Background()
		if _, err := w.queue.Enqueue(ctx, "TG-4617", 1, ingestNow.Add(-time.Minute)); err != nil {
			return err
		}
		// The persistence contract exposes the due batch (DuePending) and the append-only fired transition
		// (MarkFired). Re-entry is an AUTHENTICATED signal — modeled here by recordingSignaler, which in
		// production is the escalation controller's SignalRequeue (spec/003); the queue never re-triggers on
		// its own. This binds REQ-507's "re-enters via an authenticated signal, append-only" at the seam both
		// the in-memory oracle and the durable pgx twin satisfy.
		due, err := w.queue.DuePending(ctx, ingestNow)
		if err != nil {
			return err
		}
		w.escSig = &recordingSignaler{}
		fired := 0
		for _, it := range due {
			if err := w.escSig.SignalRequeue(ctx, it.ExternalRef); err != nil {
				return err
			}
			if err := w.queue.MarkFired(ctx, it.Seq); err != nil {
				return err
			}
			fired++
		}
		w.escFired = fired
		return nil
	})
	sc.Step(`^the row is append-only and re-enters the gated pipeline as an authenticated Temporal signal$`, func() error {
		if w.queue.Len() != 1 {
			return fmt.Errorf("append-only: firing must not delete the row, len=%d", w.queue.Len())
		}
		if w.escFired != 1 || len(w.escSig.refs) != 1 || w.escSig.refs[0] != "TG-4617" {
			return fmt.Errorf("row must re-enter via an authenticated signal, fired=%d refs=%v", w.escFired, w.escSig.refs)
		}
		if items := w.queue.Items(); items[0].Status != persist.EscalFired {
			return fmt.Errorf("fired row status must transition (not delete), got %v", items[0].Status)
		}
		return nil
	})
}
