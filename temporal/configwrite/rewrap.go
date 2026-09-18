// rewrap.go — the operator-driven DEK rewrap lane (TG-163).
//
// WHAT THIS IS FOR. Sealing runs through OpenBao Transit. Rotating the Transit key bumps its version and
// leaves every stored ciphertext readable under the OLD version — convenient, and a trap: nothing ever
// moves those ciphertexts forward, so the old version can never be retired and `min_decryption_version`
// can never be raised. Rotation was a door that opened one way. This lane walks stored DEKs forward onto
// the current key version so the old one CAN be retired.
//
// WHAT THIS DELIBERATELY IS NOT. There is no cron, no timer, and no "your keys are N days old" surface.
// This environment's owner has a standing instruction that rotation reminders are unwanted, and the
// instruction is right: the thing that was missing was never a nag, it was the CAPABILITY. So the lane runs
// exactly when an administrator posts to it and never otherwise.
//
// WHY IT RUNS IN THE WORKER. sealed_secret has one writer (this package's package comment). The grounder's
// admin surface starts this workflow and waits; it never touches the table. A rewrap driven from the
// grounder would be a second writer racing the first over the same rows — the precise race
// db.SealedSecretStore.RewrapDEK's conditional UPDATE exists to survive.
//
// WHY IT IS SAFE TO INTERRUPT. Each row is one independent, conditional UPDATE of the key-side bytes only.
// Kill the worker mid-run and the store is half at the old key version and half at the new — and EVERY row
// still opens, because a rewrap never touches the value ciphertext and Transit decrypts any version at or
// above min_decryption_version. Resume by posting again; AfterName carries the cursor. That property is
// asserted against a real Postgres in core/db/sealed_rewrap_test.go.
package configwrite

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/seal"
)

// StepRewrapScan names the row-walk step in the per-step latency record (TG-277's discipline).
const StepRewrapScan = "seal.rewrap"

// RewrapRequest is the typed, operator-driven rewrap order. It carries no key material and no secret
// material — the whole operation happens between the store and the key service.
type RewrapRequest struct {
	Rationale string
	Operator  string
	// AfterName resumes a previously interrupted run just past this name (rows are walked in name order).
	// Empty starts from the beginning, which is always safe: a rewrap of an already-current DEK is a no-op
	// that still verifies, so re-running from zero costs time and nothing else.
	AfterName string
	// Limit bounds one run's rows (0 = no bound). An operator with a large store can walk it in batches
	// and stop at any point, because stopping between rows is a legal resting state.
	Limit int
}

// RewrapOutcome is one rewrap run's report. Value-less by construction: names, counts, and key versions.
type RewrapOutcome struct {
	Scanned   int // rows read
	Rewrapped int // rows whose wrapped DEK was replaced
	Skipped   int // rows re-put underneath the run — already current, deliberately left alone
	LastName  string
	LedgerSeq int64
	// Versions counts the key version every scanned row sits at AFTER the run ("v2": 7). This is the
	// number that decides whether the old Transit key version can be retired: if anything is still at the
	// old version, raising min_decryption_version destroys it. "local" counts rows wrapped by the
	// in-process master key, which carries no version.
	Versions map[string]int
	// Partial is true when Limit stopped the walk before the store ran out, so rows beyond LastName were
	// never looked at. It exists so the Note cannot claim anything about "every row" after a bounded run —
	// the version census only covers what was actually scanned.
	Partial bool
	// Note states in words what an operator may do next, including the honest empty case.
	Note string
}

// SecretRewrapper is the slice of the sealed store this lane needs: the key-side bytes, and a conditional
// swap of them. Nothing here can read a secret value.
type SecretRewrapper interface {
	ListWrappedDEKs(ctx context.Context, afterName string) ([]WrappedDEKRow, error)
	RewrapDEK(ctx context.Context, name string, oldWrapped, oldNonce, newWrapped, newNonce []byte) (bool, error)
}

// WrappedDEKRow mirrors db.WrappedDEKRow — declared here so temporal/ keeps its own dependency direction
// (the pgx store satisfies the interface structurally at the composition root).
type WrappedDEKRow struct {
	Name       string
	WrappedDEK []byte
	DEKNonce   []byte
}

// ErrRewrapUnavailable refuses the lane when the worker has no sealer or no rewrap-capable store — the
// fail-closed direction. A rewrap that silently did nothing would report success over an untouched store,
// and the operator would then retire a key version that is still in use.
var ErrRewrapUnavailable = errors.New("configwrite: DEK rewrap unavailable — this worker has no sealer " +
	"or no sealed store configured (fail closed)")

// RewrapSecretsActivity walks the sealed store and re-wraps each DEK under the current master-key version.
//
// Ledger-FIRST, like every governed write in this package: the decision to re-key is recorded before any
// row moves. One entry per RUN, not per row — the run is the decision; the rows are its mechanics, and a
// per-row entry would bury the hash chain under an operation that changes no capability and no value.
func (a *Activities) RewrapSecretsActivity(ctx context.Context, req RewrapRequest) (RewrapOutcome, error) {
	if strings.TrimSpace(req.Rationale) == "" {
		return RewrapOutcome{}, ErrRationaleRequired
	}
	if a.D.Sealer == nil || a.D.Rewrap == nil {
		return RewrapOutcome{}, ErrRewrapUnavailable
	}
	var (
		steps []StepLatency
		entry audit.LedgerEntry
		out   = RewrapOutcome{Versions: map[string]int{}}
	)
	start := time.Now()
	err := func() error {
		if lerr := a.timed(ctx, &steps, StepLedgerAppend, func(c context.Context) error {
			var e error
			entry, e = a.D.Ledger.AppendContext(c, audit.GovDecision{
				Decision: "secret:rewrap",
				Reason:   strings.TrimSpace(req.Rationale) + " [by " + req.Operator + "]",
				ActionID: "secret:rewrap:" + req.AfterName,
				Withheld: false,
			})
			if e != nil {
				return fmt.Errorf("ledger append: %w", e)
			}
			return nil
		}); lerr != nil {
			return lerr
		}
		// NOT inside a.timed: the row walk is unbounded work over an operator-sized store, and
		// DefaultStepBudget exists to make a stalled SINGLE write legible. Clamping a multi-row migration
		// to 4s would abort a healthy run. The activity's own StartToCloseTimeout (rewrapOpts) bounds it,
		// and stopping part-way is a legal resting state — that is the whole design.
		return a.rewrapRows(ctx, req, &out)
	}()
	// Recorded even on the unbounded step so the observer still sees the shape of the run.
	steps = append(steps, StepLatency{Step: StepRewrapScan, Took: time.Since(start)})
	a.observe("secret:rewrap", steps, time.Since(start), err)
	if err != nil {
		// CARRY THE PROGRESS INTO THE ERROR. Temporal discards an activity's result value whenever it
		// returns non-nil, so a failed run would otherwise report only "it broke" — and the operator would
		// have no idea whether zero rows or four hundred had already moved, nor where to resume. This lane
		// is designed to be interrupted; an interruption that loses its own cursor is not resumable, which
		// would give back exactly the property the ticket asked for.
		return RewrapOutcome{}, fmt.Errorf("%w (progress: %d scanned, %d rewrapped, resume with after=%q)",
			err, out.Scanned, out.Rewrapped, out.LastName)
	}
	out.LedgerSeq = entry.Seq
	out.Note = rewrapNote(out)
	return out, nil
}

func (a *Activities) rewrapRows(ctx context.Context, req RewrapRequest, out *RewrapOutcome) error {
	rows, err := a.D.Rewrap.ListWrappedDEKs(ctx, req.AfterName)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if req.Limit > 0 && out.Scanned >= req.Limit {
			out.Partial = true
			break
		}
		// Honour cancellation BETWEEN rows, never inside one. A rewrap that is interrupted between rows
		// leaves a store that is fully readable; the danger is only ever a partial write, and there is no
		// partial write here because each row is a single statement.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		out.Scanned++
		sealed := seal.Sealed{WrappedDEK: row.WrappedDEK, DEKNonce: row.DEKNonce}
		next, rerr := a.D.Sealer.RewrapDEK(row.Name, sealed)
		if rerr != nil {
			// Name the row. A rewrap that fails on one credential must not read as "rewrap failed" — the
			// operator needs to know WHICH secret is unopenable, because that is a pre-existing fault this
			// run discovered rather than caused.
			return fmt.Errorf("configwrite: rewrap %q: %w", row.Name, rerr)
		}
		landed, uerr := a.D.Rewrap.RewrapDEK(ctx, row.Name, row.WrappedDEK, row.DEKNonce, next.WrappedDEK, next.DEKNonce)
		if uerr != nil {
			return fmt.Errorf("configwrite: rewrap store %q: %w", row.Name, uerr)
		}
		if landed {
			out.Rewrapped++
		} else {
			// Re-put underneath us: already sealed under the current version by that write, so it is counted
			// at the version the writer produced, not the one we computed.
			out.Skipped++
		}
		countVersion(out.Versions, next.WrappedDEK)
		// LAST *COMPLETED* ROW, set only here. It was previously set on entry, which is a resume cursor that
		// SKIPS the row it died on: the one credential the operator most needs re-keyed would be silently
		// stranded on the old key version, and the run after it would report a clean single-version census
		// that is a lie. The cursor must mean "everything up to and including this name is done".
		out.LastName = row.Name
	}
	return nil
}

func countVersion(m map[string]int, wrapped []byte) {
	if v := seal.KeyVersion(wrapped); v > 0 {
		m["v"+strconv.Itoa(v)]++
		return
	}
	m["local"]++
}

// rewrapNote turns the counts into the sentence an operator actually needs, INCLUDING the empty case.
//
// VACUITY MATTERS HERE, not just in the tests. A run over an empty store legitimately succeeds having done
// nothing, and "rewrap complete" over zero rows is the report that gets someone to retire a key version
// while rows they forgot about still depend on it. So zero rows says zero rows, and a store spread across
// more than one key version says so plainly rather than reporting a bare success.
func rewrapNote(o RewrapOutcome) string {
	if o.Scanned == 0 {
		return "no sealed secrets were scanned — nothing was rewrapped, and this run proves NOTHING about " +
			"which key versions are still in use"
	}
	vers := make([]string, 0, len(o.Versions))
	for v := range o.Versions {
		vers = append(vers, v)
	}
	sort.Strings(vers)
	parts := make([]string, 0, len(vers))
	for _, v := range vers {
		parts = append(parts, v+"="+strconv.Itoa(o.Versions[v]))
	}
	base := fmt.Sprintf("%d scanned, %d rewrapped, %d already current (re-put underneath the run); "+
		"key versions now: %s", o.Scanned, o.Rewrapped, o.Skipped, strings.Join(parts, " "))
	if len(vers) > 1 {
		return base + " — MORE THAN ONE key version is still in use; do NOT raise min_decryption_version yet"
	}
	if o.Partial {
		return base + " — PARTIAL run (bounded by limit, resume after " + o.LastName +
			"); rows beyond it were not examined, so the store as a whole is not yet accounted for"
	}
	return base + " — every scanned row is on one key version"
}

// rewrapOpts: no retries (a refused rewrap is a decision, like every other write in this lane), and a
// generous StartToCloseTimeout because this walks N rows with a key-service round trip each — unlike the
// single-row writes above, whose 15s is sized from measurement.
func rewrapOpts() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	}
}

// SecretRewrapWorkflow is the one-activity DEK rewrap workflow. Distinctly named for the same
// bare-function-name collision reason as its siblings (the 2026-07-17 worker boot-loop); it is on the
// guard list in temporal/skilltrial/finalizer_names_test.go.
func SecretRewrapWorkflow(ctx workflow.Context, req RewrapRequest) (RewrapOutcome, error) {
	var res RewrapOutcome
	err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, rewrapOpts()),
		new(Activities).RewrapSecretsActivity, req).Get(ctx, &res)
	return res, err
}
