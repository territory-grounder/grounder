package acceptance

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/regime"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// T-017-1 (REQ-1700/1701/1703) + T-017-2 (REQ-1702) — the RegimeResolver core and the Lane / interceptor
// composition seam. Registered from this file's init() as a step-registrar slice so the shared acceptance
// harness (acceptance_test.go) is never edited by parallel task work — the same pattern the awxplaybooks
// (T-017-5) slice uses.
func init() {
	stepRegistrars = append(stepRegistrars, registerRegimeCoreSteps)
}

// recordingActuator is an actuation.Actuator that records whether Exec was reached — so the oracle can prove a
// refusal did NOT execute (the effect never fires around the interceptor).
type recordingActuator struct{ execs int }

func (a *recordingActuator) Capability() string { return "acc-ssh" }
func (a *recordingActuator) ReadOnly() bool     { return false }
func (a *recordingActuator) Exec(context.Context, []string, []byte) (actuation.Result, error) {
	a.execs++
	return actuation.Result{ExitCode: 0}, nil
}

// regimeCoreWorld is the per-scenario state driving the real core/regime engine.
type regimeCoreWorld struct {
	// REQ-1700 state.
	engine     *regime.Engine
	lane       regime.Lane
	laneErr    error
	rerouted   regime.Lane // same target, data-only rule change (config-not-code proof)
	rerouteErr error

	// REQ-1701 state.
	unknownWithDefault regime.Lane
	unknownWithDefErr  error
	unknownNoDefault   regime.Lane
	unknownNoDefErr    error
	ambiguousLane      regime.Lane
	ambiguousErr       error

	// REQ-1702 state.
	leaf       *recordingActuator
	offOut     actuate.Outcome
	offErr     error
	onOut      actuate.Outcome
	onErr      error
	unwiredErr error
	nilLaneErr error

	// REQ-1703 state.
	regimeMatched     bool
	credentialMatched bool
	policyMatched     bool
}

// accRegistry wires the foundation lanes over refusing leaves for the resolution scenarios (native-ssh +
// awx-job placeholder).
func accRegistry() []regime.Lane {
	return []regime.Lane{
		regime.NewNativeSSHLane(&recordingActuator{}),
		regime.NewAWXJobLane(),
	}
}

// accRules maps host / host-glob over the SHARED object-model to the foundation regimes.
func accRules() []regime.Rule {
	return []regime.Rule{
		{ID: "host:dc1tg01", Selector: credential.Selector{Kind: credential.KindHost, Pattern: "dc1tg01"}, Regime: regime.RegimeNativeSSH},
		{ID: "glob:awxhost*", Selector: credential.Selector{Kind: credential.KindHostGlob, Pattern: "awxhost*"}, Regime: regime.RegimeAWXJob},
	}
}

func accGoodRequest() actuate.Request {
	m, err := manifest.New(
		manifest.Action{Target: "web01", OpClass: "restart-service", Op: "restart", Reversible: true},
		safety.BandAuto, "plan#acc", "pred#acc",
	)
	if err != nil {
		panic(fmt.Sprintf("acc manifest: %v", err))
	}
	return actuate.Request{
		Manifest: m,
		Gated:    true,
		Argv:     []string{"systemctl", "restart", "nginx"},
		Evidence: []actuate.Evidence{{ToolResultID: "tr-1", Captured: true, Successful: true, Recent: true, Relevant: true}},
		Observe:  func(context.Context) []verify.ObservedAlert { return []verify.ObservedAlert{} },
		Band:     safety.BandAuto, // fresh per-incident band (TG-126): AUTO admits at 1b, the lane/mode/policy gate the rest
	}
}

func registerRegimeCoreSteps(sc *godog.ScenarioContext) {
	w := &regimeCoreWorld{}

	// ---- REQ-1700: a target resolves to exactly one regime and its lane, from config DATA ----
	sc.Step(`^operator-declared regime rules mapping host glob group and device-class to one of the five regimes$`, func() error {
		w.engine = regime.NewEngine(accRules(), accRegistry())
		return nil
	})
	sc.Step(`^the engine selects a lane for a target$`, func() error {
		w.lane, w.laneErr = w.engine.SelectLane(credential.Target{Host: "dc1tg01"})
		// config-not-code: a DATA-ONLY rule change (no code change) re-routes the SAME target to another lane.
		reroute := regime.NewEngine([]regime.Rule{
			{ID: "host:dc1tg01", Selector: credential.Selector{Kind: credential.KindHost, Pattern: "dc1tg01"}, Regime: regime.RegimeAWXJob},
		}, accRegistry())
		w.rerouted, w.rerouteErr = reroute.SelectLane(credential.Target{Host: "dc1tg01"})
		return nil
	})
	sc.Step(`^exactly one regime is resolved and its bound effect lane is returned from the config data with no code change$`, func() error {
		if w.laneErr != nil || w.lane == nil {
			return fmt.Errorf("expected a resolved lane, got lane=%v err=%v", w.lane, w.laneErr)
		}
		if w.lane.Regime() != regime.RegimeNativeSSH {
			return fmt.Errorf("expected the native-ssh lane bound by config, got %q", w.lane.Regime())
		}
		// config-not-code: the same target, with only the DATA changed, resolves to a different lane.
		if w.rerouteErr != nil || w.rerouted == nil || w.rerouted.Regime() != regime.RegimeAWXJob {
			return fmt.Errorf("a data-only rule change must re-route the lane (config-not-code), got lane=%v err=%v", w.rerouted, w.rerouteErr)
		}
		return nil
	})

	// ---- REQ-1701: unknown → default lane or refuse; ambiguous → fail closed ----
	sc.Step(`^a target that matches no regime rule and a separate target that matches more than one regime$`, func() error {
		defaultLane := regime.NewNativeSSHLane(&recordingActuator{})
		// engine WITH a declared operator default (native-ssh).
		withDefault := regime.NewEngine(accRules(), accRegistry(), regime.WithDefaultLane(defaultLane))
		w.unknownWithDefault, w.unknownWithDefErr = withDefault.SelectLane(credential.Target{Host: "unknown-host-x"})
		// engine WITHOUT a declared default.
		noDefault := regime.NewEngine(accRules(), accRegistry())
		w.unknownNoDefault, w.unknownNoDefErr = noDefault.SelectLane(credential.Target{Host: "unknown-host-x"})
		// a separate target matching more than one regime (two equal-specificity rules → different regimes),
		// evaluated on an engine that HAS a default — to prove ambiguity never falls back to it.
		ambig := regime.NewEngine([]regime.Rule{
			{ID: "a", Selector: credential.Selector{Kind: credential.KindHost, Pattern: "dup01"}, Regime: regime.RegimeNativeSSH},
			{ID: "b", Selector: credential.Selector{Kind: credential.KindHost, Pattern: "dup01"}, Regime: regime.RegimeAWXJob},
		}, accRegistry(), regime.WithDefaultLane(defaultLane))
		w.ambiguousLane, w.ambiguousErr = ambig.SelectLane(credential.Target{Host: "dup01"})
		return nil
	})
	sc.Step(`^the engine selects a lane for each$`, func() error { return nil })
	sc.Step(`^the unknown target resolves to the operator default native-ssh lane or refuses and the multi-regime target fails closed and refuses rather than choosing a lane arbitrarily$`, func() error {
		// unknown WITH a default → the operator default native-ssh lane.
		if w.unknownWithDefErr != nil || w.unknownWithDefault == nil || w.unknownWithDefault.Regime() != regime.RegimeNativeSSH {
			return fmt.Errorf("an unknown target with a declared default must resolve to native-ssh, got lane=%v err=%v", w.unknownWithDefault, w.unknownWithDefErr)
		}
		// unknown WITHOUT a default → refuse (no lane).
		if w.unknownNoDefault != nil || !regime.IsRefused(w.unknownNoDefErr) || !errors.Is(w.unknownNoDefErr, regime.ErrNoRegime) {
			return fmt.Errorf("an unknown target with no default must refuse ErrNoRegime, got lane=%v err=%v", w.unknownNoDefault, w.unknownNoDefErr)
		}
		// ambiguous → fail closed (never a lane, never the default).
		if w.ambiguousLane != nil {
			return fmt.Errorf("an ambiguous target must NOT return a lane (never guessed/defaulted), got %q", w.ambiguousLane.Regime())
		}
		if !errors.Is(w.ambiguousErr, regime.ErrAmbiguousRegime) || !regime.IsRefused(w.ambiguousErr) {
			return fmt.Errorf("an ambiguous target must fail closed with ErrAmbiguousRegime, got %v", w.ambiguousErr)
		}
		return nil
	})

	// ---- REQ-1702: every lane is reachable ONLY through the spec/013 interceptor chain ----
	sc.Step(`^a selected effect lane$`, func() error {
		w.leaf = &recordingActuator{}
		w.lane = regime.NewNativeSSHLane(w.leaf)
		return nil
	})
	sc.Step(`^code attempts to reach the lane's effect$`, func() error {
		// the production posture: a read-only (Shadow) chokepoint — the interceptor refuses at the mode gate.
		seamOff := regime.NewLaneEffect(func(l actuation.Actuator) *actuate.Interceptor {
			return actuate.NewInterceptor(safety.NewReadOnlyChokepoint(), l, audit.NewLedger())
		})
		w.offOut, w.offErr = seamOff.Apply(context.Background(), w.lane, accGoodRequest())
		// a test-only actuating chokepoint proves the SAME leaf reaches Exec ONLY via Interceptor.Do.
		seamOn := regime.NewLaneEffect(func(l actuation.Actuator) *actuate.Interceptor {
			return actuate.NewInterceptor(safety.NewActuatingChokepoint(), l, audit.NewLedger())
		})
		w.onOut, w.onErr = seamOn.Apply(context.Background(), w.lane, accGoodRequest())
		// an unwired seam / a nil lane fails loud rather than reaching an effect around the chain.
		_, w.unwiredErr = (&regime.LaneEffect{}).Apply(context.Background(), w.lane, accGoodRequest())
		_, w.nilLaneErr = seamOn.Apply(context.Background(), nil, accGoodRequest())
		return nil
	})
	sc.Step(`^the only path to the effect is the spec/013 interceptor Do and there is no exported path that bypasses the never-auto floor the policy verdict the credential resolution or the mode chokepoint$`, func() error {
		if w.offErr != nil || w.onErr != nil {
			return fmt.Errorf("Apply must not error on a wired seam: off=%v on=%v", w.offErr, w.onErr)
		}
		// through the chain: under Shadow the mode chokepoint refuses BEFORE Exec (the leaf never fires).
		if !w.offOut.Refused || w.offOut.Executed {
			return fmt.Errorf("under Shadow the effect must be refused at the mode chokepoint, got %+v", w.offOut)
		}
		// the SAME leaf reaches Exec exactly once — and only through Interceptor.Do (the actuating path). Total
		// execs==1 proves the Shadow path did NOT execute and the unwired/nil-lane paths never reached a leaf.
		if !w.onOut.Executed || w.leaf.execs != 1 {
			return fmt.Errorf("the selected leaf must reach Exec exactly once via Interceptor.Do, got out=%+v execs=%d", w.onOut, w.leaf.execs)
		}
		// no exported effect path: an unwired seam and a nil lane both fail loud (never reach an effect).
		if w.unwiredErr != regime.ErrSeamUnwired || w.nilLaneErr != regime.ErrSeamUnwired {
			return fmt.Errorf("an unwired seam / nil lane must fail loud with ErrSeamUnwired, got unwired=%v nil=%v", w.unwiredErr, w.nilLaneErr)
		}
		// structural (cross-package): the Lane interface exposes Regime() and NO exported method that returns an
		// actuation.Actuator — there is no exported accessor to pull a lane's leaf out and call Exec around the
		// chain. (The in-package standing check core/regime.TestCompositionInvariant_NoExportedEffectPath asserts
		// the same over the package AST plus that the unexported effectLeaf accessor exists.)
		laneT := reflect.TypeOf((*regime.Lane)(nil)).Elem()
		actuatorT := reflect.TypeOf((*actuation.Actuator)(nil)).Elem()
		if _, ok := laneT.MethodByName("Regime"); !ok {
			return fmt.Errorf("the Lane interface must expose Regime()")
		}
		for i := 0; i < laneT.NumMethod(); i++ {
			m := laneT.Method(i)
			if m.PkgPath != "" {
				continue // unexported accessor — unreachable outside package regime
			}
			for o := 0; o < m.Type.NumOut(); o++ {
				if m.Type.Out(o) == actuatorT {
					return fmt.Errorf("REQ-1702 VIOLATION: exported Lane method %q returns an actuation.Actuator (an exported effect path around the interceptor)", m.Name)
				}
			}
		}
		return nil
	})

	// ---- REQ-1703: regime resolution keys off the SAME estate object-model as policy + credential ----
	sc.Step(`^one estate object-model of host glob group device-class and inventory$`, func() error {
		target := credential.Target{Host: "dc1tg01", Groups: []string{"edge"}, DeviceClass: "cisco-asa"}
		sel := credential.Selector{Kind: credential.KindHostGlob, Pattern: "dc1*"}

		// regime keys off the shared credential.Selector/Target/Match — a regime.Rule IS a credential.Selector.
		e := regime.NewEngine([]regime.Rule{{ID: "r", Selector: sel, Regime: regime.RegimeAWXJob}}, accRegistry())
		lane, err := e.SelectLane(target)
		w.regimeMatched = err == nil && lane != nil

		// the credential ENGINE matches the SAME target over the SAME shared object-model.
		credEng := credential.NewEngine([]credential.Rule{{ID: "c", Selector: sel, Bundle: mustBundle()}})
		_, cerr := credEng.Resolve(target)
		w.credentialMatched = cerr == nil

		// the policy path: a DIRECT call to the SAME shared primitive credential.Match — the one core/policy
		// imports (core/policy/rule.go: Match.matches → credential.Match). One grammar, no second matcher.
		w.policyMatched = credential.Match(sel, target)
		return nil
	})
	sc.Step(`^the regime resolver and the policy and credential engines match a target$`, func() error { return nil })
	sc.Step(`^all three key off the same object-model and no second inventory grammar is defined$`, func() error {
		if !w.regimeMatched || !w.credentialMatched || !w.policyMatched {
			return fmt.Errorf("the shared object-model did not agree across all three: regime=%v credential=%v policy=%v",
				w.regimeMatched, w.credentialMatched, w.policyMatched)
		}
		// compile-time proof there is no second inventory grammar: regime's Rule.Selector and SelectLane's
		// target ARE credential.Selector / credential.Target — the one model built once in core/credential.
		var _ credential.Selector = regime.Rule{}.Selector
		var _ = func(t credential.Target) (regime.Lane, error) { return regime.NewEngine(nil, nil).SelectLane(t) }
		return nil
	})
}

// mustBundle builds a valid credential bundle (sealed SecretRef, never plaintext) for the REQ-1703 credential
// engine agreement — the identity is irrelevant, only that the SAME shared object-model matched the target.
func mustBundle() credential.Bundle {
	b, err := credential.NewBundle(credential.BundleSpec{User: "acc", Port: 22, Scheme: credential.SchemeSSH, SSHKeyRef: "env:TG_ACC_REGIME_KEY"})
	if err != nil {
		panic(fmt.Sprintf("mustBundle: %v", err))
	}
	return b
}
