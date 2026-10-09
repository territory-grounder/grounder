package eval

// ON-BOX gate extensions (TG-43): two integration tests that reuse the SAME harness as TestEvalCorpusOnBox
// (runOne / loadEstateGraph / the shared judge) WITHOUT touching it, so the committed baseline stays
// byte-for-byte comparable. Both SKIP unless TG_EVAL_GATEWAY is set (so CI, which has no model, is
// unaffected). They are orchestrated by eval/eval-gate.sh on the box.
//
//   - TestEvalControlsOnBox: runs the negative-control set (controls.json) — benign / expected /
//     no-action-warranted incidents — and records, per control, whether the agent PROPOSED. The correct
//     behavior is to NOT propose; a proposal is a control VIOLATION (a manufactured action). Writes
//     controls-scorecard.json, consumed by tools/evalgate (majority-vote pooling across runs).
//   - TestEvalHoldoutOnBox: runs the SEALED holdout subset (holdout-corpus.json — the system may never tune
//     to it) through the real Runner + judge and writes holdout-scorecard.json, so make eval-holdout can
//     compute the regression-vs-holdout gap (the >20pt overfitting signal, §1.3).

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/eval/gate"
	"github.com/territory-grounder/grounder/modules/ingest/librenms"
)

func TestEvalControlsOnBox(t *testing.T) {
	gwURL := os.Getenv("TG_EVAL_GATEWAY")
	if gwURL == "" {
		t.Skip("set TG_EVAL_GATEWAY + LITELLM_MASTER_KEY to run the on-box negative-control gate")
	}
	controls, err := LoadCorpus("controls.json")
	if err != nil {
		t.Fatalf("controls corpus: %v", err)
	}
	g := loadEstateGraph(t, "estate_fixture.json")
	gw := model.NewGateway(gwURL, config.SecretRef("env:LITELLM_MASTER_KEY"))

	// CONTROL-OBSERVABILITY PASS (TG-362). The propose corpus has been freshness-checked against the live
	// estate since 2026-07-30 — a stale incident leaves the recall denominator rather than scoring as an
	// agent miss. The CONTROLS never got the same treatment, and measured 2026-08-06 three of five control
	// hosts were unmonitored: dc1freeipa01 and dc1graylog01 absent from LibreNMS entirely,
	// dc1gitea01 administratively disabled with last_polled 2025-11-16. Both failing controls were among
	// those three.
	//
	// A control on such a host cannot discriminate. The agent investigates a synthetic alert against a real
	// host whose real state has nothing to do with the control's story, and whatever it legitimately finds
	// counts as a violation. So it is EXCLUDED — counted in neither direction — and the exclusion is logged
	// and carried in the scorecard, never silent. Compare turns an emptied bar into UNMEASURED, so this can
	// never quietly convert a FAIL into a PASS.
	excluded := controlObservability(t, controls)

	run := gate.ControlRun{N: len(controls)}
	var violations []string
	for _, inc := range controls {
		t.Logf("control %s: %s @ %s (%s)", inc.ExternalRef, inc.AlertRule, inc.Host, inc.Severity)
		s := runOne(t, gw, g, inc)
		if s.Mutated {
			t.Fatalf("SAFETY: control %s mutated during read-only eval", inc.ExternalRef)
		}
		res := gate.ControlResult{
			Ref: s.Ref, Proposed: s.Proposed, Band: s.Band, Outcome: s.Outcome, Conclusion: s.Conclusion,
		}
		if why, ok := excluded[inc.ExternalRef]; ok {
			res.Excluded, res.ExcludedReason = true, why
			t.Logf("control %s EXCLUDED: %s — its benignness is not observable, so it can neither pass nor fail", s.Ref, why)
		}
		run.Results = append(run.Results, res)
		if s.Proposed && !res.Excluded {
			violations = append(violations, s.Ref)
		}
	}
	b, _ := json.MarshalIndent(run, "", "  ")
	if err := os.WriteFile("controls-scorecard.json", b, 0o644); err != nil {
		t.Fatalf("write controls-scorecard: %v", err)
	}
	// Advisory here (a single run is noisy); the binding majority-vote pooling across runs is applied by
	// tools/evalgate. We surface the count so a single-run operator sees it immediately.
	t.Logf("CONTROLS DONE: %d controls, %d proposal(s) (should be 0): %v", run.N, len(violations), violations)
}

// controlObservability returns, per control ref, why its benignness cannot be read off the live estate.
// A host LibreNMS does not know, or has administratively disabled, cannot be the subject of the alert the
// control simulates — the alert could not fire there at all.
//
// It is a NO-OP without LIBRENMS_TOKEN (the same condition the corpus freshness pass uses), so a run with no
// estate access excludes nothing and the bar applies exactly as before. It fails LOUD on an API error, for
// the same reason the corpus pass does: a guessed observability verdict is worse than none.
func controlObservability(t *testing.T, controls []Incident) map[string]string {
	t.Helper()
	out := map[string]string{}
	// NO TOKEN ⇒ EXCLUDE EVERYTHING, which drives the bar to UNMEASURED. It used to return an EMPTY
	// exclusion set and log "the bar applies in full" at t.Log — a check that silently no-ops when its
	// credential is missing, which this repo's own doctrine forbids: "a check that silently no-ops when its
	// env is missing is how `make all` came to be green while skipping 34 DB tests. Absent is visible;
	// skipped is not."
	//
	// It was not hypothetical. eval-gate.sh read `LIBRENMS_TOKEN` out of the box's .env; the secret-policy
	// migration replaced that with `TG_LIBRENMS_INGEST_TOKEN_REF` and no raw value, so the token has been
	// empty in every gate invocation since. Every control counted in full, including the three whose hosts
	// were measured UNMONITORED on 2026-08-06 — and ctl-01 (dc1freeipa01) is one of them. It failed on
	// clean main, and it blocked !1018 for two days.
	//
	// Excluding all is the fail-SAFE direction and the honest one: Compare turns an emptied bar into
	// UNMEASURED, and an UNMEASURED bar cannot be a PASS. So this can never convert a real FAIL into a
	// PASS — it converts an UNADJUDICABLE bar into one that says so.
	if os.Getenv("LIBRENMS_TOKEN") == "" {
		t.Log("control observability: no LibreNMS token — observability is UNVERIFIABLE, so every control is " +
			"excluded and the negative-control bar reports UNMEASURED rather than being applied blind")
		for _, inc := range controls {
			out[inc.ExternalRef] = "control observability unverifiable: no LibreNMS token in this run, so " +
				"whether this control's host is monitored at all could not be determined"
		}
		return out
	}
	base := os.Getenv("TG_LIBRENMS_URL")
	if base == "" {
		base = "https://dc1nms01.example.net"
	}
	c := &http.Client{Timeout: 20 * time.Second}
	if v := os.Getenv("TG_LIBRENMS_INSECURE"); v == "1" || strings.EqualFold(v, "true") {
		c.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // internal self-signed estate endpoint, opt-in
	}
	deps := []librenms.Deployment{{Site: "nl", BaseURL: base, TokenRef: "env:LIBRENMS_TOKEN"}}
	for _, inc := range controls {
		found, disabled, _, err := librenms.LiveIncidentState(context.Background(), deps, c, inc.Host)
		if err != nil {
			t.Fatalf("control observability for %s: %v (a guessed verdict is worse than none)", inc.ExternalRef, err)
		}
		if why := gate.ControlExclusionReason(found, disabled, inc.Host); why != "" {
			out[inc.ExternalRef] = why
		}
	}
	return out
}

func TestEvalHoldoutOnBox(t *testing.T) {
	gwURL := os.Getenv("TG_EVAL_GATEWAY")
	if gwURL == "" {
		t.Skip("set TG_EVAL_GATEWAY + LITELLM_MASTER_KEY to run the on-box sealed-holdout eval")
	}
	corpus, err := LoadCorpus("holdout-corpus.json")
	if err != nil {
		t.Fatalf("holdout corpus: %v", err)
	}
	g := loadEstateGraph(t, "estate_fixture.json")
	gw := model.NewGateway(gwURL, config.SecretRef("env:LITELLM_MASTER_KEY"))

	var sessions []Session
	for _, inc := range corpus {
		t.Logf("holdout %s: %s @ %s (%s)", inc.ExternalRef, inc.AlertRule, inc.Host, inc.Severity)
		sessions = append(sessions, runOne(t, gw, g, inc))
	}
	var scores []Score
	for _, s := range sessions {
		raw, err := gw.Complete(context.Background(), "eval-judge", "primary", []model.Message{{Role: "user", Content: judgePrompt(s)}})
		if err != nil {
			t.Logf("judge %s: %v", s.Ref, err)
			continue
		}
		sc, perr := ParseScore(s.Ref, raw)
		if perr != nil {
			t.Logf("judge parse %s: %v", s.Ref, perr)
			continue
		}
		scores = append(scores, sc)
	}
	SortSessions(sessions)
	card := Aggregate(sessions, scores)
	if err := os.WriteFile("holdout-scorecard.json", ScorecardJSON(card), 0o644); err != nil {
		t.Fatalf("write holdout-scorecard: %v", err)
	}
	if card.MutationCount != 0 {
		t.Fatalf("SAFETY: mutation occurred during read-only holdout eval (count=%d)", card.MutationCount)
	}
	t.Logf("HOLDOUT DONE: %d sessions, overall %.2f/5", card.N, card.Overall)
}
