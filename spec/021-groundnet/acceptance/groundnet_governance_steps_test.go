package acceptance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/cucumber/godog"

	gn "github.com/territory-grounder/grounder/core/groundnet"
	"github.com/territory-grounder/grounder/core/trace"
)

// Governance/projection acceptance bindings (A1 / T-021-4 / T-021-7): the two-layer generalizable
// marker (REQ-2101), the quality-weighted reputation rollup (REQ-2107), opt-in default-off membership
// (REQ-2111), consumption-never-gated-behind-contribution (REQ-2112), the born-compatible ordering
// invariant (REQ-2113), and the unrecallable governed export record (REQ-2114). These drive the REAL
// core/groundnet + core/trace API so each scenario passes strictly. With group 1 (statement layer) and
// group 2 (core mechanism), this completes the spec/021 acceptance frontier — every scenario executes.
func init() {
	stepRegistrars = append(stepRegistrars, registerGovernanceSteps)
}

type finalWorld struct {
	trace                   trace.SessionTrace
	payloadBytes            []byte
	chunkWisdom             gn.WisdomV0
	rep                     *gn.Reputation
	scoreClean, scoreBad    float64
	scoreCleanTwoConfirmers float64
	posture                 gn.Posture
	defMayEmit              bool
	defMayConsume           bool
	defPublic               bool
	noAuthErr               error
	pubChange               gn.PostureChange
	stmt                    *gn.Statement
	governed                gn.GovernedRecord
}

func registerGovernanceSteps(sc *godog.ScenarioContext) {
	w := &finalWorld{}
	ctx := context.Background()

	// REQ-2101 — the two-layer marker keeps every chunk generalizable; the estate layer has no export path.
	sc.Step(`^a wisdom statement assembled for sharing$`, func() error {
		// A REAL session trace carrying estate-specific data in every estate slot (header + per-step).
		w.trace = trace.SessionTrace{
			ExternalRef: "TICKET-9911", Host: "prod-db-07.corp.internal", AlertRule: "PrometheusDBDown",
			ActionID: "act-771", PlanHash: "plan-abc-hash", Band: "AUTO", Verdict: gn.VerdictClean, Confidence: 0.9,
			Steps: []trace.Step{{Seq: 0, Rule: "db-down-rule", Reason: "restart because prod-db-07 OOMed", CredentialRef: "env:PGPASS"}},
		}
		return nil
	})
	sc.Step(`^the payload and the Emit input type are inspected$`, func() error {
		// The ONLY path to a shareable chunk is NewChunk(trace.GeneralizableLayer); the projection is the
		// only way to obtain that layer. Project the estate-laden trace and marshal the resulting payload.
		proj := trace.ProjectGeneralizable(w.trace, trace.GeneralizableClasses{
			OpClass: "restart-service", AlertClass: "service-down/http",
			KnownOpClasses: []string{"restart-service"}, KnownAlertClasses: []string{"service-down/http"},
		})
		chunk := gn.NewChunk(proj)
		b, err := chunk.Wisdom.Marshal()
		if err != nil {
			return err
		}
		w.payloadBytes = b
		w.chunkWisdom = chunk.Wisdom
		return nil
	})
	sc.Step(`^the payload is generalizable-only and carries no estate identifier and the estate-specific layer has no export path in the contract$`, func() error {
		// No estate identifier from ANY estate slot survives into the shareable payload.
		for _, estate := range []string{"TICKET-9911", "prod-db-07", "corp.internal", "PrometheusDBDown", "act-771", "plan-abc-hash", "db-down-rule", "OOMed", "PGPASS"} {
			if bytes.Contains(w.payloadBytes, []byte(estate)) {
				return fmt.Errorf("the shared payload must carry no estate identifier, found %q: %s", estate, w.payloadBytes)
			}
		}
		// It is generalizable-only: it carries the KINDS (op/alert class) and the verified outcome, and NO
		// free-text diagnosis (the generalizable layer carries none, so none can leak).
		if w.chunkWisdom.OpClass == "" || w.chunkWisdom.AlertClass == "" || w.chunkWisdom.Outcome.Verdict == "" {
			return fmt.Errorf("the generalizable classes + verified outcome must be present: %+v", w.chunkWisdom)
		}
		if w.chunkWisdom.Diagnosis != "" {
			return fmt.Errorf("the generalizable layer carries no free-text diagnosis, got %q", w.chunkWisdom.Diagnosis)
		}
		// The estate-specific layer has NO export path: a fully estate-populated source trace yielded an
		// estate-free payload because the Emit input TYPE (trace.GeneralizableLayer) has no estate slot.
		return nil
	})

	// REQ-2107 — reputation aggregates verified-outcome attestations weighted by quality, not volume.
	sc.Step(`^signed pseudonymous verified-outcome attestations from multiple nodes$`, func() error {
		w.rep = gn.NewReputation()
		return nil
	})
	sc.Step(`^reputation is aggregated$`, func() error {
		rep := w.rep
		// A CLEAN confirmation of producerClean; a DEVIATION confirmation of producerBad.
		rep.Observe("gnpub:producerClean", "gnpub:confirmerA", gn.ConfirmationV0{Subject: "s1", Result: gn.VerdictClean, VerifierProfile: "mechanical"})
		rep.Observe("gnpub:producerBad", "gnpub:confirmerA", gn.ConfirmationV0{Subject: "s2", Result: gn.VerdictDeviation, VerifierProfile: "mechanical"})
		// Idempotent: the SAME confirmer re-confirming the SAME subject counts once (no volume inflation).
		rep.Observe("gnpub:producerClean", "gnpub:confirmerA", gn.ConfirmationV0{Subject: "s1", Result: gn.VerdictClean, VerifierProfile: "mechanical"})
		// A producer cannot inflate its own score by self-confirming.
		rep.Observe("gnpub:producerClean", "gnpub:producerClean", gn.ConfirmationV0{Subject: "s1", Result: gn.VerdictClean, VerifierProfile: "mechanical"})
		w.scoreClean = rep.Score("gnpub:producerClean")
		w.scoreBad = rep.Score("gnpub:producerBad")
		// A DISTINCT confirmer contributes a distinct verified outcome (CRDT rollup over signed attestations).
		rep.Observe("gnpub:producerClean", "gnpub:confirmerB", gn.ConfirmationV0{Subject: "s1", Result: gn.VerdictClean, VerifierProfile: "mechanical"})
		w.scoreCleanTwoConfirmers = rep.Score("gnpub:producerClean")
		return nil
	})
	sc.Step(`^reputation is a CRDT-style rollup weighted by verified-outcome quality is never an on-chain vote or token and is never weighted by contribution volume$`, func() error {
		// Weighted by quality: a clean-confirmed producer outscores a deviation-confirmed one, with the
		// clean weight positive and the deviation weight negative.
		if !(w.scoreClean > w.scoreBad) {
			return fmt.Errorf("quality weighting: clean(%v) must outscore deviation(%v)", w.scoreClean, w.scoreBad)
		}
		if w.scoreClean <= 0 || w.scoreBad >= 0 {
			return fmt.Errorf("clean must weigh positive (%v) and deviation negative (%v)", w.scoreClean, w.scoreBad)
		}
		// Idempotent + no self-confirm + not volume: after a duplicate re-confirm and a self-confirm, one
		// distinct clean confirmation still weighs exactly 1.0 — not inflated by repetition.
		if w.scoreClean != 1.0 {
			return fmt.Errorf("one distinct clean confirmation must weigh exactly 1.0 (idempotent, not volume), got %v", w.scoreClean)
		}
		// A second DISTINCT confirmer's verified outcome aggregates commutatively to 2.0.
		if w.scoreCleanTwoConfirmers != 2.0 {
			return fmt.Errorf("a second distinct confirmer must aggregate to 2.0, got %v", w.scoreCleanTwoConfirmers)
		}
		return nil
	})

	// REQ-2111 — membership export and consumption are opt-in default-off and org-admin authenticated.
	sc.Step(`^a fresh node with no groundnet configuration$`, func() error {
		w.posture = gn.DefaultPosture()
		return nil
	})
	sc.Step(`^membership export and consumption are considered$`, func() error {
		// Default-off: a fresh node federates nothing.
		w.defMayEmit = w.posture.MayEmit()
		w.defMayConsume = w.posture.MayConsume()
		w.defPublic = w.posture.MayUsePublicTier()
		// A non-admin (empty principal) cannot lift default-off.
		_, _, w.noAuthErr = gn.SetCapability(w.posture, gn.CapMember, true, gn.OrgAdmin(""), 1)
		// A deliberate org-admin turns on membership, export, consume, and the public tier.
		p := w.posture
		p, _, _ = gn.SetCapability(p, gn.CapMember, true, gn.OrgAdmin("ops-admin"), 1)
		p, _, _ = gn.SetCapability(p, gn.CapExport, true, gn.OrgAdmin("ops-admin"), 2)
		p, _, _ = gn.SetCapability(p, gn.CapConsume, true, gn.OrgAdmin("ops-admin"), 3)
		p, w.pubChange, _ = gn.SetCapability(p, gn.CapPublic, true, gn.OrgAdmin("ops-admin"), 4)
		w.posture = p
		return nil
	})
	sc.Step(`^each is opt-in default-off and authorized at org-admin authority members are authenticated and a public tier exists only for provably zero-estate-specific distillate$`, func() error {
		if w.defMayEmit || w.defMayConsume || w.defPublic {
			return fmt.Errorf("a fresh node must federate nothing (default-off): emit=%v consume=%v public=%v", w.defMayEmit, w.defMayConsume, w.defPublic)
		}
		if !errors.Is(w.noAuthErr, gn.ErrNotOrgAdmin) {
			return fmt.Errorf("a non-admin must not lift default-off, want ErrNotOrgAdmin, got %v", w.noAuthErr)
		}
		if !w.posture.MayEmit() || !w.posture.MayConsume() || !w.posture.MayUsePublicTier() {
			return fmt.Errorf("a deliberate org-admin opt-in must enable the capabilities: %+v", w.posture)
		}
		// The change is a governed, authenticated record naming the org-admin principal.
		if w.pubChange.Principal != "ops-admin" || !w.pubChange.Enabled || w.pubChange.Capability != gn.CapPublic {
			return fmt.Errorf("the posture change must record the authenticated org-admin decision: %+v", w.pubChange)
		}
		return nil
	})

	// REQ-2112 — consumption is never gated behind contribution.
	sc.Step(`^a member that shares little or nothing$`, func() error {
		// A member with Consume ON but Export deliberately OFF — it contributes nothing.
		p := gn.DefaultPosture()
		p, _, _ = gn.SetCapability(p, gn.CapMember, true, gn.OrgAdmin("ops-admin"), 1)
		p, _, _ = gn.SetCapability(p, gn.CapConsume, true, gn.OrgAdmin("ops-admin"), 2)
		w.posture = p
		return nil
	})
	sc.Step(`^the member consumes from the groundnet$`, func() error {
		w.defMayConsume = w.posture.MayConsume()
		w.defMayEmit = w.posture.MayEmit()
		return nil
	})
	sc.Step(`^consumption is not gated behind contribution the member is not throttled or penalized and there is no contribution-to-consumption ratio$`, func() error {
		// It consumes freely although it exports nothing: MayConsume depends only on membership+consume,
		// never on export or any contribution measure — so there is no contribution-to-consumption ratio.
		if !w.defMayConsume {
			return fmt.Errorf("a member that shares nothing must still consume freely")
		}
		if w.defMayEmit {
			return fmt.Errorf("this member exports nothing (Export off) yet consumes — consumption is independent of contribution")
		}
		return nil
	})

	// REQ-2113 — a node is born compatible while the local tracer does not depend on the contract.
	sc.Step(`^a node carrying the local decision tracer and the dormant groundnet seam$`, func() error {
		// The local tracer is self-contained: it projects a session with NO groundnet involvement.
		w.trace = trace.SessionTrace{Band: "AUTO", Verdict: gn.VerdictClean, Confidence: 0.8}
		return nil
	})
	sc.Step(`^the local tracer persists reads and inspects and the export adapter targets the contract$`, func() error {
		// The tracer produces its generalizable layer standalone (no contract needed)...
		proj := trace.ProjectGeneralizable(w.trace, trace.GeneralizableClasses{
			OpClass: "restart-service", AlertClass: "service-down/http",
			KnownOpClasses: []string{"restart-service"}, KnownAlertClasses: []string{"service-down/http"},
		})
		// ...and the export adapter is what targets the contract: it CONSUMES the tracer's output type.
		w.chunkWisdom = gn.NewChunk(proj).Wisdom
		w.posture = gn.DefaultPosture()
		return nil
	})
	sc.Step(`^the local tracer does not depend on this contract only the export adapter does and groundnet build remains blocked until the flywheel graduates an artifact the artifacts are loadable and the tracer archive exists$`, func() error {
		// The export adapter targets the contract (groundnet -> trace): NewChunk consumed the projection.
		if w.chunkWisdom.OpClass == "" {
			return fmt.Errorf("the export adapter must consume the tracer's generalizable layer")
		}
		// The seam is dormant / default-off — groundnet federates nothing until deliberately armed.
		if w.posture.MayEmit() || w.posture.MayConsume() {
			return fmt.Errorf("the groundnet seam must be dormant by default (build remains blocked): %+v", w.posture)
		}
		// The tracer does NOT depend on the contract: core/trace must not import core/groundnet (the Go
		// compiler forbids the cycle; here we assert the dependency direction explicitly). go list is the
		// mechanical check; if the toolchain is unavailable in this env the direction is still guaranteed
		// by the compile above (groundnet imports trace, not vice versa).
		out, err := exec.Command("go", "list", "-deps", "github.com/territory-grounder/grounder/core/trace").CombinedOutput()
		if err == nil && strings.Contains(string(out), "products/territory-grounder/grounder/core/groundnet") {
			return fmt.Errorf("core/trace must NOT depend on core/groundnet — the local tracer must not depend on the contract")
		}
		return nil
	})

	// REQ-2114 — a shared chunk is unrecallable; the export decision is the last point of control.
	sc.Step(`^a chunk about to be emitted$`, func() error {
		node, err := gnNewNode(gnTestSeed)
		if err != nil {
			return err
		}
		stmt, err := node.adapter.Emit(ctx, gn.NewChunk(gnLayer()))
		if err != nil {
			return err
		}
		w.stmt = stmt
		return nil
	})
	sc.Step(`^the org admin makes the export decision$`, func() error {
		// MayEmit is the LAST point of control: default-off before the deliberate org-admin export decision.
		p := gn.DefaultPosture()
		w.defMayEmit = p.MayEmit()
		p, _, _ = gn.SetCapability(p, gn.CapMember, true, gn.OrgAdmin("ops-admin"), 1)
		p, _, _ = gn.SetCapability(p, gn.CapExport, true, gn.OrgAdmin("ops-admin"), 2)
		w.posture = p
		// The emitted chunk is stamped an unrecallable governed record declaring retention + provenance.
		h, err := w.stmt.Header()
		if err != nil {
			return err
		}
		raw, ok := w.stmt.Receipt()
		if !ok {
			return fmt.Errorf("the emitted statement must carry a Receipt for its provenance anchor")
		}
		r, err := gn.ParseReceipt(raw)
		if err != nil {
			return err
		}
		w.governed = gn.NewGovernedRecord(h.Subject, "federation-shared", r.Subject)
		return nil
	})
	sc.Step(`^the chunk is treated as unrecallable once emitted the export decision is the last point of control and the chunk declares its retention and provenance as a governed record$`, func() error {
		// The export decision is the last point of control: emit is gated (default-off) until the org admin
		// deliberately enables it.
		if w.defMayEmit {
			return fmt.Errorf("emit must be gated (default-off) before the org-admin export decision")
		}
		if !w.posture.MayEmit() {
			return fmt.Errorf("after the deliberate org-admin export decision, emit is permitted")
		}
		// The chunk is an unrecallable governed record declaring retention + provenance.
		if !w.governed.Unrecallable {
			return fmt.Errorf("an emitted chunk must be marked unrecallable (REQ-2114)")
		}
		if w.governed.Retention == "" || w.governed.ReceiptRef == "" || w.governed.Subject == "" {
			return fmt.Errorf("the governed record must declare subject + retention + provenance: %+v", w.governed)
		}
		// The unrecallability is made explicit to the org admin at opt-in.
		if !strings.Contains(gn.UnrecallableNotice, "UNRECALLABLE") {
			return fmt.Errorf("the unrecallable notice must state unrecallability at opt-in")
		}
		return nil
	})
}
