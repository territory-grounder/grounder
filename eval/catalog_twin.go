package eval

import (
	"context"
	"errors"
	"sync"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/modules/ingest/librenms"
	"github.com/territory-grounder/grounder/modules/observability/hostdiag"
	"github.com/territory-grounder/grounder/modules/observability/syslogng"
)

// THE CATALOG TWIN (TG-592).
//
// The agent's system prompt renders every registered tool's catalog entry — its Description and Params
// (agent.ACITool, TG-197) — and the loop's arg screen validates calls against those Params. The fixture arm and
// the replay arm register THEIR OWN tool types under the real names, and until TG-592 those types published no
// entry: on every fixture-armed eval incident the ten network-backed tools appeared as bare names, with no
// description and no `host` parameter spec, so the model triaged under a DIFFERENT prompt than production and
// its calls were never screened. A harness that changes the prompt is probing a different harness than the one
// under test.
//
// catalogTwin builds the REAL tools from INERT configuration — reserved `.invalid` hostnames (RFC 2606), a runner
// and a resolver that refuse every call — purely to read their Description/Params. Nothing here is ever invoked:
// the deterministic arms serve observations themselves and only BORROW the real entry, so the catalog the model
// reads in the fixture or replay arm is byte-for-byte the live one. TestFixtureAndReplayArmsRenderTheLiveCatalog
// pins that equality per tool; fixtures_test.go already pins the served NAMES against the same constructors.

var (
	twinOnce sync.Once
	twin     map[string]agent.ACITool
)

// catalogTwin returns the real tools' catalog entries by name. Built once; never invoked.
func catalogTwin() map[string]agent.ACITool {
	twinOnce.Do(func() {
		twin = map[string]agent.ACITool{}
		var all []agent.Tool
		all = append(all, librenms.NewTools([]librenms.Deployment{{Site: "twin", BaseURL: "https://catalog-twin.invalid", TokenRef: "env:TG_CATALOG_TWIN_UNSET"}}, nil)...)
		all = append(all, hostdiag.NewTools([]hostdiag.Access{{Site: "twin", HostGlob: "*", SSHUser: "nobody", KeyRef: "env:TG_CATALOG_TWIN_UNSET"}}, inertRunner{}, inertResolver{})...)
		all = append(all, syslogng.NewTools([]syslogng.Server{{Site: "twin", SSHHost: "catalog-twin.invalid", SSHUser: "nobody", KeyRef: "env:TG_CATALOG_TWIN_UNSET"}}, inertRunner{})...)
		for _, tl := range all {
			if aci, ok := tl.(agent.ACITool); ok {
				twin[tl.Name()] = aci
			}
		}
	})
	return twin
}

var errCatalogTwin = errors.New("catalog twin: this tool exists only to publish its catalog entry and is never invoked")

// inertRunner satisfies syslogng.Runner (shared by hostdiag) and refuses every run.
type inertRunner struct{}

func (inertRunner) Run(context.Context, syslogng.Server, []string) (syslogng.RunResult, error) {
	return syslogng.RunResult{}, errCatalogTwin
}

// inertResolver satisfies hostdiag.IdentityResolver and resolves nothing.
type inertResolver struct{}

func (inertResolver) Resolve(context.Context, credential.Target) (credential.Bundle, error) {
	return credential.Bundle{}, errCatalogTwin
}

// describedBy returns the entry's Description and Params, or empty values when the twin has no entry for the
// name — the catalog then renders the bare name, exactly as before TG-592, never an invented description.
func describedBy(aci agent.ACITool) (string, []agent.ParamSpec) {
	if aci == nil {
		return "", nil
	}
	return aci.Description(), aci.Params()
}
