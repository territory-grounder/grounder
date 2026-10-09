package acceptance

// T-027-10 — the first EXECUTING oracles for spec/027.
//
// Until this file existed all ten scenarios carried @pending, so the suite ran zero of them: the spec's
// guarantees were specified, designed, and unexercised. That is the condition spec/027 can least afford,
// because this plane is the ALLOWLIST SOURCE for the earned op-class epic — it decides which hosts, units
// and containers a ratified class may touch.
//
// THESE DRIVE THE REAL FUNCTIONS, not a restatement of them. `worldmodel.Transition` and
// `worldmodel.NewAllowlistProvider` are the same symbols the composition root wires
// (cmd/worker/main.go:6420/6422/6486) and the same ones the console write path calls
// (core/httpapi/manifest.go). The store and ledger are fakes because they are the only seams that must be
// — everything between them is production code.
//
// GROUNDED IN A LIVE EXERCISE, 2026-08-07 (TG-348). Before writing these I adopted a real entry through
// the live console API and watched the result in the production database:
//
//	POST /v1/manifest/entries/88/adopt -> 200 {"status":"approved","ledger_seq":9761}
//	manifest_entry:      draft 368 | approved 1
//	governance_ledger 9761: decision=manifest:adopt reason="[kyriakos] TG-348 loop-closure exercise…"
//
// So the ordering and the rationale-carrying asserted below are properties observed in production, not
// inferred from the source.

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/worldmodel"
)

func init() { stepRegistrars = append(stepRegistrars, registerManifestSteps) }

// recorder captures the INTERLEAVING of ledger appends and row writes. A test that only checked "both
// happened" would pass on the exact bug REQ-2702 exists to prevent — a row updated before (or without) its
// ledger entry, i.e. an allowlist that widened with no chain entry.
type recorder struct {
	mu    sync.Mutex
	order []string
	seq   int64
	rows  []worldmodel.Entry
	// failRow makes UpdateEntry fail, so the crash-shape can be asserted: an over-recorded ledger is the
	// designed outcome, never an unrecorded state change.
	failRow bool
}

func (r *recorder) Append(d audit.GovDecision) (audit.LedgerEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	r.order = append(r.order, "ledger:"+d.Decision+"|withheld="+fmt.Sprint(d.Withheld)+"|reason="+d.Reason)
	return audit.LedgerEntry{Seq: r.seq, Decision: d.Decision, Reason: d.Reason, Withheld: d.Withheld}, nil
}

func (r *recorder) UpdateEntry(_ context.Context, e worldmodel.Entry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failRow {
		r.order = append(r.order, "row:FAILED")
		return fmt.Errorf("row write failed")
	}
	r.order = append(r.order, "row:"+string(e.Status))
	for i := range r.rows {
		if r.rows[i].ID == e.ID {
			r.rows[i] = e
			return nil
		}
	}
	r.rows = append(r.rows, e)
	return nil
}

func (r *recorder) ApprovedEntries(context.Context) ([]worldmodel.Entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []worldmodel.Entry
	for _, e := range r.rows {
		if e.Status == worldmodel.StatusApproved || e.Status == worldmodel.StatusStale {
			out = append(out, e)
		}
	}
	return out, nil
}

type manifestWorld struct {
	rec       *recorder
	entry     worldmodel.Entry
	result    worldmodel.Entry
	err       error
	envAllow  []string
	preAdopt  []string
	postAdopt []string
}

func registerManifestSteps(sc *godog.ScenarioContext) {
	w := &manifestWorld{}

	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		*w = manifestWorld{rec: &recorder{}}
		return ctx, nil
	})

	sc.Step(`^a draft manifest entry$`, w.aDraftEntry)
	sc.Step(`^the operator adopts it with a rationale$`, w.adoptWithRationale)
	sc.Step(`^the manifest:adopt ledger row precedes the row update and carries the rationale$`, w.ledgerPrecedesRow)

	sc.Step(`^the operator posts reject with an empty rationale$`, w.rejectWithoutRationale)
	sc.Step(`^the request is refused with a client error and the entry is unchanged$`, w.refusedAndUnchanged)

	sc.Step(`^an ssh effect targeting a unit absent from every allowlist source$`, w.effectOnUnadoptedUnit)
	sc.Step(`^the effect executes before and after a one-click adopt of that unit$`, w.resolveAllowlistBeforeAndAfter)
	sc.Step(`^the pre-adopt execution is refused at the leaf and the post-adopt execution passes the leaf gate$`, w.leafRefusesThenPasses)
}

// ---- REQ-2702: adopt appends the ledger BEFORE the row, carrying the rationale -------------------

func (w *manifestWorld) aDraftEntry() error {
	w.entry = worldmodel.Entry{
		ID: 88, EntityType: estate.TypeLXC, Name: "dc1reactive01",
		Host: "dc1pve01", Source: estate.SourcePVE, Confidence: 0.95,
		Status: worldmodel.StatusDraft,
	}
	w.rec.rows = []worldmodel.Entry{w.entry}
	return nil
}

func (w *manifestWorld) adoptWithRationale() error {
	w.result, w.err = worldmodel.Transition(context.Background(), w.rec, w.rec, w.entry,
		worldmodel.StatusApproved, "kyriakos", "adopting one high-confidence pve-sourced guest")
	return nil
}

func (w *manifestWorld) ledgerPrecedesRow() error {
	if w.err != nil {
		return fmt.Errorf("adopt failed: %w", w.err)
	}
	if len(w.rec.order) < 2 {
		return fmt.Errorf("only %d write(s) recorded (%v) — the oracle cannot see an ORDER in fewer than two",
			len(w.rec.order), w.rec.order)
	}
	if !strings.HasPrefix(w.rec.order[0], "ledger:") {
		return fmt.Errorf("first write was %q, want the ledger append. A row updated before its ledger "+
			"entry means an allowlist that widened with NO chain entry — the audit hole this ordering "+
			"exists to make impossible. full order: %v", w.rec.order[0], w.rec.order)
	}
	if !strings.HasPrefix(w.rec.order[1], "row:") {
		return fmt.Errorf("second write was %q, want the row update; order: %v", w.rec.order[1], w.rec.order)
	}
	if !strings.Contains(w.rec.order[0], "manifest:adopt") {
		return fmt.Errorf("ledger decision was %q, want manifest:adopt — the decision string is what a "+
			"reader greps for; a generic one makes grants and refusals indistinguishable", w.rec.order[0])
	}
	// The rationale must reach the CHAIN, not just the row: the row is mutable state, the ledger is the
	// record. And the approver must ride with it, or the chain records an unattributed grant.
	if !strings.Contains(w.rec.order[0], "adopting one high-confidence pve-sourced guest") {
		return fmt.Errorf("the ledger reason does not carry the operator's rationale: %q", w.rec.order[0])
	}
	if !strings.Contains(w.rec.order[0], "[kyriakos]") {
		return fmt.Errorf("the ledger reason does not carry the server-derived approver: %q", w.rec.order[0])
	}
	// Adoption is the ONLY manifest decision that widens, so it is the only one not withheld.
	if !strings.Contains(w.rec.order[0], "withheld=false") {
		return fmt.Errorf("adopt was ledgered as withheld: %q — a reader could not tell a grant from a "+
			"refusal without re-deriving it from the status", w.rec.order[0])
	}
	if w.result.LedgerSeq == 0 {
		return fmt.Errorf("the persisted row carries no ledger_seq — the row could not be tied back to " +
			"the decision that authorised it")
	}
	return nil
}

// ---- REQ-2703: a decision with no rationale is refused, and nothing moves ------------------------

func (w *manifestWorld) rejectWithoutRationale() error {
	w.result, w.err = worldmodel.Transition(context.Background(), w.rec, w.rec, w.entry,
		worldmodel.StatusRejected, "kyriakos", "   ")
	return nil
}

func (w *manifestWorld) refusedAndUnchanged() error {
	if w.err == nil {
		return fmt.Errorf("an empty rationale was ACCEPTED — every grant and revocation states why, and " +
			"an unexplained status change is unreviewable after the fact")
	}
	// Whitespace must not satisfy it either: " " is the shape a client sends when a form field was
	// rendered but never filled, which is exactly the unreviewable case.
	if w.err != worldmodel.ErrRationaleRequired {
		return fmt.Errorf("refused with %v, want ErrRationaleRequired — the surface maps that sentinel to "+
			"a 400, so a different error becomes a 500 and reads as a server fault", w.err)
	}
	// THE HALF THAT MATTERS: the refusal must happen BEFORE anything is written. A check that refuses the
	// caller after appending a ledger row leaves a decision recorded that never took effect.
	if len(w.rec.order) != 0 {
		return fmt.Errorf("the refused transition still wrote %v — a rejected request must touch neither "+
			"the chain nor the row", w.rec.order)
	}
	return nil
}

// ---- REQ-2704: the leaf refuses an unadopted target and passes the same one after adopt ----------

func (w *manifestWorld) effectOnUnadoptedUnit() error {
	// A unit the operator never typed into config and never adopted. The env allowlist is deliberately
	// NON-EMPTY: an empty one would make the post-adopt assertion pass for the trivial reason that the
	// list went from nothing to something, rather than because the union added this entry.
	w.envAllow = []string{"already-granted.service"}
	w.entry = worldmodel.Entry{
		ID: 91, EntityType: estate.TypeService, Name: "tg-reactive.service",
		Host: "dc1pve01", Source: estate.SourcePVE, Confidence: 0.95,
		Status: worldmodel.StatusDraft,
	}
	w.rec.rows = []worldmodel.Entry{w.entry}
	return nil
}

func (w *manifestWorld) resolveAllowlistBeforeAndAfter() error {
	// The REAL provider the worker wires — resolved per call, which is what makes an adoption visible
	// without a restart (TG-232).
	provider := worldmodel.NewAllowlistProvider(w.rec, worldmodel.KindUnit, w.envAllow)
	w.preAdopt = provider(context.Background())

	if _, err := worldmodel.Transition(context.Background(), w.rec, w.rec, w.entry,
		worldmodel.StatusApproved, "kyriakos", "one-click adopt for the leaf oracle"); err != nil {
		return fmt.Errorf("adopt failed: %w", err)
	}
	// SAME provider instance, deliberately. Building a second one would prove only that a fresh process
	// sees the grant — which is the boot-frozen behaviour TG-232 fixed, not the live resolution.
	w.postAdopt = provider(context.Background())
	return nil
}

func (w *manifestWorld) leafRefusesThenPasses() error {
	const target = "tg-reactive.service"
	if contains(w.preAdopt, target) {
		return fmt.Errorf("the unit was allowed BEFORE adoption (%v) — discovery drafting an entry would "+
			"then be enough to actuate on it, and the operator's review would grant nothing", w.preAdopt)
	}
	if !contains(w.postAdopt, target) {
		return fmt.Errorf("the unit is still absent AFTER adoption (%v) — the adopt path writes a row "+
			"nothing reads, so the console's grant would be decorative", w.postAdopt)
	}
	// UNION, never replace. The env grant must survive: DB-replaces-env silently narrows on the FIRST
	// adopt, so an operator who adopted one unit would discover they had revoked every other.
	if !contains(w.postAdopt, "already-granted.service") {
		return fmt.Errorf("the env-granted unit vanished after the first adoption (%v) — silent narrowing, "+
			"the exact failure UNION exists to prevent", w.postAdopt)
	}
	// And the pre-adopt list must not have been empty, or "refused before" is vacuous.
	if len(w.preAdopt) == 0 {
		return fmt.Errorf("the pre-adopt allowlist was EMPTY, so 'refused at the leaf' would hold for a " +
			"target that was never granted by anything — the assertion proves nothing")
	}
	return nil
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
