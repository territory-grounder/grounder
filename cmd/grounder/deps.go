package main

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	ingestadapter "github.com/territory-grounder/grounder/adapters/ingest"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/httpapi"
	coreingest "github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/persist"
	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/trace"
	"github.com/territory-grounder/grounder/modules"
	"github.com/territory-grounder/grounder/modules/resolve"
	tg "github.com/territory-grounder/grounder/temporal"
	"github.com/territory-grounder/grounder/temporal/runner"
)

// buildPublicAPI assembles the authenticated read-only public router the server serves. It registers the
// identity probe (/v1/whoami) and then MOUNTS the spec/006 console surface via httpapi.Register — the call
// cmd/grounder was missing, which left /v1/stats and session-replay built, contracted, and tested but never
// served by the running binary (so the operator console had no live data source). Every route is
// authenticated (auth.Router.Handle panics on auth=none, INV-01) and read-only; mutation stays OFF.
// commitConfirmChipRead is the armed-revert chip seam (spec/029 T-029-5), set by cmd/grounder/main.go
// before buildPublicAPI runs — the package-level wiring the IngestRefused counter established for
// additions this signature must not grow for. Nil leaves every session walk exactly as before.
var commitConfirmChipRead func(ctx context.Context, externalRef string) (*httpapi.CommitConfirmChipDTO, error)

func buildPublicAPI(verifier *auth.Verifier, gate *safety.Chokepoint, ledger httpapi.LedgerReader, ingesters httpapi.IngesterResolver, triage httpapi.TriageStarter, capabilities httpapi.CapabilitiesReader, sessions *auth.SessionAuthenticator, sessionsRead httpapi.SessionsReader, alerts httpapi.AlertLog, transitions httpapi.TransitionRecorder, governance httpapi.GovernanceReader, secretsRead httpapi.SecretsReader, models httpapi.ModelsReader, contract []byte, estate httpapi.EstateReader, grounding httpapi.GroundingReader, votes httpapi.VoteSignaler, pending persist.PendingReader, cfgResolver httpapi.ConfigResolver, skillsRead httpapi.SkillsReader, skillsWrite httpapi.SkillsWriter, wiki httpapi.WikiReader, adminSessions *auth.AdminAuthenticator, configWrite httpapi.ConfigWriter, secretsWrite httpapi.SealedSecretWriter, sealRewrap httpapi.SealRewrapper, sealedRead httpapi.SealedSecretsReader, credentials httpapi.CredentialsReader, policyRead httpapi.PolicyReader, regimeRead httpapi.RegimeReader, modeTransition httpapi.ModeTransitioner, engineToggle httpapi.EngineToggler, posture posturePeek, postureStaleAfter time.Duration, sessionDetailRead httpapi.SessionDetailReader, sessionEvidenceRead trace.AgentStepEvidenceReader, sessionDiagnosisRead trace.DiagnosisReader, credentialOnboarding httpapi.CredentialOnboardingReader, actions httpapi.ActionManifestReader, suppression httpapi.SuppressionObserver, proposalsRead httpapi.ProposalsReader, counterfactualRead httpapi.CounterfactualReader, manifestRead httpapi.ManifestReader, manifestWrite httpapi.ManifestWriter, opClassRead httpapi.OpClassCandidateReader, opClassWrite httpapi.OpClassWriter, moduleSchema httpapi.ModuleSchemaReader, moduleTest httpapi.ModuleTester, moduleSecret httpapi.ModuleSecretWriter, gateMargins httpapi.GateMarginReader, gateMarginEpsilon float64, policyTrace httpapi.PolicyTracer, rulesetWrite httpapi.RulesetWriter) *auth.Router {
	api := auth.NewRouter(verifier)
	// TG-371 item 4: the AUTH layer turns away an ingest push (wrong/absent bearer) BEFORE the handler, so
	// the handler's IngestRefused counter never sees it. Point the auth router's observer at the SAME register,
	// so an auth refusal and a handler refusal land in one tg_ingest_refused_total series (reason="auth" here).
	api.SetIngestRejectObserver(ingestRefusalCounter.record)
	// /v1/whoami is registered by httpapi.Register (it now homes there so the served surface matches the
	// generated contract, INV-15) — no inline route here. The Stats reader reports the WORKER's published
	// mutation posture (posture + postureStaleAfter), never the grounder's own read-only gate.
	httpapi.Register(api, httpapi.Deps{
		// TG-371: the front door tallies what it TURNS AWAY, not only what it admits. Wired through the
		// package-level register rather than a new buildPublicAPI parameter — that signature is already
		// the longest in the tree and a test parses it.
		IngestRefused:            ingestRefusalCounter.record,
		IngestPredrop:            ingestPredropCounter.record, // TG-380: accepted-but-no-triage drops (recovery)
		Stats:                    gateStats{gate: gate, pending: pending, posture: posture, staleAfter: postureStaleAfter},
		PendingDecisions:         pending,
		Config:                   cfgResolver,
		Snapshots:                noSnapshots{},
		Starter:                  noStarter{},
		Ledger:                   ledger,
		Ingesters:                ingesters,
		Triage:                   triage,
		Capabilities:             capabilities,
		Sessions:                 sessions,
		SessionsRead:             sessionsRead,
		SessionDetailRead:        sessionDetailRead,
		CommitConfirmChip:        commitConfirmChipRead,
		CredentialOnboardingRead: credentialOnboarding,
		// The citation's payload (TG-272). Wired at the SAME call as the walk it belongs to — this is the exact
		// step TG-251, TG-267 and TG-268 each missed: machinery built, one consumer never re-pointed.
		SessionEvidenceRead: sessionEvidenceRead,
		// The TYPED CLAIM behind the walk (TG-201), wired at the SAME call and for the SAME reason: a store, a
		// route and a renderer can all exist and the operator still sees nothing until this line exists. The
		// reader is the TriageStore — the claim is a column on the terminal triage record, so the operator reads
		// the same bytes the judge's diagnosis_grounded dimension was scored from, not a second copy of them.
		SessionDiagnosisRead: sessionDiagnosisRead,
		GateMargins:          gateMargins,
		GateMarginEpsilon:    gateMarginEpsilon,
		Alerts:               alerts,
		Suppression:          suppression,
		Actions:              actions,
		Proposals:            proposalsRead,
		// Explicit parameter rather than a type assertion on proposalsRead: an assertion that failed
		// would yield nil, the headline would simply not render, and nothing would say why. A parameter
		// makes the omission visible at the call site.
		Counterfactual: counterfactualRead,
		Manifest:       manifestRead,
		ManifestWrite:  manifestWrite,
		// spec/028 T-028-7: the earned op-class review surface and its closed-verb write lane. Both nil in
		// every test that does not exercise them, which is the fail-closed 503 the router documents.
		OpClass:       opClassRead,
		OpClassWrite:  opClassWrite,
		Transitions:   transitions,
		Governance:    governance,
		SecretsRead:   secretsRead,
		Models:        models,
		Contract:      contract,
		Estate:        estate,
		Grounding:     grounding,
		Votes:         votes,
		Skills:        skillsRead,
		SkillsWrite:   skillsWrite,
		Wiki:          wiki,
		AdminSessions: adminSessions,
		ConfigWrite:   configWrite,
		// The per-module configuration surface. ModuleSecret is deliberately LEFT NIL until the OpenBao
		// writer credential is provisioned (deploy/openbao/README.md): the route then fails closed to 503
		// with "module secret path unavailable", which is the honest state. Wiring it to a credential that
		// can read every secret would be the easy shortcut and the wrong one.
		ModuleSchema: moduleSchema,
		ModuleTest:   moduleTest,
		ModuleSecret: moduleSecret,
		SecretsWrite: secretsWrite,
		SealRewrap:   sealRewrap,
		SealedRead:   sealedRead,
		Credentials:  credentials,
		Policy:       policyRead,
		// The policy packet-tracer (TG-105): nil unless the Temporal client is wired (the trace runs in the
		// worker), which is the same fail-closed 503 the router documents for an unwired backend.
		Tracer:         policyTrace,
		Regime:         regimeRead,
		ModeTransition: modeTransition,
		EngineToggle:   engineToggle,
		// The active-ruleset write lane (TG-104): nil unless the Temporal client is wired (the write runs in
		// the worker), the same fail-closed 503 the router documents for an unwired backend.
		RulesetWrite: rulesetWrite,
		// Operator-facing MANUAL ROLLBACK (TG-462): wired via a package var (grounderRollback) rather than a new
		// buildPublicAPI parameter, matching the ingest-counter pattern — the signature is already the longest in
		// the tree. nil until the Temporal client is wired ⇒ POST /v1/actions/{action_id}/rollback is 503.
		Rollback: grounderRollback,
		// Credential "Sync now" (TG-109): package-var wiring, same positional-rebind rationale as above.
		CredentialSync: grounderCredentialSync,
		// The DB-backed native rule mapping (TG-109): the trace-read row list + the admin write lane,
		// package-var wiring for the same reason. Reads serve without Temporal; writes 503 without it.
		CredentialNativeRead: grounderNativeRules,
		NativeRuleWrite:      grounderNativeRuleWrite,
		ObjectGroupRead:      grounderObjectGroups,
		ObjectGroupWrite:     grounderObjectGroupWrite,
		// Benchmark-axis scoreboard (TG-480): package-var wiring, same rationale.
		Axes: grounderAxes,
	})
	return api
}

// temporalVotes signals the human decision to the waiting Runner workflow keyed by external_ref (the
// decision id) carrying the sealed action_id the vote binds (INV-12 — the Runner accepts it only when it
// names its gated action). Error mapping is HONEST: only a genuinely absent/closed workflow maps to the
// closed-decision-window sentinel (409 at the surface); a transient Temporal failure surfaces as
// retryable (503), never disguised as a closed window.
type temporalVotes struct{ c client.Client }

func (v temporalVotes) SignalVote(ctx context.Context, externalRef, actionID string, approve bool, voter string) error {
	err := v.c.SignalWorkflow(ctx, tg.WorkflowID(externalRef), "", runner.VoteSignalName,
		runner.VoteSignal{Approve: approve, Voter: voter, ActionID: actionID})
	if err == nil {
		return nil
	}
	var notFound *serviceerror.NotFound
	if errors.As(err, &notFound) {
		return httpapi.ErrNoWaitingDecision
	}
	return err
}

// registryIngesters resolves an ingest source type to its normalizing ingester through the module registry —
// so the alert front door can accept a payload only from a declared, enabled ingest capability (INV-17). It
// is the first hot-path consumer of registry-backed resolution.
type registryIngesters struct{ reg *modules.Registry }

func (ri registryIngesters) ResolveIngester(sourceType string) (ingestadapter.Ingester, error) {
	return resolve.Ingester(ri.reg, sourceType)
}

// temporalTriage mints the read-only Runner triage session for a normalized envelope. The workflow id is
// keyed by external_ref with reject-duplicate semantics, so a re-fire of an in-flight incident is idempotent
// — an "already started" is treated as success (the incident is already being triaged), not a failure. The
// Runner it starts drives to a gated proposal and stops; mutation stays OFF.
type temporalTriage struct{ c client.Client }

func (t temporalTriage) StartTriage(ctx context.Context, env coreingest.IncidentEnvelope) (string, error) {
	run, err := t.c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                    tg.WorkflowID(env.ExternalRef),
		TaskQueue:             tg.TaskQueueRunner,
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
	}, runner.RunnerWorkflow, env)
	if err != nil {
		var already *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &already) {
			// A re-fire of an incident whose triage is still in flight: no NEW session is minted, the
			// caller gets the existing id. TG-380 counts it — this is the pre-admission drop the pve03
			// cascade could not measure ("the upstream sent more than we triaged").
			ingestPredropCounter.record(predropRejectDuplicate)
			return tg.WorkflowID(env.ExternalRef), nil
		}
		return "", err
	}
	return run.GetID(), nil
}

// ledgerReadStore adapts the durable, hash-chained governance ledger store to the console's read surface.
// It returns the tail (most recent `limit`) of the chain in write order. Any authenticated principal may
// read the org-global audit spine — it is the deployment's single accountability record, not per-source
// data; a stricter role gate can narrow this later without changing the interface.
// sessionsReadStore adapts the durable audit-spine sessions reader (session_risk_audit ⋈ action_verdict)
// to the console's read surface (REQ-509). Signals decode from the stored jsonb; a decode failure yields
// an empty map, never an invented one.
// posturePeek is the narrow read the grounder needs over the WORKER's published runtime posture (satisfied
// by *db.PostureReadStore). Interface-typed so gateStats/governanceReader stay oracle-testable with an
// in-memory fake — CI has no Postgres, and the whole point is that the grounder no longer reads its own gate.
type posturePeek interface {
	Latest(ctx context.Context, component string) (db.PostureRow, error)
}

// postureView is the resolved actuation posture the console should see: the owner-set mode ("" = unknown),
// the derived may_actuate, plus whether the reading is fresh and where it came from.
type postureView struct {
	Mode             string
	MayActuate       bool
	EffectCapability string
	Stale            bool
	Source           string // worker | worker-stale | grounder-gate
}

// resolvePosture resolves the actuation posture the console should see from the WORKER's published row,
// never the grounder's own gate (which is read-only by construction — it refuses to boot able to actuate).
// It is the honest, fail-safe resolver:
//   - a FRESH worker row is authoritative (source "worker", not stale);
//   - a STALE row (older than staleAfter) still reports the freshest mode/may_actuate it holds but is flagged
//     stale (source "worker-stale") so the console shows "unknown" instead of a reading it cannot vouch for;
//   - NO row / no reader / a read error falls back to the grounder gate — always false, mode unknown (the
//     grounder holds no mode authority) — but STILL flagged stale (source "grounder-gate"), so a heartbeat
//     gap can NEVER advertise a false OFF for a live-ON worker.
//
// A non-positive staleAfter disables the age check (a found row is always treated fresh) — used by tests.
func resolvePosture(ctx context.Context, reader posturePeek, gate *safety.Chokepoint, staleAfter time.Duration) postureView {
	v := postureView{Mode: "", MayActuate: gate.MayActuate(), Source: "grounder-gate", Stale: true}
	if reader == nil {
		return v
	}
	// WHICH PLANE'S POSTURE (TG-112). Both worker processes used to publish the literal "worker", so the
	// two planes shared one runtime_posture row and whichever heartbeated last won — the ACTUATION plane,
	// the only one that can mutate the estate, was unrepresentable. The worker now publishes plane-
	// qualified (cmd/worker.PostureComponent).
	//
	// The ACTUATION row is preferred because this value answers "can this system mutate the estate", and
	// the triage plane's answer to that is irrelevant: it holds no actuation credential and registers no
	// effect leaf regardless of what its own chokepoint says. Falling back to the bare "worker" key keeps
	// a single-process (plane=both) deployment working unchanged, and keeps a mid-rollout deployment —
	// old worker, new grounder — reporting a real posture instead of dropping to unknown.
	row, err := reader.Latest(ctx, "worker-actuation")
	if err != nil || !row.Found {
		row, err = reader.Latest(ctx, "worker")
	}
	if err != nil || !row.Found {
		return v // no fresh worker posture — keep the fail-safe unknown fallback (never a confident false)
	}
	v.Mode = row.Mode // prefer the freshest reading we actually have
	v.MayActuate = row.MayActuate
	v.EffectCapability = row.EffectCapability
	if staleAfter > 0 && time.Since(row.UpdatedAt) > staleAfter {
		v.Source, v.Stale = "worker-stale", true
		return v
	}
	v.Source, v.Stale = "worker", false
	return v
}

// governanceReader assembles the live safety posture (REQ-511) from the authoritative components: the
// WORKER's published actuation posture (mode + may_actuate, read across the process boundary via posture
// with a staleness→unknown fallback — the grounder's own gate is read-only by construction, so it is NOT
// the posture authority), the audit spine's band distribution, and the ledger chain head. preflight_green
// stays the local gate's own bit.
type governanceReader struct {
	gate       *safety.Chokepoint
	sessions   *db.SessionReadStore
	ledger     *db.LedgerStore
	posture    posturePeek
	staleAfter time.Duration
}

func (g governanceReader) Governance(ctx context.Context, _ auth.Principal) (httpapi.GovernanceState, error) {
	bands, err := g.sessions.BandCounts(ctx)
	if err != nil {
		return httpapi.GovernanceState{}, err
	}
	seq, head, err := g.ledger.Tail(ctx)
	if err != nil {
		return httpapi.GovernanceState{}, err
	}
	pv := resolvePosture(ctx, g.posture, g.gate, g.staleAfter)
	return httpapi.GovernanceState{
		Mode:             pv.Mode,
		MayActuate:       pv.MayActuate,
		PreflightGreen:   g.gate.IsPreflightGreen(),
		Bands:            bands,
		Chain:            httpapi.ChainHead{Seq: seq, Hash: head},
		EffectCapability: pv.EffectCapability,
		PostureStale:     pv.Stale,
		PostureSource:    pv.Source,
	}, nil
}

// configSecrets lists the control plane's configured secret REFERENCES (REQ-512) — resolution is
// probed per request and the value is discarded immediately; the response type cannot carry one.
type configSecrets struct{ cfg envConfig }

func (c configSecrets) SecretRefs(_ context.Context, _ auth.Principal) ([]httpapi.SecretRefStatus, error) {
	probe := func(r config.SecretRef) bool { _, err := r.Resolve(); return err == nil }
	return []httpapi.SecretRefStatus{
		{Ref: string(c.cfg.SessionKeyRef), Purpose: "session cookie signing key", Resolved: probe(c.cfg.SessionKeyRef)},
		{Ref: string(c.cfg.OperatorTokenRef), Purpose: "operator login token", Resolved: probe(c.cfg.OperatorTokenRef)},
		{Ref: string(c.cfg.LiteLLMKeyRef), Purpose: "litellm gateway key", Resolved: probe(c.cfg.LiteLLMKeyRef)},
	}, nil
}

// estateReadStore adapts the durable estate snapshot reader to the console read surface (REQ-516).
// It maps the graph projection to the console DTO; no snapshot yet yields available=false.
type estateReadStore struct{ s *db.EstateReadStore }

func (r estateReadStore) LatestEstate(ctx context.Context, _ auth.Principal) (httpapi.EstateSnapshot, error) {
	row, err := r.s.Latest(ctx)
	if err != nil {
		return httpapi.EstateSnapshot{}, err
	}
	if !row.Found {
		return httpapi.EstateSnapshot{Available: false}, nil
	}
	out := httpapi.EstateSnapshot{
		Available: true, CapturedAt: row.CapturedAt,
		NodeCount: row.NodeCount, EdgeCount: row.EdgeCount, SourceCount: row.SourceCount,
	}
	for _, n := range row.Graph.Nodes {
		out.Nodes = append(out.Nodes, httpapi.EstateNode{Name: n.Name, Type: string(n.Type)})
	}
	for _, e := range row.Graph.Edges {
		out.Edges = append(out.Edges, httpapi.EstateEdge{
			From: e.FromName, To: e.ToName, Rel: e.Rel, Confidence: e.Confidence, Source: e.Source,
		})
	}
	return out, nil
}

// groundingReadStore adapts the pgx grounding aggregator to the console read surface (REQ-517). The raw
// counts come from three bound aggregate queries; the DERIVED rates (match-rate, precision/recall, the
// falsifiability signal ratio) are computed here in Go — a division whose divisor is checked, so an empty
// spine reports honest zeros, never a fabricated or NaN rate.
type groundingReadStore struct{ s *db.GroundingReadStore }

func (r groundingReadStore) Grounding(ctx context.Context, _ auth.Principal) (httpapi.GroundingScorecard, error) {
	agg, err := r.s.Aggregate(ctx)
	if err != nil {
		return httpapi.GroundingScorecard{}, err
	}
	return scorecardFrom(agg), nil
}

// scorecardFrom is the DERIVATION, split out from the pgx read so it is reachable without a database. It
// was inline for as long as the floored-denominator bug lived here, and the bug survived precisely because
// nothing could call the arithmetic on its own: the only way to observe SignalRatio was to run the whole
// stack against a populated spine and read the console. A derivation that needs a database to test is a
// derivation nobody tests.
func scorecardFrom(agg db.GroundingAgg) httpapi.GroundingScorecard {
	sc := httpapi.GroundingScorecard{
		Verdicts:    agg.Verdicts,
		Predictions: agg.Predictions,
		Bands:       agg.Bands,
		FloorHolds:  agg.Bands["POLL_PAUSE"],
	}
	for _, n := range agg.Verdicts {
		sc.VerdictTotal += n
	}
	if sc.VerdictTotal > 0 {
		sc.MatchRate = float64(agg.Verdicts["match"]) / float64(sc.VerdictTotal)
	}
	if agg.Predictions > 0 {
		sc.AvgRealTP = float64(agg.SumTP) / float64(agg.Predictions)
		sc.AvgControlTP = float64(agg.SumControlTP) / float64(agg.Predictions)
		// Over-prediction rate: mean blast-radius false-positives per scored prediction. Unlike precision, a
		// correctly-restrained (fp=0) prediction lowers this, so the sibling-gate's calibration is visible here.
		sc.AvgFalsePositives = float64(agg.SumFP) / float64(agg.Predictions)
		// Signal ratio: real vs shuffled-graph control. Divide by the control AS MEASURED. The old floor of 1
		// was borrowed from core/predict.Ratio(), where the operand is a per-prediction integer COUNT and 0/1
		// are adjacent; here both operands are MEANS that normally sit in (0,1), so the floor silently
		// rewrote every result — live, 0.4019/0.2305 = 1.744 was published as 0.40 and captioned "at or
		// below chance". A genuinely silent control is reported as silence, not as a denominator of 1.
		if sc.AvgControlTP > 0 {
			sc.SignalRatio = sc.AvgRealTP / sc.AvgControlTP
		} else {
			sc.SignalRatio = 0
			sc.ControlSilent = sc.AvgRealTP > 0
		}
	}
	// The rolling twin (TG-92) — same arithmetic, recent rows only, so a fixed predictor can actually show
	// as fixed. Deliberately a separate block rather than a loop over the two: the all-time figures are an
	// audit record and must keep their exact meaning even if this derivation later changes.
	sc.RecentPredictions = agg.RecentPredictions
	sc.RecentWindow = agg.RecentWindow
	if agg.RecentPredictions > 0 {
		sc.RecentAvgRealTP = float64(agg.RecentSumTP) / float64(agg.RecentPredictions)
		sc.RecentAvgControlTP = float64(agg.RecentSumControlTP) / float64(agg.RecentPredictions)
		sc.RecentAvgFalsePositives = float64(agg.RecentSumFP) / float64(agg.RecentPredictions)
		if sc.RecentAvgControlTP > 0 {
			sc.RecentSignalRatio = sc.RecentAvgRealTP / sc.RecentAvgControlTP
		} else {
			sc.RecentSignalRatio = 0
			sc.RecentControlSilent = sc.RecentAvgRealTP > 0
		}
	}
	if denom := agg.SumTP + agg.SumFP; denom > 0 {
		sc.Precision = float64(agg.SumTP) / float64(denom)
	}
	if denom := agg.SumTP + agg.SumFN; denom > 0 {
		sc.Recall = float64(agg.SumTP) / float64(denom)
	}
	return sc
}

// credentialsReadStore adapts the pgx credential read projections to the console read surface (REQ-526).
// It maps the raw db rows to the non-secret console DTOs — formatting timestamps and turning the coverage
// maps into deterministically-ordered slices. It NEVER shapes a secret field: the db rows carry only the
// non-secret columns the 0017/0018 tables hold, and the DTOs have no field that could receive one (INV-13).
type credentialsReadStore struct{ s *db.CredentialReadStore }

func (r credentialsReadStore) CredentialSources(ctx context.Context, _ auth.Principal) ([]httpapi.CredentialSource, error) {
	rows, err := r.s.Sources(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]httpapi.CredentialSource, 0, len(rows))
	for _, row := range rows {
		src := httpapi.CredentialSource{
			SourceID: row.SourceID, Plane: row.Plane,
			Added: row.Added, Changed: row.Changed, Removed: row.Removed,
			Drifted: row.Added+row.Changed+row.Removed > 0, CoveredTargets: row.CoveredTargets,
			Outcome: row.Outcome, Err: row.Err,
			Precedence: row.Precedence, // published rank (TG-109); 0 = pre-0087 projection, honestly absent
		}
		if row.LastSyncedAt != nil {
			src.LastSyncedAt = row.LastSyncedAt.UTC().Format(time.RFC3339)
		}
		out = append(out, src)
	}
	return out, nil
}

func (r credentialsReadStore) CredentialResolutions(ctx context.Context, _ auth.Principal, target string, limit int) ([]httpapi.CredentialResolution, error) {
	rows, err := r.s.Resolutions(ctx, target, limit)
	if err != nil {
		return nil, err
	}
	out := make([]httpapi.CredentialResolution, 0, len(rows))
	for _, row := range rows {
		out = append(out, httpapi.CredentialResolution{
			Target: row.Target, Plane: row.Plane, Outcome: row.Outcome, Source: row.Source,
			Native: row.Native, RuleID: row.RuleID, ResolvedUser: row.ResolvedUser, Scheme: row.Scheme,
			KeyRefScheme: row.KeyRefScheme, Shadowed: row.Shadowed, Err: row.Err,
			CreatedAt: row.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

func (r credentialsReadStore) CredentialCoverage(ctx context.Context, _ auth.Principal) (httpapi.CredentialCoverage, error) {
	agg, err := r.s.Coverage(ctx)
	if err != nil {
		return httpapi.CredentialCoverage{}, err
	}
	cov := httpapi.CredentialCoverage{
		WindowDays: agg.WindowDays,
		ByPlane:    credentialTallies(agg.ByPlane),
		BySource:   credentialTallies(agg.BySource),
	}
	for _, t := range agg.RecentResolved {
		cov.RecentResolved = append(cov.RecentResolved, httpapi.CredentialTargetOutcome{
			Target: t.Target, Outcome: t.Outcome, Source: t.Source, CreatedAt: t.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	for _, t := range agg.RecentRefused {
		cov.RecentRefused = append(cov.RecentRefused, httpapi.CredentialTargetOutcome{
			Target: t.Target, Outcome: t.Outcome, Source: t.Source, CreatedAt: t.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return cov, nil
}

// credentialTallies flattens an outcome-tally map into a key-sorted DTO slice (deterministic ordering).
func credentialTallies(m map[string]db.CredentialOutcomeTally) []httpapi.CredentialOutcomeCounts {
	out := make([]httpapi.CredentialOutcomeCounts, 0, len(m))
	for k, t := range m {
		out = append(out, httpapi.CredentialOutcomeCounts{
			Key: k, Resolved: t.Resolved, Unresolved: t.Unresolved, Ambiguous: t.Ambiguous, Total: t.Total(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// policyReadStore adapts the pgx policy read projections to the console read surface (spec/015 T-015-12). It
// maps the raw db rows to the non-secret console DTOs — formatting timestamps, deriving the mode's HONEST
// posture from the mode itself (fail-closed Shadow when no mode is persisted), and projecting the stored
// rules-as-data document to per-rule DTOs. It NEVER shapes a secret field: the db rows carry only the
// non-secret columns the 0019 tables hold, and the DTOs have no field that could receive one (INV-13).
type policyReadStore struct{ s *db.PolicyReadStore }

func (r policyReadStore) PolicyDecisions(ctx context.Context, _ auth.Principal, limit int) ([]httpapi.PolicyDecision, error) {
	rows, err := r.s.Decisions(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]httpapi.PolicyDecision, 0, len(rows))
	for _, row := range rows {
		out = append(out, httpapi.PolicyDecision{
			RuleID: row.RuleID, Verdict: row.Verdict, BandMode: row.BandMode, ComposedBand: row.ComposedBand,
			MinConfidence: row.MinConfidence, ActionID: row.ActionID, PlanHash: row.PlanHash,
			Principal: row.Principal, Mode: row.Mode, CreatedAt: row.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

func (r policyReadStore) PolicyMode(ctx context.Context, _ auth.Principal) (httpapi.PolicyMode, error) {
	name, present, err := r.s.Mode(ctx)
	if err != nil {
		return httpapi.PolicyMode{}, err
	}
	// Resolve the persisted mode fail-closed: an absent/corrupt spelling resolves to Shadow (read-only),
	// never a fabricated actuating mode. The posture is derived from the mode itself (the single source of
	// truth for "may this actuate?").
	m, perr := policy.ParseMode(name)
	if !present || perr != nil {
		m = policy.ModeShadow
	}
	return httpapi.PolicyMode{
		Mode:              m.String(),
		Persisted:         present && perr == nil,
		MayAutoActuate:    m.MayAutoActuate(),
		RequiresHumanVote: m.RequiresHumanVote(),
		Posture:           policyModePosture(m, present && perr == nil),
	}, nil
}

// policyModePosture renders the honest one-line posture for a mode: whether it may auto-actuate, routes to a
// human, or only suggests, and whether the mode was actually persisted (an unpersisted mode is the
// fail-closed Shadow default, called out so the console never reads absence as a deliberate read-only choice).
func policyModePosture(m policy.Mode, persisted bool) string {
	var base string
	switch {
	case m.MayAutoActuate():
		base = "auto-actuation permitted for graduated op-classes (mutation still gated by the actuation chokepoint)"
	case m.RequiresHumanVote():
		base = "every candidate action routes to a human vote (engine off)"
	default:
		base = "read-only: suggest and record only, never actuate"
	}
	if !persisted {
		return base + " — no mode persisted; fail-closed to Shadow"
	}
	return base
}

func (r policyReadStore) PolicyGraduation(ctx context.Context, _ auth.Principal) ([]httpapi.PolicyGraduationClass, error) {
	rows, err := r.s.Graduation(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]httpapi.PolicyGraduationClass, 0, len(rows))
	for _, row := range rows {
		out = append(out, httpapi.PolicyGraduationClass{
			OpClass: row.OpClass, Level: row.Level, CleanRunCount: row.CleanRunCount,
			LastOutcome: row.LastOutcome, UpdatedAt: row.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

func (r policyReadStore) PolicyRules(ctx context.Context, _ auth.Principal) (httpapi.PolicyRulesPage, error) {
	row, err := r.s.Ruleset(ctx)
	if err != nil {
		return httpapi.PolicyRulesPage{}, err
	}
	page := httpapi.PolicyRulesPage{Present: row.Present, RuleCount: row.RuleCount, UpdatedBy: row.UpdatedBy}
	if !row.Present {
		return page, nil
	}
	page.UpdatedAt = row.UpdatedAt.UTC().Format(time.RFC3339)
	// Parse the stored rules-as-data document and project each rule to the non-secret DTO. A document that no
	// longer parses is reported as present-but-empty rather than leaking a raw blob — the rule_count metadata
	// still reflects what was stored (honest, never fabricated).
	rs, perr := policy.ParseRuleSet(row.Document)
	if perr != nil {
		return page, nil
	}
	// The bundle_version the editor pins as expected_version for a compare-and-swap write (TG-104 slice-2):
	// the SAME canonical fingerprint every decision is stamped with (policy.BundleVersion), computed here
	// from the freshly-parsed active ruleset — a call INTO core/policy, never a second canonicalization.
	page.Version = policy.BundleVersion(rs)
	// Whether the active ruleset carries a non-empty top-level default the read surface cannot project. The
	// editor fails closed on it rather than silently dropping it on a round-trip write (fail-closed, INV-09).
	page.HasDefault = rulesetHasTopLevelDefault(rs)
	for _, rule := range rs.Rules {
		page.Rules = append(page.Rules, projectPolicyRule(rule))
	}
	return page, nil
}

// rulesetHasTopLevelDefault reports whether rs carries a NON-EMPTY top-level default params block
// (RuleSet.Default). It reads the exported policy.Params fields only — a call INTO core/policy, not a
// modification of it. A default is "empty" exactly when every tunable is unset (the zero Params the parser
// yields for an absent `default` block), so an all-unset default reads false and a default that sets any of
// min_confidence / band_mode / rate_limit reads true. The read surface projects PolicyRulesPage.HasDefault
// from this so the console rule editor can refuse a ruleset whose default it cannot represent (TG-104 slice-2).
func rulesetHasTopLevelDefault(rs policy.RuleSet) bool {
	d := rs.Default
	return d.MinConfidence != nil || d.BandMode != "" || d.RateLimit != nil
}

// projectPolicyRule maps one policy.Rule to the non-secret console DTO (rules-as-data). The estate selector is
// rendered as a single "kind:pattern" string; params are carried through as-is (all non-secret).
func projectPolicyRule(rule policy.Rule) httpapi.PolicyRule {
	out := httpapi.PolicyRule{
		ID:            rule.ID,
		Verdict:       string(rule.Verdict),
		MinConfidence: rule.Params.MinConfidence,
		BandMode:      string(rule.Params.BandMode),
		RateLimit:     rule.Params.RateLimit,
		ApproveBy:     rule.ApproveBy,
		IsDefault:     rule.IsDefault,
		Match: httpapi.PolicyRuleMatch{
			OpClass:     rule.Match.OpClass,
			ArgvPattern: rule.Match.ArgvPattern,
			Territory:   rule.Match.Territory,
			Reversible:  rule.Match.Reversible,
			InverseOnly: rule.Match.InverseOnly,
		},
	}
	if rule.Match.Selector != nil {
		out.Match.Selector = string(rule.Match.Selector.Kind) + ":" + rule.Match.Selector.Pattern
	}
	return out
}

// regimeReadStore adapts the pgx regime read projections (migration 0020) to the console read surface
// (spec/017 T-017-7, REQ-1716). It maps the raw db rows to the non-secret console DTOs — formatting
// timestamps only. It NEVER shapes a secret field: the db rows carry only the non-secret columns the 0020
// tables hold (token_ref is a SecretRef REFERENCE, never a value), and the DTOs have no field that could
// receive one (INV-13).
type regimeReadStore struct{ s *db.RegimeReadStore }

func (r regimeReadStore) RegimeResolutions(ctx context.Context, _ auth.Principal, limit int) ([]httpapi.RegimeResolution, error) {
	rows, err := r.s.Resolutions(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]httpapi.RegimeResolution, 0, len(rows))
	for _, row := range rows {
		out = append(out, httpapi.RegimeResolution{
			Target: row.Target, Regime: row.Regime, Lane: row.Lane, RuleID: row.RuleID,
			Outcome: row.Outcome, CreatedAt: row.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

func (r regimeReadStore) RegimeActuations(ctx context.Context, _ auth.Principal, limit int) ([]httpapi.RegimeActuation, error) {
	rows, err := r.s.Actuations(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]httpapi.RegimeActuation, 0, len(rows))
	for _, row := range rows {
		out = append(out, httpapi.RegimeActuation{
			ActionID: row.ActionID, Lane: row.Lane, JobTemplateID: row.JobTemplateID, OpClass: row.OpClass,
			JobID: row.JobID, TokenRef: row.TokenRef, CreatedAt: row.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

func (r regimeReadStore) RegimeDeferredVerdicts(ctx context.Context, _ auth.Principal, limit int) ([]httpapi.RegimeDeferredVerdict, error) {
	rows, err := r.s.DeferredVerdicts(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]httpapi.RegimeDeferredVerdict, 0, len(rows))
	for _, row := range rows {
		out = append(out, httpapi.RegimeDeferredVerdict{
			ActionID: row.ActionID, JobID: row.JobID, Status: row.Status, Verdict: row.Verdict,
			Graduation: row.Graduation, CreatedAt: row.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

func (r regimeReadStore) RegimeLaneCoverage(ctx context.Context, _ auth.Principal) ([]httpapi.RegimeLaneCoverage, error) {
	rows, err := r.s.LaneCoverage(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]httpapi.RegimeLaneCoverage, 0, len(rows))
	for _, row := range rows {
		out = append(out, httpapi.RegimeLaneCoverage{
			Lane: row.Lane, Resolutions: row.Resolutions, Actuations: row.Actuations,
		})
	}
	return out, nil
}

type sessionsReadStore struct{ s *db.SessionReadStore }

// SessionCount reports the spine's population so a bounded page never has to be mistaken for one.
func (r sessionsReadStore) SessionCount(ctx context.Context, _ auth.Principal) (int, error) {
	return r.s.Count(ctx)
}

func (r sessionsReadStore) RecentSessions(ctx context.Context, _ auth.Principal, limit int) ([]httpapi.SessionSummary, error) {
	rows, err := r.s.Recent(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]httpapi.SessionSummary, 0, len(rows))
	for _, row := range rows {
		var signals map[string]string
		_ = json.Unmarshal(row.SignalsJSON, &signals)
		out = append(out, httpapi.SessionSummary{
			ExternalRef:      row.ExternalRef,
			Host:             row.Host,
			Band:             row.Band,
			RiskLevel:        row.RiskLevel,
			ActionID:         row.ActionID,
			PlanHash:         row.PlanHash,
			AutoApproved:     row.AutoApproved,
			NotifyRequired:   row.NotifyRequired,
			OperatorOverride: row.OperatorOverride,
			Signals:          signals,
			Verdict:          row.Verdict,
			ClassifiedAt:     row.CreatedAt,
		})
	}
	return out, nil
}

// sessionDetailReadStore adapts the pgx trace-spine store to httpapi.SessionDetailReader: it loads the
// durable spine for one external_ref and runs the pure trace.Assemble to stitch the ordered walk (spec/020
// REQ-2011). Observe-only — a read plus a pure function, reaching no actuator.
type sessionDetailReadStore struct{ s *db.TraceSpineStore }

func (r sessionDetailReadStore) SessionDetail(ctx context.Context, _ auth.Principal, ref string) (trace.SessionTrace, error) {
	rec, err := r.s.Load(ctx, ref)
	if err != nil {
		return trace.SessionTrace{}, err
	}
	return trace.Assemble(ref, rec), nil
}

type ledgerReadStore struct{ s *db.LedgerStore }

func (l ledgerReadStore) Recent(ctx context.Context, _ auth.Principal, limit int) ([]audit.LedgerEntry, error) {
	all, err := l.s.All(ctx)
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(all) > limit {
		all = all[len(all)-limit:]
	}
	return all, nil
}

// gateStats is the production StatsReader: it reflects the authoritative control-plane posture the console
// reads but never computes. mode + may_actuate are the WORKER's published live actuation posture (the
// authoritative chokepoint lives in the worker process — the grounder's own gate is read-only by
// construction, so it is NOT the authority), read across the process boundary via posture with a
// staleness→unknown fallback, so the surface can never advertise a false OFF for a live-ON worker. Session
// and poll counters are read-only
// observations wired as they land; until then they report zero rather than a fabricated number — the console
// renders a real, if empty, deck instead of demo data.
type gateStats struct {
	gate       *safety.Chokepoint
	pending    persist.PendingReader // counts open POLL_PAUSE decisions for /v1/stats pending_polls (nil ⇒ 0)
	posture    posturePeek           // the worker's published posture (nil ⇒ the grounder-gate fallback, flagged unknown)
	staleAfter time.Duration         // a worker row older than this reads as stale/unknown
}

func (g gateStats) Stats(ctx context.Context, _ auth.Principal) (httpapi.Stats, error) {
	pending := 0
	if g.pending != nil {
		if n, err := g.pending.CountOpen(ctx); err == nil {
			pending = n
		}
	}
	pv := resolvePosture(ctx, g.posture, g.gate, g.staleAfter)
	return httpapi.Stats{
		Mode:          pv.Mode,
		MayActuate:    pv.MayActuate,
		OpenSessions:  0,
		PendingPolls:  pending,
		PostureStale:  pv.Stale,
		PostureSource: pv.Source,
	}, nil
}

// noSnapshots is the honest default SnapshotStore for a deployment with no replayable snapshot store
// wired yet: every lookup returns found=false, so a session-replay request resolves to 404 exactly as an
// unknown-or-unauthorized id does (REQ-504) — never a nil-dereference, never a fabricated snapshot. It is
// replaced by the durable store once session snapshots are persisted.
type noSnapshots struct{}

func (noSnapshots) Get(_ context.Context, _ string, _ auth.Principal) (httpapi.ContextSnapshot, bool, error) {
	return httpapi.ContextSnapshot{}, false, nil
}

// noStarter is the matching no-op WorkflowStarter. Because noSnapshots always returns found=false, the
// replay handler short-circuits at the 404 before a start is ever attempted, so this is never invoked in
// the default wiring; it exists so Deps is fully populated (no nil field) and the mount cannot panic.
type noStarter struct{}

func (noStarter) StartFromSnapshot(_ context.Context, _ httpapi.ContextSnapshot) (string, error) {
	return "", context.Canceled
}

// suppressionShadow is the production SuppressionObserver: for each accepted alert it reads that key's
// durable raise/clear history and records what incident-scoped suppression WOULD have decided.
//
// IT SUPPRESSES NOTHING. Measured on this estate, 73.3% of organic alerts are repeats (400 alerts across
// 107 keys over 7 days, worst key 25 fires), so the upside is real — but a suppressor that is wrong in the
// other direction produces an incident nobody sees, and a naive time-windowed design would additionally
// collapse A1 by dropping the first alert of every re-injection. So the judgement runs in shadow and earns
// its way in on live evidence, which is the same discipline TG applies to its own actuations: commit the
// prediction, then score it.
//
// THE IMPLEMENTATION MOVED to core/suppressionshadow so every intake shares ONE of it. It used to be a private
// type here, which is why the two other intakes (the grouped-transport arm and the pve-liveness poller, the
// latter in a DIFFERENT BINARY) could not use it and silently observed nothing.

// credentialOnboardingStore adapts the worker-published binding projection to the console read surface
// (TG-274). The AWX source lives in the WORKER; this process only reads what it published.
type credentialOnboardingStore struct {
	s          *db.CredentialBindingProjectionStore
	staleAfter time.Duration
	now        func() time.Time
}

func (r credentialOnboardingStore) CredentialOnboarding(ctx context.Context, _ auth.Principal) (httpapi.CredentialOnboarding, error) {
	rows, err := r.s.Bindings(ctx)
	if err != nil {
		return httpapi.CredentialOnboarding{}, err
	}
	now := time.Now
	if r.now != nil {
		now = r.now
	}
	cutoff := r.staleAfter
	if cutoff <= 0 {
		cutoff = 3 * time.Minute
	}
	out := httpapi.CredentialOnboarding{}
	srcSeen := map[string]bool{}
	for _, b := range rows {
		// STALE ⇒ ABSENT, the same contract 0051's reader holds. A projection that outlives its publisher
		// serves a remembered answer, and on this screen a remembered "mapped" would tell an operator a
		// credential is armed when the worker that vouched for it is gone.
		if now().Sub(b.ObservedAt) > cutoff {
			continue
		}
		srcSeen[b.SourceID] = true
		d := httpapi.CredentialBindingDTO{
			Source: b.SourceID, Name: b.Credential, Scope: b.Scope, Via: b.Via,
			Hosts: b.Hosts, Mapped: b.Mapped, Ref: b.SecretRef,
		}
		d.Usable = b.Mapped && strings.TrimSpace(b.SecretRef) != ""
		out.Bindings = append(out.Bindings, d)
		out.Total++
		if !d.Usable {
			out.Unmapped++
		}
	}
	for s := range srcSeen {
		out.Sources = append(out.Sources, s)
	}
	sort.Strings(out.Sources)
	return out, nil
}

// ingestRefusalCounter tallies front-door refusals by (source, reason) for /metrics (TG-371). Package
// level because buildPublicAPI's parameter list is already the longest in the tree and
// proposals_read_test.go parses it — threading one more argument through would change that signature for
// a counter that has exactly one producer and one consumer, both in this package.
var ingestRefusalCounter = newIngestRefusals()

// ingestPredropCounter tallies accepted-but-no-triage drops (TG-380), package-level for the same reason as
// ingestRefusalCounter: two producers (the recovery branch via Deps.IngestPredrop, the reject-duplicate
// branch in StartTriage) and one consumer (the /metrics closure), all in this package.
var ingestPredropCounter = newIngestPredrop()
