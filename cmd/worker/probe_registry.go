package main

import (
	"context"
	"fmt"
	"sort"

	"github.com/territory-grounder/grounder/core/selftest"
	"github.com/territory-grounder/grounder/temporal/moduletest"
)

// THE PROBE REGISTRY — how a module that can prove itself reaches the console's TEST button.
//
// WHY COLLECTION HAPPENS AT THE POINT OF CONSTRUCTION. The first version of the Test lane built its
// prober set in one place near the end of main(), from whatever module instances happened to still be in
// scope there. That is a filter nobody can see: main() is 5,600 lines, most modules are built inside an
// `if` that ends long before, several are built inside helper functions that return only an error, and
// the credential sources are built inside modules/bootstrap and never come back at all. So the set was
// not "every module TG can exercise" — it was "every module still visible from line 3999", which came to
// exactly one surface while the dialogs promised twenty-nine.
//
// A registry that each construction site OFFERS INTO removes the scope question entirely: the instance is
// registered where it is created, when it is unambiguously alive, and a new module gets a probe by adding
// one line next to its constructor rather than by threading a variable down a thousand lines.
//
// WHY offer() TAKES `any`. The call sites hold wildly different concrete types — *tracker.Module,
// *estate.EdgeSource, *credential.Source, an agent tool — with no common interface but the optional one.
// Asking each site to pre-assert would put the type assertion back at the call site, which is where it
// gets forgotten. The registry does it once, and a module that does not implement the capability is
// simply not registered: it then reports "no test is implemented" rather than a pass, which is the honest
// answer.
type probeRegistry struct {
	// declare, when non-nil, is told about EVERY offered construction (TG-267): the one chokepoint that
	// already sees each module's (surface, sourceType, instance) at build time, so the module registry
	// learns what is running without 25 per-site edits. Called before the self-test check on purpose —
	// declaration is about CONSTRUCTION, and a module that cannot probe itself still runs.
	declare func(surface, sourceType string, v any)
	probers map[string]moduletest.Prober
	// seen is the set of module identities that were offered at least one instance, whether or not any
	// of them could self-test.
	//
	// IT IS A SET OF IDENTITIES, NOT A COUNT OF CALLS, because several modules are constructed more than
	// once for different jobs and every construction is offered. librenms alone is built four times — a
	// registry module, an estate source, an alert source and a tool set — and only some of those hold a
	// live network client. Counting calls would report librenms as four modules, and counting the
	// non-implementers as failures would report three phantom gaps. What an operator needs is the number
	// of MODULES that can prove themselves out of the number CONSTRUCTED, so both sides are sets.
	seen map[string]bool
}

func newProbeRegistry() *probeRegistry {
	return &probeRegistry{probers: map[string]moduletest.Prober{}, seen: map[string]bool{}}
}

// offer registers v as the probe for surface/sourceType when v can self-test.
//
// It returns whether the module was registered, so a call site can log the negative case in its own
// vocabulary if that is useful. Ignoring the result is fine — the registry already accounts for it.
//
// A nil or empty identity is refused rather than stored under a blank key: a probe reachable only by a
// key nobody can name is a probe nobody can press.
func (p *probeRegistry) offer(surface, sourceType string, v any) bool {
	if p == nil || surface == "" || sourceType == "" {
		return false
	}
	key := surface + "/" + sourceType
	p.seen[key] = true
	if p.declare != nil {
		p.declare(surface, sourceType, v)
	}
	t, ok := selftest.Of(v)
	if !ok {
		return false
	}
	// LAST OFFER WINS, deliberately. Several modules are constructed more than once for different jobs
	// (librenms is built four times — a registry module, an estate source, an alert source and a tool
	// set), and the later constructions are the ones holding a live network client. A first-wins registry
	// would pin the probe to whichever construction happened to run first, which for librenms is the
	// offline Normalize-only module — a probe that cannot fail.
	p.probers[key] = selfTestProbe{t: t, key: key}
	return true
}

// set returns the assembled prober map for the moduletest activity.
func (p *probeRegistry) set() map[string]moduletest.Prober {
	if p == nil {
		return map[string]moduletest.Prober{}
	}
	return p.probers
}

// keys returns the registered module identities, sorted, for the boot log and the wiring manifest.
func (p *probeRegistry) keys() []string {
	if p == nil {
		return nil
	}
	out := make([]string, 0, len(p.probers))
	for k := range p.probers {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// declinedKeys returns the modules that were CONSTRUCTED but can prove nothing, sorted.
//
// Derived by subtraction rather than recorded on the failing path, so a module offered several instances
// counts once and only when none of them could self-test. This list is the honest remainder: every entry
// is a live module whose dialog will report "no test is implemented" — reported at boot so the gap is
// enumerable on the day it appears rather than discovered by an operator pressing the button.
func (p *probeRegistry) declinedKeys() []string {
	if p == nil {
		return nil
	}
	var out []string
	for k := range p.seen {
		if _, ok := p.probers[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// constructed returns how many distinct modules were offered, for the boot log's denominator.
func (p *probeRegistry) constructed() int {
	if p == nil {
		return 0
	}
	return len(p.seen)
}

// selfTestProbe adapts a core/selftest.Tester to the temporal/moduletest.Prober the activity calls.
//
// The adapter exists so modules depend on a stdlib-only leaf in core/ rather than on a Temporal package:
// a module importing temporal/moduletest would drag the workflow SDK into every connector, and the
// capability would then be unavailable to anything that must not link it.
type selfTestProbe struct {
	t   selftest.Tester
	key string
}

func (p selfTestProbe) Probe(ctx context.Context, req moduletest.Request) (string, string, error) {
	if p.t == nil {
		return "no probe is wired", "the module resolved to nothing — nothing was checked",
			fmt.Errorf("probe %s: nil tester", p.key)
	}
	res, err := p.t.SelfTest(ctx, req.Operator)
	if err != nil {
		summary := res.Summary
		if summary == "" {
			summary = "the check failed"
		}
		detail := res.Detail
		if detail == "" {
			// The module declined to classify. Surface the raw error rather than inventing a diagnosis
			// — a wrong explanation sends an operator to fix the wrong thing.
			detail = err.Error()
		}
		return summary, detail, err
	}
	summary := res.Summary
	if summary == "" {
		// A pass with nothing to say is suspicious enough to name. A probe that cannot describe what it
		// observed cannot distinguish a correctly configured module from one pointed at the wrong
		// instance, and that is the failure a green TEST is most likely to hide.
		summary = "the check passed but reported nothing it observed"
	}
	return summary, res.Detail, nil
}
