package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/safety"
)

// TG-664: the window's "has a later execution moved my resource?" read, through real SQL — every execution after the
// arm time with the SEALED action of its manifest, the whole host when the manifest is missing — and the new
// terminal state `superseded`, which migration 0125 admits to the state CHECK.
// KILLING MUTATION: drop the manifest join (scan only the host) — RED here (the unit is lost).
func TestExecutionsSinceCarryTheSealedActionAndSupersededIsAState(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the later-execution read")
	}
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer p.Close()
	st := NewActionExecutionStore(p)
	n := time.Now().UnixNano()
	host := fmt.Sprintf("tg664host%d", n)
	sealed, bare := fmt.Sprintf("tg664-sealed-%d", n), fmt.Sprintf("tg664-bare-%d", n)
	action := `{"op":"restart","op_class":"restart-service","target":"` + host + `","reversible":true,"params":{"unit":"nginx"}}`
	if _, err := p.Exec(ctx, `INSERT INTO action_manifest (action_id, action, band, plan_hash) VALUES ($1, $2::jsonb, 'AUTO'::band, $3)`,
		sealed, action, "plan-"+sealed); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}
	var since time.Time
	if err := p.QueryRow(ctx, `SELECT now() - interval '1 second'`).Scan(&since); err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{sealed, bare} {
		if err := st.Record(ctx, a, "tg664-ref-"+a, host, "nl", safety.VerdictMatch, true, ""); err != nil {
			t.Fatalf("record %s: %v", a, err)
		}
	}
	got, err := st.ExecutionsSince(ctx, since, host, 0)
	if err != nil {
		t.Fatal(err)
	}
	var sawSealed, sawBare bool
	for _, e := range got {
		switch e.ExternalRef {
		case "tg664-ref-" + sealed:
			sawSealed = e.Action.OpClass == "restart-service" && e.Action.Params["unit"] == "nginx" && e.Action.Target == host && e.ID > 0
		case "tg664-ref-" + bare:
			sawBare = e.Action.OpClass == "" && e.Action.Target == host
		}
	}
	if !sawSealed || !sawBare {
		t.Fatalf("both executions must come back — the sealed one with its action, the bare one as its whole host: sealed=%v bare=%v (%d rows)", sawSealed, sawBare, len(got))
	}
	if later, err := st.ExecutionsSince(ctx, time.Now().Add(time.Hour), host, 0); err != nil || len(later) != 0 {
		t.Fatalf("nothing is later than the future, got %d rows err=%v", len(later), err)
	}

	// The host scope: another host's executions are not read, and an FQDN spelling of this host is.
	if other, err := st.ExecutionsSince(ctx, since, host+"x", 0); err != nil {
		t.Fatal(err)
	} else {
		for _, e := range other {
			if e.ExternalRef == "tg664-ref-"+sealed || e.ExternalRef == "tg664-ref-"+bare {
				t.Fatalf("another host's read must not return this host's executions, got %+v", e)
			}
		}
	}
	fq := fmt.Sprintf("tg664-fqdn-%d", n)
	if err := st.Record(ctx, fq, "tg664-ref-"+fq, host+".example.net", "nl", safety.VerdictMatch, true, ""); err != nil {
		t.Fatal(err)
	}
	if got2, err := st.ExecutionsSince(ctx, since, host, 0); err != nil {
		t.Fatal(err)
	} else {
		found := false
		for _, e := range got2 {
			found = found || e.ExternalRef == "tg664-ref-"+fq
		}
		if !found {
			t.Fatal("an FQDN spelling of the host must be read (the caller's containment test canonicalises it)")
		}
	}

	cc := NewCommitConfirmStore(p)
	if err := cc.ArmCommitConfirm(ctx, CommitConfirmRow{ActionID: sealed, ExternalRef: "tg664-window-" + sealed, OpClass: "restart-service",
		TargetHost: host, Site: "nl", WindowSeconds: 600, ForwardBand: "AUTO"}); err != nil {
		t.Fatalf("arm: %v", err)
	}
	if err := cc.Resolve(ctx, sealed, "tg664-window-"+sealed, CommitConfirmSuperseded, "superseded: drill", ""); err != nil {
		t.Fatalf("superseded must be a resolvable state (migration 0125): %v", err)
	}
	if row, err := cc.Get(ctx, sealed, "tg664-window-"+sealed); err != nil || row.State != CommitConfirmSuperseded {
		t.Fatalf("the row must read superseded, got %+v err=%v", row, err)
	}
}
