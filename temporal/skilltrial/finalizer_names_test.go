package skilltrial

import (
	"testing"

	"go.temporal.io/sdk/testsuite"

	"github.com/territory-grounder/grounder/temporal/configwrite"
	"github.com/territory-grounder/grounder/temporal/manifestwrite"
	"github.com/territory-grounder/grounder/temporal/modetransition"
	"github.com/territory-grounder/grounder/temporal/nativerule"
	"github.com/territory-grounder/grounder/temporal/objectgroup"
	"github.com/territory-grounder/grounder/temporal/policytrace"
	"github.com/territory-grounder/grounder/temporal/rulesetwrite"
	"github.com/territory-grounder/grounder/temporal/skillgen"
	"github.com/territory-grounder/grounder/temporal/skilljudge"
	"github.com/territory-grounder/grounder/temporal/skillwrite"
)

// The regression guard for the 2026-07-17 boot-loop: EVERY skill workflow must register on ONE worker
// env without a name collision (Temporal registers by bare function name — two packages exporting
// `Workflow` panic at RegisterWorkflow). New cron/one-shot workflows join this list.
func TestSkillWorkflowNamesDoNotCollide(t *testing.T) {
	var wts testsuite.WorkflowTestSuite
	env := wts.NewTestWorkflowEnvironment()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("workflow registration must not panic: %v", r)
		}
	}()
	env.RegisterWorkflow(FinalizerWorkflow)
	env.RegisterWorkflow(skillwrite.TransitionWorkflow)
	env.RegisterWorkflow(manifestwrite.ManifestTransitionWorkflow)
	env.RegisterWorkflow(skilljudge.JudgeWorkflow)
	env.RegisterWorkflow(skillgen.GeneratorWorkflow)
	env.RegisterWorkflow(configwrite.ConfigWriteWorkflow)
	env.RegisterWorkflow(configwrite.SecretPutWorkflow)
	env.RegisterWorkflow(configwrite.SecretRewrapWorkflow)
	env.RegisterWorkflow(modetransition.ModeTransitionWorkflow)
	env.RegisterWorkflow(policytrace.PolicyTraceWorkflow)
	env.RegisterWorkflow(rulesetwrite.RulesetWriteWorkflow)
	env.RegisterWorkflow(nativerule.NativeRuleWriteWorkflow)
	env.RegisterWorkflow(objectgroup.ObjectGroupWriteWorkflow)
}
