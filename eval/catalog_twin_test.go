package eval

import (
	"sort"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/execclass"
	estatetools "github.com/territory-grounder/grounder/modules/estate"
	"github.com/territory-grounder/grounder/modules/ingest/librenms"
	"github.com/territory-grounder/grounder/modules/observability/hostdiag"
	"github.com/territory-grounder/grounder/modules/observability/syslogng"
)

// realInertTools builds the production tools the way fixtures_test does — the reference the twin must equal.
func realInertTools(t *testing.T) []agent.Tool {
	t.Helper()
	var all []agent.Tool
	all = append(all, librenms.NewTools([]librenms.Deployment{{Site: "nl", BaseURL: "https://unused.invalid", TokenRef: "env:X"}}, nil)...)
	all = append(all, hostdiag.NewTools([]hostdiag.Access{{Site: "nl", HostGlob: "*", SSHUser: "root", KeyRef: "env:X"}}, fakeRunner{}, staticResolver{fixtureBundle(t)})...)
	all = append(all, syslogng.NewTools([]syslogng.Server{{Site: "NL", SSHHost: "syslog.example.net", SSHUser: "root", KeyRef: "env:X"}}, fakeRunner{})...)
	return all
}

// TG-592's oracle: in BOTH deterministic arms every served tool publishes the REAL tool's Description and Params,
// so the catalog the model reads (ToolSet.CatalogFor) is byte-for-byte the live one for those tools — and the
// loop's arg screen has the same schema (a call without `host` is refused exactly as production refuses it).
//
// What this proves and what it does not: the reference and the twin call the SAME constructors, so the oracle
// pins the WIRING (every served name carries an entry, in both arms, and the rendered catalog groups and lists
// exactly the live tools) rather than the entries' text against an independent source. Today no production-only
// constructor option changes a Description (syslogng's search cap option leaves the Description on the default
// constant); a future option that did would need the twin to receive it. The displaced-estate-tool entry path of
// the replay arm is covered by TestReplayServesARecordedEstateReadOverTheLiveGraph (replay_test.go).
func TestFixtureAndReplayArmsRenderTheLiveCatalog(t *testing.T) {
	inc := fixtureArmedIncidents(t)[0]
	real := map[string]agent.ACITool{}
	for _, tl := range realInertTools(t) {
		aci, ok := tl.(agent.ACITool)
		if !ok {
			t.Fatalf("production tool %s publishes no catalog entry — the twin cannot borrow what does not exist", tl.Name())
		}
		real[tl.Name()] = aci
	}
	arms := map[string]*agent.ToolSet{
		"fixture": NewFixtureToolSet(inc, estate.NewGraph()),
		"replay":  NewReplayToolSet(RecordedFromFixtures(inc), inc, estate.NewGraph()),
	}
	for arm, ts := range arms {
		for _, name := range fixtureServedTools {
			tl, ok := ts.Get(name)
			if !ok {
				t.Fatalf("%s arm lacks %s", arm, name)
			}
			aci, ok := tl.(agent.ACITool)
			if !ok {
				t.Fatalf("%s arm: %s publishes no catalog entry — the model would see a bare name", arm, name)
			}
			if aci.Description() == "" || aci.Description() != real[name].Description() {
				t.Fatalf("%s arm: %s description differs from production", arm, name)
			}
			want, got := real[name].Params(), aci.Params()
			if len(want) == 0 || len(got) != len(want) {
				t.Fatalf("%s arm: %s has %d params, production %d", arm, name, len(got), len(want))
			}
			for i := range want {
				if !(got[i].Name == want[i].Name && got[i].Type == want[i].Type && got[i].Required == want[i].Required && got[i].Example == want[i].Example && got[i].Description == want[i].Description && strings.Join(got[i].Enum, ",") == strings.Join(want[i].Enum, ",")) {
					t.Fatalf("%s arm: %s param %d differs: %+v vs %+v", arm, name, i, got[i], want[i])
				}
			}
		}
		// The rendered catalog — what the model actually reads — is identical to a set of the real tools plus the
		// same offline tools, for every exec class.
		ref := agent.NewReadOnlyToolSet()
		_ = ref.Register(IncidentContextTool(inc))
		for _, tl := range estatetools.New(func() *estate.Graph { return estate.NewGraph() }) {
			_ = ref.Register(tl)
		}
		for _, tl := range realInertTools(t) {
			_ = ref.Register(tl)
		}
		for _, class := range []execclass.Class{execclass.Deterministic, execclass.FastAgent, execclass.StandardAgent, execclass.DeepInvestigation, execclass.HumanLed} {
			if got, want := ts.CatalogFor(class), ref.CatalogFor(class); got != want {
				t.Fatalf("%s arm renders a different catalog than production for class %q:\n--- arm ---\n%s\n--- production ---\n%s", arm, class, got, want)
			}
		}
	}
	// A call without the required host is refused by the arg screen in the deterministic arms exactly as live.
	tl, _ := arms["fixture"].Get("get-device-status")
	if err := agent.ValidateArgs(tl, map[string]string{}); err == nil {
		t.Fatal("the fixture arm must screen a host-less call the way production does")
	}
	// The twin's key set is EXACTLY the served names — neither a missing entry (a bare name in the prompt) nor an
	// extra one (a real tool the arms never serve, which would mean the served list drifted from the constructors).
	names := append([]string{}, fixtureServedTools...)
	sort.Strings(names)
	var twinNames []string
	for n := range catalogTwin() {
		twinNames = append(twinNames, n)
	}
	sort.Strings(twinNames)
	if strings.Join(twinNames, ",") != strings.Join(names, ",") {
		t.Fatalf("the twin's names differ from the served tools:\n twin: %v\n arms: %v", twinNames, names)
	}
}
