// Package safety implements the inviolable mechanical safety core of Territory Grounder.
//
// Provenance: [F] founding "graded fail-closed autonomy" · [R] paradigm-rule 8 ·
// [O] INV-09 (autonomy graded, fails closed), P0-5/P0-9 (mutation off by construction).
//
// Two properties are enforced here *by construction* and cannot be relaxed by any config:
//  1. Every safety enum's zero value is its MOST-restrictive option, so any un-initialised,
//     errored, or panicked path fails closed rather than open.
//  2. Global mutation is disabled until an explicit boot preflight proves the trust boundaries
//     are wired; it can never be flipped on implicitly.
package safety

import (
	"errors"
	"regexp"
	"sort"
	"strings"
)

// Band is the autonomy band. The ZERO VALUE is BandPollPause — the most restrictive band —
// so a zero/unmatched/errored classification escalates to the human circuit-breaker. [O] INV-09.
type Band int

const (
	// BandPollPause: pause and require human approval. This is the zero value on purpose.
	BandPollPause Band = iota
	// BandAutoNotice: act autonomously but notify the org's on-call in parallel.
	BandAutoNotice
	// BandAuto: act autonomously and silently (still only reversible actions).
	BandAuto
)

func (b Band) String() string {
	switch b {
	case BandAuto:
		return "AUTO"
	case BandAutoNotice:
		return "AUTO_NOTICE"
	default:
		return "POLL_PAUSE" // covers BandPollPause and any invalid value → fail closed
	}
}

// Verdict is the mechanical post-action verdict, written only by the verifier (never the acting
// model). [O] INV-10. There is no valid zero Verdict; callers must use ValidVerdict.
type Verdict string

const (
	VerdictMatch     Verdict = "match"
	VerdictPartial   Verdict = "partial"
	VerdictDeviation Verdict = "deviation"
)

// ValidVerdict reports whether v is one of the three mechanical verdicts. An unknown verdict is
// treated as a deviation by callers (never auto-resolved).
func ValidVerdict(v Verdict) bool {
	return v == VerdictMatch || v == VerdictPartial || v == VerdictDeviation
}

// neverAutoFloor is the non-configurable set of operation classes that may NEVER be auto-resolved,
// regardless of confidence, band, policy, or any sentinel. [R] paradigm-rule 8, [F] risk-appetite.
// Membership is a mechanical property, not a tunable one — so the map is UNEXPORTED and reachable only
// through IsNeverAuto/NeverAutoClasses. An exported map var could be mutated (`safety.NeverAutoFloor[...]
// = ...` or delete) by any package during a live canary, silently lifting a floor entry; unexporting
// makes the floor immutable-by-construction (the Phase-2 readiness review's §4.B.8 hardening).
var neverAutoFloor = map[string]struct{}{
	"mkfs": {}, "dropdb": {}, "zpool-destroy": {}, "zfs-destroy": {},
	"tofu-destroy": {}, "terraform-destroy": {}, "kubectl-delete": {}, "kubectl-drain": {},
	"credential-revoke": {}, "config-overwrite": {}, "reboot": {}, "jailbreak": {},
	// filesystem / block destroy
	"wipefs": {}, "shred": {}, "blkdiscard": {}, "dd": {},
	// LVM removal
	"vgremove": {}, "lvremove": {}, "pvremove": {},
	// ZFS non-destroy but irreversible
	"zfs-rollback": {}, "zpool-offline": {},
	// SQL destructive DDL/DML
	"drop-table": {}, "truncate-table": {}, "drop-database": {},
	// prune (irreversible reclaim)
	"docker-system-prune": {}, "docker-volume-prune": {}, "docker-network-prune": {},
	// host power
	"shutdown": {}, "halt": {}, "poweroff": {},
}

// IsNeverAuto reports whether opClass is on the mechanical never-auto floor. The op-class is normalized
// (trimmed + lowercased) before the lookup so a case or whitespace variant — "Reboot", " kubectl-delete "
// — can never slip past the floor. This is fail-closed: normalization can only make MORE inputs match the
// canonical lowercase-kebab floor slugs, never fewer, so no floor op can be smuggled through by casing.
func IsNeverAuto(opClass string) bool {
	_, ok := neverAutoFloor[strings.ToLower(strings.TrimSpace(opClass))]
	return ok
}

// NeverAutoClasses returns a fresh sorted copy of the floor slugs (for tests, docs, and the console's
// read-only "what can never auto-resolve" surface). It is a COPY — the caller cannot mutate the floor.
func NeverAutoClasses() []string {
	out := make([]string, 0, len(neverAutoFloor))
	for c := range neverAutoFloor {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// highRiskCategories are the alert categories whose remediation almost always ENDS in an infra change, so
// a session in one forces a POLL_PAUSE by default regardless of how reversible each individual op looks: a
// planned maintenance change, a security-incident containment (a ban / shun / isolate IS an infra change),
// and a deployment / release (modifies by definition). Ported from the predecessor's HIGH_RISK_CATEGORIES
// (classify-session-risk.py) — the category-driven band default the typed spine had dropped.
var highRiskCategories = map[string]struct{}{
	"maintenance":       {},
	"security-incident": {},
	"deployment":        {},
}

// HighRiskCategory reports whether an alert category is one that forces a poll by default. It is a
// SAFE-DIRECTION clamp: a true result can only RAISE review (force POLL_PAUSE), never lower a band. An
// unknown or empty category is NOT high-risk — the mechanical floor and reversibility gates still govern it,
// so a missing category never wrongly grants AUTO; it just adds no extra clamp.
func HighRiskCategory(category string) bool {
	_, ok := highRiskCategories[strings.ToLower(strings.TrimSpace(category))]
	return ok
}

// statefulDenyRE matches a stateful-workload identity — a database / queue / store / statefulset — in a
// target or op string. A restart/scale/reboot of such a workload can lose data during sync or break quorum
// (SeaweedFS is replication-0), so it can never be an auto action even when "reversible". Broad by design
// (safety): any DB/queue/store name or a statefulset clamps to POLL_PAUSE. Ported verbatim from the
// predecessor's _STATEFUL_DENY_RE (classify-session-risk.py).
var statefulDenyRE = regexp.MustCompile(`(?i)\b(?:etcd|postgres\w*|mysql\w*|mariadb\w*|seaweedfs|thanos|redis\w*|prometheus|` +
	`mongo\w*|cassandra|elasticsearch|opensearch|vault|consul|clickhouse|kafka|` +
	`zookeeper|rabbitmq|nats|minio|influxdb\w*|victoria\w*|loki|cockroach\w*|` +
	`mssql|sqlserver|oracle\w*|couch\w*|neo4j|qdrant|weaviate|valkey|` +
	`percona\w*|proxysql|graylog|` +
	`statefulset|[\w-]+-db|[\w-]+-database)\b|\bsts/`)

// IsStatefulWorkload reports whether any of the given strings (a target host, an op, its params) names a
// stateful workload whose disruption risks data loss / quorum loss. A mutating action on such a workload is
// never auto — the classifier clamps it to POLL_PAUSE.
func IsStatefulWorkload(parts ...string) bool {
	return statefulDenyRE.MatchString(strings.Join(parts, " "))
}

// restartClassRE matches a service-restart / reload / start class operation — the conservative-remediation
// verbs whose auto-grant the predecessor carved out (systemctl restart/start/reload, docker(-compose) restart,
// kubectl rollout restart, a pct/qm guest reboot). It also matches the bare op_class TOKENS of every actuatable
// restart/reload/start class — `restart-service`, `reload-service`, `start-service`, `restart-container` (and
// their reversed spellings) — because the self-protected control-plane guard is fed only the terse (op, op_class)
// pair, not the built argv (see temporal/runner/activities.go SelfProtectedRestart). Missing a class token here
// silently disables the orphan-the-session veto for that class, so this MUST list every op-class the effect
// leaves can actuate. Used to decide whether an action is a restart (for the self-protected control-plane
// guard), NOT to grant autonomy on its own.
var restartClassRE = regexp.MustCompile(`(?i)\b(?:` +
	`systemctl\s+(?:restart|start|reload|reload-or-restart|try-restart)|` +
	`docker(?:\s+compose)?\s+restart|` +
	`kubectl\s+rollout\s+restart|` +
	`(?:pct|qm)\s+reboot|` +
	`restart[-_]service|service[-_]restart|` +
	`start[-_]service|service[-_]start|` +
	`reload[-_]service|service[-_]reload|` +
	`restart[-_]container|container[-_]restart)\b`)

// IsRestartClass reports whether any of the given strings names a service-restart / reload class operation.
func IsRestartClass(parts ...string) bool {
	return restartClassRE.MatchString(strings.Join(parts, " "))
}

// destructiveOpRE matches an irreversible / destructive operation in the ACTUAL op string, independent of
// whatever op_class the model declared. This is the server-side backstop for INV: "a plan cannot hide a
// mutation" — a proposal that declares op_class="restart-service" but whose op is `dropdb prod` is caught
// here and forced to the never-auto floor. Covers the floor verbs the classifier must never trust the model
// to self-report: fs-destroy, LVM/ZFS removal, SQL drop/truncate, k8s resource deletion, prune, host power,
// terraform destroy, credential revoke.
var destructiveOpRE = regexp.MustCompile(`(?i)\b(?:` +
	`mkfs\w*|wipefs|shred|blkdiscard|dd\b|` + // filesystem destroy
	`vgremove|lvremove|pvremove|` + // LVM
	`zpool\s+(?:destroy|offline)|zfs\s+(?:destroy|rollback)|` + // ZFS
	`drop\s+(?:table|database|schema)|truncate\s+table|dropdb|` + // SQL
	`rm\s+-rf|rmdir|` + // generic delete
	`terraform\s+destroy|tofu\s+destroy|` + // IaC destroy
	`shutdown|halt|poweroff|` + // host power
	`(?:qm|pct)\s+(?:destroy|reset)|` + // Proxmox guest destroy (irreversible) / hard reset — predecessor floor
	`revoke|` + // credential/cert revoke
	`kubectl\s+delete\s+(?:pvc|persistentvolumeclaim|pv|persistentvolume\w*|namespace|ns|secret)|` + // k8s destructive delete (full spellings too — a `delete persistentvolumeclaim` is data loss)
	`helm\s+(?:uninstall|delete|rollback)|` + // helm teardown: uninstall/delete a release + its PVCs, or rollback a revision
	`kubectl\s+apply\b[^|;&]*--prune|` + // kubectl apply --prune deletes any resource absent from the manifest
	`docker\s+(?:system|volume|network)\s+prune|` + // docker prune
	`certbot\s+revoke` +
	`)\b`)

// IsDestructiveOp reports whether the actual operation (its command / params, not the model-declared class)
// is irreversible/destructive. The classifier uses it to override a model that under-declares its op's
// blast radius, forcing POLL_PAUSE.
func IsDestructiveOp(parts ...string) bool {
	return destructiveOpRE.MatchString(strings.Join(parts, " "))
}

// FailLane distinguishes the two lanes of the two-lane fail model. [O] INV / [F] two-lane principle.
type FailLane int

const (
	// LaneRemediation fails CLOSED: absent a committed prediction/authorization, deny the action.
	// Zero value on purpose — an unspecified lane is treated as the mutation lane.
	LaneRemediation FailLane = iota
	// LaneAdvisory fails OPEN: triage/context degrade to pre-feature behaviour on error.
	LaneAdvisory
)

// ErrMutationDisabled is returned whenever a mutating path is attempted while mutation is off.
var ErrMutationDisabled = errors.New("safety: mutation is disabled (mutation_enabled=false)")

// ErrPreflightNotGreen is returned when enabling mutation (a mode escalation into Semi-auto/Full-auto, or
// marking the boot preflight green) is attempted before the boot preflight passed.
var ErrPreflightNotGreen = errors.New("safety: cannot enable mutation — boot preflight is not green")

// The process-global mutation switch was the retired core/safety.MutationGate. It has been ABSORBED into the
// mode-driven actuation chokepoint (mutation_chokepoint.go, spec/015 T-015-13, REQ-1520/1521): the active mode
// is now the single source of truth for "may this action actuate?", so there is no separate enabled/preflight
// gate object to keep in sync. The proof obligation below (PreflightProver) is preserved and is discharged by
// Chokepoint.ProvePreflight; enabling actuation is a policy.ModeController transition into Semi-auto/Full-auto,
// gated on that green preflight. See docs/adr/0013-mode-is-the-actuation-chokepoint.md.

// PreflightProver is the proof obligation that must be discharged before mutation can be enabled: its
// SelfTest must pass, meaning the wired-by-construction actuation chain is fully assembled. It is an
// interface — satisfied by core/actuate.Interceptor — so the mechanical safety core need not import the
// actuate package (which would be an import cycle). The enable transition thus lives inside the safety
// core while being bound to a proof only a wired interceptor can supply. [O] INV-21/S8-5.
type PreflightProver interface {
	// SelfTest returns nil only when every governed collaborator of the actuation chain is wired.
	SelfTest() error
}

// The former EnableMutation (proof-gated flip of the retired gate) and the gate's GuardMutation are absorbed
// into the mode chokepoint (mutation_chokepoint.go): Chokepoint.ProvePreflight discharges the proof obligation
// (marks the preflight green without enabling), Chokepoint.GuardMutation refuses unless MayActuate, and
// enabling actuation is a policy.ModeController transition into Semi-auto/Full-auto gated on the green
// preflight. See docs/adr/0013-mode-is-the-actuation-chokepoint.md (REQ-1520/1521).
