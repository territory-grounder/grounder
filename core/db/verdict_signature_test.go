package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verdictsig"
)

const b3Seed = "9f3c1a2b4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8"

func verdictSigFixture(t *testing.T) (context.Context, *Pool) {
	t.Helper()
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to an empty database to run the verdict-signature round-trip")
	}
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(p.Close)
	return ctx, p
}

// seedPriorVerdictJoin builds the minimal REAL join fixture RecentForHost's action_verdict arm reads:
// the session row (alert rule), the prediction bridge (action_id→external_ref), and the executed
// gate row. Unique refs per call; nothing deleted (the chained-tables rule).
func seedPriorVerdictJoin(t *testing.T, ctx context.Context, p *Pool, ref, actionID, host string) {
	t.Helper()
	if _, err := p.Exec(ctx,
		`INSERT INTO session_triage (external_ref, host, alert_rule) VALUES ($1, $2, 'NginxDown') ON CONFLICT DO NOTHING`,
		ref, host); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if _, err := p.Exec(ctx, `
		INSERT INTO infragraph_prediction (plan_hash, action_id, target_host, prediction_hash, schema_version, kind, external_ref)
		VALUES ($1, $2, $3, $4, 1, 'action', $5) ON CONFLICT DO NOTHING`,
		"plan#"+actionID, actionID, host, "pred#"+actionID, ref); err != nil {
		t.Fatalf("seed prediction: %v", err)
	}
	if _, err := p.Exec(ctx,
		`INSERT INTO interceptor_gate_verdict (action_id, ordinal, gate, verdict) VALUES ($1, 1, 'execute', 'pass') ON CONFLICT DO NOTHING`,
		actionID); err != nil {
		t.Fatalf("seed gate row: %v", err)
	}
}

// TG-81 b3, the whole spine against real Postgres: an ARMED writer's row carries a signature that
// verifies; the ARMED reader keeps signed-valid and unsigned rows and DROPS a forged one. KILLING
// MUTATION: remove the verify filter in RecentForHost — the forged row survives and the count pins it.
func TestSignedVerdictRoundTripAndForgedRowIsDropped(t *testing.T) {
	ctx, p := verdictSigFixture(t)
	signer, err := verdictsig.NewSigner(b3Seed)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := verdictsig.NewVerifier(signer.PublicKeyHex())
	if err != nil {
		t.Fatal(err)
	}
	host := fmt.Sprintf("b3-web-%d", time.Now().UnixNano())

	// 1. Signed by the armed writer — must survive the armed read.
	signedID := "b3-signed-" + host
	seedPriorVerdictJoin(t, ctx, p, "ref-"+signedID, signedID, host)
	if err := NewVerdictStore(p).WithSigner(signer.Sign).Commit(ctx, signedID, "plan#"+signedID, host, "dc1", safety.VerdictMatch); err != nil {
		t.Fatalf("signed commit: %v", err)
	}
	var gotSig string
	if err := p.QueryRow(ctx, `SELECT signature FROM action_verdict WHERE action_id = $1`, signedID).Scan(&gotSig); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if gotSig == "" || !verifier.Verify(signedID, "plan#"+signedID, "match", host, "dc1", gotSig) {
		t.Fatalf("the armed writer's signature must verify, got %q", gotSig)
	}

	// 2. Unsigned (pre-feature / unarmed writer) — must survive the armed read as history.
	unsignedID := "b3-unsigned-" + host
	seedPriorVerdictJoin(t, ctx, p, "ref-"+unsignedID, unsignedID, host)
	if err := NewVerdictStore(p).Commit(ctx, unsignedID, "plan#"+unsignedID, host, "dc1", safety.VerdictPartial); err != nil {
		t.Fatalf("unsigned commit: %v", err)
	}

	// 3. FORGED: written around the VerdictSink with a signature that does not verify.
	forgedID := "b3-forged-" + host
	seedPriorVerdictJoin(t, ctx, p, "ref-"+forgedID, forgedID, host)
	if _, err := p.Exec(ctx, `
		INSERT INTO action_verdict (action_id, plan_hash, verdict, target_host, site, schema_version, signature)
		VALUES ($1, $2, 'match', $3, 'dc1', 2, $4)`,
		forgedID, "plan#"+forgedID, host, signer.Sign(forgedID, "plan#"+forgedID, "deviation", host, "dc1")); err != nil {
		t.Fatalf("forge: %v", err)
	}

	since := time.Now().Add(-time.Hour)
	armed, err := NewPriorVerdictStore(p).WithVerifier(verifier.Verify).RecentForHost(ctx, host, since, 10)
	if err != nil {
		t.Fatalf("armed read: %v", err)
	}
	if len(armed) != 2 {
		t.Fatalf("armed read must keep signed-valid + unsigned and DROP the forged row: got %d rows %+v", len(armed), armed)
	}
	for _, r := range armed {
		if r.Verdict == "match" && r.AlertRule != "NginxDown" {
			t.Fatalf("join broke: %+v", r)
		}
	}

	// 4. The UNARMED reader is byte-identical pre-b3: all three rows, forged included.
	unarmed, err := NewPriorVerdictStore(p).RecentForHost(ctx, host, since, 10)
	if err != nil {
		t.Fatalf("unarmed read: %v", err)
	}
	if len(unarmed) != 3 {
		t.Fatalf("the unarmed reader must not filter (pre-b3 shape): got %d", len(unarmed))
	}
}
