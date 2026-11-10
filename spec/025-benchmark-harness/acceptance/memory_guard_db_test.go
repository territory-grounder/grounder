package acceptance

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/tools/faultinjector"
)

// TG-710 — the memory guard's refusals land in injection_skip, a record kept apart from injected_fault: a skipped
// target was never faulted, so it can never enter a fault denominator. This pins the store's write against the
// real migration (columns, constraints, the append-only grant and the plane declaration); the guard's decisions
// are pinned without a database in tools/faultinjector.
func TestASkippedTargetIsRecordedApartFromTheInjectionLedger(t *testing.T) {
	dsn := os.Getenv("TG_TEST_DSN")
	if dsn == "" {
		t.Skip("TG_TEST_DSN not set — the skip record needs a migrated Postgres (the CI harness job supplies one)")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	note := fmt.Sprintf("tg710-acceptance-%d", time.Now().UnixNano())
	read := time.Now().UTC().Truncate(time.Microsecond)
	st := faultinjector.NewStore(pool.Pool)
	withReading := faultinjector.GuardSkip{
		Host: "guest-g1", Class: faultinjector.ClassDeviceDown, Node: "node-g", Stage: faultinjector.SkipAtPlan,
		Reason: faultinjector.SkipBelowThreshold, Detail: "node-g has 12.00 GiB available; needs 19.00 GiB",
		HasTelemetry: true, AvailableBytes: 12 << 30, PSISomeAvg60: 0.07, TelemetryReadAt: read, RequiredBytes: 19 << 30, Note: note,
	}
	blind := faultinjector.GuardSkip{
		Host: "guest-g2", Class: faultinjector.ClassServiceDown, Node: "node-g", Stage: faultinjector.SkipBeforeEffect,
		Reason: faultinjector.SkipTelemetryUnavailable, Detail: "node-g telemetry unavailable: read exited 255", Note: note,
	}
	for _, s := range []faultinjector.GuardSkip{withReading, blind} {
		if err := st.RecordSkip(ctx, s); err != nil {
			t.Fatalf("record %s: %v", s.Host, err)
		}
	}

	rows, err := pool.Query(ctx, `
		SELECT host, fault_type, node, stage, reason_key, detail, available_bytes, required_bytes, psi_some_avg60, telemetry_read_at
		  FROM injection_skip WHERE note = $1 ORDER BY seq`, note)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	defer rows.Close()
	type row struct {
		host, class, node, stage, reason, detail string
		avail, need                              *int64
		psi                                      *float64
		at                                       *time.Time
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.host, &r.class, &r.node, &r.stage, &r.reason, &r.detail, &r.avail, &r.need, &r.psi, &r.at); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if len(got) != 2 {
		t.Fatalf("want both skips recorded, got %d", len(got))
	}
	a, b := got[0], got[1]
	if a.host != "guest-g1" || a.class != "device-down" || a.node != "node-g" || a.stage != "plan" || a.reason != "below-threshold" ||
		a.avail == nil || *a.avail != 12<<30 || a.need == nil || *a.need != 19<<30 || a.psi == nil || *a.psi != 0.07 || a.at == nil || !a.at.Equal(read) {
		t.Errorf("the reading-backed skip must keep its reading and its threshold: %+v", a)
	}
	if b.stage != "pre-effect" || b.reason != "telemetry-unavailable" || b.avail != nil || b.need != nil || b.psi != nil || b.at != nil || b.detail == "" {
		t.Errorf("a skip with no reading must store no invented numbers, only why: %+v", b)
	}

	var faulted int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM injected_fault WHERE note = $1`, note).Scan(&faulted); err != nil {
		t.Fatalf("injected_fault: %v", err)
	}
	if faulted != 0 {
		t.Errorf("a skip wrote %d injected_fault row(s); a skipped target was never faulted", faulted)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO injection_skip (host, fault_type, node, stage, reason_key, note) VALUES ('g','device-down','n','plan','because',$1)`, note); err == nil {
		t.Error("a reason outside the closed set was accepted")
	}
	var canInsert, canSelect, canUpdate, canDelete bool
	if err := pool.QueryRow(ctx, `SELECT has_table_privilege('tg_runtime','injection_skip','INSERT'), has_table_privilege('tg_runtime','injection_skip','SELECT'),
	        has_table_privilege('tg_runtime','injection_skip','UPDATE'), has_table_privilege('tg_runtime','injection_skip','DELETE')`).Scan(&canInsert, &canSelect, &canUpdate, &canDelete); err != nil {
		t.Fatalf("privileges: %v", err)
	}
	if !canInsert || !canSelect || canUpdate || canDelete {
		t.Errorf("tg_runtime must append and read skips and never rewrite them: insert=%v select=%v update=%v delete=%v", canInsert, canSelect, canUpdate, canDelete)
	}
	var plane string
	if err := pool.QueryRow(ctx, `SELECT obj_description('injection_skip'::regclass, 'pg_class')`).Scan(&plane); err != nil || plane != "plane: both" {
		t.Errorf("injection_skip must declare its plane like injected_fault does, got %q (%v)", plane, err)
	}
}
