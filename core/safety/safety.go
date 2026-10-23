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
	// NETWORK-CATASTROPHIC (the predecessor's `irreversible:network-catastrophic` floor entry). These are
	// REMOTELY-UNRECOVERABLE lockouts: they erase a device's config, kill its routing, or reset an interface,
	// and the estate they partition includes the path TG would need to undo them. The predecessor's risk
	// appetite put them on the hard floor even while the rest of the network tier was gate-governable, and
	// its own network estate's syntax validator names the same verbs as dangerous.
	"write-erase": {}, "erase-startup-config": {}, "erase-nvram": {}, "erase-flash": {},
	"no-ip-routing": {}, "default-interface": {}, "clear-configure-all": {},
	"no-interface": {}, "interface-shutdown": {},
	// The teardown verbs one level down from a full config erase — a removed VLAN, route, ACL or trunk
	// partitions exactly as effectively and is not undoable from the far side of the partition.
	"vlan-delete": {}, "route-delete": {}, "acl-delete": {}, "trunk-remove": {}, "no-spanning-tree": {},
	// CODE-DEPLOY / REPO-WRITE (the predecessor's `code-deploy-or-repo-write` HELD class, which it kept OUT
	// of its gate-governable set — permanently a human decision). Deploying unreviewed code and destroying
	// refs are the same hazard from two directions: the review flow that makes a change safe is the thing
	// being bypassed, and a deleted ref/pipeline/deploy-key takes its own audit trail with it.
	"force-push": {}, "branch-delete": {}, "tag-delete": {}, "ref-delete": {},
	"deploy-key-revoke": {}, "pipeline-delete": {}, "environment-destroy": {},
	"repo-delete": {}, "release-delete": {}, "runner-unregister": {},
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
// (safety): any DB/queue/store name or a statefulset clamps to POLL_PAUSE. Ported from the predecessor's
// _STATEFUL_DENY_RE (classify-session-risk.py).
//
// ★ NO LEADING \b — deliberately. The port carried a word-boundary anchor tuned to the predecessor's naming,
// and on THIS estate's unbroken hostnames it made the whole control INERT: `dc1cl01mariadb01` has no word
// boundary between "01" and "mariadb", so IsStatefulWorkload returned FALSE for a real MariaDB host while
// returning TRUE only for the bare word "mariadb". A clamp meant to stop TG auto-restarting a database
// mid-sync had almost certainly never fired on a real target. Substring matching over-matches by design:
// a false positive costs one extra human review, a false negative costs a database.
//
// The TRAILING \b had the identical defect at the other end: `openbao01` has no boundary after "openbao",
// so the anchor rejected it. `mariadb\w*` survived only because its own \w* consumed the digits. Both
// anchors are gone; entries match as substrings.
var statefulDenyRE = regexp.MustCompile(`(?i)(?:etcd|postgres\w*|mysql\w*|mariadb\w*|seaweedfs|thanos|redis\w*|prometheus|` +
	`mongo\w*|cassandra|elasticsearch|opensearch|vault|openbao|consul|clickhouse|kafka|` +
	`zookeeper|rabbitmq|nats|minio|influxdb\w*|victoria\w*|loki|cockroach\w*|` +
	`mssql|sqlserver|oracle\w*|couch\w*|neo4j|qdrant|weaviate|valkey|` +
	`percona\w*|proxysql|graylog|` +
	`statefulset|[\w-]+-db|[\w-]+-database)|sts/`)

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
	`docker(?:\s+compose)?\s+start|` +
	`restart[-_]container|container[-_]restart|` +
	// start-container arrived as the first verb added as pure registry DATA, and this list is one of the three
	// places the registry-driven oracles made it announce itself. `docker start` can orphan the session issuing
	// it exactly as `systemctl start` can, so it belongs to the same self-protected class.
	`start[-_]container|container[-_]start|` +
	// start-guest was MISSING, and it is the class with the most demonstrated autonomy on this estate
	// (219 hands-off heals across 14 hosts). A guest lifecycle verb can orphan the very session issuing it
	// when the target guest hosts the control plane, which is exactly what the veto exists to prevent.
	// The oracle in safety_registry_test.go now asserts this list against the LIVE registry, so the next
	// lifecycle op-class cannot be added without either matching here or failing CI.
	`start[-_]guest|guest[-_]start|stop[-_]guest|guest[-_]stop|` +
	`stop[-_]service|service[-_]stop|stop[-_]container|container[-_]stop|` +
	`recreate[-_]container|container[-_]recreate)\b`)

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
	`certbot\s+revoke|` +
	// ---- NETWORK-CATASTROPHIC (predecessor: irreversible:network-catastrophic) ----
	// Ported from the predecessor's own floor entry and the never-do list its network estate maintains, NOT
	// invented: `write erase`, `erase startup-config|nvram|flash`, `no ip routing`, `default interface`,
	// `clear configure all`. Each one erases the config, kills the routing, or resets the interface that TG's
	// OWN management path runs over — so there is no far side of the partition from which to undo it, which is
	// why the predecessor kept them on the hard floor even while the rest of its network tier was
	// gate-governable. `no interface` is here because the network estate's diff tooling separately classifies a
	// removed interface block as destructive; the teardown verbs one level down (a removed VLAN, route, ACL,
	// trunk or spanning-tree, a deleted link/bridge/port, a flushed ruleset) partition just as effectively.
	//
	// TWO DELIBERATE EXCLUSIONS, both following the predecessor rather than widening past it.
	// (1) `reload` — the predecessor recorded WHY it declined to match it: it collides with `systemctl reload`,
	// a conservative-carve remediation verb on this estate, so matching it would turn every service reload into
	// a poll (a behavior change, not a safety gain). A bare `shutdown` is already on this list under host power
	// and covers the interface-level spelling too, so the intent behind the doc's "global shutdown" entry is met.
	// (2) `no interface` / `no vlan` / `no router` are NOT regex branches. They are not in the predecessor's
	// pattern either, and unlike the verbs below they are plausible ENGLISH — this regex is fed the proposal's
	// RATIONALE as well as its op, and "there is no interface configured on that host" must not band a restart.
	// They are carried on the floor-slug list instead (`no-interface`, `vlan-delete`, …), which is where a
	// registered op-class meets them, and the registered-family oracle in core/policy pins that route.
	`write\s+erase|erase\s+(?:startup-config|start|nvram|flash)|` +
	`no\s+ip\s+routing|default\s+interface|clear\s+config(?:ure)?\s+all|` +
	`no\s+(?:ip\s+route|access-list|spanning-tree|switchport\s+trunk)|` +
	`ip\s+link\s+delete|brctl\s+del(?:br|if)|ovs-vsctl\s+del-(?:br|port)|` +
	`nft\s+flush\s+ruleset|iptables\s+-[FX]|` +
	// ---- CODE-DEPLOY / REPO-WRITE (predecessor: code-deploy-or-repo-write — a HELD class it kept OUT of its
	// gate-governable set, i.e. permanently a human decision) ----
	// Deploying unreviewed code and destroying refs are the same hazard from two sides: the review flow that
	// makes a change safe is the thing being bypassed, and a deleted ref / pipeline / deploy-key takes its own
	// audit trail with it. The gh/glab branches are the predecessor's own three. The FLAG-LEVEL git branches
	// close the gap its coarse `git-write` verb match could not express — to it, `git push` and
	// `git push --force` were indistinguishable, and only the second one destroys history.
	`git\s+push\b[^\n|;&]{0,120}?(?:--force-with-lease|--force|--delete|\s-f|\s-d)|` +
	`git\s+branch\b[^\n|;&]{0,120}?(?:--delete|\s-D|\s-d)|` +
	`git\s+tag\b[^\n|;&]{0,120}?(?:--delete|\s-d)|` +
	// The trailing `[a-z]*` is load-bearing, not decoration: the alternation sits inside a `\b(?:…)\b` wrapper,
	// so a match that ENDS mid-flag (`-f` of `-fdx`) has a word character after it, no boundary, and the whole
	// alternation rejects — the identical anchor defect that made statefulDenyRE inert one function down.
	`git\s+reset\s+--hard|git\s+clean\b[^\n|;&]{0,60}?-[a-z]*f[a-z]*|` +
	`git\s+update-ref\s+-d|git\s+filter-(?:branch|repo)|` +
	`(?:gh|glab)\s+(?:[\w.-]+\s+)*(?:pr|mr)\s+merge|` +
	`(?:gh|glab)\s+(?:release|repo|run|variable|ssh-key|deploy-key)\s+(?:create|delete)|` +
	`(?:gh|glab)\s+api\b[^\n]{0,200}?(?:-X|--method)\s+(?:DELETE|PUT|POST)|` +
	// Pipeline / environment / deploy-key / runner destruction is CLI-ANCHORED on purpose. A bare
	// `(?:pipeline|environment|runner)…delete` alternation would match the sentence "the pipeline was deleted"
	// in a model rationale and poll a restart for narrating history, which is noise rather than safety.
	`glab\s+(?:pipeline|ci|deploy-key|environment)\s+(?:delete|stop|revoke)|` +
	`gh\s+(?:run|cache|secret|workflow)\s+delete|` +
	`gitlab-runner\s+unregister|` +
	`deploy[-_]key\s+(?:delete|revoke|remove)` +
	`)\b` +
	// ---- branches that CANNOT live inside the \b(?:...)\b wrapper ----
	// A branch ending in \s fails the trailing \b: in `rm /var/log/x` the match ends on a space and the
	// next character is `/` — both non-word, so there is no boundary and the whole alternation rejects it.
	// That is the same anchor defect that made statefulDenyRE inert, one function down. These branches
	// therefore sit OUTSIDE the wrapper, each carrying its own LEADING \b — which is what still stops `rm`
	// matching inside "confirm", "transform" or "performance".
	//
	// WHY IT MATTERS: `rm -rf` was the entire generic-delete cover, so a plain `rm <path>` slipped past the
	// backstop that exists precisely to catch a proposal declaring a benign op_class while its op destroys
	// data — and a plain `rm` is the third most common mutation shape in the predecessor's history (153 of
	// 1,165). Emptying a file (`truncate -s 0`) destroys its contents just as surely and leaves the path
	// looking intact afterwards.
	`|\brm\s|\bunlink\s|\btruncate\s+-s\s*0`)

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

// ErrMutationDisabled is returned whenever a mutating path is attempted while the mode does not permit
// actuation. (The identifier keeps its historical name — it is matched at 15 call sites; the MESSAGE speaks
// the current model, TG-112.)
var ErrMutationDisabled = errors.New("safety: actuation refused — the mode does not permit it (may_actuate=false)")

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
