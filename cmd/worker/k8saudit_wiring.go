package main

// wireK8sAuditReader registers the k8s audit-log actor-evidence reader (spec/023 T-023-9) — OUT of
// main() per the TG-501 ratchet. Config-gated exactly like the journal reader it patterns on: no
// declared control plane (TG_K8SAUDIT_DEPLOYMENTS unset) ⇒ not registered, k8s subjects read
// unattributable, and the boot line says so. PLANE-SCOPED (TG-153): it SSHes an estate control plane
// and reads apiserver audit log text — triage plane only, hence planeEnv.

import (
	"log"
	"time"

	"github.com/territory-grounder/grounder/adapters/actorevidence"
	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/modules/actorevidence/k8saudit"
	"github.com/territory-grounder/grounder/modules/observability/syslogng"
)

func wireK8sAuditReader(readers []actorevidence.Reader, resolver *credential.AuditedResolver, planeEnv, getenv func(string, string) string) []actorevidence.Reader {
	planes := k8saudit.ParseAccess(planeEnv("TG_K8SAUDIT_DEPLOYMENTS", ""))
	if len(planes) == 0 {
		log.Printf("attribution: k8s audit reader NOT registered (TG_K8SAUDIT_DEPLOYMENTS unset/empty on this plane) — k8s subjects read unattributable")
		return readers
	}
	runner := syslogng.NewNativeRunner(getenv("TG_JOURNAL_KNOWN_HOSTS", "") /* == k8saudit.KnownHostsEnv; a LITERAL so the env-parity guard can see it */)
	log.Printf("attribution: k8s audit-log reader armed (%d control plane(s), read-only SSH via the credential engine) — WHO-CHANGED-THIS active for cluster objects", len(planes))
	// A bounded per-plane read (the pve reader's pattern): an audit log is large and a control plane can
	// be slow, but a hung read must never stall triage — the module's compiled ceiling caps this anyway.
	return append(readers, k8saudit.New(planes, runner, resolver, k8saudit.WithTimeout(10*time.Second)))
}
