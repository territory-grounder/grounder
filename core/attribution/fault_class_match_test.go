package attribution

import (
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
)

// REQ-2302 keys self-recognition on (target, FAULT CLASS). Only the target half was implemented: `faultClass`
// was a parameter of Attribute and appeared exactly once — in the signature.
//
// Every pre-existing test passed faultClass "start-guest" alongside `vzstart` evidence, i.e. a fault class
// that happened to correspond to the evidence. The mismatch case was never expressed, so an unused parameter
// looked exactly like a working one for the life of the feature. That is the same shape as a safety regex
// validated only against its author's examples.
//
// Measured live 2026-07-28: nginx stopped on a guest, TG proposed `start-service` (the correct verb), and
// attribution matched it against a `vzstart` — a Proxmox GUEST start TG had performed 29 minutes earlier for
// an unrelated device-down. The session terminated "already remediated" and nginx stayed down.

func selfEv(kind, target string, at time.Time) Evidence {
	return Evidence{Domain: "pve", Actor: "root@pam!tg-actuate", ActionKind: kind, Target: target,
		ObservedAt: at, Ref: "UPID:test", Covered: true}
}

// TestSelfRecognitionRequiresAMatchingActionKind is the live defect, as an oracle.
func TestSelfRecognitionRequiresAMatchingActionKind(t *testing.T) {
	now := nowFunc()
	ev := []Evidence{selfEv("vzstart", "web01", now.Add(-5*time.Minute))}

	if f := Attribute("web01", "start-service", ev, nil, baseCfg); f.Taxonomy == AttributedSelf {
		t.Errorf("a `vzstart` (GUEST start) was accepted as proof that `start-service` had already been done — "+
			"TG stands down and the service stays down (taxonomy=%v)", f.Taxonomy)
	}
	if f := Attribute("web01", "restart-container", ev, nil, baseCfg); f.Taxonomy == AttributedSelf {
		t.Error("a `vzstart` was accepted as proof that `restart-container` had already been done")
	}
}

// TestSelfRecognitionStillFiresForAMatchingActionKind — the narrowing must not blanket-deny. Recognising its
// OWN in-flight remediation is the whole point of REQ-2302, and that must still work.
func TestSelfRecognitionStillFiresForAMatchingActionKind(t *testing.T) {
	now := nowFunc()
	ev := []Evidence{selfEv("vzstart", "web01", now.Add(-5*time.Minute))}
	if f := Attribute("web01", "start-guest", ev, nil, baseCfg); f.Taxonomy != AttributedSelf {
		t.Errorf("a `vzstart` must still self-recognise a `start-guest` proposal, got %v — over-clamping here "+
			"means TG re-actuates on top of its own settling heal", f.Taxonomy)
	}
}

// TestNarrowingDoesNotWeakenTheSecurityPath is the guard on the guard. The fault-class match is applied ONLY
// to the self branch; an unsanctioned actor must still dominate no matter what it did or what was proposed.
// Narrowing admissibility globally would have turned a remediation fix into a security regression.
func TestNarrowingDoesNotWeakenTheSecurityPath(t *testing.T) {
	now := nowFunc()
	intruder := Evidence{Domain: "pve", Actor: "mallory@pve", ActionKind: "vzstop", Target: "web01",
		ObservedAt: now.Add(-2 * time.Minute), Ref: "UPID:x", Covered: true}

	// every fault class, matching or not — suspicion must survive all of them
	for _, fc := range []string{"start-guest", "start-service", "restart-container", "not-a-class", ""} {
		f := Attribute("web01", fc, []Evidence{intruder}, nil, baseCfg)
		if f.Taxonomy != AttributedSuspicious {
			t.Errorf("fault class %q: an unsanctioned actor resolved %v, want suspicious — a positively-unknown "+
				"actor must dominate regardless of what was proposed", fc, f.Taxonomy)
		}
	}

	// and a suspicious record must still dominate a CO-OCCURRING matching self record (REQ-2304)
	f := Attribute("web01", "start-guest", []Evidence{
		selfEv("vzstart", "web01", now.Add(-5*time.Minute)), intruder,
	}, nil, baseCfg)
	if f.Taxonomy != AttributedSuspicious {
		t.Errorf("a matching self record masked an intruder into %v — REQ-2304 requires suspicion to dominate", f.Taxonomy)
	}
}

// TestAGuestVerbNeverSelfRecognisesANonGuestClass walks the LIVE op-class registry, so it keeps holding for
// verbs that do not exist yet. `vzstart`/`vzstop` are the only action kinds any reader emits on this estate
// (measured: 361 and 698 records respectively, and nothing else), which makes them the only thing that can
// wrongly satisfy a class today.
func TestAGuestVerbNeverSelfRecognisesANonGuestClass(t *testing.T) {
	specs := opschema.Specs()
	if len(specs) == 0 {
		t.Fatal("registry is empty — this oracle would pass vacuously")
	}
	checked := 0
	for _, s := range specs {
		if s.Family == opschema.FamilyGuestLifecycle {
			continue
		}
		checked++
		for _, kind := range []string{"vzstart", "vzstop", "qmstart"} {
			if selfActionAccomplishes(s.OpClass, kind) {
				t.Errorf("op-class %q (family %q) is satisfied by the GUEST verb %q — a guest lifecycle action "+
					"cannot have accomplished it, and accepting it suppresses a real remediation",
					s.OpClass, s.Family, kind)
			}
		}
	}
	if checked == 0 {
		t.Fatal("every registered class is guest-lifecycle — this oracle would pass vacuously")
	}
}

// TestAnUnknownOpClassIsNeverSelfRecognised — fail toward remediating. An op-class the table does not know
// must not be silently stood down on the strength of any evidence.
func TestAnUnknownOpClassIsNeverSelfRecognised(t *testing.T) {
	now := nowFunc()
	ev := []Evidence{selfEv("vzstart", "web01", now.Add(-time.Minute))}
	for _, fc := range []string{"", "   ", "not-a-registered-class", "prune-images"} {
		if f := Attribute("web01", fc, ev, nil, baseCfg); f.Taxonomy == AttributedSelf {
			t.Errorf("unknown fault class %q self-recognised — an absent mapping must never read as "+
				"'already remediated'", fc)
		}
	}
}
