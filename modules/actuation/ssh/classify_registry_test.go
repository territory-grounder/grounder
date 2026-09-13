package ssh

import (
	"errors"
	"testing"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/safety"
)

// classifyArgv used to be a linear if-chain naming four verbs by hand, beside a registry that BUILDS the same
// four shapes. Two lists that must agree and are maintained separately do not stay agreeing — and here a
// disagreement is not cosmetic. guardMutatingArgv classifies an argv, re-derives the canonical argv from the
// class it got back, and refuses on mismatch; a verb missing from the chain is silently unexecutable.
//
// These oracles are driven by the REGISTRY, so they keep holding for verbs that do not exist yet. That is the
// whole point: the next verb is meant to be a JSON block, and a JSON block cannot remember to update a switch.

// sampleFor returns a plausible value for a class's single template slot.
func sampleFor(param string) string {
	switch param {
	case opschema.ParamContainer:
		return "some-container"
	default:
		return "some.service"
	}
}

// templatedSpecs is every class classifyArgv could possibly recognise. A helper rather than a literal list, so
// this file has no second copy of the thing it is testing.
func templatedSpecs(t *testing.T) []opschema.OpClassSpec {
	t.Helper()
	var out []opschema.OpClassSpec
	for _, s := range opschema.Specs() {
		if len(s.ArgvTemplate) > 0 {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		t.Fatal("no templated op-class in the registry — every oracle in this file would pass vacuously")
	}
	return out
}

// TestEveryTemplatedClassRoundTrips is the integrity property guardMutatingArgv depends on: build the argv
// from a class, classify it back, and get the SAME class and the same value. A break here means the effect
// leaf refuses a legitimate action, or worse, records it under another verb's governance.
func TestEveryTemplatedClassRoundTrips(t *testing.T) {
	for _, s := range templatedSpecs(t) {
		var param string
		for _, p := range s.Params {
			if p.Required {
				param = p.Name
				break
			}
		}
		if param == "" {
			t.Errorf("%s is templated but declares no required param", s.OpClass)
			continue
		}
		want := sampleFor(param)
		argv, err := s.Argv(map[string]string{param: want})
		if err != nil {
			t.Errorf("%s: %v", s.OpClass, err)
			continue
		}
		gotClass, gotVal, side, ok := classifyArgv(argv)
		if !ok {
			t.Errorf("%s built %v and classifyArgv did not recognise it — the verb is unexecutable on this leaf",
				s.OpClass, argv)
			continue
		}
		if gotClass != s.OpClass {
			t.Errorf("%s built %v but it classifies as %q — an action would be governed as the WRONG verb",
				s.OpClass, argv, gotClass)
		}
		if gotVal != want {
			t.Errorf("%s round-trip changed the value: sent %q, got %q", s.OpClass, want, gotVal)
		}
		if side != sideForward {
			t.Errorf("%s built the FORWARD argv %v but it classified as side %d — guardMutatingArgv would "+
				"compare it against the rollback canonical and refuse a legitimate forward", s.OpClass, argv, side)
		}
	}
}

// TestEveryDeclaredRollbackShapeClassifiesToItsClass is TG-464 gap A's round-trip oracle, the rollback-side
// twin of the forward test above and registry-driven for the same reason (the next reversible verb is meant
// to be a JSON block). For every class declaring a DISTINCT compensating rollback_template, the argv
// spec.RollbackArgv renders — the EXACT argv the manual-rollback lane seals and executes (TG-462) — must
// classify back to that class, on the ROLLBACK side. Before TG-464 this was red for every such class: the
// classifier read only ArgvTemplate, so a sealed inverse (`systemctl stop`, `docker stop`) reached
// guardMutatingArgv, classified to nothing, and refused with ErrNoExecutionPath — "zero inverses have ever
// run".
//
// KILLING MUTATION (executed for TG-464): disable the rollback-side consider() branch in classifyAgainst and
// this goes RED for start-service + start-container while every forward oracle stays green — proving the test
// discriminates the rollback side specifically, not classification in general. Restored → green.
func TestEveryDeclaredRollbackShapeClassifiesToItsClass(t *testing.T) {
	checked := 0
	for _, s := range templatedSpecs(t) {
		if len(s.RollbackTemplate) == 0 || argvEqual(s.RollbackTemplate, s.ArgvTemplate) {
			continue // no distinct compensating shape declared — the forward side is the only shape
		}
		checked++
		var param string
		for _, p := range s.Params {
			if p.Required {
				param = p.Name
				break
			}
		}
		if param == "" {
			t.Errorf("%s declares a rollback_template but no required param", s.OpClass)
			continue
		}
		want := sampleFor(param)
		argv, err := s.RollbackArgv(map[string]string{param: want})
		if err != nil {
			t.Errorf("%s: RollbackArgv: %v", s.OpClass, err)
			continue
		}
		gotClass, gotVal, side, ok := classifyArgv(argv)
		if !ok {
			t.Errorf("%s's compensating argv %v has NO classification — the sealed inverse the rollback lane "+
				"executes would refuse at guardMutatingArgv with ErrNoExecutionPath (the exact TG-464 gap)",
				s.OpClass, argv)
			continue
		}
		if gotClass != s.OpClass || gotVal != want {
			t.Errorf("%s's compensating argv %v classified as (%q,%q) — an inverse would be governed as the "+
				"WRONG verb or target", s.OpClass, argv, gotClass, gotVal)
		}
		if side != sideRollback {
			t.Errorf("%s's compensating argv %v classified on side %d, want the ROLLBACK side — the guard "+
				"would compare it against the forward canonical and refuse", s.OpClass, argv, side)
		}
	}
	if checked == 0 {
		t.Fatal("no registered class declares a distinct rollback_template — this oracle would pass vacuously " +
			"(start-service and start-container are expected to declare one)")
	}
}

// TestNoTwoClassesClaimTheSameArgv is the injectivity oracle, and it is why the reverse lookup refuses on a
// double match instead of taking the first. Two classes matching one argv means the round-trip re-derives
// whichever the loop happened to return. Since TG-464 the claimable set is the forward∪rollback UNION —
// every canonical argv (forward AND rendered compensating) must be claimed by exactly ONE (class, side), or
// classifyAgainst's union-wide ambiguity refusal makes the colliding verbs unexecutable.
func TestNoTwoClassesClaimTheSameArgv(t *testing.T) {
	specs := templatedSpecs(t)
	// claimants counts how many (class, side) template shapes in the union claim an argv.
	claimants := func(argv []string) []string {
		var out []string
		for _, other := range specs {
			if _, _, hit := opschema.MatchTemplate(other, argv); hit {
				out = append(out, other.OpClass+"/forward")
			}
			if len(other.RollbackTemplate) == 0 || argvEqual(other.RollbackTemplate, other.ArgvTemplate) {
				continue
			}
			rb := opschema.OpClassSpec{OpClass: other.OpClass, ArgvTemplate: other.RollbackTemplate}
			if _, _, hit := opschema.MatchTemplate(rb, argv); hit {
				out = append(out, other.OpClass+"/rollback")
			}
		}
		return out
	}
	for _, s := range specs {
		var param string
		for _, p := range s.Params {
			if p.Required {
				param = p.Name
				break
			}
		}
		if fwd, err := s.Argv(map[string]string{param: sampleFor(param)}); err == nil {
			if c := claimants(fwd); len(c) != 1 {
				t.Errorf("forward argv %v is claimed by %v — a classification must be unique or the round-trip "+
					"check validates one verb while the ledger records another", fwd, c)
			}
		}
		if len(s.RollbackTemplate) == 0 || argvEqual(s.RollbackTemplate, s.ArgvTemplate) {
			continue
		}
		if rb, err := s.RollbackArgv(map[string]string{param: sampleFor(param)}); err == nil {
			if c := claimants(rb); len(c) != 1 {
				t.Errorf("compensating argv %v is claimed by %v — an inverse must classify to exactly one "+
					"(class, side) or the union-wide ambiguity refusal makes it unexecutable", rb, c)
			}
		}
	}
}

// TestANonTemplatedClassIsNeverClassified — a compiled builder is an opaque function and cannot be reversed.
// It must produce NO execution path on this leaf rather than a guess.
func TestANonTemplatedClassIsNeverClassified(t *testing.T) {
	checked := 0
	for _, s := range opschema.Specs() {
		if len(s.ArgvTemplate) > 0 {
			continue
		}
		checked++
		// start-guest is argv-shaped ([start, <guest>]) but compiled and routed to the proxmox lane.
		if _, _, hit := opschema.MatchTemplate(s, []string{"start", "some-guest"}); hit {
			t.Errorf("compiled op-class %q was matched by the template reverse lookup", s.OpClass)
		}
	}
	if checked == 0 {
		// NOT a skip: this loop's whole job is to prove no COMPILED class is reachable through the template
		// reverse lookup. Zero compiled classes means the scan examined nothing, so a pass here would assert
		// the property over an empty set — the vacuity floor every gate in this repo carries.
		t.Fatal("no compiled classes left in the registry — the reverse-lookup oracle scanned nothing, so its pass would be vacuous")
	}
}

// TestEveryRegisteredSSHClassIsReachableOrDeclared is the REACHABILITY oracle, and it exists because the gap
// it closes was measured live: `start-container` was added as a pure JSON block, and it registered, validated,
// classified, round-tripped and passed the entire suite — while refusing at this leaf with "op_class has no
// reversible execution path (not allowlisted)". Nothing failed. The verb was simply inert, exactly as
// AutoEligible and the tier floor once were.
//
// Behaviour tests cannot close this: they name the verb they exercise, so the verb nobody wrote a test for is
// the verb nothing checks. The oracle has to be keyed on the REGISTRY, which is the same list a new verb is
// added to.
//
// A class MAY be deliberately unreachable here — a family this leaf has no vocabulary for must NOT resolve,
// because there is no way to know whether its slot is a unit, a container or something else. That is a
// DECLARED exclusion, and it is named below rather than inferred, so adding a family without teaching the leaf
// about it fails loudly instead of producing a verb that silently never runs.

// TestAStartVerbRollsBackToItsInverseNotToItself — the one rollback shape that cannot be a re-run. Re-running
// a start is not an inverse of a start; it is the same action again, and recording it as the compensating
// action means a rollback that does nothing while the ledger says it undid something.

// TestAmbiguousClassificationRefusesBOTH — the branch the live registry can never reach. Two classes that
// build the same argv must produce NO execution path, not the first one the loop happened to see:
// guardMutatingArgv re-derives the canonical argv from whatever class comes back, so returning either would
// pass the round-trip while recording the action under the OTHER verb's governance and allowlist.
func TestAmbiguousClassificationRefusesBOTH(t *testing.T) {
	twin := func(name string) opschema.OpClassSpec {
		return opschema.OpClassSpec{OpClass: name, Family: opschema.FamilyServiceLifecycle,
			SafetyTier:   opschema.TierLowReversible,
			ArgvTemplate: []string{"systemctl", "restart", "${unit}"},
			Params:       []opschema.ParamSpec{{Name: "unit", Type: "string", Required: true}}}
	}
	argv := []string{"systemctl", "restart", "nginx"}

	// sanity: ONE of them alone classifies, so a refusal below is ambiguity and not a broken fixture
	if _, _, _, ok := classifyAgainst([]opschema.OpClassSpec{twin("verb-a")}, argv); !ok {
		t.Fatal("the fixture does not classify on its own — the ambiguity result would be meaningless")
	}
	if cls, _, _, ok := classifyAgainst([]opschema.OpClassSpec{twin("verb-a"), twin("verb-b")}, argv); ok {
		t.Errorf("two classes build %v and it classified as %q — an ambiguous argv must have NO execution "+
			"path, or an action is governed as a verb it is not", argv, cls)
	}
}

// TestCrossSideAmbiguityRefusesBOTH extends the ambiguity refusal over the forward∪rollback UNION (TG-464):
// one class's FORWARD template colliding with another class's ROLLBACK template must classify to nothing,
// exactly as two colliding forwards do — returning either side would let guardMutatingArgv validate one
// verb's canonical argv while the ledger records the action under the other verb's governance (and, worse
// here, under the wrong SIDE's inverse derivation). The live registry cannot reach this branch (injectivity
// is asserted over the union above), so the pair is synthetic, through classifyAgainst's seam.
func TestCrossSideAmbiguityRefusesBOTH(t *testing.T) {
	fwd := opschema.OpClassSpec{OpClass: "halt-thing", Family: opschema.FamilyServiceLifecycle,
		SafetyTier:   opschema.TierLowReversible,
		ArgvTemplate: []string{"systemctl", "halt", "${unit}"},
		Params:       []opschema.ParamSpec{{Name: "unit", Type: "string", Required: true}}}
	rbOwner := opschema.OpClassSpec{OpClass: "raise-thing", Family: opschema.FamilyServiceLifecycle,
		SafetyTier:       opschema.TierLowReversible,
		ArgvTemplate:     []string{"systemctl", "raise", "${unit}"},
		RollbackTemplate: []string{"systemctl", "halt", "${unit}"}, // collides with halt-thing's FORWARD
		Params:           []opschema.ParamSpec{{Name: "unit", Type: "string", Required: true}}}
	argv := []string{"systemctl", "halt", "nginx"}

	// sanity in BOTH directions: each spec alone classifies the argv (on its own side), so the refusal below
	// is ambiguity, not a broken fixture.
	if _, _, side, ok := classifyAgainst([]opschema.OpClassSpec{fwd}, argv); !ok || side != sideForward {
		t.Fatalf("forward fixture alone must classify (forward side): ok=%v side=%d", ok, side)
	}
	if _, _, side, ok := classifyAgainst([]opschema.OpClassSpec{rbOwner}, argv); !ok || side != sideRollback {
		t.Fatalf("rollback fixture alone must classify (rollback side): ok=%v side=%d", ok, side)
	}
	if cls, _, _, ok := classifyAgainst([]opschema.OpClassSpec{fwd, rbOwner}, argv); ok {
		t.Errorf("argv %v is one class's forward AND another's rollback, yet classified as %q — a cross-side "+
			"ambiguity must refuse BOTH", argv, cls)
	}
	// order-independence: the refusal must not depend on which spec the loop saw first.
	if cls, _, _, ok := classifyAgainst([]opschema.OpClassSpec{rbOwner, fwd}, argv); ok {
		t.Errorf("cross-side ambiguity classified as %q when the rollback owner came first — the union "+
			"refusal must be order-independent", cls)
	}
}

// TestARollbackTemplateEqualToTheForwardDoesNotSelfAmbiguate — a class that DECLARES its rollback as an
// element-for-element re-run of the forward (redundant but legal data) must keep classifying: the re-run IS
// the forward shape, so the classifier treats it as one shape rather than double-hitting the class into a
// self-ambiguous refusal that would silently make the verb unexecutable — the exact "inert verb" failure
// this file exists to prevent.
func TestARollbackTemplateEqualToTheForwardDoesNotSelfAmbiguate(t *testing.T) {
	rerun := opschema.OpClassSpec{OpClass: "poke-thing", Family: opschema.FamilyServiceLifecycle,
		SafetyTier:       opschema.TierLowReversible,
		ArgvTemplate:     []string{"systemctl", "poke", "${unit}"},
		RollbackTemplate: []string{"systemctl", "poke", "${unit}"},
		Params:           []opschema.ParamSpec{{Name: "unit", Type: "string", Required: true}}}
	cls, val, side, ok := classifyAgainst([]opschema.OpClassSpec{rerun}, []string{"systemctl", "poke", "nginx"})
	if !ok || cls != "poke-thing" || val != "nginx" || side != sideForward {
		t.Errorf("a declared re-run rollback self-ambiguated its class: (%q,%q,%d,%v) — the verb would be "+
			"silently unexecutable", cls, val, side, ok)
	}
}

// TestAnAllLiteralTemplateIsNeverClassified — a template with no slot cannot yield the (class, value) pair the
// allowlist speaks in, and an empty argv must not match a compiled class's empty template.
func TestAnAllLiteralTemplateIsNeverClassified(t *testing.T) {
	noSlot := opschema.OpClassSpec{OpClass: "no-slot", Family: opschema.FamilyServiceLifecycle,
		SafetyTier: opschema.TierLowReversible, ArgvTemplate: []string{"systemctl", "daemon-reload"}}
	if _, _, _, ok := classifyAgainst([]opschema.OpClassSpec{noSlot}, []string{"systemctl", "daemon-reload"}); ok {
		t.Error("an all-literal template classified — there is no value to hand the allowlist")
	}
	for _, s := range opschema.Specs() {
		if len(s.ArgvTemplate) > 0 {
			continue
		}
		if _, _, hit := opschema.MatchTemplate(s, nil); hit {
			t.Errorf("compiled op-class %q matched an EMPTY argv — an empty template must never be a wildcard",
				s.OpClass)
		}
		if _, _, hit := opschema.MatchTemplate(s, []string{}); hit {
			t.Errorf("compiled op-class %q matched a zero-length argv", s.OpClass)
		}
	}
}

// TestClassifyRefusesNearMisses — the match is structural, so anything that is not the exact shape has no
// execution path. A prefix match or a length-tolerant match would let an argv with extra arguments through.
// Since TG-464 the rollback shapes are held to the SAME strictness: a near-miss of a compensating argv
// (extra flag, blank slot, case drift) refuses exactly as a forward near-miss does.
//
// NOTE `docker stop mealie` LEFT this list with TG-464: it is start-container's declared compensating shape
// and now classifies (rollback side) — see TestEveryDeclaredRollbackShapeClassifiesToItsClass.
func TestClassifyRefusesNearMisses(t *testing.T) {
	for _, argv := range [][]string{
		{"systemctl", "restart"},                   // too short
		{"systemctl", "restart", "nginx", "--now"}, // too long
		{"systemctl", "restart", ""},               // empty slot
		{"systemctl", "restart", "   "},            // blank slot
		{"systemctl", "reboot", "nginx"},           // wrong literal
		{"Systemctl", "restart", "nginx"},          // case differs
		{"bash", "-c", "systemctl restart nginx"},  // the shape a shell lane would need
		{"/usr/bin/systemctl", "restart", "nginx"}, // absolute path is a different literal
		{"systemctl", "stop"},                      // rollback shape, too short
		{"systemctl", "stop", "nginx", "--now"},    // rollback shape, extra argument
		{"systemctl", "stop", ""},                  // rollback shape, empty slot
		{"Systemctl", "stop", "nginx"},             // rollback shape, case differs
		{"docker", "stop", "mealie", "-t", "0"},    // rollback shape, extra arguments
		{"docker", "stop", "   "},                  // rollback shape, blank slot
	} {
		if cls, _, _, ok := classifyArgv(argv); ok {
			t.Errorf("argv %v classified as %q — it is not a shape any registered class builds", argv, cls)
		}
	}
}

// TestTheFourMigratedVerbsClassifyExactlyAsBefore pins the pre-migration behaviour with goldens. The registry-
// driven oracles above keep holding as verbs are added; this one guarantees the change that INTRODUCED them
// did not move an existing verb — and, since TG-464, that every forward stays on the FORWARD side (the
// rollback-side extension moved no forward classification).
func TestTheFourMigratedVerbsClassifyExactlyAsBefore(t *testing.T) {
	for _, c := range []struct {
		argv  []string
		class string
		val   string
	}{
		{[]string{"systemctl", "restart", "nginx"}, OpClassRestartService, "nginx"},
		{[]string{"systemctl", "reload", "nginx"}, OpClassReloadService, "nginx"},
		{[]string{"systemctl", "start", "nginx"}, OpClassStartService, "nginx"},
		{[]string{"docker", "restart", "mealie"}, OpClassRestartContainer, "mealie"},
	} {
		cls, val, side, ok := classifyArgv(c.argv)
		if !ok || cls != c.class || val != c.val || side != sideForward {
			t.Errorf("argv %v classified as (%q,%q,side=%d,%v), want (%q,%q,forward,true)",
				c.argv, cls, val, side, ok, c.class, c.val)
		}
	}
}

// TestGuardAcceptsCanonicalRollbackAndRefusesNearMiss is TG-464 gap A at the GUARD — the check the mutating
// Exec actually runs. The canonical compensating argv of an allowlisted target passes (the sealed inverse
// can now execute on this leaf), while everything around it stays refused: an extra argument, a
// non-allowlisted unit's inverse, and a stateful container's inverse. The floors and allowlists are
// side-blind — an inverse traverses exactly the controls its forward does.
func TestGuardAcceptsCanonicalRollbackAndRefusesNearMiss(t *testing.T) {
	m := New("web01", "svc-agent", &fakeRunner{},
		WithMutation(safety.NewActuatingChokepoint(), []string{"nginx"}, []string{"librespeed", "postgres"}))

	// the canonical rollback argv for an allowlisted unit/container is ACCEPTED (red before TG-464:
	// classifyArgv knew no rollback shape, so this refused with ErrNoExecutionPath).
	if err := m.guardMutatingArgv([]string{"systemctl", "stop", "nginx"}); err != nil {
		t.Errorf("the canonical compensating argv of an allowlisted unit must pass the guard, got %v", err)
	}
	if err := m.guardMutatingArgv([]string{"docker", "stop", "librespeed"}); err != nil {
		t.Errorf("the canonical compensating argv of an allowlisted container must pass the guard, got %v", err)
	}
	// a near-miss of the accepted shape is still refused — the exact-match discipline is side-blind.
	if err := m.guardMutatingArgv([]string{"systemctl", "stop", "nginx", "--now"}); !errors.Is(err, ErrNoExecutionPath) {
		t.Errorf("a rollback argv with an extra argument must be refused, got %v", err)
	}
	// the allowlist bites on the rollback side exactly as on the forward side.
	if err := m.guardMutatingArgv([]string{"systemctl", "stop", "apache2"}); !errors.Is(err, ErrUnitNotAllowed) {
		t.Errorf("a non-allowlisted unit's inverse must be refused by the allowlist, got %v", err)
	}
	// the stateful floor bites on the rollback side too (postgres IS allowlisted above, and still refused).
	if err := m.guardMutatingArgv([]string{"docker", "stop", "postgres"}); !errors.Is(err, ErrStatefulWorkload) {
		t.Errorf("a stateful container's inverse must be floored even when allowlisted, got %v", err)
	}
}

// TestAFamilyTheLeafDoesNotUnderstandNeverResolves — the default branch of the family switch, which no
// registry-driven test can reach: every ssh-argv class today is in a known family, and a class in an unknown
// family is refused EARLIER by the resolvable-set gate. The two guards mask each other, so the durable state
// is SEEDED past the first one to isolate the second. A control that cannot fail is not a control.
