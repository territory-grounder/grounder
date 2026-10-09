package main

// ORACLES FOR THE RATIFIED-OVERLAY REFRESH (TG-227 blockers 2+3).
//
// Every test names the mutation that turns it RED, because the defect this file guards against is the
// repository's own: machinery that exists, validates, and is consulted by nothing. These drive the REAL
// overlayRefresher against the REAL opschema registry and the REAL policy.Ladder — only the store is a
// fake, because the store is the seam.
//
// The registry is package-global (opschema's atomic snapshot), so these tests never run in parallel and
// every one restores embedded-only state on cleanup.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/policy"
)

type fakeOverlayStore struct {
	rows []db.RatifiedRow
	err  error
}

func (f *fakeOverlayStore) LiveOverlay(context.Context) ([]db.RatifiedRow, error) {
	return f.rows, f.err
}

// tg227Spec is a well-formed, template-encoded overlay class (argv[0] literal, whole-element slot bound to
// a required param) — the shape an operator actually ratifies. Distinct slugs per test keep the global
// registry honest between tests.
func tg227Spec(slug string) opschema.OpClassSpec {
	return opschema.OpClassSpec{
		OpClass:      slug,
		Op:           "rotate log",
		Family:       opschema.FamilyServiceLifecycle,
		SafetyTier:   opschema.TierLowReversible,
		EffectKind:   string(opschema.EffectSSHArgv),
		ArgvTemplate: []string{"logrotate", "--force", "${config}"},
		Params:       []opschema.ParamSpec{{Name: "config", Required: true}},
	}
}

func tg227Row(t *testing.T, slug string, threshold int) db.RatifiedRow {
	t.Helper()
	spec := tg227Spec(slug)
	h, err := opschema.CanonicalHash(spec)
	if err != nil {
		t.Fatal(err)
	}
	return db.RatifiedRow{OpClass: slug, Spec: spec, EntryHash: h, Family: spec.Family,
		Tier: spec.SafetyTier, PromoteThreshold: threshold}
}

func refresherForTest(t *testing.T, store *fakeOverlayStore) (*overlayRefresher, *[]string) {
	t.Helper()
	t.Cleanup(opschema.ClearOverlay)
	var logs []string
	r := newOverlayRefresher(store, func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) })
	return r, &logs
}

func logsContain(logs []string, frag string) bool {
	for _, l := range logs {
		if strings.Contains(l, frag) {
			return true
		}
	}
	return false
}

// KILLING MUTATION: pass the ledger row through with OverlayEntry.Hash recomputed from the spec instead of
// the stored entry_hash — a tampered row would then always "verify". RED.
func TestTamperedOverlayRowIsDroppedLoudlyAtRefresh(t *testing.T) {
	row := tg227Row(t, "tg227-tampered", 10)
	row.Spec.ArgvTemplate = []string{"rm", "-rf", "${config}"} // content no longer matches the attested hash
	store := &fakeOverlayStore{rows: []db.RatifiedRow{row}}
	r, logs := refresherForTest(t, store)

	if err := r.RefreshOnce(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, ok := opschema.Lookup("tg227-tampered"); ok {
		t.Fatal("a row whose content disagrees with its ledger-attested hash reached the composed registry")
	}
	if _, ok := r.ThresholdFor("tg227-tampered"); ok {
		t.Fatal("a DROPPED row's promote threshold was armed — registry and ladder disagree about the row")
	}
	if !logsContain(*logs, "DROPPED") {
		t.Fatal("the drop was silent — a shrinking capability set must be loud")
	}
}

// KILLING MUTATION: skip SetOverlay when the read returns zero rows ("keep last good on empty"). RED —
// a revoked class would keep actuating until restart, the unsafe staleness direction.
func TestRevokedClassExitsComposedRegistryWithinOneRefresh(t *testing.T) {
	store := &fakeOverlayStore{rows: []db.RatifiedRow{tg227Row(t, "tg227-revocable", 10)}}
	r, _ := refresherForTest(t, store)

	if err := r.RefreshOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := opschema.Lookup("tg227-revocable"); !ok {
		t.Fatal("the ratified class never reached the composed registry")
	}
	if n, ok := r.ThresholdFor("tg227-revocable"); !ok || n != 10 {
		t.Fatalf("threshold not armed: got (%d,%v), want (10,true)", n, ok)
	}

	store.rows = nil // the revoke: the live view no longer returns the row
	if err := r.RefreshOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := opschema.Lookup("tg227-revocable"); ok {
		t.Fatal("a REVOKED class is still served by the composed registry after a successful refresh")
	}
	if _, ok := r.ThresholdFor("tg227-revocable"); ok {
		t.Fatal("a REVOKED class's promote threshold survived the refresh")
	}
}

// KILLING MUTATION: build the threshold map from the raw rows before SetOverlay's admission filter. RED —
// the rejected row's threshold would ride along while its capability was refused.
func TestThresholdMapAndRegistrySnapshotCannotDiverge(t *testing.T) {
	good := tg227Row(t, "tg227-good", 12)
	bad := tg227Row(t, "tg227-bad", 99)
	bad.EntryHash = "0000000000000000000000000000000000000000000000000000000000000000"
	store := &fakeOverlayStore{rows: []db.RatifiedRow{good, bad}}
	r, _ := refresherForTest(t, store)

	if err := r.RefreshOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n, ok := r.ThresholdFor("tg227-good"); !ok || n != 12 {
		t.Fatalf("accepted row's threshold: got (%d,%v), want (12,true)", n, ok)
	}
	if _, ok := r.ThresholdFor("tg227-bad"); ok {
		t.Fatal("a registry-REJECTED row contributed a promote threshold — the two views diverged")
	}
}

// KILLING MUTATION: on a store read error, install an empty overlay (fail-open to embedded-only with no
// loud line). RED — a config-plane blip would strip live capabilities silently.
func TestOverlayReadErrorKeepsLastGoodRegistryLoudly(t *testing.T) {
	store := &fakeOverlayStore{rows: []db.RatifiedRow{tg227Row(t, "tg227-durable", 8)}}
	r, logs := refresherForTest(t, store)

	if err := r.RefreshOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	store.err = errors.New("connection refused")
	if err := r.RefreshOnce(context.Background()); err == nil {
		t.Fatal("a failed read reported success")
	}
	if _, ok := opschema.Lookup("tg227-durable"); !ok {
		t.Fatal("a store outage stripped a live capability — must keep the last good snapshot")
	}
	if n, ok := r.ThresholdFor("tg227-durable"); !ok || n != 8 {
		t.Fatalf("threshold lost on outage: got (%d,%v), want (8,true)", n, ok)
	}
	if !logsContain(*logs, "FAILED") {
		t.Fatal("the outage was silent — an operator watching for their grant must hear it")
	}
}

// THE TG-248 DEFECT ITSELF. KILLING MUTATION: drop the WithPerClassThreshold chain from
// buildGraduationLadder — the ratified-at-10 class then promotes at the compiled bar. RED (the control
// half of this test proves the compiled bar IS the promotion point when no resolver is chained, so the
// assertion cannot pass vacuously).
func TestProductionLadderReadsRatifiedPerClassThreshold(t *testing.T) {
	const slug = "tg227-slow-climber"
	const ratifiedAt = policy.DefaultPromoteThreshold + 5

	store := &fakeOverlayStore{rows: []db.RatifiedRow{tg227Row(t, slug, ratifiedAt)}}
	r, _ := refresherForTest(t, store)
	if err := r.RefreshOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	// CONTROL: an unchained ladder promotes at the compiled bar. This is the exact behaviour the
	// mutation reintroduces, held here so the main assertion is proven non-vacuous.
	control := buildGraduationLadder(policy.NewMemGraduationStore(), nil)
	for i := 1; i <= policy.DefaultPromoteThreshold; i++ {
		res, err := control.Record(context.Background(), slug, policy.OutcomeVerifiedClean)
		if err != nil {
			t.Fatal(err)
		}
		if i == policy.DefaultPromoteThreshold && !res.Promoted {
			t.Fatalf("control: expected promotion at the compiled bar (%d clean runs)", i)
		}
	}

	// THE REAL CHAIN: same ladder construction production uses, with the refresher's resolver. The class
	// ratified at a HIGHER bar must not promote at the compiled one.
	ladder := buildGraduationLadder(policy.NewMemGraduationStore(), r.ThresholdFor)
	for i := 1; i <= ratifiedAt; i++ {
		res, err := ladder.Record(context.Background(), slug, policy.OutcomeVerifiedClean)
		if err != nil {
			t.Fatal(err)
		}
		if i < ratifiedAt && res.Promoted {
			t.Fatalf("promoted after %d clean runs; the ratification set the bar at %d — the per-class "+
				"threshold is not being read (TG-248)", i, ratifiedAt)
		}
		if i == ratifiedAt && !res.Promoted {
			t.Fatalf("NOT promoted at the ratified bar (%d clean runs) — the resolver is raising the bar "+
				"beyond what the ratification stored", ratifiedAt)
		}
	}
}

// TG-177 END TO END: the composed-registry refresh carries the ratify verb's fail-closed graduation reset
// into the LIVE enforcement ladder — the reviewer's exploit, proven closed. An overlay class graduated in a
// prior life is revoked and re-ratified; the ratify reset writes the durable store directly, bypassing the
// ladder's per-process cache, and a refresh pass must evict the warm cache so the gate reloads the reset
// level instead of the inherited one.
//
// KILLING MUTATION: drop the r.WithLadderEvict wiring here (or the r.evict call in RefreshOnce). RED — the
// ladder keeps enforcing auto_notice after the reset + refresh, the exact stale-trust hole this closes.
func TestRefreshCarriesTheRatifyResetIntoTheEnforcementLadder(t *testing.T) {
	ctx := context.Background()
	const slug = "tg177-reused-overlay"

	// The enforcement ladder over a store seeded as if the slug graduated before its revoke. Overlay classes
	// cap at auto_notice (ADR-0016) — the realistic inherited level for a revoked-then-re-ratified slug.
	store := policy.NewMemGraduationStore().Seed(policy.ClassState{OpClass: slug, Level: policy.LevelAutoNotice, NoticeRunCount: 5})
	ladder := buildGraduationLadder(store, nil)

	ostore := &fakeOverlayStore{rows: []db.RatifiedRow{tg227Row(t, slug, 10)}}
	r, _ := refresherForTest(t, ostore)
	r.WithLadderEvict(ladder.Forget)

	// Pass 1 admits the class; the enforcement read warms the cache at the inherited auto_notice.
	if err := r.RefreshOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := ladder.LevelOf(ctx, slug); got != policy.LevelAutoNotice {
		t.Fatalf("pre-reset: the enforcement ladder should serve the seeded auto_notice; got %v", got)
	}

	// The ratify verb resets the DURABLE row to approve (its Deps.Ladder is the store — it never touches this
	// ladder's cache). This is the write the refresher must carry through to the gate.
	store.Seed(policy.ClassState{OpClass: slug, Level: policy.LevelApprove})

	// A refresh pass — the ratify kick in this process, or the next TTL tick in any other enforcement
	// process — must evict the slug so the reload sees approve.
	if err := r.RefreshOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := ladder.LevelOf(ctx, slug); got != policy.LevelApprove {
		t.Fatalf("after the reset + one refresh the enforcement ladder still serves %v — the warm cache is "+
			"handing a re-ratified class inherited trust it never re-earned (TG-177 hole)", got)
	}
}
