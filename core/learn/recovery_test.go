package learn

import (
	"testing"
	"time"
)

// TG-188 organic recovery: the learner pairs a host's ONSET (its episode's first alert) with its later
// clear/recovery transition and accrues an observed time-to-recover, surfaced as MeanRecoverySeconds on
// every co-occurrence pair the host depends in. These are the learner-side oracles; the estate carrier is
// covered in core/estate (learned edges), and the feed in cmd/worker.

var recT0 = time.Date(2026, 8, 14, 6, 0, 0, 0, time.UTC)

// THE DoD's NAMED TEST: an onset→clear pairing produces a recovery mean on the dependent's pairs. RED before
// ObserveClear exists; killing mutation: drop the recoverySum/recoveryCount accrual in ObserveClear and this
// reddens (mean stays 0).
func TestObserveClearAccruesRecoveryOnDependentPairs(t *testing.T) {
	l := NewCoOccurrenceLearner(0)
	// root alerts, then dep alerts twice inside the cascade window (2 observations → a counted pair), then
	// dep clears 15 minutes after its ONSET (the FIRST dep alert — first-raise wins, not the re-fire).
	l.Observe(AlertObservation{Host: "root", At: recT0})
	l.Observe(AlertObservation{Host: "dep", At: recT0.Add(2 * time.Minute)})
	l.Observe(AlertObservation{Host: "dep", At: recT0.Add(4 * time.Minute)})
	l.ObserveClear(ClearObservation{Host: "dep", At: recT0.Add(2*time.Minute + 15*time.Minute)})

	for _, co := range l.CoOccurrences() {
		if co.Primary == "root" && co.Dependent == "dep" {
			if co.MeanRecoverySeconds != 900 {
				t.Errorf("MeanRecoverySeconds = %v, want 900 (clear 15m after the episode's FIRST alert)", co.MeanRecoverySeconds)
			}
			return
		}
	}
	t.Fatal("root→dep co-occurrence missing")
}

// A clear that cannot be honestly attributed teaches nothing: unknown host (no onset), clear at/before the
// onset (skew/replay), or a clear beyond the recovery window. Empty-input discipline: none of these may
// fabricate an observation.
func TestObserveClearRefusesUnattributableClears(t *testing.T) {
	l := NewCoOccurrenceLearner(0)
	l.Observe(AlertObservation{Host: "root", At: recT0})
	l.Observe(AlertObservation{Host: "dep", At: recT0.Add(time.Minute)})

	l.ObserveClear(ClearObservation{Host: "never-alerted", At: recT0.Add(time.Hour)}) // no onset
	l.ObserveClear(ClearObservation{Host: "dep", At: recT0.Add(time.Minute)})         // not after the onset
	l.ObserveClear(ClearObservation{Host: ""})                                        // blank host

	for _, co := range l.CoOccurrences() {
		if co.MeanRecoverySeconds != 0 {
			t.Errorf("%s→%s MeanRecoverySeconds = %v, want 0 (no attributable clear was seen)", co.Primary, co.Dependent, co.MeanRecoverySeconds)
		}
	}

	// Beyond the recovery window: the onset is closed but no observation accrues.
	l2 := NewCoOccurrenceLearner(0)
	l2.Observe(AlertObservation{Host: "root", At: recT0})
	l2.Observe(AlertObservation{Host: "dep", At: recT0.Add(time.Minute)})
	l2.ObserveClear(ClearObservation{Host: "dep", At: recT0.Add(time.Minute + DefaultRecoveryWindow + time.Second)})
	for _, co := range l2.CoOccurrences() {
		if co.MeanRecoverySeconds != 0 {
			t.Errorf("out-of-window clear accrued: MeanRecoverySeconds = %v, want 0", co.MeanRecoverySeconds)
		}
	}
}

// A clear closes the episode: the NEXT alert opens a NEW onset, so a second clear pairs with the second
// episode's own onset — never the first's (the re-pairing bug the strictly-advancing feed watermark also
// guards against, asserted here at the learner layer).
func TestObserveClearClosesTheEpisode(t *testing.T) {
	l := NewCoOccurrenceLearner(0)
	l.Observe(AlertObservation{Host: "root", At: recT0})
	l.Observe(AlertObservation{Host: "dep", At: recT0.Add(time.Minute)})
	l.ObserveClear(ClearObservation{Host: "dep", At: recT0.Add(time.Minute + 10*time.Minute)}) // episode 1: 600s

	// episode 2, hours later: onset re-recorded, clear 20 minutes after it.
	t1 := recT0.Add(3 * time.Hour)
	l.Observe(AlertObservation{Host: "root", At: t1})
	l.Observe(AlertObservation{Host: "dep", At: t1.Add(time.Minute)})
	l.ObserveClear(ClearObservation{Host: "dep", At: t1.Add(time.Minute + 20*time.Minute)}) // episode 2: 1200s

	for _, co := range l.CoOccurrences() {
		if co.Primary == "root" && co.Dependent == "dep" {
			if co.MeanRecoverySeconds != 900 { // mean(600, 1200)
				t.Errorf("MeanRecoverySeconds = %v, want 900 (two independent episodes, 600s and 1200s)", co.MeanRecoverySeconds)
			}
			return
		}
	}
	t.Fatal("root→dep co-occurrence missing")
}

// Decay ages recovery evidence in lockstep (sum and count by the same factor ⇒ the MEAN is preserved), and
// evidence below one whole observation is dropped. Killing mutation: decay only the sum (or only the count)
// and the preserved-mean assertion reddens.
func TestDecayPreservesRecoveryMeanAndPrunes(t *testing.T) {
	l := NewCoOccurrenceLearner(0)
	l.Observe(AlertObservation{Host: "root", At: recT0})
	l.Observe(AlertObservation{Host: "dep", At: recT0.Add(time.Minute)})
	l.ObserveClear(ClearObservation{Host: "dep", At: recT0.Add(time.Minute + 5*time.Minute)}) // 300s

	l.Decay(recT0.Add(24*time.Hour), 0) // no-op halfLife guard
	l.Decay(recT0.Add(24*time.Hour), 30*24*time.Hour)
	l.Decay(recT0.Add(48*time.Hour), 30*24*time.Hour) // one real day of decay
	found := false
	for _, co := range l.CoOccurrences() {
		if co.Primary == "root" && co.Dependent == "dep" && co.MeanRecoverySeconds != 0 {
			found = true
			if co.MeanRecoverySeconds < 299.9 || co.MeanRecoverySeconds > 300.1 {
				t.Errorf("decay changed the recovery MEAN: %v, want 300 (sum and count must decay in lockstep)", co.MeanRecoverySeconds)
			}
		}
	}
	if !found {
		t.Fatal("recovery mean vanished after a mild decay — it should only shrink the evidence weight")
	}

	// A hard decay prunes the evidence entirely (count below one whole observation).
	l.Decay(recT0.Add(48*time.Hour+365*24*time.Hour), 24*time.Hour)
	for _, co := range l.CoOccurrences() {
		if co.MeanRecoverySeconds != 0 {
			t.Errorf("recovery evidence survived a year of daily half-lives: %v", co.MeanRecoverySeconds)
		}
	}
}

// Snapshot/Restore round-trips the recovery evidence; onsets are ephemeral (an open episode does NOT survive
// a restore — its later clear is unattributable, never mispaired). Killing mutation: drop Recoveries from
// Restore and the restored mean reads 0.
func TestSnapshotRestoreRoundTripsRecoveries(t *testing.T) {
	l := NewCoOccurrenceLearner(0)
	l.Observe(AlertObservation{Host: "root", At: recT0})
	l.Observe(AlertObservation{Host: "dep", At: recT0.Add(time.Minute)})
	l.ObserveClear(ClearObservation{Host: "dep", At: recT0.Add(time.Minute + 5*time.Minute)}) // 300s
	// leave a second episode OPEN at snapshot time
	l.Observe(AlertObservation{Host: "dep", At: recT0.Add(2 * time.Hour)})

	snap := l.Snapshot()
	if len(snap.Recoveries) != 1 || snap.Recoveries[0].Host != "dep" || snap.Recoveries[0].Count != 1 || snap.Recoveries[0].Sum != 300 {
		t.Fatalf("snapshot recoveries = %+v, want [{dep 300 1}]", snap.Recoveries)
	}

	l2 := NewCoOccurrenceLearner(0)
	l2.Restore(snap)
	for _, co := range l2.CoOccurrences() {
		if co.Primary == "root" && co.Dependent == "dep" && co.MeanRecoverySeconds != 300 {
			t.Errorf("restored MeanRecoverySeconds = %v, want 300", co.MeanRecoverySeconds)
		}
	}
	// The open episode's onset did not survive: a post-restore clear for it is a no-op.
	l2.ObserveClear(ClearObservation{Host: "dep", At: recT0.Add(2*time.Hour + 10*time.Minute)})
	for _, co := range l2.CoOccurrences() {
		if co.Primary == "root" && co.Dependent == "dep" && co.MeanRecoverySeconds != 300 {
			t.Errorf("a clear for a pre-restore episode was attributed: mean = %v, want 300 unchanged", co.MeanRecoverySeconds)
		}
	}
	// A corrupt row never seeds a phantom estimate.
	l3 := NewCoOccurrenceLearner(0)
	l3.Restore(Snapshot{Recoveries: []HostRecovery{{Host: "", Sum: 10, Count: 1}, {Host: "x", Sum: -5, Count: 1}, {Host: "y", Sum: 100, Count: 0}}})
	if got := l3.Snapshot(); len(got.Recoveries) != 0 {
		t.Errorf("corrupt recovery rows restored: %+v", got.Recoveries)
	}
}
