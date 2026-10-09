package db

import (
	"fmt"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/auth"
)

// TG-532 — THE READ SURFACE MUST NOT PRESENT A SHAPE'S HISTORY AS A SESSION'S OUTCOME.
//
// action_id is content-addressed over the operation SHAPE and the manifest is sealed first-wins, so ONE
// ribbon can be the identity of many sessions: measured live 2026-08-22, 69 action_ids on this deployment
// are shared by more than one session and the worst by 198. The ribbon published approval_choice and
// verdict bare, so an operator reading a session's action saw another session's approval — in the filed
// case, one recorded a month earlier. The ribbon now carries WHOSE resolution each label is and how many
// sessions share the shape.
//
// KILLING MUTATION: drop approval_ref/verdict_ref from the Recent() projection (or the sessions-sharing
// subquery) — the ownership assertions below fail and the ribbon is silent about the shape again.
func TestRibbonNamesWhoseResolutionItPublishes(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()
	defer seedManifests(ctx, t, p)()

	// THIS TEST OWNS ITS FIXTURE. It used to borrow gold-act-2, which broke a sibling
	// (TestAnUnrecordedConfidenceIsNotZero asserts gold-act-2 "has no risk audit and no triage row at
	// all") — because session_risk_audit is APPEND-ONLY (0015 revokes DELETE from the runtime role), so a
	// row seeded against a shared manifest is permanent and silently changes what every later reader sees.
	// Seed unique, delete nothing, disturb nobody.
	aid := fmt.Sprintf("tg532-shape-%d", time.Now().UnixNano())
	if _, err := p.Exec(ctx, `
		INSERT INTO action_manifest (action_id, action, band, plan_hash, prediction_hash, sealed_at,
		                             approval_choice, verdict, approval_ref, verdict_ref)
		VALUES ($1, $2::jsonb, 'POLL_PAUSE', 'plan-'||$1, 'pred-'||$1, now(), 'approve', 'match',
		        'gold-sess-A', 'gold-sess-A')`,
		aid, `{"op":"restart","op_class":"restart-service","target":"tg532-host","reversible":true}`); err != nil {
		t.Fatalf("seal own manifest: %v", err)
	}
	for _, ref := range []string{aid + "-sessA", aid + "-sessB"} {
		if _, err := p.Exec(ctx, `
			INSERT INTO infragraph_prediction (plan_hash, action_id, target_host, prediction_hash, schema_version, kind, external_ref)
			VALUES ($1, $2, 'tg532-host', $3, 1, 'action', $4)
			ON CONFLICT DO NOTHING`, "plan-"+ref, aid, "predh-"+ref, ref); err != nil {
			t.Fatalf("seed sharing session %s: %v", ref, err)
		}
	}

	got, err := NewActionManifestReadStore(p).Recent(ctx, auth.Principal{}, 200)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	r, ok := ribbonsByID(got)[aid]
	if !ok {
		t.Fatal("the test's own manifest is missing from the ribbon page")
	}
	if r.ApprovalChoice != "approve" || r.Verdict == "" {
		t.Fatalf("fixture drifted — this oracle needs a labelled ribbon, got %+v", r)
	}
	if r.ApprovalRef != "gold-sess-A" || r.VerdictRef != "gold-sess-A" {
		t.Fatalf("the ribbon must name WHOSE resolution it publishes, got approval_ref=%q verdict_ref=%q",
			r.ApprovalRef, r.VerdictRef)
	}
	if r.SessionsSharing < 2 {
		t.Fatalf("two sessions share this shape; the ribbon must say so (got %d) — otherwise a reader takes "+
			"a shape's history for their own session's outcome", r.SessionsSharing)
	}
	before := r.SessionsSharing

	// THE UNDER-COUNT THE PREDICTION BRIDGE ALONE PRODUCES. A manifest can be sealed and CLASSIFIED with
	// no committed prediction (prediction_hash is nullable), so a session that exists only in
	// session_risk_audit is invisible to the prediction leg. Measured live 2026-08-22: that happens on 21
	// of 139 sealed manifests, and the prediction leg NEVER sees more — so counting it alone under-reports,
	// which is the dangerous direction (a ribbon saying "1" invites the "this label must be mine" reading).
	//
	// KILLING MUTATION: drop the session_risk_audit leg of the UNION — this session vanishes from the count
	// and the assertion below fails.
	// session_risk_audit is APPEND-ONLY (migration 0015 revokes DELETE from the runtime role on the audit
	// spine), so this row cannot be cleaned up and a fixed ref would make the test pass once and then
	// compare 3-against-3 forever. A UNIQUE ref per run keeps the delta honest and leaves the spine intact
	// — the chained-tables rule: seed unique, delete nothing.
	classifyOnlyRef := aid + "-classify-only"
	if _, err := p.Exec(ctx, `
		INSERT INTO session_risk_audit (external_ref, risk_level, band, action_id, schema_version, signals_json)
		VALUES ($1, 'medium', 'POLL_PAUSE', $2, 1, '{}')`, classifyOnlyRef, aid); err != nil {
		t.Fatalf("seed prediction-less session: %v", err)
	}
	got2, err := NewActionManifestReadStore(p).Recent(ctx, auth.Principal{}, 200)
	if err != nil {
		t.Fatalf("Recent (after risk-audit-only session): %v", err)
	}
	if after := ribbonsByID(got2)[aid].SessionsSharing; after != before+1 {
		t.Fatalf("a session that classified this shape WITHOUT committing a prediction must still count "+
			"(before=%d after=%d) — counting only the prediction bridge under-reports sharing on 21 of this "+
			"deployment's 139 manifests", before, after)
	}
	if len(got2) != len(got) {
		t.Fatalf("the sharing subquery multiplied ribbons (%d -> %d) — the identity-collapse hazard this "+
			"file guards against twice", len(got), len(got2))
	}

	// The unshared, unlabelled case stays honest in the other direction: no owner invented, no sharing
	// implied. (Without this the assertions above could pass on a store that stamped every row.)
	if u := ribbonsByID(got)["gold-act-4"]; u.ApprovalRef != "" || u.VerdictRef != "" {
		t.Fatalf("an unresolved manifest must claim no owner, got approval_ref=%q verdict_ref=%q",
			u.ApprovalRef, u.VerdictRef)
	}
}
