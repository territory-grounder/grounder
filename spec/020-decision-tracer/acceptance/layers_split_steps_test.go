package acceptance

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/core/trace"
)

// T-020-14 (schema-split half) binds REQ-2017: the trace schema separates an ESTATE-SPECIFIC
// layer from a GENERALIZABLE layer. The oracle builds a SessionTrace loaded with estate
// identifiers (a host, a ticket id, a rule id, a target, a free-text reason, a credential ref),
// projects it through the REAL core/trace.ProjectGeneralizable, and asserts the estate layer
// still holds those identifiers while the generalizable layer holds only alert-class →
// resolution → verified-outcome + graduated-artifact refs with NONE of them — de-identification
// by TYPE, not by a scrubber. The export-lane half (REQ-2020: the OTel/Langfuse redacted-LLM-
// subset lane) is the other part of T-020-14 and stays @pending until it lands.
func init() {
	stepRegistrars = append(stepRegistrars, registerSchemaSplitSteps)
}

type schemaSplitWorld struct {
	estate        trace.SessionTrace
	generalizable trace.GeneralizableLayer
}

func registerSchemaSplitSteps(sc *godog.ScenarioContext) {
	w := &schemaSplitWorld{}

	sc.Step(`^the trace schema$`, func() error {
		w.estate = trace.SessionTrace{
			ExternalRef: "TG-9001-ticket",
			Host:        "dc1edge03.estate.internal",
			AlertRule:   "BGPSessionDown_edge03",
			ActionID:    "act-777",
			PlanHash:    "plan-cafef00d",
			Band:        "AUTO_NOTICE",
			Verdict:     "clean",
			Confidence:  0.9,
			Steps: []trace.Step{{
				Kind:          trace.StepPropose,
				Reason:        "restart bgpd on dc1edge03",
				Rule:          "BGPSessionDown_edge03",
				CredentialRef: "ssh",
				PlanOps:       []trace.PlanOp{{Op: "change", T: "dc1edge03:bgpd"}},
			}},
		}
		return nil
	})

	sc.Step(`^the estate-specific layer and the generalizable layer are inspected$`, func() error {
		w.generalizable = trace.ProjectGeneralizable(w.estate, trace.GeneralizableClasses{
			OpClass:           "restart-service",
			AlertClass:        "service-down/http",
			Reversible:        true,
			BlastClass:        "single-host",
			Artifacts:         []trace.ArtifactRef{{Kind: "runbook", Ref: "sha256:3c96deadbeefa0df3c96deadbeefa0df3c96deadbeefa0df3c96deadbeefa0df"}},
			KnownOpClasses:    []string{"restart-service"},
			KnownAlertClasses: []string{"service-down/http"},
			KnownBlastClasses: []string{"single-host"},
		})
		return nil
	})

	sc.Step(`^the estate-specific layer holds hosts IPs topology credential identities and raw traces and the generalizable layer holds alert-class to resolution to verified-outcome plus graduated artifacts with no estate identifier so a future federated export needs no schema rewrite and v1 shares nothing$`, func() error {
		// The estate-specific layer (the SessionTrace) DOES hold the estate identifiers.
		estateBlob, _ := json.Marshal(w.estate)
		for _, id := range []string{"dc1edge03", "TG-9001", "BGPSessionDown", "bgpd"} {
			if !strings.Contains(string(estateBlob), id) {
				return fmt.Errorf("the estate-specific layer should hold %q but does not: %s", id, estateBlob)
			}
		}
		// The generalizable layer holds NONE of the estate identifiers.
		genBlob, _ := json.Marshal(w.generalizable)
		for _, leak := range []string{
			"dc1", "edge03", "TG-9001", "act-777", "plan-cafef00d",
			"bgpd", "estate.internal", "BGPSessionDown", "ssh",
		} {
			if strings.Contains(string(genBlob), leak) {
				return fmt.Errorf("estate identifier %q leaked into the generalizable layer: %s", leak, genBlob)
			}
		}
		// The generalizable layer DOES hold alert-class → resolution → verified-outcome + artifacts.
		g := w.generalizable
		if g.AlertClass != "service-down/http" || g.OpClass != "restart-service" || g.Verdict != "clean" {
			return fmt.Errorf("the generalizable layer dropped a class or the verified outcome: %+v", g)
		}
		if len(g.Artifacts) != 1 || g.Artifacts[0].Ref != "sha256:3c96deadbeefa0df3c96deadbeefa0df3c96deadbeefa0df3c96deadbeefa0df" {
			return fmt.Errorf("the generalizable layer dropped the graduated-artifact ref: %+v", g.Artifacts)
		}
		return nil
	})
}
