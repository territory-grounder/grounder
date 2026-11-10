package main

import (
	"log"
	"strings"
	"sync"

	"github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/safety"
)

const actuationScopeAll = "all"

// actuationScope is the one scope every chain in this process shares, read once.
var actuationScope = sync.OnceValue(wireActuationScope)

func wireActuationScope() *actuate.ActuationScope {
	return resolveActuationScope(planeEnv("TG_ACTUATION_SCOPE_HOSTS", ""), independentConditionRequired())
}

// resolveActuationScope reads the effect-path host allowlist. While the independent condition enforces, a missing or
// empty list refuses every effect, and only the explicit word "all" leaves the chain unrestricted.
func resolveActuationScope(raw string, enforcing bool) *actuate.ActuationScope {
	if strings.EqualFold(strings.TrimSpace(raw), actuationScopeAll) {
		log.Print("actuation scope: OFF (TG_ACTUATION_SCOPE_HOSTS=all) — every lane keeps its own allowlists")
		return nil
	}
	s := actuate.ParseActuationScope(raw)
	if s == nil {
		if enforcing {
			log.Print("actuation scope: REFUSING EVERY EFFECT — the independent condition enforces and TG_ACTUATION_SCOPE_HOSTS names no host")
			return actuate.RefuseAllScope()
		}
		log.Print("actuation scope: OFF (TG_ACTUATION_SCOPE_HOSTS unset) — every lane keeps its own allowlists")
		return nil
	}
	log.Printf("actuation scope: ARMED — every effect is refused outside these %d host(s): %s", s.Len(), strings.Join(s.Hosts(), ","))
	return s
}

// newEffectChain is the only place an effect chain is built, so every path carries the scope and the condition.
func newEffectChain(chokepoint *safety.Chokepoint, leaf actuation.Actuator, ledger *audit.Ledger) *actuate.Interceptor {
	return boundEffectChain(chokepoint, leaf, ledger, actuationScope(), independentConditionRequired())
}

func boundEffectChain(chokepoint *safety.Chokepoint, leaf actuation.Actuator, ledger *audit.Ledger, scope *actuate.ActuationScope, enforcing bool) *actuate.Interceptor {
	return actuate.NewInterceptor(chokepoint, leaf, ledger).WithActuationScope(scope).WithIndependentCondition(enforcing, conditionGate{})
}
