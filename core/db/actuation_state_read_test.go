package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestStateReadsAreRecordedAndReadBackPerHostInTheirWindow(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the state-read round-trip")
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
	s := NewActuationStateReadStore(p)
	host := fmt.Sprintf("tg709-%d-%d", os.Getpid(), time.Now().UnixNano())
	if err := s.Record(ctx, "", "independent-condition"); err == nil {
		t.Fatal("a read with no host must be refused")
	}
	if err := s.Record(ctx, host, " "); err == nil {
		t.Fatal("a read with no purpose must be refused")
	}
	before := time.Now().Add(-time.Minute)
	if err := s.Record(ctx, host, "independent-condition"); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(ctx, "other-"+host, "independent-condition"); err != nil {
		t.Fatal(err)
	}
	got, err := s.TimesOn(ctx, "  "+upperASCII(host)+" ", before, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("exactly the one read on this host, matched case-insensitively, got %v", got)
	}
	if got, err := s.TimesOn(ctx, host, before.Add(-time.Hour), before); err != nil || len(got) != 0 {
		t.Fatalf("a window that ends before the read sees nothing, got %v err=%v", got, err)
	}
}

func upperASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 32
		}
	}
	return string(b)
}
