package db

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/episodes"
)

// TG-672 / REQ-2534 — the episode loader over a real Postgres: the lookback, the note-prefix selection, the
// virtualisation kind from guest_config_baseline, and the joins the pure attribution then runs. The fixture
// is the 1670/1675 shape (two overlapping faults on one guest, one execution) plus a pre-window owner, in a
// private 2h window somewhere in 1980–1989 (rows accrete; the note prefix is unique per run).
func TestEpisodeRead_LoadsTheCohortWithItsLookback(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()
	run := fmt.Sprintf("tg672-%d", time.Now().UnixNano())
	since := time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(rand.Int64N(10*365*24*60)) * time.Minute)
	until := since.Add(2 * time.Hour)
	at := func(min int) time.Time { return since.Add(time.Duration(min) * time.Minute) }
	hostA, hostD := "dc1"+run+"-a", "dc1"+run+"-d"
	exec := func(what, q string, args ...any) {
		t.Helper()
		if _, err := p.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seed %s: %v", what, err)
		}
	}
	fault := func(host, typ, ref string, inj, due int) int64 {
		t.Helper()
		var id int64
		if err := p.QueryRow(ctx, `
			INSERT INTO injected_fault (host, fault_type, note, injected_at, restore_due_at, restored_at, restore_state, fault_ref)
			VALUES ($1,$2,$3,$4,$5,$5,'restored',$6) RETURNING id`, host, typ, run+" soak", at(inj), at(due), ref).Scan(&id); err != nil {
			t.Fatalf("seed fault %s: %v", host, err)
		}
		return id
	}
	f1 := fault(hostA, "container-hang", run+"-vm-a", 10, 55)
	f2 := fault(hostA, "device-down", run+"-vm-a", 51, 96)
	fD := fault(hostD, "device-down", "", -30, 15)
	// A fault of ANOTHER campaign in the window: excluded by the note prefix.
	exec("other campaign", `INSERT INTO injected_fault (host, fault_type, note, injected_at) VALUES ($1,'device-down','tg672-other campaign',$2)`,
		"dc1"+run+"-other", at(20))
	// guest_config_baseline knows guest a as a qemu VM by its vmid (fault_ref); guest d is unknown.
	exec("baseline", `INSERT INTO guest_config_baseline (vmid, guest, node, kind, config_hash) VALUES ($1,$2,'',$3,'h')`,
		rand.Int64N(1<<40)+1000000, hostA, "qemu")
	exec("baseline ref", `UPDATE injected_fault SET fault_ref = (SELECT vmid::text FROM guest_config_baseline WHERE guest = $1 LIMIT 1) WHERE host = $1`, hostA)
	alert := func(ref, host, rule string, min int) {
		t.Helper()
		exec("alert "+ref, `INSERT INTO ingest_alert (external_ref, source_type, alert_rule, host, site, received_at) VALUES ($1,'librenms',$2,$3,'dc1',$4)`,
			run+"-"+ref, rule, host, at(min))
	}
	alert("hang", hostA, "Service up/down", 16)
	alert("down", hostA, "Device Down (SNMP unreachable)", 52)
	alert("d-early", hostD, "Device-Down", -20)
	exec("session", `INSERT INTO session_triage (external_ref, host, alert_rule, band, op, op_class, proposed, outcome, conclusion, created_at, synthetic)
		VALUES ($1,$2,'Device Down (SNMP unreachable)','AUTO','start','start-guest',true,'proposed','c',$3,true)`, run+"-down", hostA, at(52))
	exec("exec down", `INSERT INTO action_execution (action_id, external_ref, verdict, unverifiable, target_host, site, executed_at)
		VALUES ($1,$2,'match'::verdict,false,$3,'dc1',$4)`, run+"-act", run+"-down", hostA, at(52).Add(5*time.Second))
	exec("exec d", `INSERT INTO action_execution (action_id, external_ref, verdict, unverifiable, target_host, site, executed_at)
		VALUES ($1,$2,'match'::verdict,false,$3,'dc1',$4)`, run+"-act", run+"-d-early", hostD, at(-18))
	exec("credit", `INSERT INTO graduation_credit (op_class, external_ref, outcome, credited_at) VALUES ('start-guest',$1,'verified_clean',$2)`, run+"-down", at(53))

	in, err := NewEpisodeReadStore(p).Load(ctx, EpisodeScope{Since: since, Until: until, NotePrefix: run + " soak", Lookback: time.Hour})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// EVERY fault around the window is loaded for ownership — f1, f2, the pre-window fD via the lookback, and
	// the other campaign's fault — and the prefix only decides which are REPORTED as the cohort.
	if len(in.Faults) != 4 {
		t.Fatalf("faults loaded = %d, want 4 (ownership sees every campaign; the prefix selects the report)", len(in.Faults))
	}
	virt := map[int64]string{}
	for _, f := range in.Faults {
		virt[f.ID] = f.Virt
	}
	if virt[f1] != "qemu" || virt[f2] != "qemu" || virt[fD] != "" {
		t.Fatalf("virt by fault = %v, want qemu for guest a (baseline by vmid) and unknown for guest d", virt)
	}
	r := episodes.Attribute(in)
	// TG-679: f2 (device-down) was injected inside f1's (container-hang) raw window, and the evidence separates the
	// pair — f2's alert is a device rule a container-hang cannot raise and its execution is start-guest, device-
	// down's healing op — so the truncation stands: f2 credited, f1 not. Guest d's execution at -18 min happened
	// BEFORE the window: attached to its pre-window owner, not listed as the window's unowned evidence.
	if len(r.Episodes) != 2 || r.Summary.Total.Credited != 1 || r.Summary.Total.Ambiguous != 0 ||
		r.Summary.Total.Executed != 1 || r.Summary.ExecutionsOwnerOutside != 0 || r.Summary.OverlapPairs != 1 || r.Summary.OverlapsSeparated != 1 {
		t.Fatalf("attribution: cohort=%d credited=%d ambiguous=%d executed=%d owner-outside=%d overlaps=%d/%d, want 2/1/0/1/0/1/1", len(r.Episodes),
			r.Summary.Total.Credited, r.Summary.Total.Ambiguous, r.Summary.Total.Executed, r.Summary.ExecutionsOwnerOutside, r.Summary.OverlapPairs, r.Summary.OverlapsSeparated)
	}
	var dOwnerExecs int
	for _, o := range r.OwnersOutside {
		if o.Host == hostD {
			dOwnerExecs = len(o.Executions)
		}
	}
	if dOwnerExecs != 1 {
		t.Fatalf("guest d's pre-window owner must still carry its execution (got %d)", dOwnerExecs)
	}
	var otherListed bool
	for _, o := range r.OwnersOutside {
		if o.Note == "tg672-other campaign" && !o.InCohort {
			otherListed = true
		}
	}
	if !otherListed {
		t.Fatal("the other campaign's in-window fault must be listed beside the cohort, not silently dropped")
	}
	for _, e := range r.Episodes {
		switch e.ID {
		case f1:
			if e.Disposition != episodes.DispositionAlertedNoExec || len(e.Executions) != 0 || e.Ambiguous {
				t.Errorf("f1 (container-hang) = %s execs=%d ambiguous=%v, want alerted-no-exec with no execution", e.Disposition, len(e.Executions), e.Ambiguous)
			}
		case f2:
			if e.Disposition != episodes.DispositionCredited || e.Ambiguous || len(e.Credits) != 1 || len(in.Sessions) == 0 || !in.Sessions[run+"-down"].Synthetic {
				t.Errorf("f2 (device-down) = %s ambiguous=%v credits=%d sessions=%d, want credited with its synthetic session loaded", e.Disposition, e.Ambiguous, len(e.Credits), len(in.Sessions))
			}
		}
	}
	if _, err := NewEpisodeReadStore(p).Load(ctx, EpisodeScope{Since: since}); err == nil {
		t.Error("an open-ended episode window was accepted — a cohort is a fixed window")
	}
}
