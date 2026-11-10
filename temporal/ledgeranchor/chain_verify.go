package ledgeranchor

// THE SCHEDULED FULL RE-WALK (TG-674, deep-review F16). core/audit.VerifyChain recomputes every row's content
// hash and checks the prev-hash linkage — the LedgerVerifier the package doc promised — but until this file no
// production job ever called it over the database ledger. What ran on a schedule compared STORED (seq, hash)
// pairs against recorded anchors (VerifyJob / TemporalVerifyJob): that catches a truncated tail and a rewritten
// prefix, but not a row whose decision text was edited while its stored hash was left alone (the stored hash
// is what the anchors compare; the CONTENT is what VerifyChain recomputes from). ChainVerifyJob closes that gap:
// every pass reads the whole chain and recomputes it. OBSERVE-ONLY like its siblings — it surfaces a broken
// chain and reaches no actuator; the operator response is a separate decision.

import (
	"context"
	"errors"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
)

// errNoLedgerReader is the fail-safe outcome of a ChainVerifyJob wired without a chain to read.
var errNoLedgerReader = errors.New("ledgeranchor: ChainVerifyJob has no ledger reader — nothing can be re-walked")

// ErrEmptyChain is the fail-safe outcome when the ledger holds NO rows: absent is not clear. On a deployment
// that has ever written a governance row an empty chain is a wipe or a restore of the wrong database, and a
// fresh deployment writes its first rows at boot — so "nothing to verify" is reported as UNVERIFIED (the
// warning rule), never as a clean pass.
var ErrEmptyChain = errors.New("ledgeranchor: the governance ledger is EMPTY — nothing to re-walk, which is not a clean verdict (absent is not clear)")

// ChainVerifyJob re-walks the full governance ledger with audit.VerifyChain.
type ChainVerifyJob struct {
	Ledger LedgerReader
}

// ChainVerifyResult is one pass's outcome.
type ChainVerifyResult struct {
	Rows   int
	Broken error
	At     time.Time
}

// Run reads the whole chain and recomputes it. Return shape mirrors VerifyJob.Run: (result, err, ok) where
// ok=false means the chain could not be READ (fail-safe, never a clean verdict), ok=true with result.Broken set
// means TAMPER, and ok=true with result.Broken nil means every row re-hashed to its stored hash. An EMPTY chain
// is ok=false (ErrEmptyChain): a verifier that reads an absent ledger as clean would certify a wipe.
func (j ChainVerifyJob) Run(ctx context.Context) (ChainVerifyResult, error, bool) {
	if j.Ledger == nil {
		return ChainVerifyResult{}, errNoLedgerReader, false
	}
	entries, err := j.Ledger.All(ctx)
	if err != nil {
		return ChainVerifyResult{}, err, false
	}
	if len(entries) == 0 {
		return ChainVerifyResult{At: time.Now().UTC()}, ErrEmptyChain, false
	}
	res := ChainVerifyResult{Rows: len(entries), At: time.Now().UTC()}
	res.Broken = audit.VerifyChain(entries)
	return res, nil, true
}

// RunChainVerifyPeriodically verifies once IMMEDIATELY (a deploy is when a fresh re-walk is worth most) and
// then every `every` until ctx is done. onBroken fires on a detected break (the critical operator signal);
// onErr when the pass could not run; onPass on a clean pass with the row count. Same independent-goroutine
// shape as the anchor verifiers, so a defect in one cross-check can never silence another.
func RunChainVerifyPeriodically(ctx context.Context, j ChainVerifyJob, every time.Duration, onBroken func(ChainVerifyResult), onErr func(error), onPass func(ChainVerifyResult)) {
	pass := func() {
		cctx, cancel := context.WithTimeout(ctx, every)
		defer cancel()
		res, err, ok := j.Run(cctx)
		switch {
		case !ok:
			if onErr != nil {
				onErr(err)
			}
		case res.Broken != nil:
			if onBroken != nil {
				onBroken(res)
			}
		default:
			if onPass != nil {
				onPass(res)
			}
		}
	}
	pass()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pass()
		}
	}
}
