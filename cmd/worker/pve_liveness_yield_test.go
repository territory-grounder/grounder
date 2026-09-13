package main

import (
	"strings"
	"testing"
	"time"
)

// TG-350 follow-through. The register exists because six days of silence from TG's fastest detector
// could not be diagnosed: it is bound, polling every 37s, its module self-test passes every boot, and
// `tg_ingest_source_last_seen_seconds{source_id="pve-liveness"}` read 147 HOURS. During the pve03 NVMe
// failure (2026-08-06 02:54Z) ingest_alert took 12 alerts naming four guests this detector watches —
// every one from LibreNMS or Alertmanager, none from it.
//
// The property under test is that the four indistinguishable states become four distinct readings.

func yieldByName(t *testing.T, y *pveLivenessYield, now time.Time) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for _, s := range y.samples(now) {
		out[s.Name] = s.Value
	}
	if len(out) < 5 {
		t.Fatalf("VACUITY FLOOR: the register emitted %d sample(s); every assertion below would pass on "+
			"an empty set, which is the exact failure mode this register exists to end", len(out))
	}
	return out
}

// TestAQuietEstateIsDistinguishableFromADeadLoop is the finding. Both produce zero minted triages; only
// one of them is healthy, and until now they were the same observation.
func TestAQuietEstateIsDistinguishableFromADeadLoop(t *testing.T) {
	now := time.Now()

	quiet := &pveLivenessYield{}
	quiet.watched.Store(20)
	for i := 0; i < 10; i++ {
		quiet.recordSuccess(now, 0, 0, 0) // ten clean polls, nothing down
	}
	dead := &pveLivenessYield{}
	dead.watched.Store(20) // wired, never ticked

	q := yieldByName(t, quiet, now)
	d := yieldByName(t, dead, now)

	if q["tg_pve_liveness_minted_total"] != d["tg_pve_liveness_minted_total"] {
		t.Fatal("fixture is wrong: both states must produce the SAME minted count — that is why they were " +
			"indistinguishable")
	}
	if q["tg_pve_liveness_polls_total"] == d["tg_pve_liveness_polls_total"] {
		t.Error("a quiet estate and a dead loop report the same poll count. The poll counter is the only " +
			"thing that advances while the loop lives and nothing is wrong; without it, six days of " +
			"silence cannot be told from six days of a dead goroutine.")
	}
	if d["tg_pve_liveness_seconds_since_poll"] != -1 {
		t.Errorf("a detector that never completed a tick reports seconds_since_poll = %v, want -1. "+
			"\"never\" and \"a long time ago\" are different facts and only one of them means the loop "+
			"failed to start.", d["tg_pve_liveness_seconds_since_poll"])
	}
	if q["tg_pve_liveness_seconds_since_poll"] < 0 {
		t.Errorf("a polling detector reports seconds_since_poll = %v — a live loop must report a real age",
			q["tg_pve_liveness_seconds_since_poll"])
	}
}

// TestABlindFetchIsDistinguishableFromAQuietEstate. Both see zero guests down. One is a broken
// credential producing a log line every 37s that nothing aggregates.
func TestABlindFetchIsDistinguishableFromAQuietEstate(t *testing.T) {
	now := time.Now()

	quiet := &pveLivenessYield{}
	quiet.watched.Store(20)
	quiet.recordSuccess(now, 0, 0, 0)

	broken := &pveLivenessYield{}
	broken.watched.Store(20)
	for i := 0; i < 5; i++ {
		broken.recordFailure(now)
	}

	q := yieldByName(t, quiet, now)
	b := yieldByName(t, broken, now)

	if b["tg_pve_liveness_poll_failures_total"] == 0 {
		t.Error("a detector whose fetch failed five times reports zero failures — the only trace of a " +
			"broken credential is then a log line repeated every 37 seconds with nothing counting it")
	}
	if q["tg_pve_liveness_poll_failures_total"] != 0 {
		t.Error("a healthy detector reports poll failures")
	}
	if b["tg_pve_liveness_polls_total"] != 0 {
		t.Error("a failed fetch counted as a SUCCESSFUL poll — the success counter is the liveness signal " +
			"and must not advance on a fetch that returned nothing")
	}
}

// TestTheDenominatorIsPublishedEvenAtZero. A zero down-count means nothing without knowing how many
// guests are watched — TG-343's lesson applied to this detector.
func TestTheDenominatorIsPublishedEvenAtZero(t *testing.T) {
	now := time.Now()
	y := &pveLivenessYield{}
	y.watched.Store(20)
	y.recordSuccess(now, 0, 0, 0)

	g := yieldByName(t, y, now)
	if g["tg_pve_liveness_guests_watched"] != 20 {
		t.Errorf("guests_watched = %v, want 20. Without the denominator, 'no guest is down' and 'no guest "+
			"is being watched' are the same reading.", g["tg_pve_liveness_guests_watched"])
	}

	// And an UNWIRED detector must still publish the zero rather than omitting the series.
	empty := &pveLivenessYield{}
	names := map[string]bool{}
	for _, s := range empty.samples(now) {
		names[s.Name] = true
	}
	for _, want := range []string{"tg_pve_liveness_guests_watched", "tg_pve_liveness_polls_total"} {
		if !names[want] {
			t.Errorf("%s is absent on an unwired detector. A series that appears only once the detector "+
				"fires makes 'quiet' and 'gone' the same observation.", want)
		}
	}
}

// TestLosingTheRaceIsNotFailing. A detection that finds a triage already open is the detector WORKING
// and being beaten — recording it as nothing would make TG's fastest detector look idle exactly when
// the push path is fast.
func TestLosingTheRaceIsNotFailing(t *testing.T) {
	now := time.Now()
	y := &pveLivenessYield{}
	y.watched.Store(20)
	y.recordSuccess(now, 3, 0, 3) // saw 3 down, minted 0, all already being triaged

	g := yieldByName(t, y, now)
	if g["tg_pve_liveness_already_open_total"] != 3 {
		t.Errorf("already_open_total = %v, want 3", g["tg_pve_liveness_already_open_total"])
	}
	if g["tg_pve_liveness_guests_down"] != 3 {
		t.Errorf("guests_down = %v, want 3 — what the poll SAW is separate from what it produced",
			g["tg_pve_liveness_guests_down"])
	}
	if g["tg_pve_liveness_minted_total"] != 0 {
		t.Errorf("minted_total = %v, want 0", g["tg_pve_liveness_minted_total"])
	}

	// guests_down is a GAUGE: on recovery it must return to 0, not latch.
	y.recordSuccess(now, 0, 0, 0)
	if v := yieldByName(t, y, now)["tg_pve_liveness_guests_down"]; v != 0 {
		t.Errorf("guests_down latched at %v after recovery — a latched gauge reports an outage that ended", v)
	}
}

// TestTheRegisterIsWiredAtTheCompositionRoot. The type existing and the poll loop feeding it are
// different facts; this repo's signature defect is the first without the second.
func TestThePVELivenessRegisterIsWiredAtTheCompositionRoot(t *testing.T) {
	src := stripGoComments(readWorkerMain(t))
	if len(src) < 10_000 {
		t.Fatalf("VACUITY FLOOR: main.go stripped to %d bytes", len(src))
	}
	for _, want := range []string{
		"pveLivenessReg.recordSuccess(",
		"pveLivenessReg.recordFailure(",
		"withPVELivenessYield(",
		"pveLivenessReg.watched.Store(",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("main.go no longer contains %s — the register would exist and publish frozen zeros, "+
				"which is worse than no series at all because it looks like a measured healthy detector", want)
		}
	}
	// recordSuccess must be reached on EVERY successful poll, not only the minting one — the all-quiet
	// poll is precisely the reading that was missing.
	k := strings.Index(src, "pveLivenessReg.recordSuccess(")
	j := strings.Index(src, `if minted > 0 || already > 0 {`)
	if k < 0 || j < 0 || k > j {
		t.Error("recordSuccess is not called BEFORE the minted>0 log guard. Inside that branch it would " +
			"record only the polls that found something — leaving the quiet poll unmeasured, which is the " +
			"exact silence this register exists to break.")
	}
}
