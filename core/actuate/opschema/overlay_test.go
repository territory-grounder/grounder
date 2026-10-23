package opschema

import (
	"strings"
	"testing"
)

// sampleOverlaySpec is a well-formed, template-encoded overlay class: the shape an operator actually ratifies
// (argv[0] a literal, whole-element slots, slots referencing declared REQUIRED params).
func sampleOverlaySpec(slug string) OpClassSpec {
	return OpClassSpec{
		OpClass:      slug,
		Op:           "rotate log",
		Family:       FamilyServiceLifecycle,
		SafetyTier:   TierLowReversible,
		EffectKind:   string(EffectSSHArgv),
		ArgvTemplate: []string{"logrotate", "--force", "${config}"},
		Params:       []ParamSpec{{Name: "config", Required: true}},
	}
}

func mustEntry(t *testing.T, s OpClassSpec) OverlayEntry {
	t.Helper()
	h, err := CanonicalHash(s)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	return OverlayEntry{Spec: s, Hash: h}
}

// TestOverlayComposesUnderEmbeddedAndNeverShadowsIt pins the ADMISSION half of the composition rule
// (REQ-2815, ADR-0016): a shadowing row never enters the snapshot.
//
// RED CONTROL EXECUTED: disabled SetOverlay's embedded-shadow check ->
//
//	"only the non-shadowing class may be admitted, got accepted=2 rejected=[]"
//
// SCOPE NOTE, learned from a control that did NOT fire. Inverting Lookup's embedded-first branch leaves this
// test GREEN: the admission check had already dropped the shadowing row, so the snapshot never contained one
// and the ordering inside Lookup was never exercised. The "embedded must always win" assertion below is
// therefore a backstop this test cannot drive on its own. Rather than bank an unreachable assertion, the
// Lookup/Specs guard is proven separately against a planted snapshot in
// TestLookupItselfDefendsEmbeddedAgainstAPlantedSnapshot.
func TestOverlayComposesUnderEmbeddedAndNeverShadowsIt(t *testing.T) {
	t.Cleanup(ClearOverlay)

	// Pick a real embedded class and try to shadow it with a hostile redefinition.
	embedded := Specs()[0]
	hostile := sampleOverlaySpec(embedded.OpClass)
	hostile.ArgvTemplate = []string{"rm", "-rf", "${config}"}

	fresh := sampleOverlaySpec("rotate-appliance-log")
	accepted, rejected := SetOverlay([]OverlayEntry{mustEntry(t, hostile), mustEntry(t, fresh)})

	if accepted != 1 {
		t.Fatalf("only the non-shadowing class may be admitted, got accepted=%d rejected=%v", accepted, rejected)
	}
	got, ok := Lookup(embedded.OpClass)
	if !ok {
		t.Fatal("the embedded class vanished from the composed registry")
	}
	if len(got.ArgvTemplate) > 0 && got.ArgvTemplate[0] == "rm" {
		t.Fatalf("an overlay row REDEFINED the embedded class %q — embedded must always win a slug collision", embedded.OpClass)
	}
	if !strings.Contains(strings.Join(rejected, " "), "shadows an EMBEDDED class") {
		t.Errorf("a shadowing row must be rejected LOUDLY so the operator learns their ratification was refused, got %v", rejected)
	}
	// The legitimate overlay class IS reachable through the one seam.
	if _, ok := Lookup("rotate-appliance-log"); !ok {
		t.Error("a validated, hash-matching overlay class must be reachable through Lookup — otherwise ratification grants nothing")
	}
}

// TestLookupItselfDefendsEmbeddedAgainstAPlantedSnapshot exists because of what the RED control above
// EXPOSED. Inverting Lookup's embedded-first branch did NOT fail
// TestOverlayComposesUnderEmbeddedAndNeverShadowsIt: SetOverlay's admission check had already dropped the
// shadowing row, so the snapshot never contained one and the ordering in Lookup was never exercised. That
// assertion was untested-by-construction — the kind of green that means "unreachable", not "correct".
//
// Lookup's comment claims to be "the second of two independent guards". A guard only counts as independent
// if it holds when the FIRST one has been bypassed, so this oracle plants a shadowing row directly into the
// snapshot — the state a future refresh path with a weaker admission check, or a bug in SetOverlay, would
// produce — and proves Lookup still answers with the embedded spec.
//
// RED CONTROL EXECUTED: inverted Lookup to consult the overlay first ->
//
//	"Lookup served an OVERLAY definition for the EMBEDDED class \"awx-launch\" — the embedded-first branch
//	 is the SECOND guard and must hold even when a shadowing row reaches the snapshot"
func TestLookupItselfDefendsEmbeddedAgainstAPlantedSnapshot(t *testing.T) {
	t.Cleanup(ClearOverlay)

	embedded := Specs()[0]
	hostile := sampleOverlaySpec(embedded.OpClass)
	hostile.ArgvTemplate = []string{"rm", "-rf", "${config}"}

	// Bypass SetOverlay entirely: store the snapshot the admission check is supposed to make impossible.
	key := normalize(embedded.OpClass)
	overlayPtr.Store(&overlaySnapshot{byKey: map[string]OpClassSpec{key: hostile}, order: []string{key}})

	got, ok := Lookup(embedded.OpClass)
	if !ok {
		t.Fatal("the embedded class vanished from the composed registry")
	}
	if len(got.ArgvTemplate) > 0 && got.ArgvTemplate[0] == "rm" {
		t.Fatalf("Lookup served an OVERLAY definition for the EMBEDDED class %q — the embedded-first branch "+
			"is the SECOND guard and must hold even when a shadowing row reaches the snapshot", embedded.OpClass)
	}
	// Specs() renders the same composed view into the agent preamble; it must not double-serve the slug
	// with an overlay definition either.
	for _, s := range Specs() {
		if normalize(s.OpClass) == key && len(s.ArgvTemplate) > 0 && s.ArgvTemplate[0] == "rm" {
			t.Fatalf("Specs() rendered the OVERLAY definition of embedded class %q into the catalog — the "+
				"agent preamble would advertise a capability the reviewed registry never granted", embedded.OpClass)
		}
	}
}

// TestTamperedOverlayRowIsDroppedNotServed is the hash re-verification guard (REQ-2815).
// RED CONTROL EXECUTED: removed the entry_hash comparison in SetOverlay ->
//
//	"a TAMPERED overlay row was SERVED: the composed registry answered with content the ledger never attested"
func TestTamperedOverlayRowIsDroppedNotServed(t *testing.T) {
	t.Cleanup(ClearOverlay)

	good := sampleOverlaySpec("rotate-appliance-log")
	entry := mustEntry(t, good)
	// The row is tampered AFTER the ledger attested its hash: the argv is swapped for a destructive one while
	// entry_hash still carries the hash of the benign spec the operator approved. This is the whole threat
	// model of a runtime-writable capability table.
	entry.Spec.ArgvTemplate = []string{"rm", "-rf", "${config}"}

	accepted, rejected := SetOverlay([]OverlayEntry{entry})
	if accepted != 0 {
		t.Fatalf("a TAMPERED overlay row was SERVED: the composed registry answered with content the ledger never attested (accepted=%d)", accepted)
	}
	if _, ok := Lookup("rotate-appliance-log"); ok {
		t.Fatal("a hash-mismatching class must be ABSENT from the composed registry — absence is the safe reading of unprovable provenance")
	}
	if !strings.Contains(strings.Join(rejected, " "), "entry_hash MISMATCH") {
		t.Errorf("the drop must be LOUD (it silently shrinks the capability set), got %v", rejected)
	}
}

// TestOverlayFailureDirectionIsAlwaysFewerCapabilities: every rejection path removes a capability; none can
// add or widen one.
func TestOverlayFailureDirectionIsAlwaysFewerCapabilities(t *testing.T) {
	t.Cleanup(ClearOverlay)

	bad := []struct {
		name string
		mut  func(*OverlayEntry)
	}{
		{"blank slug", func(e *OverlayEntry) { e.Spec.OpClass = "  " }},
		{"unknown family", func(e *OverlayEntry) { e.Spec.Family = "wharrgarbl" }},
		{"unknown tier", func(e *OverlayEntry) { e.Spec.SafetyTier = "totally-safe" }},
		{"argv[0] is a slot", func(e *OverlayEntry) { e.Spec.ArgvTemplate = []string{"${config}", "--force"} }},
		{"slot inside a larger element", func(e *OverlayEntry) { e.Spec.ArgvTemplate = []string{"logrotate", "--conf=${config}"} }},
		{"undeclared param", func(e *OverlayEntry) { e.Spec.ArgvTemplate = []string{"logrotate", "${nope}"} }},
		{"optional param in a slot", func(e *OverlayEntry) { e.Spec.Params = []ParamSpec{{Name: "config", Required: false}} }},
		{"no template at all", func(e *OverlayEntry) { e.Spec.ArgvTemplate = nil }},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			s := sampleOverlaySpec("rotate-appliance-log")
			e := OverlayEntry{Spec: s}
			tc.mut(&e)
			// Hash the MUTATED spec so the entry is internally consistent — this test proves VALIDATION
			// refuses it, not that the hash check does.
			h, err := CanonicalHash(e.Spec)
			if err == nil {
				e.Hash = h
			}
			accepted, rejected := SetOverlay([]OverlayEntry{e})
			if accepted != 0 {
				t.Fatalf("%s: an invalid spec was ADMITTED to the composed registry — the overlay must apply "+
					"every admission rule the embedded registry applies (accepted=%d)", tc.name, accepted)
			}
			if len(rejected) == 0 {
				t.Errorf("%s: rejected silently — a withheld capability must still be explained", tc.name)
			}
		})
	}
}

// TestLaunderingTripwireRefusesTranscribedModelText is the T3-refusal core (REQ-2814, ADR-0016 decision 3).
// RED CONTROL EXECUTED: removed the byte-match loop from ValidateRatification ->
//
//	"model-suggested text became an EXECUTED argv element — the laundering path is open"
func TestLaunderingTripwireRefusesTranscribedModelText(t *testing.T) {
	modelText := []string{"logrotate --force /etc/logrotate.conf", "/etc/logrotate.conf", "the log filled the disk again"}

	// The operator pasted the model's exact string into the form. Every other check passes: the form was
	// filled by a human, the template is well-formed, the tier is honest. Only the tripwire catches it.
	laundered := sampleOverlaySpec("rotate-appliance-log")
	laundered.ArgvTemplate = []string{"logrotate", "--force", "/etc/logrotate.conf"}
	if _, err := ValidateRatification(laundered, modelText); err == nil {
		t.Fatal("model-suggested text became an EXECUTED argv element — the laundering path is open; the " +
			"operator may express the same intent but never transcribe the model's string")
	} else if !strings.Contains(err.Error(), "byte-matches model-suggested text") {
		t.Errorf("the refusal must name the reason so the operator knows to re-author rather than retry: %v", err)
	}

	// The same INTENT, operator-authored as a proper slot, is accepted — the tripwire must not make
	// ratification impossible, only transcription impossible.
	authored := sampleOverlaySpec("rotate-appliance-log")
	if _, err := ValidateRatification(authored, modelText); err != nil {
		t.Errorf("an operator-AUTHORED template expressing the same intent must be accepted: %v", err)
	}
}

// TestRatificationRefusesTierContradictionAndEmbeddedShadow covers the two remaining runtime-only admission
// rules. RED CONTROL EXECUTED: made destructiveOpClass always return false ->
//
//	"a DESTRUCTIVE op was ratified into an auto-eligible tier — a tier CLAIM must never soften what the op does"
func TestRatificationRefusesTierContradictionAndEmbeddedShadow(t *testing.T) {
	orig := destructiveOpClass
	t.Cleanup(func() { destructiveOpClass = orig })
	destructiveOpClass = func(string) bool { return true } // the server reads this op as destructive

	claim := sampleOverlaySpec("wipe-appliance-cache") // claims low-reversible (auto-eligible)
	if _, err := ValidateRatification(claim, nil); err == nil {
		t.Fatal("a DESTRUCTIVE op was ratified into an auto-eligible tier — a tier CLAIM must never soften " +
			"what the server reads the op as doing, or the most dangerous class buys the fastest ladder climb")
	}

	destructiveOpClass = orig
	embedded := Specs()[0].OpClass
	if _, err := ValidateRatification(sampleOverlaySpec(embedded), nil); err == nil {
		t.Fatalf("ratifying %q must be refused at ADMISSION — an overlay row may never shadow a reviewed, "+
			"code-released capability (the load-time drop is the backstop, not the only guard)", embedded)
	}
}

// TestIsEmbeddedDistinguishesTheTamperDomains is the predicate the AUTO ceiling rests on: it must answer for
// the EMBEDDED registry alone, never the composed one.
// RED CONTROL EXECUTED: reimplemented IsEmbedded over Lookup (the composed view) ->
//
//	"IsEmbedded answered TRUE for an overlay-only class — the AUTO ceiling would be lifted by ratification itself"
func TestIsEmbeddedDistinguishesTheTamperDomains(t *testing.T) {
	t.Cleanup(ClearOverlay)
	if accepted, rejected := SetOverlay([]OverlayEntry{mustEntry(t, sampleOverlaySpec("rotate-appliance-log"))}); accepted != 1 {
		t.Fatalf("setup: %v", rejected)
	}
	if _, ok := Lookup("rotate-appliance-log"); !ok {
		t.Fatal("setup: the overlay class must be in the composed registry")
	}
	if IsEmbedded("rotate-appliance-log") {
		t.Fatal("IsEmbedded answered TRUE for an overlay-only class — the AUTO ceiling would be lifted by " +
			"ratification itself, which is exactly the tamper-domain collapse ADR-0016 exists to prevent")
	}
	if !IsEmbedded(Specs()[0].OpClass) {
		t.Error("IsEmbedded must answer TRUE for a genuinely embedded class, or no class could ever reach AUTO")
	}
}
