package main

import (
	"log"
	"net/http"

	"github.com/territory-grounder/grounder/core/httpfailover"
)

// installPVEFailover layers the Proxmox cluster failover (TG-572) over http.DefaultTransport, ON TOP of the
// egress meter (so every re-addressed retry is still metered and, in enforce mode, still allowlisted — the
// TG_PVE_FAILOVER_URLS hosts are declared destinations by the _URLS suffix).
//
// The group is every configured Proxmox API base URL — the estate-read TG_PVE_URL, the actuation
// TG_PROXMOX_BASE_URL, and the operator's TG_PVE_FAILOVER_URLS (the other cluster members). A Proxmox cluster
// serves /api2/json from any quorate member and an API token is cluster-wide, so a request addressed to a dead
// node is re-addressed to a live one. Unset TG_PVE_FAILOVER_URLS ⇒ a group of at most the configured URLs,
// which the transport treats as a pass-through when fewer than two distinct members remain.
//
// SCOPE: clients that resolve to http.DefaultTransport (every PVE reader and the actuation leaf in the default,
// TLS-verifying posture). estateHTTPClient(insecure=true) owns its Transport and is NOT covered; prod runs with
// TLS verification on (TG_PVE_INSECURE empty), so nothing it serves to PVE bypasses the failover.
func installPVEFailover() {
	members := append([]string{getenv("TG_PVE_URL", ""), getenv("TG_PROXMOX_BASE_URL", "")},
		splitTokens(getenv("TG_PVE_FAILOVER_URLS", ""))...)
	t := httpfailover.New(http.DefaultTransport, members, log.Printf)
	if len(t.Members()) < 2 {
		log.Printf("pve failover (TG-572): OFF — %d distinct Proxmox API endpoint(s) configured; set TG_PVE_FAILOVER_URLS to the other cluster members so one dead node does not blind every PVE reader", len(t.Members()))
		return
	}
	http.DefaultTransport = t
	log.Printf("pve failover (TG-572): ARMED over %d cluster API members %v — a transport failure on one is retried on the next; any HTTP response is final", len(t.Members()), t.Members())
}
