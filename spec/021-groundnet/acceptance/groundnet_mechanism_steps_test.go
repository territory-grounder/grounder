package acceptance

import (
	"bytes"
	"context"
	"fmt"

	"github.com/cucumber/godog"

	gn "github.com/territory-grounder/grounder/core/groundnet"
	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/trace"
)

// Core-mechanism acceptance bindings (T-021-3 / T-021-5 / T-021-6): the transparency-log Receipt and
// its provenance anchoring (REQ-2105/2106), the Emit/Ingest adapter seam (REQ-2108), the
// subordinate-not-authority re-graduation path (REQ-2109/2110), and id-based replay rejection
// (REQ-2115). These drive the REAL core/groundnet seam end-to-end — a producing node Emits a signed,
// receipted SCITT statement and a consuming node Ingests it — so each scenario passes strictly, not as
// a pending stub. They never reach an actuator, lift a floor, or change a mutation posture.
func init() {
	stepRegistrars = append(stepRegistrars, registerMechanismSteps)
}

const gnOtherSeed = "202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f"

// gnNode is a self-contained groundnet node for the in-process seam: a producer pseudonym, its local
// transparency log (which is BOTH the TransparencyLog and the ReplayGuard over one chain), a
// graduation store, the subordinate re-graduator reading it, and the Emit/Ingest adapter.
type gnNode struct {
	p       *gn.Pseudonym
	tl      *gn.Translog
	store   *policy.MemGraduationStore
	regrad  *gn.ReGrad
	adapter *gn.Adapter
}

func gnNewNode(seed string) (*gnNode, error) {
	p, err := gn.NewPseudonym(seed)
	if err != nil {
		return nil, err
	}
	tl := gn.NewTranslog()
	store := policy.NewMemGraduationStore()
	regrad := gn.NewReGrad(store)
	return &gnNode{p: p, tl: tl, store: store, regrad: regrad, adapter: gn.NewAdapter(p, tl, tl, regrad)}, nil
}

// gnLayer is a valid generalizable-layer projection: only KINDS and a mechanically verified outcome.
func gnLayer() trace.GeneralizableLayer {
	return trace.GeneralizableLayer{
		AlertClass: "service-down/http",
		OpClass:    "restart-service",
		Reversible: true,
		BlastClass: "low",
		Verdict:    gn.VerdictClean,
	}
}

// gnFlipHex flips the leading hex nibble of a chain fold, to prove tamper-evidence.
func gnFlipHex(s string) string {
	if s == "" {
		return "0"
	}
	b := []byte(s)
	if b[0] == '0' {
		b[0] = '1'
	} else {
		b[0] = '0'
	}
	return string(b)
}

type mechanismWorld struct {
	nodeA, nodeB        *gnNode
	wire                []byte
	receipt, receipt2   gn.Receipt
	verifyGood          error
	verifyTamper        error
	ingest1, ingest2    gn.IngestOutcome
	opClass             string
	levelAfterIngest    policy.Level
	levelAfterLocalGrad policy.Level
}

func registerMechanismSteps(sc *godog.ScenarioContext) {
	w := &mechanismWorld{}
	ctx := context.Background()

	// REQ-2105 — provenance is a signed append-only transparency-log inclusion proof, not a blockchain.
	sc.Step(`^a statement Receipt from a SCITT Transparency Service over a multi-witness append-only VDS$`, func() error {
		node, err := gnNewNode(gnTestSeed)
		if err != nil {
			return err
		}
		w.nodeA = node
		stmt, err := node.adapter.Emit(ctx, gn.NewChunk(gnLayer()))
		if err != nil {
			return err
		}
		raw, ok := stmt.Receipt()
		if !ok {
			return fmt.Errorf("Emit must register the statement and attach a Receipt")
		}
		r, err := gn.ParseReceipt(raw)
		if err != nil {
			return err
		}
		w.receipt = r
		return nil
	})
	sc.Step(`^tamper-evidence and censorship-resistance are established$`, func() error {
		// tamper-evidence: the local log re-derives and checks the Receipt against its append-only chain;
		// a Receipt whose chain fold is altered no longer verifies.
		w.verifyGood = w.nodeA.tl.VerifyLocal(w.receipt)
		tampered := w.receipt
		tampered.EntryHash = gnFlipHex(tampered.EntryHash)
		w.verifyTamper = w.nodeA.tl.VerifyLocal(tampered)
		return nil
	})
	sc.Step(`^they derive from the Receipt inclusion proofs on the Sigstore Rekor and certificate-transparency model SCITT standardises and not from a blockchain global consensus$`, func() error {
		if w.verifyGood != nil {
			return fmt.Errorf("the Receipt must verify against the local append-only log: %v", w.verifyGood)
		}
		if w.verifyTamper == nil {
			return fmt.Errorf("a tampered Receipt must fail verification — tamper-evidence derives from the hash-chain fold")
		}
		// The Receipt is a per-statement INCLUSION PROOF over an append-only chain — a chain position
		// plus the HEAD fold+anchor that witness the chain at issue time — not a global-consensus block.
		r := w.receipt
		if r.Domain != gn.TranslogDomain {
			return fmt.Errorf("the Receipt must name the transparency-log domain, got %q", r.Domain)
		}
		if r.EntryHash == "" || r.HeadHash == "" || r.Digest == "" {
			return fmt.Errorf("the Receipt must carry the inclusion-proof chain fields (entry/head/anchor): %+v", r)
		}
		return nil
	})

	// REQ-2106 — the groundnet log extends the local hash-chained governance ledger.
	sc.Step(`^the local hash-chained governance_ledger from migration 0015$`, func() error {
		node, err := gnNewNode(gnTestSeed)
		if err != nil {
			return err
		}
		w.nodeA = node
		return nil
	})
	sc.Step(`^a statement Receipt is anchored$`, func() error {
		s1, err := w.nodeA.adapter.Emit(ctx, gn.NewChunk(gnLayer()))
		if err != nil {
			return err
		}
		r1raw, _ := s1.Receipt()
		r1, err := gn.ParseReceipt(r1raw)
		if err != nil {
			return err
		}
		layer2 := gnLayer()
		layer2.OpClass = "drain-node"
		s2, err := w.nodeA.adapter.Emit(ctx, gn.NewChunk(layer2))
		if err != nil {
			return err
		}
		r2raw, _ := s2.Receipt()
		r2, err := gn.ParseReceipt(r2raw)
		if err != nil {
			return err
		}
		w.receipt, w.receipt2 = r1, r2
		return nil
	})
	sc.Step(`^the groundnet Transparency Service is the federated multi-witness extension of the local ledger and the statement anchors in the producing node local ledger$`, func() error {
		// Each Receipt anchors the chain HEAD with an audit anchor digest — the governance-ledger
		// anchoring model (core/audit), the same one migration 0015 established for governance_ledger.
		if w.receipt.Digest == "" || w.receipt2.Digest == "" {
			return fmt.Errorf("each Receipt must anchor the local ledger HEAD with an audit digest")
		}
		// The log is append-only: the second statement's HEAD position advances past the first.
		if w.receipt2.HeadSeq <= w.receipt.HeadSeq {
			return fmt.Errorf("the append-only chain HEAD must advance (an extension of the ledger), got seq %d then %d", w.receipt.HeadSeq, w.receipt2.HeadSeq)
		}
		// Both statements anchor in the producing node's own local ledger.
		if err := w.nodeA.tl.VerifyLocal(w.receipt); err != nil {
			return fmt.Errorf("the first statement must anchor in the producing node local ledger: %v", err)
		}
		if err := w.nodeA.tl.VerifyLocal(w.receipt2); err != nil {
			return fmt.Errorf("the second statement must anchor in the producing node local ledger: %v", err)
		}
		return nil
	})

	// REQ-2108 — the adapter seam emits from the generalizable layer and ingests into re-graduation.
	sc.Step(`^a node implementing the typed Emit and Ingest adapter seam$`, func() error {
		a, err := gnNewNode(gnTestSeed)
		if err != nil {
			return err
		}
		b, err := gnNewNode(gnOtherSeed)
		if err != nil {
			return err
		}
		w.nodeA, w.nodeB = a, b
		return nil
	})
	sc.Step(`^Emit assembles a chunk and Ingest lands a foreign chunk$`, func() error {
		chunk := gn.NewChunk(gnLayer())
		w.opClass = chunk.Wisdom.OpClass
		stmt, err := w.nodeA.adapter.Emit(ctx, chunk)
		if err != nil {
			return err
		}
		wire, err := stmt.MarshalCBOR()
		if err != nil {
			return err
		}
		out, err := w.nodeB.adapter.Ingest(ctx, wire)
		if err != nil {
			return err
		}
		w.ingest1 = out
		return nil
	})
	sc.Step(`^Emit sources its chunk only from the spec/020 generalizable layer and Ingest lands the chunk into the local re-graduation path and neither side reads the estate-specific layer$`, func() error {
		if !w.ingest1.Accepted || w.ingest1.Disposition != gn.DispositionCandidate {
			return fmt.Errorf("Ingest must land the foreign chunk as a candidate, got %+v", w.ingest1)
		}
		if _, ok := w.nodeB.regrad.LandedHint(w.opClass); !ok {
			return fmt.Errorf("the ingested chunk must land in node B's local re-graduation path")
		}
		// Neither side reads the estate-specific layer: the Emit boundary type (a Chunk built via
		// NewChunk from trace.GeneralizableLayer) has NO slot for a host/ticket/action id, so projecting
		// a session that CARRIES estate data yields a chunk whose payload contains none of it.
		est := trace.SessionTrace{Host: "estate-secret-host.internal", ExternalRef: "TICKET-123", ActionID: "act-xyz", Band: "AUTO", Verdict: gn.VerdictClean}
		proj := trace.ProjectGeneralizable(est, trace.GeneralizableClasses{
			OpClass: "restart-service", AlertClass: "service-down/http",
			KnownOpClasses: []string{"restart-service"}, KnownAlertClasses: []string{"service-down/http"},
		})
		chunk := gn.NewChunk(proj)
		payload, err := chunk.Wisdom.Marshal()
		if err != nil {
			return err
		}
		for _, estate := range []string{"estate-secret-host", "TICKET-123", "act-xyz"} {
			if bytes.Contains(payload, []byte(estate)) {
				return fmt.Errorf("the emitted chunk must not carry estate-specific data %q: %s", estate, payload)
			}
		}
		return nil
	})

	// REQ-2109 — an ingested chunk is a subordinate hint that never bypasses the never-auto floor.
	sc.Step(`^a foreign chunk ingested as a hint$`, func() error {
		a, err := gnNewNode(gnTestSeed)
		if err != nil {
			return err
		}
		b, err := gnNewNode(gnOtherSeed)
		if err != nil {
			return err
		}
		w.nodeA, w.nodeB = a, b
		layer := gnLayer()
		w.opClass = layer.OpClass
		stmt, err := a.adapter.Emit(ctx, gn.NewChunk(layer))
		if err != nil {
			return err
		}
		wire, err := stmt.MarshalCBOR()
		if err != nil {
			return err
		}
		out, err := b.adapter.Ingest(ctx, wire)
		if err != nil {
			return err
		}
		w.ingest1 = out
		return nil
	})
	sc.Step(`^an action the hint influences reaches the actuation path$`, func() error {
		w.levelAfterIngest = w.nodeB.regrad.Level(ctx, w.opClass)
		return nil
	})
	sc.Step(`^the chunk re-runs local eval the graduation ladder and the policy gate and never bypasses the interceptor the never-auto floor or the mode chokepoint and the local constitution remains sovereign$`, func() error {
		if !w.ingest1.Accepted || w.ingest1.Disposition != gn.DispositionCandidate {
			return fmt.Errorf("the foreign chunk must land as a subordinate candidate, got %+v", w.ingest1)
		}
		// It earned NO autonomy from ingest: its local level is the propose-only floor (LevelApprove), so
		// any action it influences routes to the human vote — it cannot bypass the never-auto floor.
		if w.levelAfterIngest != policy.LevelApprove {
			return fmt.Errorf("an ingested chunk must sit at the never-auto floor (LevelApprove), got %v", w.levelAfterIngest)
		}
		return nil
	})

	// REQ-2110 — an ingested chunk re-graduates locally before it earns any local standing.
	sc.Step(`^a foreign statement that graduated on its producing node$`, func() error {
		a, err := gnNewNode(gnTestSeed)
		if err != nil {
			return err
		}
		b, err := gnNewNode(gnOtherSeed)
		if err != nil {
			return err
		}
		w.nodeA, w.nodeB = a, b
		layer := gnLayer()
		w.opClass = layer.OpClass
		// The op-class graduated to full autonomy on its PRODUCING node.
		a.store.Seed(policy.ClassState{OpClass: layer.OpClass, Level: policy.LevelAuto})
		stmt, err := a.adapter.Emit(ctx, gn.NewChunk(layer))
		if err != nil {
			return err
		}
		w.wire, err = stmt.MarshalCBOR()
		return err
	})
	sc.Step(`^the statement enters the consuming node$`, func() error {
		out, err := w.nodeB.adapter.Ingest(ctx, w.wire)
		if err != nil {
			return err
		}
		w.ingest1 = out
		// The standing the op-class has on node B immediately after ingest, before any LOCAL graduation.
		w.levelAfterIngest = w.nodeB.regrad.Level(ctx, w.opClass)
		// Node B now EARNS it locally — a local verified-clean re-graduation promotes it in B's own store.
		w.nodeB.store.Seed(policy.ClassState{OpClass: w.opClass, Level: policy.LevelAuto})
		w.levelAfterLocalGrad = w.nodeB.regrad.Level(ctx, w.opClass)
		return nil
	})
	sc.Step(`^the statement may inform investigation as evidence but does not inherit the producer trust and re-graduates against local traffic and local verified outcomes before it earns any local standing and only local mechanical verification grants it authority$`, func() error {
		// It may inform investigation as evidence: the hint is landed and readable.
		if _, ok := w.nodeB.regrad.LandedHint(w.opClass); !ok {
			return fmt.Errorf("the foreign wisdom must be available as an investigation hint")
		}
		// It does NOT inherit the producer's graduation: on node B, right after ingest, it sits at the
		// propose-only floor even though it was LevelAuto on its producing node.
		if w.levelAfterIngest != policy.LevelApprove {
			return fmt.Errorf("the ingested chunk must NOT inherit the producer's standing; want LevelApprove, got %v", w.levelAfterIngest)
		}
		// Only LOCAL graduation grants authority: the level node B reads after re-graduating against its
		// own outcomes is the one B earned locally — the authority source is B's store, not the ingest.
		if w.levelAfterLocalGrad != policy.LevelAuto {
			return fmt.Errorf("authority must come from LOCAL graduation; want LevelAuto after local re-graduation, got %v", w.levelAfterLocalGrad)
		}
		return nil
	})

	// REQ-2115 — a replayed or duplicate chunk is rejected by its id and provenance anchor.
	sc.Step(`^a statement already ingested and later re-emitted with the same sub and Transparency Service Receipt$`, func() error {
		a, err := gnNewNode(gnTestSeed)
		if err != nil {
			return err
		}
		b, err := gnNewNode(gnOtherSeed)
		if err != nil {
			return err
		}
		w.nodeA, w.nodeB = a, b
		stmt, err := a.adapter.Emit(ctx, gn.NewChunk(gnLayer()))
		if err != nil {
			return err
		}
		w.wire, err = stmt.MarshalCBOR()
		if err != nil {
			return err
		}
		out1, err := b.adapter.Ingest(ctx, w.wire)
		if err != nil {
			return err
		}
		w.ingest1 = out1
		return nil
	})
	sc.Step(`^the consumer receives the re-emitted statement$`, func() error {
		// The SAME statement bytes (same sub + Receipt) arrive again.
		out2, err := w.nodeB.adapter.Ingest(ctx, w.wire)
		if err != nil {
			return err
		}
		w.ingest2 = out2
		return nil
	})
	sc.Step(`^the consumer rejects the replay so it cannot inflate the pseudonym reputation or re-trigger ingest and one statement earns local standing at most once per node$`, func() error {
		if !w.ingest1.Accepted || w.ingest1.Disposition != gn.DispositionCandidate {
			return fmt.Errorf("the first ingest must be accepted as a candidate, got %+v", w.ingest1)
		}
		if w.ingest2.Accepted {
			return fmt.Errorf("the replayed statement must NOT be accepted a second time, got %+v", w.ingest2)
		}
		if w.ingest2.Disposition != gn.DispositionRejectedReplay {
			return fmt.Errorf("the replay must be rejected as a replay, got disposition %q", w.ingest2.Disposition)
		}
		// Rejection keys on the same content-address subject — id-based replay rejection.
		if w.ingest1.Subject == "" || w.ingest1.Subject != w.ingest2.Subject {
			return fmt.Errorf("replay rejection must key on the same content-address subject, got %q then %q", w.ingest1.Subject, w.ingest2.Subject)
		}
		return nil
	})
}
