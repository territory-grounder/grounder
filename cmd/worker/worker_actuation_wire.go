package main

// The ACTUATION REGIME ENGINE wiring (spec/017, TG-110), carved out of main()'s composition root (TG-501
// LOC-debt paydown). wireActuationRegime assembles the regime.Engine that routes each authorized operation to
// its actuation lane (native-ssh / awx-job / gitops-mr / k8s / api) behind the safety chokepoint, wiring the
// per-lane actuators, the async-verify + deferred-verdict graduation feedback, and the AWX job source probe.
// Behaviour is unchanged by the move; the regime_wiring + graduation-async guards pin its bindings.

import (
	_ "time/tzdata" // embed the IANA zoneinfo DB so time.LoadLocation resolves on distroless (no OS tzdata)

	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	actuation "github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/db"
	coreingest "github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/regime"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
	"github.com/territory-grounder/grounder/core/worldmodel"
	"github.com/territory-grounder/grounder/modules/actuation/awxjob"
	"github.com/territory-grounder/grounder/modules/actuation/gitopsmr"
	proxmoxactuation "github.com/territory-grounder/grounder/modules/actuation/proxmox"
	"github.com/territory-grounder/grounder/modules/bootstrap"
)

// wireActuationRegime returns the engine AND, when the async channel is armed, the *regime.AsyncVerify that
// serves as the deferred-verify PRODUCER seam (TG-122 slice 0): the runner Reserves/BindHandles launches on
// it. nil launcher ⇒ no async lane can launch (LaneEffect's structural refusal stands — fail closed).
func wireActuationRegime(chokepoint *safety.Chokepoint, ledger *audit.Ledger, sshLeaf actuation.Actuator, grad *policy.Ladder, pool *db.Pool, modeName string, siteOf verify.SiteAuthority, probeReg *probeRegistry) (*regime.Engine, *regime.AsyncVerify) {
	// (1) Config-not-code regime rules over the shared object-model (REQ-1700/1703). A malformed rule is a
	//     boot refusal — fail closed rather than silently dropping a regime mapping.
	rules, err := parseRegimeRules(getenv("TG_REGIME_RULES", ""))
	if err != nil {
		log.Fatalf("actuation regime: %v (fail closed — a malformed regime rule never routes a target down an undefined channel)", err)
	}

	// (2) native-ssh lane: the EXISTING spec/013 SSH effect leaf re-expressed as one lane among several
	//     (REQ-1700). It is the operator-declared default lane for an unmatched target (REQ-1701) unless
	//     TG_REGIME_DEFAULT_LANE=none disables the default (then an unmatched target refuses, fail closed).
	//     With TG_ACTUATION_SSH_PER_TARGET set (REQ-1717, P3-B2) it becomes a PER-TARGET lane — each action's
	//     leaf binds to its OWN target host rather than a single configured host — else the static leaf.
	// REQ-2704 seams: adopted manifest entries become an allowlist SOURCE, composed as a UNION with the
	// boot-frozen env allowlists (ADR-0016 OQ-2 — never DB-replaces-env, whose failure mode is silent
	// narrowing on first adopt). A nil pool ⇒ nil store ⇒ the providers yield the env grant alone.
	var manifestStore worldmodel.Store
	if pool != nil {
		manifestStore = db.NewWorldManifestStore(pool)
	}
	// PLANE-SCOPED (TG-153): the operator's mutation GRANT is actuation-plane configuration. Withheld on the
	// triage plane, where the union degrades to the empty env grant — the fail-closed direction.
	unitsProvider := worldmodel.NewAllowlistProvider(manifestStore, worldmodel.KindUnit,
		bootstrap.ParseAllowedUnits(planeEnv("TG_ACTUATION_ALLOWED_UNITS", "")))
	containersProvider := worldmodel.NewAllowlistProvider(manifestStore, worldmodel.KindContainer,
		bootstrap.ParseAllowedContainers(planeEnv("TG_ACTUATION_ALLOWED_CONTAINERS", "")))
	sshLane := nativeSSHLaneFor(chokepoint, sshLeaf, unitsProvider, containersProvider)

	// (3) awx-job lane: its REAL effect leaf (modules/actuation/awxjob) is injected ONLY when the operator
	//     declares an AWX base URL AND a DISTINCT launch-capable token (REQ-1706/1708). ABSENT ⇒ the lane
	//     keeps its pendingActuator fail-closed default (it can only REFUSE) — never a permissive default.
	// PLANE-SCOPED (TG-153): TG_AWXJOB_LAUNCH_TOKEN_REF is the SECOND actuation channel — a token that can
	// launch an AWX job template against the estate. Withheld on the triage plane, where this lane therefore
	// keeps its pendingActuator fail-closed default and can only refuse.
	awxBase := strings.TrimSpace(getenv("TG_AWXJOB_BASE_URL", ""))
	awxTokenRef := strings.TrimSpace(planeEnv("TG_AWXJOB_LAUNCH_TOKEN_REF", ""))
	var (
		awxClient   *awxjob.Client
		awxState    = "pendingActuator — FAIL-CLOSED refuse (TG_AWXJOB_BASE_URL / TG_AWXJOB_LAUNCH_TOKEN_REF unset)"
		awxLaneOpts []regime.AWXJobLaneOption
	)
	if awxBase != "" && awxTokenRef != "" {
		allowlist, aerr := parseAWXJobAllowlist(getenv("TG_AWXJOB_ALLOWLIST", ""))
		if aerr != nil {
			log.Fatalf("actuation regime: %v (fail closed)", aerr)
		}
		client, cerr := awxjob.NewClient(awxjob.ClientConfig{
			BaseURL:    awxBase,
			TokenRef:   config.SecretRef(awxTokenRef),
			CACertPath: getenv("TG_AWXJOB_CA", ""),
		})
		if cerr != nil {
			log.Fatalf("actuation regime: awx-job launch client (fail closed): %v", cerr)
		}
		// The mode chokepoint is passed as the actuator's own defense-in-depth gate: ReadOnly() is true and
		// Exec refuses at Shadow BEFORE any network launch, independent of the interceptor's gate.
		actuator, xerr := awxjob.New(awxjob.Config{Client: client, Allowlist: allowlist, ModeGate: chokepoint})
		if xerr != nil {
			log.Fatalf("actuation regime: awx-job actuator (fail closed): %v", xerr)
		}
		awxClient = client
		// The CLIENT is offered, not the actuator: the actuator's only surface method is Exec, which
		// LAUNCHES A JOB. A probe built from it would be an unreviewed actuation triggered from a
		// settings dialog — exactly what temporal/moduletest's package doc forbids.
		probeReg.offer("actuation", awxjob.SourceType, client)
		awxLaneOpts = append(awxLaneOpts, regime.WithAWXActuator(actuator))
		awxState = fmt.Sprintf("real awxjob actuator (read_only=%v, allowlist=%d template(s)) — routes beneath the mode chokepoint (Shadow), inert until the owner-present flip", actuator.ReadOnly(), len(allowlist))
	}
	awxLane := regime.NewAWXJobLane(awxLaneOpts...)

	// (3b) proxmox lane: the PVE hypervisor guest-LIFECYCLE channel (start-guest). Its REAL effect leaf
	//      (modules/actuation/proxmox) is injected ONLY when the operator declares a PVE base URL AND an API
	//      token ref. ABSENT ⇒ the lane keeps its pendingActuator fail-closed default (refuse only). It is
	//      selected by the proxmox-lifecycle effect KIND (not the target regime). The actuator floor-clamps the
	//      lifecycle verb (reboot/shutdown/reset/destroy never auto-execute), allowlists the guest, and re-checks
	//      the mode chokepoint at its own leaf — defense in depth, inert at Shadow.
	pveBase := strings.TrimSpace(getenv("TG_PROXMOX_BASE_URL", ""))
	pveTokenRef := strings.TrimSpace(getenv("TG_PROXMOX_TOKEN_REF", ""))
	var (
		proxmoxState    = "pendingActuator — FAIL-CLOSED refuse (TG_PROXMOX_BASE_URL / TG_PROXMOX_TOKEN_REF unset)"
		proxmoxLaneOpts []regime.ProxmoxLaneOption
	)
	if pveBase != "" && pveTokenRef != "" {
		// REQ-2704: the proxmox guest allowlist is composed the same way, and it now resolves LIVE
		// (TG-232). The provider was already a live resolver — main called it once at boot and passed the
		// frozen slice, so an operator who adopted a guest in the console saw the manifest say "approved"
		// and watched TG decline until the next worker start, with nothing explaining the gap. The
		// provider is handed through intact and consulted per attempt, matching the ssh lane.
		allowedGuests := worldmodel.NewAllowlistProvider(manifestStore, worldmodel.KindGuest,
			splitTokens(getenv("TG_PROXMOX_ALLOWED_GUESTS", "")))
		// A DEDICATED actuation HTTP client (estateHTTPClient is scoped read-only). PVE serves a self-signed cert
		// on :8006, so certificate verification is opt-in-disabled via TG_PROXMOX_INSECURE for the internal
		// endpoint; otherwise system roots apply. A short timeout bounds a stuck launch.
		pveClient := &http.Client{Timeout: 30 * time.Second}
		if truthyEnv("TG_PROXMOX_INSECURE") {
			pveClient.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
		}
		pveActuator := proxmoxactuation.New(pveBase, config.SecretRef(pveTokenRef),
			proxmoxactuation.WithMutationProvider(chokepoint, allowedGuests),
			proxmoxactuation.WithHTTPClient(pveClient))
		proxmoxLaneOpts = append(proxmoxLaneOpts, regime.WithProxmoxActuator(pveActuator))
		// The count is a BOOT-TIME READING of a live list, and says so. Reporting it as though it were the
		// standing value would recreate the confusion this change removes, one layer up.
		proxmoxState = fmt.Sprintf("real proxmox actuator (read_only=%v, allowed_guests=%d at boot, resolved LIVE per attempt) — routes beneath the mode chokepoint, inert until the owner-present flip", pveActuator.ReadOnly(), len(allowedGuests(context.Background())))
	}
	proxmoxLane := regime.NewProxmoxLane(proxmoxLaneOpts...)

	// (4) Build the resolver. Unmatched targets take the operator default lane (native-ssh) unless disabled.
	var engOpts []regime.EngineOption
	defaultLane := strings.ToLower(strings.TrimSpace(getenv("TG_REGIME_DEFAULT_LANE", "native-ssh")))
	switch defaultLane {
	case "", "native-ssh":
		engOpts = append(engOpts, regime.WithDefaultLane(sshLane))
		defaultLane = "native-ssh"
	case "none", "refuse":
		defaultLane = "none (unmatched targets refuse — fail closed)"
	default:
		log.Fatalf("actuation regime: TG_REGIME_DEFAULT_LANE=%q unsupported (use native-ssh or none)", defaultLane)
	}
	// (4b) gitops-mr lane (TG-122). DARK by default: no actuator injected (pendingActuator refuses) and it is
	//      async-refused on the synchronous verify path (returnsHandleNotOutcome). The real actuator (the
	//      concrete GitLab opener + structured renderer, modules/actuation/gitopsmr) is injected ONLY at the
	//      owner-present arm-live flip (slice 4) — TG_GITOPSMR_ARM set AND the allowlist carries write-side
	//      field_rules — and EVEN THEN routes beneath the mode chokepoint (its own ReadOnly()/gate re-check
	//      means it can only refuse at Shadow). Unarmed (the default) is byte-identical: no actuator, and no
	//      gitops-mr rule routes here anyway. The allowlist is parsed HERE, once, so the arm reads it and the
	//      sensor (6a) reuses it.
	gitopsAllowlist, gerr := parseGitOpsMRAllowlist(planeEnv("TG_GITOPSMR_ALLOWLIST", ""))
	if gerr != nil {
		log.Fatalf("actuation regime: %v (fail closed)", gerr)
	}
	var gitopsLaneOpts []regime.GitOpsMRLaneOption
	var gitopsActuator *gitopsmr.Actuator // shared with the k8s-declarative lane below (one MR channel)
	gitopsActuatorState := "DARK (no actuator — pendingActuator refuses)"
	if truthyEnv("TG_GITOPSMR_ARM") && gitopsHasFieldRules(gitopsAllowlist) {
		var aerr error
		gitopsActuator, aerr = gitopsmr.New(gitopsmr.Config{
			Opener:    gitopsmr.NewOpener(nil),
			Renderer:  gitopsmr.NewRenderer(nil),
			Allowlist: gitopsAllowlist,
			ModeGate:  chokepoint,
		})
		if aerr != nil {
			log.Fatalf("actuation regime: gitops-mr actuator (fail closed): %v", aerr)
		}
		gitopsLaneOpts = append(gitopsLaneOpts, regime.WithGitOpsMRActuator(gitopsActuator))
		gitopsActuatorState = fmt.Sprintf("ARMED real actuator (read_only=%v, %d repo(s) with field_rules) — beneath the mode chokepoint, inert until the mode is escalated", gitopsActuator.ReadOnly(), len(gitopsAllowlist))
	}
	gitopsLane := regime.NewGitOpsMRLane(gitopsLaneOpts...)
	log.Printf("actuation regime: gitops-mr lane actuator = %s", gitopsActuatorState)
	// (4c) Cisco WRITE lane posture (TG-85 write slice 4). The write lane (a distinct gated actuator, its
	//      config-mode transport, and the commit-confirmed primitive) is built but unrouted; this parses the
	//      operator's device policy fail-closed and REPORTS the posture every boot, so "is the Cisco write lane
	//      armed, and on what?" is answerable from the log instead of inferred from env vars. Unconfigured (the
	//      default) constructs nothing.
	ciscoWritePolicies, cerr := parseCiscoWriteDevices(planeEnv("TG_CISCO_WRITE_DEVICES", ""))
	if cerr != nil {
		log.Fatalf("actuation regime: %v (fail closed)", cerr)
	}
	log.Printf("actuation regime: cisco write lane = %s", ciscoWritePreflight(ciscoWritePolicies, chokepoint != nil && chokepoint.MayActuate(), truthyEnv("TG_CISCO_WRITE_ARM")))
	// (4d) The cisco-interactive lane (TG-85 arm-live slice): ALWAYS constructed so the regime set is
	//      complete and a rule naming it resolves; it carries the fail-closed pending leaf unless the
	//      operator both declared writable devices AND set TG_CISCO_WRITE_ARM, in which case each action's
	//      target resolves to ITS device's WriteModule (+ reversible-op registry when declared) through the
	//      per-target seam. A registry the never-auto floor refuses fails the BOOT here — an operator who
	//      declared a forbidden op learns it now, not at rollback time.
	ciscoLane, claneErr := ciscoInteractiveLaneFor(ciscoWritePolicies, chokepoint, truthyEnv("TG_CISCO_WRITE_ARM"))
	if claneErr != nil {
		log.Fatalf("actuation regime: %v (fail closed)", claneErr)
	}
	engine := regime.NewEngine(rules, []regime.Lane{sshLane, awxLane, proxmoxLane, gitopsLane, ciscoLane}, engOpts...)

	// Boot self-validation (fail-safe, non-fatal): every declared rule must resolve to a WIRED lane. A rule
	// that resolves to a regime with no wired lane (e.g. gitops-mr, a future lane) is logged as inert — an
	// operator config warning, not a crash (mutation is OFF regardless).
	wired, inert := 0, 0
	for _, r := range rules {
		if _, serr := engine.SelectLane(targetForSelector(r.Selector)); serr != nil {
			log.Printf("actuation regime: rule %q (regime %s) resolves to NO wired lane yet — inert (%v)", r.ID, r.Regime, serr)
			inert++
			continue
		}
		wired++
	}

	// (5) Append-only regime audit writer (REQ-1715, migration 0020): one row per resolution / launch /
	//     deferred verdict, each also chained into the governance ledger; no secret value is ever written
	//     (only a SecretRef reference). Constructed ready; the execute-path callers (RecordResolution /
	//     RecordActuation) land with the runner integration at the flip — today only the deferred-verify poll
	//     cron writes, and only after a launch, which cannot happen at Shadow.
	regimeAudit := regime.NewAudit(db.NewRegimeAuditWriteStore(pool), ledger)

	// (6) GLOBAL deferred-verify channel + poll cron (REQ-1709..1712) — armed ONLY when the awx-job launch
	//     client exists (its read-only GetJob is the poller). It OBSERVES; it launches nothing. At Shadow no
	//     launch ever reserves a pending record, so the cron polls an EMPTY queue. The pending-verification
	//     store is now the DURABLE pgx table (migration 0022, T-017-8): a launched job's deferred verify
	//     SURVIVES a worker restart — the flip-prerequisite — so a mutation whose effect was never confirmed
	//     stays a visible pending/unverified record instead of being forgotten. pool is always present here
	//     (wireActuationRegime is called only from the DB-present boot path); the in-memory fake stays the
	//     no-DB fallback so the channel is never left unwired.
	// (6a) gitops-mr SENSOR (TG-122 slice 2): the read-only MR-lifecycle poller over the per-repo operator
	//      allowlist (planeEnv — the allowlist carries per-repo api-scoped token REFS, so it is withheld on
	//      the triage plane like every actuation credential). Empty ⇒ no poller; the lane stays observable-by
	//      -nothing and any reserved gitops-mr launch resolves `unverified` at its bound (fail-safe). The
	//      poller NEVER emits `successful` — merge is not convergence; see modules/actuation/gitopsmr/poller.go.
	var gitopsPoller *gitopsmr.Poller
	if len(gitopsAllowlist) > 0 {
		var pollerOpts []gitopsmr.PollerOption
		// (6b) TG-555 reconcile-convergence reader (spec/017 REQ-1720): a read-only Argo CD Application status
		//      read over a private-CA k8s client, all SecretRefs (INV-13), actuate-plane-scoped like the
		//      allowlist (planeEnv — it carries the k8s SA token ref). When configured it lets a MERGED MR
		//      resolve `successful` — but ONLY once the target Argo Application is Synced+Healthy at the merge
		//      commit. Absent/unresolvable ⇒ the poller stays convergence-blind (merged rides to the bound,
		//      `unverified`); a merged MR is NEVER fabricated into success.
		convNS := planeEnv("TG_K8S_CONVERGENCE_NS", "argocd")
		convAPI := planeEnv("TG_K8S_CONVERGENCE_API", "")
		convTok := planeEnv("TG_K8S_CONVERGENCE_TOKEN", "")
		convState := "off (no TG_K8S_CONVERGENCE_API/_TOKEN — a merged MR rides to the bound, unverified)"
		if convAPI != "" && convTok != "" {
			conv, cerr := gitopsmr.NewConvergenceReader(gitopsmr.ConvergenceConfig{
				APIRef:    config.SecretRef(convAPI),
				TokenRef:  config.SecretRef(convTok),
				CARef:     config.SecretRef(planeEnv("TG_K8S_CONVERGENCE_CA", "")),
				Namespace: convNS,
			})
			if cerr != nil {
				convState = fmt.Sprintf("NOT wired (%v) — a merged MR rides to the bound, unverified", cerr)
			} else {
				pollerOpts = append(pollerOpts, gitopsmr.WithConvergenceReader(conv))
				convState = fmt.Sprintf("ARMED (Argo CD Application status, ns=%s) — a merged MR resolves `successful` only on Synced+Healthy at the merge commit", convNS)
			}
		}
		log.Printf("actuation regime: gitops-mr convergence reader = %s", convState)
		gitopsPoller = gitopsmr.NewPoller(gitopsAllowlist, pollerOpts...)
	}

	asyncState := "off (no awx-job launch client and no gitops-mr allowlist — no async lane to verify)"
	var launcher *regime.AsyncVerify
	if awxClient != nil || gitopsPoller != nil {
		bound := envDuration("TG_REGIME_VERIFY_BOUND", regime.DefaultVerificationBound)
		var pendingStore regime.PendingStore
		pendingKind := "in-memory (no DB pool)"
		if pool != nil {
			pendingStore = db.NewRegimePendingWriteStore(pool)
			pendingKind = "durable pgx (pending_verification, 0022 — survives restart)"
		} else {
			pendingStore = regime.NewMemPendingStore()
		}
		avOpts := []regime.Option{
			regime.WithGraduationSink(regimeGradSink{ladder: grad}),
			regime.WithVerificationBound(bound),
			regime.WithLogger(log.Printf),
			// The estate site authority (REQ-107): the deferred verdict excludes a surprise ONLY when the
			// estate knows BOTH sites and they differ — the same scoped author the interceptor calls. A nil
			// authority (no estate graph) is ignored inside the option: nothing is ever excluded (fail closed).
			regime.WithSiteAuthority(siteOf),
		}
		if pool != nil {
			// The deferred verifier's baseline arm, anchored at each record's LaunchedAt: without it every
			// alert already firing at launch adjudicates as the job's cascade minutes later — the async twin
			// of the 2026-07-28 synchronous false deviation.
			avOpts = append(avOpts, regime.WithPreAnomalous(db.OpenIncidentsBaseline(db.NewAlertHistoryStore(pool), coreingest.MaxOpenIncident)))
		}
		// Per-lane pollers (TG-122 slice 2): the channel is global, its tenants poll different surfaces. The
		// channel-wide fallback is the awx poller when the AWX client exists, else a refusing poller — a
		// record whose lane has NO registered poller then stays pending (transient error) and resolves
		// `unverified` at its bound, never a fabricated terminal.
		basePoller := regime.JobPoller(refusingJobPoller{})
		if awxClient != nil {
			basePoller = awxJobPoller{client: awxClient}
			avOpts = append(avOpts, regime.WithLanePoller(regime.RegimeAWXJob, awxJobPoller{client: awxClient}))
		}
		if gitopsPoller != nil {
			avOpts = append(avOpts,
				regime.WithLanePoller(regime.RegimeGitOpsMR, gitOpsMRJobPoller{poller: gitopsPoller}),
				// A gitops-mr proposal rides a human review cycle — hours/days, not awx-job minutes.
				regime.WithLaneVerificationBound(regime.RegimeGitOpsMR, envDuration("TG_GITOPSMR_VERIFY_BOUND", 72*time.Hour)))
		}
		av, verr := regime.NewAsyncVerify(pendingStore, basePoller, avOpts...)
		if verr != nil {
			log.Fatalf("actuation regime: async-verify channel (fail closed): %v", verr)
		}
		interval := envDuration("TG_REGIME_VERIFY_INTERVAL", time.Minute)
		go regimeVerifyLoop(av, regimeAudit, interval)
		launcher = av // TG-122 slice 0: the SAME channel is the runner's Reserve/BindHandle producer seam
		sensors := "awx-job poller"
		switch {
		case awxClient != nil && gitopsPoller != nil:
			sensors = fmt.Sprintf("awx-job + gitops-mr pollers (%d repo(s), gitops bound %s)", len(gitopsAllowlist), envDuration("TG_GITOPSMR_VERIFY_BOUND", 72*time.Hour))
		case gitopsPoller != nil:
			sensors = fmt.Sprintf("gitops-mr poller only (%d repo(s), bound %s; awx lane unpollable)", len(gitopsAllowlist), envDuration("TG_GITOPSMR_VERIFY_BOUND", 72*time.Hour))
		}
		asyncState = fmt.Sprintf("ARMED (poll every %s, bound %s; %s pending store, empty at Shadow; PRODUCER wired — handle-returning launches Reserve here; sensors: %s)", interval, bound, pendingKind, sensors)
	}

	log.Printf("actuation regime engine: WIRED (spec/017) — resolver over %d rule(s) (%d→wired lane, %d→inert), default lane=%s; lanes registered: native-ssh (effect leaf=%s, read_only=%v), awx-job (%s), proxmox (%s); async-verify=%s; audit=pgx regime_resolution/regime_actuation/deferred_verdict (0020, append-only) + governance ledger; mode=%s may_actuate=%v — every lane routes beneath the interceptor + mode chokepoint, no path actuates at Shadow",
		len(rules), wired, inert, defaultLane, sshLeaf.Capability(), sshLeaf.ReadOnly(), awxState, proxmoxState, asyncState, modeName, chokepoint.MayActuate())
	// Return the engine so the caller routes the execute activity's dispatch through it (SelectLane → LaneEffect
	// → the per-lane spec/013 interceptor). Before this wave the engine was built for its INERT side effects
	// (audit + async-verify loop) and discarded; now it is the actuation path's regime resolver (REQ-1700).
	return engine, launcher
}
