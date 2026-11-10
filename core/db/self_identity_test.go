package db

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/safety"
)

// Gated on TG_TEST_POSTGRES_DSN: the published self identity (0129) and the execution-time read the journal
// reader correlates against, through real SQL (TG-684).
func TestSelfIdentityIsPublishedOncePerKeyAndReadNewestFirst(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the self_identity round-trip")
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
	s := NewSelfIdentityStore(p)
	domain := fmt.Sprintf("tg684-%d-%d", os.Getpid(), time.Now().UnixNano())
	if _, ok, err := s.Latest(ctx, domain); err != nil || ok {
		t.Fatalf("nothing published yet must read ok=false, got ok=%v err=%v", ok, err)
	}
	if _, err := s.Publish(ctx, domain, "", "actuation"); err == nil {
		t.Fatal("an empty actor must be refused")
	}
	if added, err := s.Publish(ctx, domain, "root!SHA256:first", "actuation"); err != nil || !added {
		t.Fatalf("first publication must add a row, got added=%v err=%v", added, err)
	}
	if added, err := s.Publish(ctx, domain, "root!SHA256:first", "actuation"); err != nil || added {
		t.Fatalf("the same identity again must add nothing, got added=%v err=%v", added, err)
	}
	if added, err := s.Publish(ctx, domain, "root!SHA256:rotated", "actuation"); err != nil || !added {
		t.Fatalf("a rotated key must add a row, got added=%v err=%v", added, err)
	}
	if a, ok, err := s.Latest(ctx, domain); err != nil || !ok || a != "root!SHA256:rotated" {
		t.Fatalf("Latest must answer the newest identity, got %q ok=%v err=%v", a, ok, err)
	}
}

func TestExecutionsOnAreHostScopedWindowedAndCarryTheOpClass(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the execution-time read")
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
	host := fmt.Sprintf("TG684host%d", n)
	sshAction, pveAction, bareAction := fmt.Sprintf("tg684-ssh-%d", n), fmt.Sprintf("tg684-pve-%d", n), fmt.Sprintf("tg684-bare-%d", n)
	// The op-class lives on the sealed manifest (action_manifest.action->>'op_class'); bareAction has none.
	for _, m := range []struct{ id, opClass string }{{sshAction, "restart-service"}, {pveAction, "start-guest"}} {
		action := `{"op":"restart","op_class":"` + m.opClass + `","target":"` + host + `","reversible":true,"params":{}}`
		if _, err := p.Exec(ctx, `INSERT INTO action_manifest (action_id, action, band, plan_hash) VALUES ($1, $2::jsonb, 'AUTO'::band, $3)`,
			m.id, action, "plan-"+m.id); err != nil {
			t.Fatalf("seed manifest %s: %v", m.id, err)
		}
	}
	for _, a := range []string{sshAction, pveAction, bareAction} {
		if err := st.Record(ctx, a, "tg684-ref", host, "nl", safety.VerdictMatch, true, ""); err != nil {
			t.Fatalf("record %s: %v", a, err)
		}
	}
	if err := st.Record(ctx, sshAction, "tg684-ref", "other-"+host, "nl", safety.VerdictMatch, true, ""); err != nil {
		t.Fatalf("record other: %v", err)
	}
	now := time.Now()
	got, err := st.ExecutionsOn(ctx, strings.ToLower(host), now.Add(-time.Minute), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	classes := map[string]int{}
	for _, e := range got {
		classes[e.OpClass]++
	}
	if len(got) != 3 || classes["restart-service"] != 1 || classes["start-guest"] != 1 || classes[""] != 1 {
		t.Fatalf("exactly this host's three executions, matched case-insensitively, each with its manifest's op-class "+
			"(\"\" where no manifest names one), must be returned, got %+v", got)
	}
	if got, err := st.ExecutionsOn(ctx, host, now.Add(-2*time.Hour), now.Add(-time.Hour)); err != nil || len(got) != 0 {
		t.Fatalf("an execution outside the window must not be returned, got %v err=%v", got, err)
	}
}
