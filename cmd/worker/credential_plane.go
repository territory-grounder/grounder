package main

// credential_plane.go — THE PROCESS SPLIT (TG-153, spec/022 T-022-4, REQ-2203).
//
// THE DEFECT. Until 2026-08-04 one worker process did both of these things:
//
//	cmd/worker/main.go:816   tools := agent.NewReadOnlyToolSet()          // the LLM triage agent, driven over
//	                                                                     // UNTRUSTED alert / syslog / host text
//	cmd/worker/main.go:1987  sshactuation.NewNativeRunner(knownHosts, key) // the credential that MUTATES the estate
//
// Same process, same address space. Every credential improvement that landed before this one moved WHERE the
// secret lives (OpenBao, per-use scoping, cert auth, transit-wrapped seal); none moved WHICH PROCESS MAY
// FETCH IT. A worker that can ask OpenBao for the actuation key is, to an intruder, a worker that holds it —
// the extra hop costs one API call. The July-2026 HuggingFace intrusion that prompted this review was exactly
// that chain: untrusted data reached a processing worker, and from that foothold the actor harvested the
// credentials the worker could reach.
//
// THE SUBSTRATE HALF IS ALREADY LIVE AND PROVEN (2026-08-04, by hand): OpenBao policy `tg-triage-ro` reads
// secret/data/tg/* but DENIES tg/actuator and tg/proxmox; `tg-actuate-ro` reads ONLY tg/actuator and
// tg/proxmox; AppRoles `tg-triage` and `tg-actuate` bind to them. Measured: the triage token gets 403 on
// tg/actuator and 200 on hostdiag; the actuate token gets 200 on tg/actuator and 403 on hostdiag. OpenBao
// already refuses each plane the other's credentials. What was missing was a TG that ran as two processes
// under two identities, so that the substrate split had anything to bind to.
//
// THIS FILE IS THAT BINDING, AND ITS SHAPE IS THE POINT. The omission is at ACQUISITION, not at use:
// planeEnv refuses to hand this process the off-plane configuration AT ALL, so on the triage plane
// `TG_ACTUATION_SSH_KEY` never reaches config.SecretRef.Resolve, no ssh.Runner is constructed, no private key
// is read into memory, and no OpenBao lookup for tg/actuator is ever issued. That is a different property
// from `if plane == actuation { runner.Do() }` around an already-constructed runner: the guarded-object shape
// leaves the key in the address space, and an address space is what an intrusion reads.
//
// DEFAULT = `both`, and that is deliberate. This is a security fix that ships to every existing installation;
// one that broke every single-worker deployment on upgrade would be reverted, and a reverted control protects
// nobody. Under `both` planeEnv is the identity function over getenv and BOTH task queues are polled by the
// one process — byte-identical to the pre-TG-153 worker. The split is opt-in, per deployment, and the boot
// log says plainly which posture is running (a co-holding worker that printed "plane split OK" is precisely
// how this gap survived from TG-157 to TG-153).

import (
	"fmt"
	"log"
	"os"
	"sort"
	"strings"

	"go.temporal.io/sdk/worker"

	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/core/db"
)

// CredentialPlaneEnv is the operator's plane declaration. Unset ⇒ `both` (the pre-TG-153 posture).
const CredentialPlaneEnv = "TG_CREDENTIAL_PLANE"

// credentialPlane is THIS process's declared plane. It is a package var, set once by resolveCredentialPlane
// at the very top of main() — before any credential is read — because planeEnv is consulted from call sites
// scattered across the composition root and threading a parameter through all of them would guarantee that
// one site is missed, which is the same class of defect this whole ticket is about. Tests set and restore it.
var credentialPlane = credential.ProcessPlaneBoth

// actuationPlaneEnvKeys names EVERY configuration key through which this process could acquire a credential
// (or the target of one) that MUTATES the estate. A key on this list is withheld from a triage-plane process.
//
// It is a list, not a prefix match, because a prefix match is a promise that silently stops being true: a new
// `TG_PROXMOX_WRITE_TOKEN_REF` would not match `TG_ACTUATION_*` and would leak onto the triage plane with no
// test failing. TestActuationPlaneKeysCoverEveryActuationRefTheWorkerDeclares pins this list against the
// PlaneSet the worker actually asserts on, so adding an actuation reference to main() without adding it here
// fails CI.
//
// TG_ACTUATION_SSH_USER is deliberately ABSENT: it is a login NAME, not a credential, and it is read only to
// derive TG's own journal actor identity — which fails closed to "" anyway once the KEY ref is withheld.
var actuationPlaneEnvKeys = []string{
	"TG_ACTUATION_SSH_KEY",         // the SSH private-key REFERENCE the mutating runner authenticates with
	"TG_ACTUATION_SSH_KNOWN_HOSTS", // the host-key pin the mutating runner verifies against
	"TG_ACTUATION_SSH_HOST",        // the single-host actuation target (selects the mutating effect leaf)
	"TG_ACTUATION_SSH_IDENTITY",    // the login identity the mutating leaf presents
	"TG_ACTUATION_SSH_PER_TARGET",  // arms the per-target mutating lane (REQ-1717)
	"TG_ACTUATION_ALLOWED_UNITS",   // the operator's grant of what a mutating leaf may touch
	"TG_ACTUATION_ALLOWED_CONTAINERS",
	"TG_AWXJOB_LAUNCH_TOKEN_REF", // the AWX job-launch WRITE token (the second actuation channel)
	"TG_PROXMOX_TOKEN_REF",       // the proxmox guest-lifecycle WRITE token
	// TG-122: the gitops-mr repo allowlist CONTAINS per-repo api-scoped GitLab PAT refs (token_ref fields) —
	// withheld WHOLE on the triage plane so that plane can neither poll with nor (later) write through them.
	"TG_GITOPSMR_ALLOWLIST",
	// TG-122 slice 3: the op-class → propose mapping names actuation repos (keys into the allowlist above).
	// It carries no secret itself, but the triage plane has no business resolving a WRITE proposal against an
	// actuation repo — withheld for defense in depth, so a declarative op fails closed on the triage plane.
	"TG_GITOPSMR_PROPOSE_MAP",
	// TG-555: the read-only k8s ServiceAccount TOKEN ref for the gitops-mr reconcile-convergence reader — a
	// credential, withheld from the triage plane (which runs no gitops-mr poller). The non-secret API URL, CA,
	// and namespace are plain config, not credential keys, so they are not listed here.
	"TG_K8S_CONVERGENCE_TOKEN",
	// TG-423: the ssh-CA signed-cert acquisition path. Withheld WHOLE so a triage-plane process constructs no
	// sshca.Engine at all — it must never hold the token that mints the estate-mutating actuation certificate,
	// the same isolation TG_ACTUATION_SSH_KEY above relies on. (ADDR/ROLE/TOKEN_REF are how the mutating cert
	// is acquired; MOUNT/CA are its supporting config — all withheld so the engine cannot even be built.)
	"TG_SSHCA_ADDR",
	"TG_SSHCA_MOUNT",
	"TG_SSHCA_ROLE",
	"TG_SSHCA_TOKEN_REF",
	"TG_SSHCA_CA",
}

// triagePlaneEnvKeys names EVERY configuration key through which this process could acquire an
// UNTRUSTED-CONTENT reader — an alert body, a device syslog line, the stdout of a command run on an estate
// host, a ticket comment. A key on this list is withheld from an actuation-plane process, so that process
// registers no agent investigation tool and no alert ingest poller: there is no path by which
// attacker-authored text reaches the process that holds the key to mutate the estate.
//
// This is the direction people forget. The triage plane not holding the actuation key bounds what a popped
// TRIAGE worker yields; withholding untrusted content from the actuation worker is what stops the actuation
// worker from becoming the thing that gets popped.
//
// NOTE, stated rather than glossed: the actuation process still reads the estate TOPOLOGY (device inventory,
// NetBox/PVE object graph) because the interceptor's host-match and blast-radius gates are evaluated against
// it and a mutation gate reasoning over an empty graph is a gate that cannot refuse. Topology is a structured
// inventory read, not attacker-authored prose; the alert BODIES, syslog, host command output and ticket text
// — the fields an attacker actually controls — are all on this list.
var triagePlaneEnvKeys = []string{
	"TG_LIBRENMS_DEPLOYMENTS_AGENT_TOOLS", // sentinel: see planeEnvAlias below
	"TG_NETBOX_URL_AGENT_TOOLS",           // sentinel: the read-only NetBox investigation tool (TG-56) — an
	//                                        agent tool surfacing NetBox record TEXT into the model loop, so
	//                                        the actuation plane constructs none. The plain TG_NETBOX_URL is
	//                                        NOT on this list: the CMDB/topology read stays on both planes.
	"TG_SYSLOGNG_DEPLOYMENTS",    // device syslog reads (agent get-host-logs / search-host-logs)
	"TG_HOSTDIAG_DEPLOYMENTS",    // SSH df/free/systemctl output from estate hosts
	"TG_JOURNAL_DEPLOYMENTS",     // host journal / sudo reads (actor attribution)
	"TG_DISCOVERY_SYSTEMD_HOSTS", // service-observing discovery probes (SSH per host)
	"TG_DISCOVERY_DOCKER_HOSTS",
	"TG_LIBRENMS_ALERT_POLL_INTERVAL", // the LibreNMS active-alert PULL (untrusted alert bodies)
	"TG_PVE_LIVENESS_POLL_INTERVAL",   // the PVE liveness poller, which MINTS triage sessions
	"TG_AUTHLOG_POLL_INTERVAL",        // the authlog collector, which MINTS triage from device auth logs
	"TG_AUTHLOG_HOSTS",                // and the explicit host set it reads (TG-315)
}

// planeEnvAlias maps a SITE-SPECIFIC alias to the real environment key. TG_LIBRENMS_DEPLOYMENTS is read at
// several call sites for two different purposes — the agent's read-only investigation TOOLS (untrusted
// content: alert text, event-log lines) and the estate TOPOLOGY refresh (structured inventory the mutation
// gate needs). Only the first is withheld from the actuation plane, so the tool site reads through the alias
// and the topology site keeps reading the plain key. Naming the split explicitly beats a comment claiming a
// single key means two things.
var planeEnvAlias = map[string]string{
	"TG_LIBRENMS_DEPLOYMENTS_AGENT_TOOLS": "TG_LIBRENMS_DEPLOYMENTS",
	"TG_NETBOX_URL_AGENT_TOOLS":           "TG_NETBOX_URL",
}

// resolveCredentialPlane reads and installs the process's plane. It is called FIRST in main(), before any
// credential resolves, and it FAILS THE BOOT on an unrecognised value: a typo'd TG_CREDENTIAL_PLANE that
// silently fell back to `both` would leave an operator believing they had split the planes while the
// actuation key sat next to the agent — the worst of the three possible outcomes, because it is the one
// nobody investigates.
func resolveCredentialPlane(get func(k, def string) string) credential.ProcessPlane {
	p, err := credential.ParseProcessPlane(get(CredentialPlaneEnv, ""))
	if err != nil {
		log.Fatalf("%v", err)
	}
	return p
}

// planeEnv is the PLANE-SCOPED configuration reader, and the only way an off-plane key is read in this
// binary. For a key that belongs to a plane this process does not run it returns "" — regardless of def —
// so every downstream "absent ⇒ this subsystem is not constructed" branch (which every one of these call
// sites already had, because all of these features are optional) takes the absent path. The credential is
// not fetched, the runner is not built, the tool is not registered, and the process never holds it.
//
// Returning "" rather than def is load-bearing: a defaulted value would re-arm the very construction the
// plane is meant to omit.
//
// For an on-plane key, and for every key not on either list, planeEnv is exactly getenv — which is why the
// default `both` plane is byte-identical to the pre-TG-153 worker.
func planeEnv(k, def string) string {
	real := k
	if alias, ok := planeEnvAlias[k]; ok {
		real = alias
	}
	if withheldFromPlane(k, credentialPlane) {
		return ""
	}
	return getenv(real, def)
}

// withheldFromPlane reports whether key k is off-plane for plane p — the single predicate both planeEnv and
// the boot report consult, so the log can never describe a filter different from the one that ran.
func withheldFromPlane(k string, p credential.ProcessPlane) bool {
	if !p.HoldsActuation() && contains(actuationPlaneEnvKeys, k) {
		return true
	}
	if !p.HoldsTriage() && contains(triagePlaneEnvKeys, k) {
		return true
	}
	return false
}

func contains(list []string, k string) bool {
	for _, e := range list {
		if e == k {
			return true
		}
	}
	return false
}

// planeWithheldKeys is the boot report: which configured keys this plane REFUSED to read. It names only keys
// the operator actually SET, because "withheld 9 keys" over an .env that declared none of them is a number
// that reads like protection and measures nothing — the vacuity trap this repo has paid for repeatedly. An
// empty result on a split plane is itself the interesting fact and the caller says so.
func planeWithheldKeys(p credential.ProcessPlane) []string {
	var out []string
	for _, k := range append(append([]string{}, actuationPlaneEnvKeys...), triagePlaneEnvKeys...) {
		if !withheldFromPlane(k, p) {
			continue
		}
		real := k
		if alias, ok := planeEnvAlias[k]; ok {
			real = alias
		}
		if strings.TrimSpace(getenv(real, "")) == "" {
			continue // not configured here — nothing was withheld, so claiming so would be theatre
		}
		out = append(out, real)
	}
	sort.Strings(out)
	return dedupe(out)
}

func dedupe(in []string) []string {
	var out []string
	for i, s := range in {
		if i > 0 && s == in[i-1] {
			continue
		}
		out = append(out, s)
	}
	return out
}

// ---------------------------------------------------------------------------------------------------------
// THE DATABASE HALF: which Postgres role this process authenticates as (TG-164).
//
// TG-153 stopped at the secret store. Measured on the live box 2026-08-04, after it shipped:
//
//	postgres roles: postgres(super) | tg_migration | tg_runtime
//	worker          -> tg_runtime
//	worker-actuate  -> tg_runtime
//
// Two processes, two AppRoles, ONE database identity. OpenBao refused the triage worker the actuation KEY;
// the shared DB role let it write action_verdict, action_execution, interceptor_gate_verdict and
// policy_decision anyway — i.e. forge the RECORD of the actuation it could not perform, and poison the state
// the gates and the console read back. The withholding above bounds what a popped triage worker can FETCH;
// these two keys bound what it can WRITE. See core/db/plane_roles.go for how the two table lists were traced.
//
// OPT-IN, EXACTLY LIKE THE PROCESS SPLIT. Unset ⇒ TG_DB_DSN, byte-identical to the pre-TG-164 worker. There
// is no default plane DSN and no derived one: a guessed connection string is a boot failure at best and a
// connection to the wrong authority at worst.
const (
	TriageDBDSNEnv   = "TG_DB_DSN_TRIAGE"  // the tg_triage DSN — used only by a process whose plane is `triage`
	ActuateDBDSNEnv  = "TG_DB_DSN_ACTUATE" // the tg_actuate DSN — used only by a process whose plane is `actuation`
	sharedDBDSNEnv   = "TG_DB_DSN"         // the un-split DSN (tg_runtime) — the default and the fallback
	planeDSNFallback = "no plane DSN configured for this plane"
)

// planeDBDSN resolves the DSN this process opens its durable pool with, and the WHY for the boot log.
//
// The rules, and the reason each one is not the obvious alternative:
//
//   - plane `both` ⇒ TG_DB_DSN, always. A both-plane process serves tg.runner AND tg.actuate in one address
//     space, so it needs the union of both authorities — which is precisely tg_runtime. Handing it a plane
//     role would make it fail at the first off-plane write, and that is the failure this whole ticket says is
//     the worst kind: not a boot failure, a permission error deep inside an activity. A both-plane process
//     with a plane DSN SET is an operator who believes they split the database and did not, so it is called
//     out loudly — but it is not fatal, because docker-compose hands every service the same .env and a split
//     deployment legitimately defines both keys.
//
//   - a SPLIT plane ⇒ that plane's key when set, else TG_DB_DSN. Falling back rather than refusing keeps the
//     TG-153 upgrade path intact: a deployment that split the PROCESSES last week and has not yet created the
//     database roles keeps working, and its boot line says plainly that the DB half is not in force.
//
// get is os.Getenv, not the console-override getenv: a database cannot supply the address of the database it
// lives in (see boot_config.go). Both keys are on bootConfigForbiddenEnvKeys for the same reason.
func planeDBDSN(p credential.ProcessPlane, get func(string) string) (dsn, why string) {
	shared := strings.TrimSpace(get(sharedDBDSNEnv))
	key := ""
	switch {
	case p.HoldsTriage() && p.HoldsActuation():
		// `both`: no plane key applies. Report any that are set — silently ignoring them is how an operator
		// ends up believing a control is armed that is not.
		var stray []string
		for _, k := range []string{TriageDBDSNEnv, ActuateDBDSNEnv} {
			if strings.TrimSpace(get(k)) != "" {
				stray = append(stray, k)
			}
		}
		if len(stray) > 0 {
			return shared, fmt.Sprintf("%s (plane=%s holds BOTH queues, so it needs the union of both authorities: %v IGNORED — a plane role would fail this process at its first off-plane write)", sharedDBDSNEnv, p, stray)
		}
		return shared, sharedDBDSNEnv + " (plane=both — the un-split posture, unchanged)"
	case p.HoldsTriage():
		key = TriageDBDSNEnv
	case p.HoldsActuation():
		key = ActuateDBDSNEnv
	}
	if v := strings.TrimSpace(get(key)); v != "" {
		return v, key
	}
	return shared, fmt.Sprintf("%s — %s (set %s to split the database authority too)", sharedDBDSNEnv, planeDSNFallback, key)
}

// planeDBDSNFromEnv is the ONE site in this binary that reads a plane DSN from the real environment. It is
// separated from planeDBDSN (which takes an injected getter, so the resolution rules are unit-testable
// without mutating the process environment) and is named in cmd/worker's direct-env-read allowlist for the
// same reason installBootConfig is: a connection string cannot be served by the connection it names.
func planeDBDSNFromEnv(p credential.ProcessPlane) (dsn, why string) {
	return planeDBDSN(p, os.Getenv)
}

// planeWithheldTables names the tables THIS plane's database role must not be able to write. It is the input
// to the boot self-check: the triage plane must not write what records or authorises an actuation, and the
// actuation plane must not write the untrusted-content corpus a mutation is grounded in.
//
// A `both` process withholds NOTHING and that is correct, not an oversight — it runs both planes, so there is
// no off-plane half to withhold. The boot report says so rather than printing an empty list that reads like a
// clean bill of health.
func planeWithheldTables(p credential.ProcessPlane) []string {
	switch {
	case p.HoldsTriage() && p.HoldsActuation():
		return nil
	case p.HoldsTriage():
		return db.ActuationAuthorityTables
	case p.HoldsActuation():
		return db.TriageContentTables
	}
	return nil
}

// ---------------------------------------------------------------------------------------------------------
// THE TEMPORAL HALF: which queues this process polls.
//
// planeWorker is the slice of go.temporal.io/sdk/worker.Worker this composition root actually uses
// (RegisterWorkflow / RegisterActivity / Start / Run / Stop). It exists so main() can hold EITHER a real
// worker or the off-plane stub in the same variable, without cmd/worker taking a direct dependency on the
// Nexus registration surface it never touches. A real worker.Worker satisfies it.
//
// offPlaneWorker registers NOTHING and polls NOTHING. It is what the ACTUATION plane gets in place of the
// tg.runner worker, so the triage workflow and its ~30 activities — above all InvestigateActivity, which
// drives the LLM agent over untrusted alert/syslog/host content — are not merely idle in that process. They
// are absent from its registry, and it never asks Temporal for their tasks. Symmetrically the TRIAGE plane
// gets one of these in place of the tg.actuate worker: an actuation task cannot be delivered to a process
// that does not poll for it, whatever an intruder inside that process would like.
//
// It is NOT a silent discard: Start/Run REFUSE loudly, so a wiring mistake that handed the off-plane worker
// to the run loop fails closed at boot rather than producing a process that looks healthy and serves nothing.
// That distinction matters here more than usual — "registered, green, and reached by nothing" is this
// codebase's signature defect, and it would be an especially poor way to discover that the actuation plane
// had stopped executing anything.
// ---------------------------------------------------------------------------------------------------------

type planeWorker interface {
	RegisterWorkflow(w any)
	RegisterActivity(a any)
	Start() error
	Run(interruptCh <-chan interface{}) error
	Stop()
}

// compile-time proof that the real SDK worker satisfies the narrow seam.
var _ planeWorker = (worker.Worker)(nil)

type offPlaneWorker struct {
	queue string
	plane credential.ProcessPlane
}

func newOffPlaneWorker(queue string, plane credential.ProcessPlane) *offPlaneWorker {
	return &offPlaneWorker{queue: queue, plane: plane}
}

func (o *offPlaneWorker) RegisterWorkflow(any)         {}
func (o *offPlaneWorker) RegisterActivity(any)         {}
func (o *offPlaneWorker) Stop()                        {}
func (o *offPlaneWorker) Start() error                 { return o.refuse() }
func (o *offPlaneWorker) Run(<-chan interface{}) error { return o.refuse() }

func (o *offPlaneWorker) refuse() error {
	return fmt.Errorf("credential plane split (TG-153): this process declared %s=%s, so it does NOT poll %s — refusing to start a worker on a queue this plane must not serve, because doing so would silently re-merge the two planes the deployment split", CredentialPlaneEnv, o.plane, o.queue)
}

// PostureComponent is the runtime_posture key this process publishes under (TG-112).
//
// THE DEFECT. Both worker processes published the literal "worker", and runtime_posture is keyed on
// `component` with ON CONFLICT DO UPDATE — so the two planes shared ONE row and whichever heartbeated
// last won. Measured 2026-08-06: exactly one row, `component=worker`. The grounder reads that single key
// and reports it on /v1/whoami and /v1/governance as "the worker's posture", so the ACTUATION plane —
// the only plane that can mutate the estate — was unrepresentable.
//
// It has not yet caused a wrong answer, because both planes currently publish the same values
// (may_actuate=true, effect_capability=actuation.local.readonly). It becomes wrong the moment they
// diverge, which is exactly when mutation is switched on. The metrics half of this was already fixed —
// tg_may_actuate carries a `plane` label — and the table was left behind.
//
// `both` keeps the bare "worker" key so a single-process deployment is byte-identical to before.
func PostureComponent(plane credential.ProcessPlane) string {
	switch plane {
	case credential.ProcessPlaneTriage:
		return "worker-triage"
	case credential.ProcessPlaneActuation:
		return "worker-actuation"
	default:
		return "worker"
	}
}
