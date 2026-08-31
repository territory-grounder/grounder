package deploy

// A SAFETY PARAMETER THAT DIFFERS BY PLANE BY ACCIDENT IS NOT A POSTURE (TG-352).
//
// Both worker processes run the SAME binary and the same core/predict code. What differs between them is
// deliberate and enumerated: credentials (credential_plane.go), and the queue each polls. Everything else
// must be identical or the two planes reason differently about the same incident — the plane that GATES a
// proposal and the plane that EXECUTES it disagreeing about how wide its blast radius is.
//
// Measured live on 2026-08-06:
//
//	worker          TG_PREDICT_MIN_CONFIDENCE=0.70   TG_BLAST_RADIUS_WIDE_THRESHOLD=8
//	worker-actuate  both ABSENT -> code defaults      0 and 8
//
// MinConfidence 0 keeps EVERY impact regardless of path-product confidence; 0.70 drops the low-confidence
// far/learned edges. So the executing plane computed a WIDER radius than the gating plane, for every
// incident, because two lines existed in one compose block and not the other.
//
// The direction happens to be conservative — for blast radius, over-prediction is the cautious side. That is
// luck, not design, and the next such divergence has no reason to land the safe way round. Nothing in the
// tree recorded an intent either way.
//
// This is deploy/egress_parity_test.go's doctrine on a different axis: the expected posture is declared HERE,
// in Go, and the test fails on disagreement in EITHER direction — adding a plane-specific tuning key is as
// much a decision as removing one.

import (
	"sort"
	"strings"
	"testing"
)

// planeSharedTuningPrefixes are the config families that MUST be identical across both worker planes,
// because both binaries read them through the same code paths and a divergence changes what TG concludes
// rather than what it may touch.
//
// Prefix-matched, deliberately, and this is the one place in this repo where a prefix beats a list: the
// families are open (a new TG_PREDICT_* knob is expected), and the failure mode of missing one is silent
// divergence in a safety calculation. credential_plane.go uses an explicit list for the opposite reason —
// there, a prefix match would silently stop covering a renamed credential.
var planeSharedTuningPrefixes = []string{
	"TG_PREDICT_",      // blast-radius model tuning (path-product confidence floor, …)
	"TG_BLAST_RADIUS_", // the width at which an action's blast radius ceilings at AUTO_NOTICE
}

// planeTuningExempt names keys that legitimately differ, each with the reason. Empty today — recorded as a
// seam so a future exemption is a written decision rather than a quiet edit to the prefixes above.
var planeTuningExempt = map[string]string{}

// KILLING MUTATION: delete TG_PREDICT_MIN_CONFIDENCE from the worker-actuate block (the state this shipped
// in). RED, naming the key and both planes.
func TestBothWorkerPlanesShareThePredictionTuning(t *testing.T) {
	worker := serviceEnv(t, "worker")
	actuate := serviceEnv(t, "worker-actuate")

	// VACUITY FLOOR. If either block is renamed or reshaped, serviceEnv already fatals — but an empty map
	// would make every loop below iterate over nothing and pass.
	if len(worker) < 50 || len(actuate) < 10 {
		t.Fatalf("read %d worker key(s) and %d worker-actuate key(s) — the compose reader has stopped "+
			"matching and this guard is examining nothing", len(worker), len(actuate))
	}

	shared := func(k string) bool {
		for _, p := range planeSharedTuningPrefixes {
			if strings.HasPrefix(k, p) {
				return true
			}
		}
		return false
	}

	var onlyWorker, onlyActuate, differing []string
	for k, wv := range worker {
		if !shared(k) {
			continue
		}
		if _, exempt := planeTuningExempt[k]; exempt {
			continue
		}
		av, ok := actuate[k]
		switch {
		case !ok:
			onlyWorker = append(onlyWorker, k)
		case av != wv:
			differing = append(differing, k+" (worker="+wv+" actuate="+av+")")
		}
	}
	for k := range actuate {
		if !shared(k) {
			continue
		}
		if _, exempt := planeTuningExempt[k]; exempt {
			continue
		}
		if _, ok := worker[k]; !ok {
			onlyActuate = append(onlyActuate, k)
		}
	}
	sort.Strings(onlyWorker)
	sort.Strings(onlyActuate)
	sort.Strings(differing)

	// The floor that stops this passing on a configuration with no tuning keys at all.
	checked := 0
	for k := range worker {
		if shared(k) {
			checked++
		}
	}
	if checked == 0 {
		t.Fatalf("none of the %d shared-tuning prefixes matched any worker key — either the families were "+
			"renamed or this guard is checking nothing", len(planeSharedTuningPrefixes))
	}

	if len(onlyWorker) > 0 {
		t.Errorf("the triage worker declares %v and worker-actuate does NOT. Both planes run the same binary "+
			"and the same core/predict code, so the executing plane falls back to a CODE DEFAULT while the "+
			"gating plane uses the operator's value — the two disagree about the blast radius of the same "+
			"incident. Forward the same variable to both, or add it to planeTuningExempt WITH the reason.",
			onlyWorker)
	}
	if len(onlyActuate) > 0 {
		t.Errorf("worker-actuate declares %v and the triage worker does not — the same divergence, mirrored.",
			onlyActuate)
	}
	if len(differing) > 0 {
		t.Errorf("both planes declare these but interpolate DIFFERENT variables: %v. A per-plane value is a "+
			"posture and belongs in planeTuningExempt with its reason, not in an unremarked difference.",
			differing)
	}
}
